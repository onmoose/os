package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/version"
)

// registerSystem registers the box-level system routes. Only the one-time
// storage poll and the build-version read live here today; the live resource
// stream (GET /api/v1/system/live) is a raw SSE handler registered in Handler,
// not huma.
func (s *Server) registerSystem(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-system-storage", Method: "GET", Path: "/api/v1/system/storage",
		Summary: "Per-disk storage usage (used/total bytes) for the system-resources panel",
	}, s.systemStorage)
	huma.Register(api, huma.Operation{
		OperationID: "get-system-version", Method: "GET", Path: "/api/v1/system/version",
		Summary: "What this box is running: brain version and commit, host-agent version, UI image",
	}, s.systemVersion)
}

// SystemVersionDTO is the GET /api/v1/system/version body: what this box is
// running, component by component.
//
// Version and Commit are the **brain's** build identity — the repo SemVer
// (VERSION file, BUILD.md # Versioning — one version for the whole monorepo,
// DECISIONS.md 2026-07-16) plus the short git commit, both stamped at build
// time (internal/version). They keep their original meaning and position: the
// API is additive-only within a minor (BRAIN_UI_PROTOCOL.md # Versioning), so
// the two fields that shipped first are never repurposed to mean "the box".
//
// HostAgentVersion and UIImage describe the other two components of the box, so
// one read answers "what is this box running" rather than "what is this binary"
// (UPDATES.md # 8.4 step 5 — the box reports its versions; stream A is the
// host-agent, stream B is the brain and UI).
//
// Both are omitted when unknown, and **absent means "could not read it", never
// "not installed"**. The brain's own identity is compiled in and always
// present, so a partial answer is still a useful one — a host-agent that is
// briefly unreachable must not turn this endpoint into a 502 and cost the
// caller the two facts that are still true.
type SystemVersionDTO struct {
	Version          string `json:"version"`
	Commit           string `json:"commit"`
	HostAgentVersion string `json:"host_agent_version,omitempty"`
	UIImage          string `json:"ui_image,omitempty"`
}

// hostVersionReadTimeout bounds the host-agent leg of the version read. A var,
// not a const, so a test can shrink it and assert the degrade path is prompt
// without sleeping for real.
var hostVersionReadTimeout = 3 * time.Second

// systemVersion answers "what is this box running" from three sources with
// three different failure modes, and deliberately degrades rather than failing
// whole. The brain's own identity is compiled into this binary. The host-agent's
// is a socket round trip that can fail transiently. The UI's is a read of the
// staged control-plane compose, which is absent by design in dev.
//
// This is NOT systemStorage's posture (a host read failure there is a 502)
// because the two answer different questions. An empty storage panel would be a
// lie — it would render as "no disks". A version report missing one of three
// components is self-describing: the field is absent, and the caller can see
// exactly which one it could not learn. Failing the whole read would throw away
// the brain version, which is both always available and the one an updater
// needs first.
//
// Available to every signed-in user with no role gate, same posture as
// systemStorage: build identity isn't per-user or sensitive data.
func (s *Server) systemVersion(ctx context.Context, _ *struct{}) (*struct{ Body SystemVersionDTO }, error) {
	if _, ok := auth.FromContext(ctx); !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	out := SystemVersionDTO{
		Version: version.Version,
		Commit:  version.Commit,
	}

	// Bound the host round trip well under the host client's shared 30s timeout
	// (hostclient.New). Without this, "degrade instead of failing" degrades only
	// after the caller has already waited 30 seconds for a component that is
	// optional in the answer — which is the opposite of the point. A healthy
	// host-agent answers this off a local unix socket in milliseconds, so a
	// short bound costs nothing and a stalled one is reported as unknown
	// promptly.
	hostCtx, cancel := context.WithTimeout(ctx, hostVersionReadTimeout)
	defer cancel()
	if status, err := s.host.SystemStatus(hostCtx); err != nil {
		// Warn, not Error: the caller still gets a useful answer, and the
		// host-agent being unreachable already raises its own health issue
		// (HEALTH.md locus B). Logging this at Error would double-report a
		// condition that has an owner elsewhere.
		slog.Warn("system-version: host status read failed", "err", err)
	} else {
		out.HostAgentVersion = status.AgentVersion
	}

	if img, err := lifecycle.ControlPlaneUIImage(s.controlPlaneDir); err != nil {
		slog.Warn("system-version: control-plane ui image read failed", "err", err)
	} else {
		out.UIImage = img
	}

	return &struct{ Body SystemVersionDTO }{Body: out}, nil
}

// DiskSpaceDTO is one volume's fullness for the Storage bars: a human Label
// ("System", "Data") plus its free and total bytes (LOCAL_ANALYTICS.md #
// Real-time system resources). Used is derived UI-side as Total − Free.
type DiskSpaceDTO struct {
	Label      string `json:"label"`
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

// SystemStorageDTO is the GET /api/v1/system/storage body: one entry per
// mounted volume of interest the host-agent reports (OS drive always, data
// drive when present). Disks is always present, possibly empty (no reporter
// wired host-side); the panel then shows no Storage section.
type SystemStorageDTO struct {
	Disks []DiskSpaceDTO `json:"disks"`
}

// systemStorage proxies the host-agent's per-volume disk fullness to the UI as a
// one-time poll (the install-plan dialog reads the same SystemStatus the same
// way). Available to every signed-in user with no role gate — host-level storage
// state isn't per-user data, same posture as the live resource stream
// (LOCAL_ANALYTICS.md # Privacy model). A host read failure is a 502: the panel
// shows "storage unavailable" rather than a misleading empty disk.
func (s *Server) systemStorage(ctx context.Context, _ *struct{}) (*struct{ Body SystemStorageDTO }, error) {
	if _, ok := auth.FromContext(ctx); !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	status, err := s.host.SystemStatus(ctx)
	if err != nil {
		slog.Error("system-storage: host status read failed", "err", err)
		return nil, huma.Error502BadGateway("could not read system storage")
	}
	out := SystemStorageDTO{Disks: []DiskSpaceDTO{}}
	for _, d := range status.Disks {
		out.Disks = append(out.Disks, DiskSpaceDTO{
			Label:      d.Label,
			FreeBytes:  d.FreeBytes,
			TotalBytes: d.TotalBytes,
		})
	}
	return &struct{ Body SystemStorageDTO }{Body: out}, nil
}
