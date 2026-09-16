// Command host-agent-real is the production host-agent binary. The shared code
// here binds and serves the UNIX socket, mounts the hostagent HTTP handlers,
// and launches the brain container on first boot (CONTROL_PLANE.md # Locked:
// host-agent launches the brain container). Which host-op seams get wired onto
// the agent is build-profile-specific and lives in two build-tagged files:
//
//   - wiring_appliance.go (default, //go:build !hosted) — the full appliance
//     integration: real PAM, user management, the health/system reporters, and
//     the LAN discovery stack (NetworkManager via netstate + per-interface
//     Avahi announcements via avahipublisher, kept aligned by a network watcher).
//   - wiring_hosted.go (//go:build hosted) — the slim hosted-cloud variant
//     (ENVIRONMENT.md # How the profile is realized): the same kept seams with
//     the LAN/discovery stack compiled out (no NetworkManager, no Avahi, no
//     watcher). Built with `go build -tags hosted ./cmd/host-agent-real` for the
//     cloud image (#203/C2).
//
// Both variants wire real PAM (POST /v1/auth/verify-password) and real user
// management, so both builds are Linux + CGO and need libpam0g-dev +
// /etc/pam.d/moose and must run as root (pam_unix.so requires privilege). The
// appliance build additionally needs avahi-daemon running with the system DBus
// accessible; the hosted build does not (it publishes nothing).
//
// See docs/progress/host-agent-pam-verify.md, docs/progress/avahi-dbus-publisher.md,
// and docs/progress/slim-cloud-host-agent.md for full context and known gaps.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/onmoose/os/internal/hostagent"
	"github.com/onmoose/os/internal/hostagent/brainlaunch"
	"github.com/onmoose/os/internal/hostagent/controlplane"
	"github.com/onmoose/os/internal/hostagent/cpupdate"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
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
	// 0660 root:moose — brain's container UID is in the moose group.
	_ = os.Chmod(sockPath, 0o660)

	// buildAgent wires the host-op seams for this build profile and returns a
	// cleanup to run on shutdown (appliance: stop the network watcher + close the
	// Avahi/NM DBus connections; hosted: nothing). It is defined per build tag in
	// wiring_appliance.go (!hosted) / wiring_hosted.go (hosted).
	a, cleanup := buildAgent()
	defer cleanup()

	// The brain's launch config is built once and used twice: to launch the
	// brain at boot, and as the base of every control-plane update. Reusing it
	// is the point — an updated brain has to be identical to a first-boot one
	// except for the image, and a second builder would drift the first time a
	// mount or env var is added here (docs/progress/control-plane-update-transaction.md).
	brainCfg := brainLaunchConfig(sockPath)
	// POST /v1/jobs/system-update. Wired in both build profiles: UPDATES.md # 8
	// makes the stream-B transaction shared by appliance and hosted.
	a.Updater = cpupdate.Runner{
		Docker: cpupdate.NewCLIDocker(),
		Prober: cpupdate.HTTPProber{},
		Base: cpupdate.Options{
			ControlPlaneDir: brainCfg.ControlPlaneDir,
			BrainCfg:        brainCfg,
			SnapshotRoot:    filepath.Join(brainCfg.DataDir, "brain-snapshots"),
		},
	}

	mux := http.NewServeMux()
	a.Mount(mux)

	// First-boot brain bootstrap (CONTROL_PLANE.md # Locked: host-agent launches
	// the brain container; BUILD.md # First-boot brain bootstrap). Docker is
	// ready by here — host-agent.service is ordered After=docker.service. The
	// socket is already bound above, so the brain can reach it on first call.
	// Best-effort: a failure (including a refused protocol-major mismatch) leaves
	// host-agent serving its socket so the box stays diagnosable; it does not
	// tear host-agent down.
	brainCtx, brainCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	// Seed the brain's Docker transport (ingress network + socket-proxy) before
	// launching the brain — the brain reaches Docker only through this proxy
	// (CONTROL_PLANE.md # Docker socket exposure), and the brain then reconciles
	// Caddy + moose-ui through it. Best-effort, like the launch itself: a failure
	// leaves the brain degraded (no Docker reach) but host-agent keeps serving so
	// the box stays diagnosable.
	if err := brainlaunch.EnsureTransport(brainCtx, brainlaunch.NewCLIDocker(), brainCfg); err != nil {
		slog.Error("seed brain transport failed; host-agent continues serving", "err", err)
	}
	if err := brainlaunch.Launch(brainCtx, brainlaunch.NewCLIDocker(), brainCfg); err != nil {
		slog.Error("brain launch failed; host-agent continues serving", "err", err)
	}
	brainCancel()

	// The appliance release-manifest poll (RELEASE_MANIFEST.md), started last
	// and on purpose: it tells the box which version it *could* run, and nothing
	// about booting depends on it, so a slow or unreachable CDN must not sit in
	// front of anything above. A hosted box has a no-op here — its target comes
	// from the cloud, not from a signed broadcast (UPDATES.md # 8.1). A build
	// with no baked signing key does not poll at all.
	stopPoll, poller := startReleasePoll(brainCfg.DataDir)
	defer stopPoll()

	// The update-target loop: what tells this box which control plane to run
	// (UPDATES.md # 8.4 step 1, internal/hostagent/updatetarget). One loop for
	// both profiles — only the source and whether the box may apply without a
	// prompt differ, and both come from the build-tagged updateTargetSource.
	// Started last for the same reason the poll is: nothing about booting waits
	// on it.
	stopTarget := startUpdateTarget(brainCfg, a, poller)
	defer stopTarget()

	slog.Info("host-agent-real listening", "sock", sockPath)
	srv := &http.Server{Handler: hostagent.LogRequests(mux)}
	if err := srv.Serve(ln); err != nil {
		slog.Error("serve", "err", err)
		// os.Exit skips the deferred cleanup; run it by hand first.
		stopPoll()
		cleanup()
		os.Exit(1)
	}
}

// brainLaunchConfig builds the brain bootstrap config from the environment.
// Defaults are the production paths (BUILD.md # First-boot brain bootstrap); the
// image ref and bundled-tarball path are overridable so the QEMU test lane can
// point at its dev tag and baked bundle. The data root is fixed at /var/lib/moose
// (STORAGE.md), with the brain's SQLite state under it; the brain dials the same
// agent socket host-agent just bound.
func brainLaunchConfig(sockPath string) brainlaunch.Config {
	const dataDir = "/var/lib/moose"
	// The control-plane compose + caddy.json are staged under dataDir so the
	// brain's `docker compose up` bind-mounts caddy.json at a path the Docker
	// daemon resolves identically on host and in the brain container (the
	// same-path constraint — socket-proxy-compose-validation.md). The proxy image
	// + bundle default to the names baked by dev/test-qemu / the ISO build.
	controlPlaneDir := env("MOOSE_CONTROL_PLANE_DIR", filepath.Join(dataDir, "control-plane"))
	// The brain reads the environment-profile marker from inside its container,
	// which mounts neither /etc/moose nor anything covering it — so host-agent
	// must hand it across. Resolve the host marker path (the brain's own default,
	// overridable for tests) and mount it only when it exists as a regular file:
	// an unmarked appliance box has no marker, and a same-path bind of a missing
	// source would make Docker auto-create a root-owned directory there. The brain
	// reads its default /etc/moose/profile inside the container, so the mount is
	// same-path; see brainlaunch.Config.ProfileMarkerPath.
	profileMarker := env("MOOSE_PROFILE_PATH", profile.DefaultMarkerPath)
	if fi, err := os.Stat(profileMarker); err != nil || !fi.Mode().IsRegular() {
		profileMarker = ""
	}
	// Which brain to launch is the ledger's call when it has one: it records the
	// pair this box last *applied*, while the env default records what it last
	// *shipped with* — older by definition once an update has landed
	// (internal/hostagent/controlplane). brainlaunch leaves an existing brain
	// container alone, so this only decides the case where there is none to
	// leave: a first boot, or a box whose brain container was removed. Without
	// it, that second case silently rolls an updated box back to the baked image.
	brainImage, fromLedger := controlplane.ResolveBrainImage(controlPlaneDir, env("MOOSE_BRAIN_IMAGE", "moose-brain:latest"))
	// from_ledger, not src: CLAUDE.md reserves src for a source filesystem path.
	// Logged either way so the fallback is visible rather than silent — "which
	// brain did this box decide to run, and did an applied update decide it" is
	// the first question a bad update raises.
	slog.Info("resolved brain image", "image", brainImage, "from_ledger", fromLedger)

	return brainlaunch.Config{
		Image:         brainImage,
		ImageTar:      env("MOOSE_BRAIN_IMAGE_TAR", filepath.Join(dataDir, "brain-image.tar")),
		ContainerName: "moose-brain",
		DataDir:       dataDir,
		StateDir:      filepath.Join(dataDir, "state"),
		SocketPath:    sockPath,

		Network:            env("MOOSE_INGRESS_NETWORK", "moose-ingress"),
		ProxyImage:         env("MOOSE_PROXY_IMAGE", "tecnativa/docker-socket-proxy:v0.4.2"),
		ProxyImageTar:      env("MOOSE_PROXY_IMAGE_TAR", filepath.Join(controlPlaneDir, "images", "docker-socket-proxy.tar")),
		ProxyContainerName: "moose-docker-proxy",
		ControlPlaneDir:    controlPlaneDir,
		UIUpstream:         env("MOOSE_DASHBOARD_UI_UPSTREAM", "moose-ui:80"),
		// Empty by default: the control-plane compose then runs stock caddy:2-alpine
		// (the appliance, no ACME). The hosted image sets MOOSE_CADDY_IMAGE to the
		// caddy-dns/acmedns build for the wildcard cert (os #207/C3b).
		CaddyImage: env("MOOSE_CADDY_IMAGE", ""),
		// The control-plane catalog the brain syncs the Door-1 store from, plus the
		// asset cache dir (under dataDir, so it rides the brain's DataDir mount).
		// CatalogURL empty ⇒ the brain uses its own default (the public control
		// plane); the air-gapped lane overrides it to an inert address and seeds the
		// store from a staged snapshot file instead (brainlaunch.Config.CatalogFile).
		// The snapshot is never cached on disk — only icons and screenshots are.
		CatalogURL:        env("MOOSE_CATALOG_URL", ""),
		CatalogCacheDir:   env("MOOSE_CATALOG_CACHE_DIR", filepath.Join(dataDir, "catalog-cache")),
		CatalogFile:       env("MOOSE_CATALOG_FILE", ""),
		OfflineInstall:    envBool("MOOSE_OFFLINE_INSTALL"),
		ProfileMarkerPath: profileMarker,
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reports whether key is set to a truthy value (strconv.ParseBool:
// 1/t/true/…). Unset or unparseable → false (the safe default — a box with a
// registry). Deliberately no `def` parameter and no warn-on-unparseable (unlike
// cmd/brain's envBool): every host-agent caller wants false-on-anything-odd, and
// host-agent startup must never block or get noisy over a malformed *optional*
// env. The two are not shared for the same reason `env` isn't — small per-binary
// helpers, no internal package for two cmd/ consumers (CLAUDE.md # no premature
// abstraction).
func envBool(key string) bool {
	b, err := strconv.ParseBool(os.Getenv(key))
	return err == nil && b
}
