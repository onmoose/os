package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/hostclient"
	"github.com/onmoose/moose/internal/store"
	"github.com/onmoose/moose/internal/version"
)

// TestSystemStorage_RequiresAuth: the Storage poll needs a session like every
// non-allowlisted route.
func TestSystemStorage_RequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/v1/system/storage", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/v1/system/storage: want 401, got %d", resp.StatusCode)
	}
}

// TestSystemStorage_ReturnsDisks: an authenticated user gets the host-agent's
// per-volume figures mapped through, in order.
func TestSystemStorage_ReturnsDisks(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/system/storage", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/system/storage: want 200, got %d", resp.StatusCode)
	}
	body := decodeJSON[SystemStorageDTO](t, resp)
	if len(body.Disks) != 2 {
		t.Fatalf("want 2 disks, got %+v", body.Disks)
	}
	if body.Disks[0].Label != "System" || body.Disks[0].TotalBytes != 64<<30 {
		t.Errorf("System entry: got %+v", body.Disks[0])
	}
	if body.Disks[1].Label != "Data" || body.Disks[1].FreeBytes != harnessFreeBytes {
		t.Errorf("Data entry: got %+v", body.Disks[1])
	}
}

// TestSystemStorage_NoIdentity_401: the handler's own auth guard (belt over the
// middleware) returns 401 when no identity rode the context.
func TestSystemStorage_NoIdentity_401(t *testing.T) {
	s := &Server{}
	_, err := s.systemStorage(context.Background(), nil)
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", err)
	}
}

// TestSystemStorage_HostError_502: a host-agent read failure (dead socket) maps
// to 502, not a misleading empty-disk 200.
func TestSystemStorage_HostError_502(t *testing.T) {
	s := &Server{host: hostclient.New(filepath.Join(t.TempDir(), "absent.sock"))}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{
		User: store.User{ID: "u_x", Role: store.RoleMember},
	})
	_, err := s.systemStorage(ctx, nil)
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != http.StatusBadGateway {
		t.Fatalf("want 502, got %v", err)
	}
}

// TestSystemStorage_MemberAllowed: host-level storage isn't per-user data, so a
// member gets it too — no admin gate (same posture as the live resource stream).
func TestSystemStorage_MemberAllowed(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob001", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("GET", "/api/v1/system/storage", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET /api/v1/system/storage: want 200, got %d", resp.StatusCode)
	}
	body := decodeJSON[SystemStorageDTO](t, resp)
	if len(body.Disks) != 2 {
		t.Fatalf("member: want 2 disks, got %+v", body.Disks)
	}
}

// TestSystemVersion_RequiresAuth: build identity isn't secret, but it isn't on
// the unauthenticated allowlist either — a session is required like every other
// non-allowlisted route.
func TestSystemVersion_RequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/v1/system/version", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/v1/system/version: want 401, got %d", resp.StatusCode)
	}
}

// TestSystemVersion_ReportsBrainAndHostAgent: the whole point of the slice —
// one read answers "what is this box running", not just "what is this binary".
// The host-agent's version must come from the host round trip, which is why the
// harness reports a version that is deliberately not the brain's.
func TestSystemVersion_ReportsBrainAndHostAgent(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/system/version", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/system/version: want 200, got %d", resp.StatusCode)
	}
	body := decodeJSON[SystemVersionDTO](t, resp)
	if body.Version != version.Version || body.Commit != version.Commit {
		t.Errorf("brain identity: got %+v, want version=%q commit=%q", body, version.Version, version.Commit)
	}
	if body.HostAgentVersion != harnessAgentVersion {
		t.Errorf("host-agent version: got %q, want %q", body.HostAgentVersion, harnessAgentVersion)
	}
}

// TestSystemVersion_HostDown_StillAnswers is the behaviour that separates this
// endpoint from systemStorage: a dead host-agent must NOT cost the caller the
// brain version, which is compiled in and always true. Degrade, don't fail.
func TestSystemVersion_HostDown_StillAnswers(t *testing.T) {
	s := &Server{host: hostclient.New(filepath.Join(t.TempDir(), "absent.sock"))}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{
		User: store.User{ID: "u_x", Role: store.RoleMember},
	})
	out, err := s.systemVersion(ctx, nil)
	if err != nil {
		t.Fatalf("host down: want a 200 answer, got error %v", err)
	}
	if out.Body.Version != version.Version {
		t.Errorf("host down: brain version lost, got %q", out.Body.Version)
	}
	if out.Body.HostAgentVersion != "" {
		t.Errorf("host down: want host-agent version omitted, got %q", out.Body.HostAgentVersion)
	}
}

// TestSystemVersion_ReportsUIImage: with a staged compose present the UI image
// is reported, and it is the ref the brain reconciles to — the same file
// EnsureControlPlane reads.
func TestSystemVersion_ReportsUIImage(t *testing.T) {
	dir := t.TempDir()
	compose := "services:\n  moose-ui:\n    image: ghcr.io/onmoose/ui:v1.2.3\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatalf("stage compose: %v", err)
	}
	s := &Server{host: hostclient.New(filepath.Join(t.TempDir(), "absent.sock"))}
	s.SetControlPlaneDir(dir)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{
		User: store.User{ID: "u_x", Role: store.RoleMember},
	})
	out, err := s.systemVersion(ctx, nil)
	if err != nil {
		t.Fatalf("systemVersion: %v", err)
	}
	if out.Body.UIImage != "ghcr.io/onmoose/ui:v1.2.3" {
		t.Fatalf("ui image: got %q", out.Body.UIImage)
	}
}

// TestSystemVersion_NoControlPlaneDir_OmitsUI: dev has no staged compose. The
// field is absent rather than empty-string-present, and nothing errors.
func TestSystemVersion_NoControlPlaneDir_OmitsUI(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/system/version", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Asserting on the wire bytes, not the decoded struct: omitempty is the
	// contract ("absent means could not read"), and a decoded zero value cannot
	// tell an absent field from an empty one.
	if strings.Contains(string(raw), "ui_image") {
		t.Fatalf("dev (no control-plane dir): want ui_image absent, got %s", raw)
	}
}

// TestSystemVersion_MemberAllowed: no role gate, same posture as systemStorage.
func TestSystemVersion_MemberAllowed(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob001", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("GET", "/api/v1/system/version", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET /api/v1/system/version: want 200, got %d", resp.StatusCode)
	}
}

// TestSystemVersion_HostStalls_DegradesPromptly: a host-agent that accepts the
// connection but never answers must not hold this endpoint for the host
// client's full 30s timeout. The whole design is "degrade instead of failing",
// and degrading only after half a minute defeats it. Uses a socket that accepts
// and hangs — a dead socket fails fast and would not exercise the timeout.
func TestSystemVersion_HostStalls_DegradesPromptly(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "stalled.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without replying.
			go func() { <-stop; _ = c.Close() }()
		}
	}()

	prev := hostVersionReadTimeout
	hostVersionReadTimeout = 50 * time.Millisecond
	defer func() { hostVersionReadTimeout = prev }()

	s := &Server{host: hostclient.New(sock)}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{
		User: store.User{ID: "u_x", Role: store.RoleMember},
	})

	start := time.Now()
	out, err := s.systemVersion(ctx, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("stalled host: want a 200 answer, got %v", err)
	}
	if out.Body.Version != version.Version {
		t.Errorf("stalled host: brain version lost, got %q", out.Body.Version)
	}
	if out.Body.HostAgentVersion != "" {
		t.Errorf("stalled host: want host-agent version omitted, got %q", out.Body.HostAgentVersion)
	}
	// Generous bound: the point is "bounded by our timeout", not "bounded by
	// the host client's 30s".
	if elapsed > 5*time.Second {
		t.Errorf("stalled host: took %s, want prompt degradation", elapsed)
	}
}
