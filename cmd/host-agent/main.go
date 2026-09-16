// Command host-agent is the FAKE host-agent: it speaks the real
// BRAIN_HOST_PROTOCOL.md wire format over a real UNIX socket, but its host
// operations are canned (no LUKS, no apt, no PAM). This is the binary used in
// the inner dev loop (make dev / make run-agent).
// See docs/dev/running-locally.md for the real binary (cmd/host-agent-real).
//
// Env vars:
//
//	MOOSE_AGENT_SOCK  — UNIX socket path (default protocol.SocketPath)
//	MOOSE_STATE_DIR   — when set, persist the fake user maps (passwords +
//	                    roles) to <dir>/fake-shadow.json so accounts survive a
//	                    restart, standing in for /etc/shadow. When unset, the
//	                    maps are in-memory only and a restart forgets every
//	                    account. The dev stack exports this (same dir as the
//	                    brain's moose.db).
//	MOOSE_HEALTH_PATH — when set, back the storage category of GET
//	                    /v1/health/system from this file (read via the same
//	                    FilesystemHealthSource the real binary uses). When
//	                    unset, the storage category is an empty findings list
//	                    ("storage looks healthy").
//	MOOSE_DEV_AVAHI   — when "1", publish per-app .local names via the real
//	                    Avahi DBus publisher instead of the in-memory fake, so
//	                    <slug>.local resolves on the LAN (and from other
//	                    devices) in dev. Requires avahi-daemon running; runs
//	                    unprivileged. `make dev` sets this. All other host ops
//	                    stay fake — this only swaps the discovery publisher.
//	MOOSE_FAKE_NO_GPU — when "1", GET /v1/system/gpu reports no usable GPU
//	                    instead of the default synthetic Intel iGPU, so the
//	                    `gpu: true` install refusal is exercisable in dev.
//	MOOSE_FAKE_UPDATE_TARGET — the state GET /v1/system/update-target reports:
//	                    "none" (default), "available", "current", "refused",
//	                    "unreachable" or "disabled". The dev loop has no control
//	                    plane to update, so the honest default is "nothing to
//	                    offer"; the other values exist so the dashboard's update
//	                    surfaces can be built against every state.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/onmoose/moose/internal/hostagent"
	"github.com/onmoose/moose/internal/hostagent/avahipublisher"
	"github.com/onmoose/moose/internal/hostagent/healthsource"
	"github.com/onmoose/moose/internal/hostagent/netstate"
	"github.com/onmoose/moose/internal/protocol"
	"github.com/onmoose/moose/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String() + " (fake)")
		return
	}

	sockPath := os.Getenv("MOOSE_AGENT_SOCK")
	if sockPath == "" {
		sockPath = protocol.SocketPath
	}

	if err := os.RemoveAll(sockPath); err != nil {
		slog.Error("remove stale socket", "sock", sockPath, "err", err)
		os.Exit(1)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		slog.Error("listen", "sock", sockPath, "err", err)
		os.Exit(1)
	}
	defer ln.Close()
	_ = os.Chmod(sockPath, 0o660)

	// The discovery publisher is the only host op that can be made real
	// unprivileged (Avahi's default DBus policy allows any user to publish),
	// so dev can opt into real .local announcements while every other host op
	// stays fake. Default is the in-memory fake, keeping make run-agent and
	// the hermetic test-health.sh free of any avahi-daemon dependency.
	var pub hostagent.Publisher = hostagent.NewFakePublisher(protocol.AppHostSuffix)
	if os.Getenv("MOOSE_DEV_AVAHI") == "1" {
		pub = &avahipublisher.DBusPublisher{HostSuffix: protocol.AppHostSuffix}
		slog.Info("host-agent (fake) using real Avahi DBus publisher for .local names")
	}

	a := hostagent.New(nil, pub) // verifier wired after construction
	a.Verifier = hostagent.NewFakeVerifier(a)
	// The fake agent stands in for /etc/shadow, which the real binary persists
	// for free. Without a backing file, a `make dev` restart wipes every
	// account's password while the brain's SQLite keeps the user + session
	// rows, so a fresh login after clearing cookies fails. Persist into the
	// same MOOSE_STATE_DIR the brain uses (the dev stack exports it).
	if dir := os.Getenv("MOOSE_STATE_DIR"); dir != "" {
		statePath := filepath.Join(dir, "fake-shadow.json")
		if err := a.EnablePersistence(statePath); err != nil {
			slog.Error("host-agent (fake) load persisted user state", "path", statePath, "err", err)
			os.Exit(1)
		}
		slog.Info("host-agent (fake) persisting user state", "path", statePath)
	}
	if healthPath := os.Getenv("MOOSE_HEALTH_PATH"); healthPath != "" {
		a.Health = healthsource.New(healthPath)
		slog.Info("host-agent (fake) wired to storage health file", "path", healthPath)
	}
	// No journald in the dev loop, so stream real Docker container output via
	// `docker logs -f --timestamps`. The compose replica suffix (-1) is probed
	// automatically; falls back to the bare stem for standalone containers.
	a.Logs = &dockerLogSource{}
	// On Linux, wire real /proc counters so make dev shows live CPU and RAM.
	// On other platforms newSystemSampler returns nil and the agent falls back
	// to synthetic monotonic counters (agent.go:447).
	a.System = newSystemSampler()
	// No real data drive in the dev loop, so GET /v1/system/status reports a
	// canned free/total (≈412 GiB free of a 1 TiB drive) — enough for the
	// install plan's free_bytes to render a plausible figure natively.
	a.Disk = hostagent.NewFakeDiskReporter(412<<30, 1<<40)
	// No real drives either, so the Storage bars report two canned volumes
	// (System ≈18 GiB free of 64 GiB, Data ≈412 GiB free of 1 TiB) — the panel
	// shows both bars in dev without a second physical drive. Data matches the
	// FakeDiskReporter figure above so the two status fields stay coherent.
	a.DiskSpace = hostagent.NewFakeDiskSpaceReporter(
		protocol.DiskSpace{Label: "System", FreeBytes: 18 << 30, TotalBytes: 64 << 30},
		protocol.DiskSpace{Label: "Data", FreeBytes: 412 << 30, TotalBytes: 1 << 40},
	)
	// No real /dev/dri in the dev loop, so GET /v1/system/gpu reports a
	// synthetic Intel iGPU (render GID 104, Debian's usual `render` group) so
	// a `gpu: true` install exercises the full override path natively.
	// MOOSE_FAKE_NO_GPU=1 flips it to "no usable GPU" for the refusal path.
	gpu := protocol.SystemGPU{Present: true, Vendor: "intel", RenderGID: 104}
	if os.Getenv("MOOSE_FAKE_NO_GPU") == "1" {
		gpu = protocol.SystemGPU{}
		slog.Info("host-agent (fake) reporting no GPU (MOOSE_FAKE_NO_GPU=1)")
	}
	a.GPU = hostagent.NewFakeGPUReporter(gpu)
	// No NetworkManager in the dev loop either: a fixed plausible LAN set
	// keeps GET /v1/discovery/state's interfaces field stable regardless of
	// the dev box's real network.
	a.Net = hostagent.NewFakeNetState(netstate.LANInterface{Name: "eth0", Index: 2, IPv4: "192.168.1.20"})
	a.UpdateTarget = hostagent.NewFakeUpdateTargetReporter(fakeUpdateTarget())

	mux := http.NewServeMux()
	a.Mount(mux)

	slog.Info("host-agent (fake) listening", "sock", sockPath)
	srv := &http.Server{Handler: hostagent.LogRequests(mux)}
	if err := srv.Serve(ln); err != nil {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
