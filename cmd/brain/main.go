// Command brain is moose-brain — the control-plane daemon (CONTROL_PLANE.md).
// In production it runs as a container supervised by host-agent; in dev it runs
// natively (`go run`) against the local Docker socket and the fake host-agent.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/onmoose/os/internal/api"
	"github.com/onmoose/os/internal/applog"
	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/caddy"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/health"
	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/notify"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
	"github.com/onmoose/os/internal/systemlive"
	"github.com/onmoose/os/internal/version"
)

// caddyReadyTimeout bounds the wait for Caddy's admin API after the brain brings
// the control-plane stack up. Caddy's admin is reachable within ~1s of the
// container starting, so this is generous for the healthy path while capping the
// degraded-startup stall when Caddy never came up (see the call site).
const caddyReadyTimeout = 10 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	cfg := loadConfig()
	installLogger(cfg.logLevel, cfg.logFormat)

	// Resolve the environment profile once at startup (ENVIRONMENT.md # How the
	// profile is realized). An unmarked box resolves to appliance — the no-op
	// default — so `make dev` and existing appliance boxes are unchanged. The
	// resolved profile is handed to the lifecycle manager below; each hosted
	// behavior branches on it when its own feature lands (#196).
	prof := profile.Read(cfg.profilePath)
	slog.Info("environment profile resolved", "profile", string(prof))

	if err := os.MkdirAll(cfg.stateDir, 0o755); err != nil {
		fatal("create state dir", "err", err)
	}

	st, err := store.Open(filepath.Join(cfg.stateDir, "moose.db"))
	if err != nil {
		fatal("open store", "err", err)
	}
	defer st.Close()

	// Hosted profile: ingest the first-boot provisioning seed (box-id, the
	// portal's assertion-verification key, and the acme-dns enrollment
	// credentials) and resolve the box's identity (ENVIRONMENT.md # Provisioning).
	// Done early so the lifecycle manager (per-app route hosts) and the
	// wildcard-cert pass below both see the box-id. Appliance is a no-op: every
	// value stays zero-valued and the hosted seams below are skipped.
	boxID, assertionKeyB64, enrollment := loadHostedEnvironment(prof, st, cfg.seedPath)
	// Decode the portal verification key the SSO handler checks ownership
	// assertions against (internal/assertion). An invalid key disables SSO (nil
	// key ⇒ /_moose/sso returns 503) rather than crashing the brain — the box can
	// still serve once re-seeded. Empty on appliance / an un-seeded hosted box.
	var assertionKey ed25519.PublicKey
	if assertionKeyB64 != "" {
		if k, err := decodeAssertionKey(assertionKeyB64); err != nil {
			slog.Error("hosted: assertion verification key invalid; SSO disabled", "err", err)
		} else {
			assertionKey = k
		}
	}

	// Catalog source (CATALOG step 3, cloud #62). Every box — appliance and hosted
	// alike — is a thin client of the control plane's public-read catalog API: it
	// fetches the GET /catalog browse payload for its own environment, holds it in
	// memory, and projects the six-method surface locally; each app's manifest and
	// compose are fetched only when that app is installed (cloud specs/CATALOG.md #
	// Consume). The box keeps no copy of the catalog on disk, so it always renders
	// what the endpoint serves now; before the first sync lands the store is empty
	// (the documented, accepted behavior — the catalog API is public-read precisely
	// so an appliance with no portal account can use it, and installing an app needs
	// internet to pull images regardless). Only icons and screenshots are cached, per
	// request and with a TTL. No baked catalog ships in the image. Env filtering is by
	// the resolved profile.
	cat := catalog.NewRemote(catalog.RemoteOptions{
		BaseURL:         cfg.catalogBaseURL,
		Environment:     string(prof),
		AssetCacheDir:   cfg.catalogAssetCacheDir,
		SnapshotFile:    cfg.catalogSnapshotFile,
		RefreshInterval: cfg.catalogRefresh,
	})
	slog.Info("catalog: remote control-plane source",
		"profile", string(prof), "base_url", cfg.catalogBaseURL, "cache_dir", cfg.catalogAssetCacheDir,
		"src", cfg.catalogSnapshotFile)
	host := hostclient.New(cfg.agentSock)
	cd := caddy.New(cfg.caddyAdmin)
	bus := events.NewBus()
	// One Docker driver, shared by the lifecycle manager and the
	// container-restart-loop detector — the detector reuses lifecycle's seam
	// rather than opening a parallel Docker client (issue #35).
	dock := lifecycle.NewCLIDocker()
	life := lifecycle.NewManager(st, cat, host, cd, dock, bus, cfg.stateDir)
	life.SetOfflineInstall(cfg.offlineInstall)
	// On hosted, per-app routes and surfaced URLs use the public
	// "<slug>.<box-id>.onmoose.network" scheme instead of "<slug>.local"
	// (ENVIRONMENT.md # Networking & discovery), and the resolved profile also
	// gates the hosted-only resource-limit CPU cap (#211). Appliance leaves the
	// box-id empty and the lifecycle keeps its .local/mDNS path.
	life.SetEnvironment(prof, boxID)
	// The box Caddy dials this same brain upstream for the hosted per-app
	// forward_auth verify subrequest (#306) that it dials for the dashboard's
	// /api + /_moose legs; wire it from the one config value.
	life.SetBrainUpstream(cfg.dashboardBrainUpstream)

	// Production: the brain owns the control-plane stack (Caddy + moose-ui) and
	// brings it up from the compose staged by host-agent before it configures any
	// routes (CONTROL_PLANE.md # Caddy is moose substrate / # the dashboard UI is
	// a brain-launched container). The socket-proxy itself is host-agent-seeded
	// transport, not part of this stack. In dev controlPlaneDir is empty: Caddy is
	// a standalone dev container and the UI is Vite, so this whole block is
	// skipped and startup behaves exactly as before.
	//
	// This runs ONCE, here, and a failure is logged and dropped on purpose. It
	// must not grow a retry loop or a background re-run: host-agent's updater
	// drives compose on this same project, and a second concurrent compose can
	// destroy the container it is recreating. See EnsureControlPlane's doc
	// comment for the failure and UPDATES.md # 3 step 3c for the ordering that
	// depends on this staying startup-only.
	if cfg.controlPlaneDir != "" {
		cpCtx, cpCancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := life.EnsureControlPlane(cpCtx, cfg.controlPlaneDir); err != nil {
			slog.Warn("control-plane stack up failed; continuing", "err", err)
		}
		cpCancel()
		// The route configuration below is one-shot — nothing re-runs it on a
		// transient failure — so wait for Caddy's admin API before driving it.
		// This gets its own short budget, not the compose budget above: on a
		// healthy box (Caddy just came up, or is already running across a
		// restart) it returns in well under a second; if Caddy never started
		// (first-boot compose failure) this caps the degraded-startup stall at a
		// few seconds instead of polling away the whole remaining 60s.
		waitCtx, waitCancel := context.WithTimeout(context.Background(), caddyReadyTimeout)
		if err := cd.WaitReady(waitCtx); err != nil {
			slog.Warn("caddy admin not ready; route config may be incomplete", "err", err)
		}
		waitCancel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	life.EnsureIngress(ctx)
	// Startup reconcile: converge Docker + routing to SQLite desired state and
	// re-assert Caddy routes lost when EnsureIngress reset the server block.
	if err := life.Reconcile(ctx); err != nil {
		slog.Warn("startup reconcile failed", "err", err)
	}
	// Catch-all 404: ensure the tail route is present after reconcile has
	// re-populated per-app routes at index 0. Best-effort — Caddy being
	// transiently unreachable here doesn't block the brain from serving.
	if err := cd.EnsureCatchAll(ctx); err != nil {
		slog.Warn("caddy: ensure catch-all failed; continuing", "err", err)
	}
	// Hosted wildcard HTTPS (ENVIRONMENT.md # Networking & discovery): tell Caddy
	// to obtain "*.<box-id>.onmoose.network" via its acme-dns DNS-01 issuer and bind
	// :443, always-on (no toggle). Skipped on appliance and on a hosted box with no
	// complete enrollment. Synchronous, fast, and best-effort: EnsureWildcardTLS
	// only applies config (the automate entry + policy + :443) — Caddy obtains the
	// cert on its own schedule afterwards, so a slow or unreachable ACME never
	// blocks startup or delays the dashboard route installed just before Serve. A
	// transient Caddy config error here does not stop the brain from serving.
	if prof == profile.Hosted && enrollment.Complete() {
		if err := cd.EnsureWildcardTLS(ctx, profile.CertSubjects(boxID), cfg.acmeDNSEndpoint, caddy.EnrollmentCredentials{
			Subdomain: enrollment.Subdomain,
			Username:  enrollment.Username,
			Password:  enrollment.Password,
		}); err != nil {
			slog.Warn("caddy: configure wildcard TLS failed; continuing on HTTP", "err", err)
		}
	}
	cancel()
	// The dashboard host route (which points Caddy's /api leg at this brain) is
	// installed *after* the listener is bound, just before Serve — see the end of
	// main. Advertising it here, ahead of ListenAndServe, would leave Caddy a
	// window where /api/* routes to a brain that isn't accepting yet (a 502).

	if status, err := host.SystemStatus(context.Background()); err != nil {
		slog.Warn("host-agent not reachable; host ops will fail",
			"sock", cfg.agentSock, "err", err)
	} else {
		slog.Info("host-agent ready",
			"hostname", status.Hostname, "agent_version", status.AgentVersion)
	}

	authMgr := auth.NewManager(st)
	auditor := audit.New(st)
	notifier := notify.New(st, bus)
	healthMgr := health.NewManager(st)

	// Restore persisted health issues before serving requests. Non-fatal:
	// the brain starts with an empty registry on error (degraded, not dead).
	if err := healthMgr.LoadFromStore(); err != nil {
		slog.Warn("health: failed to restore issues from store; starting empty", "err", err)
	}

	// Pull host-agent's system health report (HEALTH.md # Detector catalog,
	// locus B) once at startup and reconcile it into the health registry per
	// category, then keep refreshing on a slow poll. Failure here is non-fatal
	// — the brain runs degraded just like everything else.
	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()
	// Start the remote catalog's background sync loop (an immediate first sync,
	// then one per interval), bound to the process-lifetime poll context. No-op for
	// the appliance's baked disk catalog, so it's called unconditionally.
	cat.StartRefresh(pollCtx)
	pullSystemHealth(pollCtx, host, healthMgr, auditor, notifier, bus)
	go systemHealthPollLoop(pollCtx, host, healthMgr, auditor, notifier, bus, cfg.healthPollPeriod)

	// App-runtime health detectors on the health-poll cadence, both driven from
	// ONE goroutine (issue #54: reuse the restart-loop timer, no parallel
	// goroutine):
	//   - container-restart-loop (locus D): sample managed containers' cumulative
	//     RestartCount and raise per-instance when restarts climb past the
	//     threshold within the window.
	//   - app-unresponsive (locus C): for each steady-running instance that
	//     declares a health_probe, GET the probe path through the app's Caddy
	//     route and raise when it fails.
	// Both read Docker through the shared lifecycle seam (no host-agent change, no
	// EVENTS proxy grant). Order matters: the restart-loop check runs first so the
	// probe can defer to a freshly-raised container-restart-loop and not
	// double-banner a crash-looping app (HEALTH.md # app-unresponsive anti-flap).
	rld := newRestartLoopDetector(dock, healthMgr, auditor, notifier, bus)
	probe := newAppProbeDetector(dock, st, life, cfg.caddyProbeURL, healthMgr, auditor, notifier, bus)
	probe.SetEnvironment(prof, boxID)
	go appRuntimeHealthLoop(pollCtx, cfg.healthPollPeriod, rld.check, probe.check)

	// Bound the notifications table (NOTIFICATIONS.md # Locked decisions, the
	// retention bullet): prune aged / over-cap rows once at boot, then on a slow
	// loop. Non-fatal — an unbounded
	// table degrades gracefully (the bell just carries more history).
	if err := st.PruneNotifications(time.Now()); err != nil {
		slog.Warn("notification prune failed; continuing", "err", err)
	}
	go notificationPruneLoop(pollCtx, st, cfg.notifyPrunePeriod)

	// Locus-C version check (HEALTH.md # Detector catalog): reconcile
	// version-mismatch against host-agent's reported agent_version, once at
	// startup (the first handshake) then on the same loose poll cadence.
	checkAgentVersion(pollCtx, host, healthMgr, auditor, notifier, bus)
	go versionCheckPollLoop(pollCtx, host, healthMgr, auditor, notifier, bus, cfg.healthPollPeriod)

	// Locus-C brain-DB integrity check (HEALTH.md # Detector catalog): PRAGMA
	// integrity_check at boot + every 6h, reconciling brain-db-corrupt. Runs
	// entirely on its own goroutine — the boot run is inside the loop, never
	// before ListenAndServe — because a corrupt DB must raise a banner, never
	// gate startup (HEALTH.md # Stance: a brain that can't boot has no UI; that
	// path is bootstrap-state-mismatch / recovery, not this issue).
	go brainDBIntegrityLoop(pollCtx, st, healthMgr, auditor, notifier, bus, dbIntegrityCheckPeriod)

	// Live system-resources hub (BRAIN_UI_PROTOCOL.md Pattern C, stream 3): a
	// ref-counted 1 Hz poller of host-agent's raw counters, fanned out as
	// derived rates over GET /api/v1/system/live. It polls only while ≥1 SSE
	// subscriber is connected (zero idle cost); pollCtx bounds any active poll
	// to the process lifetime.
	live := systemlive.New(pollCtx, host, time.Second)

	// applogs owns the per-app log fan-out: one ref-counted host-agent follow per
	// instance behind the dashboard's many readers, with the ring buffer + replay
	// the per-app Logs tab needs. Idle when nobody is watching; pollCtx bounds any
	// active follow to the process lifetime.
	applogs := applog.NewRegistry(pollCtx, host)

	// The resolved hosted identity (box-id + the portal assertion key) drives the
	// SSO bootstrap (ENVIRONMENT.md # Provisioning; the portal-to-box handshake in
	// internal/api/sso.go); both come from loadHostedEnvironment above. Appliance
	// leaves them empty and keeps its open-on-empty-box /setup.
	srv := api.NewServer(st, cat, life, bus, authMgr, host, auditor, healthMgr, live, applogs)
	srv.SetEnvironment(prof, boxID, assertionKey)
	srv.SetTrustedProxies(trustedProxies(cfg.trustedProxies))
	// Same dir EnsureControlPlane reconciles from (above), so the UI image
	// GET /api/v1/system/version reports is the one the box converges to.
	srv.SetControlPlaneDir(cfg.controlPlaneDir)
	httpSrv := &http.Server{Handler: srv.Handler()}

	// Bind the listener before advertising the dashboard route to Caddy. Once the
	// socket is bound the kernel accepts connections (no connection-refused → no
	// 502); Serve below starts answering them a beat later. Binding first, then
	// registering with the proxy, then serving is the standard ordering — it
	// closes the window where Caddy's /api leg could point at a brain that isn't
	// listening yet.
	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		fatal("listen", "listen", cfg.listen, "err", err)
	}
	slog.Info("moose-brain listening",
		"listen", cfg.listen, "state_dir", cfg.stateDir, "catalog_cache_dir", cfg.catalogAssetCacheDir)

	// Dashboard host route (WEB_UI.md # deploy model): /api/v1/* → brain,
	// everything else → moose-ui. Production-only — gated on the UI upstream being
	// set, which dev never does. Inserted at index 0 (PUT) so it sorts before the
	// catch-all. Installed here, after the listener is bound, so Caddy's /api leg
	// only goes live once this brain can answer it.
	if cfg.dashboardUIUpstream != "" {
		// On hosted the dashboard is served at the box apex
		// "<box-id>.onmoose.network" (under the box's wildcard cert), not the
		// appliance's "moose.local" (ENVIRONMENT.md # Networking & discovery).
		dashboardHost := cfg.dashboardHost
		if prof == profile.Hosted && boxID != "" {
			dashboardHost = profile.HostedDashboardHost(boxID)
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := cd.EnsureDashboard(dctx, dashboardHost, cfg.dashboardBrainUpstream, cfg.dashboardUIUpstream); err != nil {
			slog.Warn("caddy: ensure dashboard route failed; continuing", "err", err)
		}
		dcancel()
	}

	if err := httpSrv.Serve(ln); err != nil {
		fatal("http server", "err", err)
	}
}

// installLogger replaces the default slog handler with a TextHandler at the
// configured level. Single-process appliance → setting the default once and
// using slog.Default() everywhere beats threading a *slog.Logger through every
// constructor.
func installLogger(level, format string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(format) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// fatal is the slog equivalent of log.Fatalf: structured Error + exit(1).
// Used only for startup failures we genuinely can't recover from.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// boxMetaStore is the brain's consumer-side slice of the store the hosted-seed
// ingestion needs (CLAUDE.md: interfaces live with the consumer). *store.Store
// satisfies it; tests pass a fake to drive each branch without a real database.
type boxMetaStore interface {
	GetBoxMeta(key string) (string, error)
	SetBoxMeta(key, value string) error
}

// loadHostedEnvironment resolves the hosted box's provisioning identity
// (ENVIRONMENT.md # Provisioning) for the SSO bootstrap and the wildcard-cert
// pass. It returns the box-id, the portal's base64 assertion-verification key,
// and the per-box acme-dns enrollment credentials. An empty box-id/key means "no
// SSO" (appliance, or a hosted box not yet provisioned — which keeps SSO closed,
// never falling back to the appliance's open /setup); an incomplete enrollment
// means "no cert pass" (the box still bootstraps via SSO, it just can't get
// HTTPS).
//
// On appliance it is a no-op. On hosted: a box-id already persisted is the
// install's frozen identity (MOOSE_NETWORK.md) — load it, the stored key, and the
// stored enrollment, and ignore the seed on every subsequent boot (so a
// re-delivered seed cannot re-key a provisioned box). Otherwise this is the first
// hosted boot: read the seed, persist the key and enrollment *then* the box-id
// (box-id last as the commit marker, so a crash mid-write re-ingests next boot
// rather than leaving a box-id with no key), and return them. An absent or
// unreadable seed logs and returns empty — the brain stays pre-setup rather than
// crashing.
func loadHostedEnvironment(prof profile.Profile, bm boxMetaStore, seedPath string) (boxID, assertionKeyB64 string, enr profile.EnrollmentCredentials) {
	if prof != profile.Hosted {
		return "", "", profile.EnrollmentCredentials{}
	}
	if id, err := bm.GetBoxMeta(store.BoxMetaBoxID); err == nil {
		key, kErr := bm.GetBoxMeta(store.BoxMetaAssertionKey)
		if kErr != nil {
			// The key is persisted *before* the box-id (the box-id is the
			// commit marker), so a box-id with no key should be unreachable.
			// If it happens anyway (a store read error, or the key row gone),
			// log it loudly — SSO stays closed (503) and the silent symptom
			// would otherwise be an unexplained "not provisioned" on a box that
			// already has an identity.
			if errors.Is(kErr, store.ErrNotFound) {
				slog.Error("hosted: box-id persisted but assertion key missing; SSO stays closed", "box_id", id)
			} else {
				slog.Error("hosted: read persisted assertion key failed; SSO stays closed", "err", kErr)
			}
			return id, "", profile.EnrollmentCredentials{}
		}
		return id, key, loadEnrollment(bm)
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Error("hosted: read persisted box-id failed; staying pre-setup", "err", err)
		return "", "", profile.EnrollmentCredentials{}
	}

	seed, err := profile.ReadSeed(seedPath)
	if errors.Is(err, profile.ErrSeedAbsent) {
		slog.Warn("hosted box has no provisioning seed; SSO stays closed until one lands", "src", seedPath)
		return "", "", profile.EnrollmentCredentials{}
	}
	if err != nil {
		slog.Error("hosted: provisioning seed unreadable; staying pre-setup", "src", seedPath, "err", err)
		return "", "", profile.EnrollmentCredentials{}
	}

	if err := bm.SetBoxMeta(store.BoxMetaAssertionKey, seed.AssertionVerificationKey); err != nil {
		slog.Error("hosted: persist assertion key failed; staying pre-setup", "err", err)
		return "", "", profile.EnrollmentCredentials{}
	}
	// Enrollment is persisted *before* the box-id commit marker, and a complete
	// enrollment must persist for the ingest to commit. Every later (frozen-
	// identity) boot reads the stored enrollment to reconfigure Caddy's DNS-01
	// issuer; if this write silently failed but the box-id still committed, this
	// boot would get a cert and no subsequent boot ever would. So a write failure
	// aborts before the commit marker — the seed is re-ingested next boot — exactly
	// like the key-persist abort above. A seed with no enrollment is a legitimate
	// "no HTTPS" box: it provisions, bootstraps via SSO, and skips the cert pass.
	if seed.Enrollment.Complete() {
		if err := persistEnrollment(bm, seed.Enrollment); err != nil {
			slog.Error("hosted: persist enrollment failed; staying pre-setup", "err", err)
			return "", "", profile.EnrollmentCredentials{}
		}
	} else {
		slog.Warn("hosted: seed carries no acme-dns enrollment; wildcard cert pass will be skipped", "box_id", seed.BoxID)
	}
	if err := bm.SetBoxMeta(store.BoxMetaBoxID, seed.BoxID); err != nil {
		slog.Error("hosted: persist box-id failed; staying pre-setup", "err", err)
		return "", "", profile.EnrollmentCredentials{}
	}
	slog.Info("hosted: provisioning seed ingested", "box_id", seed.BoxID)
	return seed.BoxID, seed.AssertionVerificationKey, seed.Enrollment
}

// decodeAssertionKey decodes the portal's standard-base64 Ed25519 public key (as
// the cloud writes it into the seed) and validates its length, so a truncated or
// wrong-type key fails loudly here (SSO disabled) rather than at the first
// ed25519.Verify. Standard base64 because the seed is JSON, not a URL component.
func decodeAssertionKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode assertion key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("assertion key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// persistEnrollment stores the acme-dns credentials as JSON under
// BoxMetaEnrollment so later boots can reconfigure Caddy's DNS-01 issuer without
// the seed. Written before the box-id commit marker; the caller aborts the
// ingest on error so a provisioned identity is never frozen with no recorded
// enrollment (which would leave the box permanently certless).
func persistEnrollment(bm boxMetaStore, enr profile.EnrollmentCredentials) error {
	b, err := json.Marshal(enr)
	if err != nil {
		return fmt.Errorf("marshal enrollment: %w", err)
	}
	if err := bm.SetBoxMeta(store.BoxMetaEnrollment, string(b)); err != nil {
		return fmt.Errorf("persist enrollment: %w", err)
	}
	return nil
}

// loadEnrollment reads the persisted acme-dns credentials on a frozen-identity
// boot. An absent or unparseable value yields an incomplete (zero) credential,
// which the cert pass treats as "skip" — a provisioned box with no recorded
// enrollment simply serves no HTTPS rather than failing to boot.
func loadEnrollment(bm boxMetaStore) profile.EnrollmentCredentials {
	v, err := bm.GetBoxMeta(store.BoxMetaEnrollment)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("hosted: read persisted enrollment failed; wildcard cert pass will be skipped", "err", err)
		}
		return profile.EnrollmentCredentials{}
	}
	var enr profile.EnrollmentCredentials
	if err := json.Unmarshal([]byte(v), &enr); err != nil {
		slog.Error("hosted: persisted enrollment unparseable; wildcard cert pass will be skipped", "err", err)
		return profile.EnrollmentCredentials{}
	}
	return enr
}

type config struct {
	listen                 string
	stateDir               string
	catalogBaseURL         string
	catalogAssetCacheDir   string
	catalogSnapshotFile    string
	catalogRefresh         time.Duration
	agentSock              string
	caddyAdmin             string
	caddyListen            string
	caddyProbeURL          string
	controlPlaneDir        string
	dashboardHost          string
	dashboardBrainUpstream string
	dashboardUIUpstream    string
	logLevel               string
	logFormat              string
	healthPollPeriod       time.Duration
	notifyPrunePeriod      time.Duration
	offlineInstall         bool
	profilePath            string
	seedPath               string
	acmeDNSEndpoint        string
	trustedProxies         string
}

func loadConfig() config {
	caddyListen := env("MOOSE_CADDY_LISTEN", ":80")
	return config{
		listen:   env("MOOSE_LISTEN", ":8080"),
		stateDir: env("MOOSE_STATE_DIR", "./.dev/state"),
		// Control-plane catalog: the public-read catalog origin every box syncs the
		// GET /catalog browse payload from (CATALOG step 3, cloud #62). Served on the apex
		// (cloud specs/CATALOG.md), overridable to point a box at staging or an inert
		// address (the air-gapped test lane). The asset cache dir holds proxied icons
		// and screenshots only — never the snapshot, which the box holds in memory and
		// re-fetches (APP_STORE.md # Failure modes).
		//
		// MOOSE_CATALOG_FILE is a dev/test seam, not a box setting: a local snapshot to
		// start from when there is no reachable control plane (make dev-app, the QEMU
		// boot proofs). The brain reads it and never writes it. Production leaves it
		// unset.
		catalogBaseURL:       env("MOOSE_CATALOG_URL", "https://onmoose.network"),
		catalogAssetCacheDir: env("MOOSE_CATALOG_CACHE_DIR", "/var/lib/moose/catalog-cache"),
		catalogSnapshotFile:  env("MOOSE_CATALOG_FILE", ""),
		catalogRefresh:       envDuration("MOOSE_CATALOG_REFRESH", 0), // 0 ⇒ package default
		agentSock:            env("MOOSE_AGENT_SOCK", protocol.SocketPath),
		caddyAdmin:           env("MOOSE_CADDY_ADMIN", "http://localhost:2019"),
		caddyListen:          caddyListen,
		caddyProbeURL:        env("MOOSE_CADDY_PROBE_URL", probeBaseURL(caddyListen)),
		// Control-plane / dashboard wiring is production-only. controlPlaneDir and
		// dashboardUIUpstream default empty so the containerless dev brain skips
		// both the compose bring-up and the dashboard route; the containerized
		// brain (host-agent's run-spec) sets them.
		controlPlaneDir:        env("MOOSE_CONTROL_PLANE_DIR", ""),
		dashboardHost:          env("MOOSE_DASHBOARD_HOST", "moose.local"),
		dashboardBrainUpstream: env("MOOSE_DASHBOARD_BRAIN_UPSTREAM", "moose-brain:8080"),
		dashboardUIUpstream:    env("MOOSE_DASHBOARD_UI_UPSTREAM", ""),
		logLevel:               env("MOOSE_LOG_LEVEL", "info"),
		logFormat:              env("MOOSE_LOG_FORMAT", "text"),
		healthPollPeriod:       envDuration("MOOSE_HEALTH_POLL", 60*time.Second),
		notifyPrunePeriod:      envDuration("MOOSE_NOTIFY_PRUNE", time.Hour),
		// A baked, air-gapped box has no registry: it docker-loads every image
		// from the offline bundle and trusts the catalog-promised digest on a pull
		// failure (CONTROL_PLANE.md # First-boot brain bootstrap). Off by default —
		// a box with a registry pulls and verifies against it.
		offlineInstall: envBool("MOOSE_OFFLINE_INSTALL", false),
		// Environment-profile marker (ENVIRONMENT.md # How the profile is realized).
		// The image stamps /etc/moose/profile; the path is overridable for tests and
		// `make dev`, where no marker exists and the brain defaults to appliance.
		profilePath: env("MOOSE_PROFILE_PATH", profile.DefaultMarkerPath),
		// Hosted first-boot provisioning seed (ENVIRONMENT.md # Provisioning). Read
		// only when profile == hosted; absent on appliance. Overridable for tests
		// and the cloud-lane harness.
		seedPath: env("MOOSE_SEED_PATH", profile.DefaultSeedPath),
		// Public acme-dns API endpoint the box's Caddy pushes its `_acme-challenge`
		// TXT to for DNS-01 (C3b). A box-side constant — the same for every box, so
		// it is not part of the seeded payload (cloud specs/ARCHITECTURE.md
		// Contract 2). The canonical value is pinned cloud-side once the public
		// acme-dns face is deployed (cloud issue tracking it); overridable here so
		// the box can be pointed at staging or a self-hosted acme-dns.
		acmeDNSEndpoint: env("MOOSE_ACMEDNS_ENDPOINT", "https://auth.onmoose.network"),
		// Proxies the brain will read an X-Forwarded-For from when deriving the
		// client IP its per-IP throttles key on (#329). Comma-separated IPs/CIDRs.
		// Unset ⇒ the private ranges (see api.DefaultTrustedProxies), which is every
		// address a peer on the box's own Docker network can have. Set it empty to
		// pin the brain to the peer address alone.
		trustedProxies: envRaw("MOOSE_TRUSTED_PROXIES", defaultTrustedProxiesSentinel),
	}
}

// defaultTrustedProxiesSentinel marks "MOOSE_TRUSTED_PROXIES was not set", so an
// operator can still set it to the empty string to mean "trust no proxy" — a
// distinction a plain default string would erase.
const defaultTrustedProxiesSentinel = "\x00default"

// trustedProxies resolves the configured trusted-proxy spec into prefixes. An
// unparseable value is fatal rather than silently ignored: falling back to a
// wider default would be a security downgrade the operator never asked for, and
// falling back to none would lock every LAN client into one shared login-throttle
// bucket. Both are worse than refusing to start with a clear message.
func trustedProxies(spec string) []netip.Prefix {
	if spec == defaultTrustedProxiesSentinel {
		return api.DefaultTrustedProxies()
	}
	prefixes, err := api.ParseTrustedProxies(spec)
	if err != nil {
		fatal("parse MOOSE_TRUSTED_PROXIES", "err", err)
	}
	slog.Info("trusted proxies configured", "trusted_proxies", spec)
	return prefixes
}

// probeBaseURL turns the Caddy site-listener address (the same value passed to
// EnsureServer) into a base URL the app-unresponsive probe can dial. ":80" →
// "http://127.0.0.1:80". The probe sets the route Host header and Caddy routes
// by Host, so the dial target is just Caddy's listener. Override with
// MOOSE_CADDY_PROBE_URL when Caddy isn't at localhost (e.g. the brain in a
// container reaching a Caddy container by service name).
func probeBaseURL(caddyListen string) string {
	host, port, err := net.SplitHostPort(caddyListen)
	if err != nil {
		slog.Warn("caddy listen address unparseable; probe base defaults to :80", "caddy_listen", caddyListen, "err", err)
		return "http://127.0.0.1"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("invalid duration, using default", "var", k, "value", v, "default", def)
	}
	return def
}

// envRaw is env() for a var whose empty value is meaningful: it distinguishes
// "unset" (def) from "set to the empty string" (""), which env() collapses.
func envRaw(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envBool parses a boolean env var (strconv.ParseBool: 1/t/true/0/f/false, …).
// An unset var uses def; a set-but-unparseable value warns and uses def.
func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("unparseable bool env var; using default", "env_var", k, "err", "not a bool")
		return def
	}
	return b
}

// pullSystemHealth fetches host-agent's system health report and reconciles it
// into the health registry one category at a time. Non-blocking: if host-agent
// isn't reachable yet, we log and return — the brain still starts. The poll loop
// will catch up once host-agent comes online. Transitions are audited per issue
// (see emitHealthTransitions) and fan out to the notification center (see
// emitHealthNotifications).
//
// Each category the report covers is reconciled independently (clearing any
// host-reported issue in it that's now absent); categories not in the report are
// left alone. Sorted iteration keeps the per-issue audit / log order stable.
func pullSystemHealth(ctx context.Context, host *hostclient.Client, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sh, err := host.SystemHealth(c)
	if err != nil {
		slog.Warn("system health: host-agent unreachable; skipping",
			"err", err)
		return
	}
	cats := make([]protocol.HealthCategory, 0, len(sh.Categories))
	for cat := range sh.Categories {
		cats = append(cats, cat)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i] < cats[j] })
	var raised, cleared []health.IssueKey
	for _, cat := range cats {
		r, cl := healthMgr.ApplyFindings(cat, sh.Categories[cat])
		raised = append(raised, r...)
		cleared = append(cleared, cl...)
	}
	if len(raised) > 0 || len(cleared) > 0 {
		slog.Info("system health: reconciled",
			"raised", len(raised), "cleared", len(cleared), "categories", len(cats))
	}
	emitHealthTransitions(ctx, auditor, bus, raised, cleared)
	emitHealthNotifications(notifier, healthMgr, raised, cleared)
}

// emitHealthTransitions writes one audit record per transitioned health issue,
// targeting {kind: health_issue, id: <id>}, so the Activity view attributes
// each raise/clear to a specific issue rather than a bulk count, and publishes
// the matching SSE event so the dashboard's degraded-mode banner updates live
// (HEALTH.md # Display; issue #12). The event payload is advisory — {id,
// instance_key} — and the UI re-fetches GET /api/v1/health on receipt rather
// than merging it, mirroring the notification pattern. No-op when both slices
// are empty — the steady-state case, since most polls change nothing. bus is
// nil only in tests that assert audit/notify alone; the publish is skipped then.
func emitHealthTransitions(ctx context.Context, auditor *audit.Recorder, bus *events.Bus, raised, cleared []health.IssueKey) {
	for _, k := range raised {
		auditor.Record(ctx, audit.ActionHealthIssueRaised,
			audit.Target{Kind: "health_issue", ID: k.ID}, nil, true)
		if bus != nil {
			bus.Publish(events.HealthIssueRaised, map[string]any{"id": k.ID, "instance_key": k.InstanceKey})
		}
	}
	for _, k := range cleared {
		auditor.Record(ctx, audit.ActionHealthIssueCleared,
			audit.Target{Kind: "health_issue", ID: k.ID}, nil, true)
		if bus != nil {
			bus.Publish(events.HealthIssueCleared, map[string]any{"id": k.ID, "instance_key": k.InstanceKey})
		}
	}
}

// emitHealthNotifications fans transitioned health issues out to the
// notification center (NOTIFICATIONS.md): a notification per allowlisted
// raise, a resolve per allowlisted clear. The allowlist gate lives in
// notify, so this dispatches every transition and lets notify drop the ones
// that don't notify. Raises need the issue's severity/summary, which
// ApplyFindings doesn't return — look it up by key. The ok guard skips
// a key with no live issue; in the current sequential poll a just-raised key
// is always still active when looked up, so this is defensive (and notify
// would drop the resulting zero-value Issue anyway, since "" isn't allowlisted).
func emitHealthNotifications(notifier *notify.Notifier, healthMgr *health.Manager, raised, cleared []health.IssueKey) {
	for _, k := range raised {
		if iss, ok := healthMgr.Get(k.ID, k.InstanceKey); ok {
			notifier.HealthRaised(iss)
		}
	}
	for _, k := range cleared {
		notifier.HealthCleared(k.ID, k.InstanceKey)
	}
}

// systemHealthPollLoop keeps the health registry in sync with what host-agent
// reports. 60s is the loose-by-design cadence — host-measured findings don't
// change often, and the dashboard's view of "active issues" gets a refresh on
// every dashboard load via the same registry.
func systemHealthPollLoop(ctx context.Context, host *hostclient.Client, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pullSystemHealth(ctx, host, healthMgr, auditor, notifier, bus)
		}
	}
}

// notificationPruneLoop bounds the notifications table on a slow cadence
// (NOTIFICATIONS.md # Locked decisions, the retention bullet). Hourly by
// default — retention is
// housekeeping, not latency-sensitive. Each tick is independently best-effort;
// a failure logs and the next tick retries.
func notificationPruneLoop(ctx context.Context, st *store.Store, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.PruneNotifications(time.Now()); err != nil {
				slog.Warn("notification prune failed; continuing", "err", err)
			}
		}
	}
}

// minimumAgentVersion is the oldest host-agent version this brain build still
// works with — the box-side stand-in for the release manifest's
// minimum_host_agent field (UPDATES.md # 7 Compatibility matrix,
// RELEASE_MANIFEST.md # Manifest schema) until that manifest is actually wired
// up. It is deliberately NOT the same value as this brain's own
// internal/version.Version: with one repo version (DECISIONS.md 2026-07-16),
// host-agent and brain still ship from the same commit, but host-agent updates
// ride apt on their own cadence and can lag the brain by up to 24h during a
// rollout (UPDATES.md # 2) — an update ordering where host-agent updates
// *first*, so a brain running a newer build than the agent it's paired with is
// the normal, expected mid-rollout state, not a mismatch. Bump this by hand
// only when a real host-agent<->brain protocol break lands (i.e. an agent
// older than this genuinely can't serve this brain), not on every release.
const minimumAgentVersion = "0.4.0"

// agentVersionAcceptable reports whether reportedVersion is new enough to
// satisfy minimum — i.e. NOT older than it. A newer agent is always
// acceptable (the brain declares a compat floor, not an exact pair —
// UPDATES.md # Compatibility matrix); only an older agent is a mismatch.
// Comparison is on the release core
// (MAJOR.MINOR.PATCH) only: a build/prerelease suffix (e.g. the fake
// host-agent's "-fake", DECISIONS.md 2026-07-16) must not make an
// otherwise-current agent look older — semver's prerelease-sorts-before-release
// rule would otherwise flag the in-repo dev fake agent as a mismatch against
// its own brain on every `make dev`. Uses golang.org/x/mod/semver (already in
// the module graph via modernc.org/sqlite's test deps — see go.sum) rather than
// hand-rolling comparison.
func agentVersionAcceptable(reportedVersion, minimum string) bool {
	return semver.Compare(versionCore(reportedVersion), versionCore(minimum)) >= 0
}

// versionCore strips any semver prerelease/build suffix and normalizes to the
// "v"-prefixed form golang.org/x/mod/semver requires, e.g. "0.4.0-fake" ->
// "v0.4.0". An unparseable core sorts before every valid version (semver.Compare
// semantics), which is the safe default: an agent reporting a garbled version
// string is treated as too old rather than silently accepted.
func versionCore(v string) string {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// agentStatusReader is the slice of the host client the version check needs — a
// single GET /v1/system/status read. Consumer-side interface (CLAUDE.md) so
// checkAgentVersion is unit-testable with a fake host-agent reporting a chosen
// agent_version; *hostclient.Client satisfies it.
type agentStatusReader interface {
	SystemStatus(ctx context.Context) (protocol.SystemStatus, error)
}

// checkAgentVersion is the locus-C version-mismatch detector (HEALTH.md
// # Detector catalog, locus C). It reads host-agent's reported agent_version
// and reconciles the version-mismatch issue against a **compatible range**,
// not exact equality: raise only when the agent is older than
// minimumAgentVersion, clear otherwise (a newer agent is fine — the brain
// declares a floor, not an exact pair). The older-agent case is reachable:
// stream B (this brain) updates before stream A (host-agent), so a new brain
// can land ahead of the host-agent it needs — UPDATES.md # Update ordering.
// There is no dedicated handshake RPC,
// so each successful status read is the handshake (HEALTH.md: "each
// handshake"). A version string is deterministic and authoritative — it cannot
// flap like a threshold sample — so the check is 1-shot (no debounce): it
// raises/clears on the first definitive reading. A transient unreachable
// host-agent neither raises nor clears, so the issue state survives a blip.
// Transitions are audited per issue and fanned out to notifications, mirroring
// pullStorageHealth. NOTIFICATIONS.md's v1 allowlist routes version-mismatch
// (error) to Admin, but it is not yet wired into internal/notify healthRules
// (like disk-full / brain-db-corrupt — allowlisted in the spec but not yet
// sourced), so the fan-out is a no-op until that entry lands; see the progress
// entry for why wiring it is a separate, deferred step.
func checkAgentVersion(ctx context.Context, host agentStatusReader, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := host.SystemStatus(c)
	if err != nil {
		slog.Warn("version check: host-agent unreachable; skipping", "err", err)
		return
	}

	var raised, cleared []health.IssueKey
	if !agentVersionAcceptable(status.AgentVersion, minimumAgentVersion) {
		details := fmt.Sprintf("The system agent is running an older version (%s) than this moose needs (%s or newer). It will update automatically; if this persists, check the box's internet connection.",
			status.AgentVersion, minimumAgentVersion)
		if healthMgr.Raise("version-mismatch", "", details) {
			raised = []health.IssueKey{{ID: "version-mismatch"}}
			slog.Warn("version check: agent older than the minimum this brain requires",
				"agent_version", status.AgentVersion, "minimum", minimumAgentVersion)
		}
	} else if healthMgr.Clear("version-mismatch", "") {
		cleared = []health.IssueKey{{ID: "version-mismatch"}}
	}
	emitHealthTransitions(ctx, auditor, bus, raised, cleared)
	emitHealthNotifications(notifier, healthMgr, raised, cleared)
}

// versionCheckPollLoop re-runs the locus-C version check on the same loose
// cadence as the storage poll — each periodic status read is a "handshake"
// (HEALTH.md). version-mismatch only changes when a component is upgraded, so
// the cadence is loose by design.
func versionCheckPollLoop(ctx context.Context, host agentStatusReader, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			checkAgentVersion(ctx, host, healthMgr, auditor, notifier, bus)
		}
	}
}

// dbIntegrityCheckPeriod is the locus-C brain-db-corrupt cadence (HEALTH.md #
// Detector catalog: "boot + 6h"). A constant, not an env knob: the spec pins
// the value and there's no reason to tune it per-deployment.
const dbIntegrityCheckPeriod = 6 * time.Hour

// integrityChecker is the consumer-side seam for the store's PRAGMA
// integrity_check (CLAUDE.md: interfaces live with the consumer). *store.Store
// satisfies it; tests pass a fake to drive each branch without a real database.
type integrityChecker interface {
	IntegrityCheck() (string, error)
}

// checkBrainDBIntegrity runs one PRAGMA integrity_check and reconciles the
// brain-db-corrupt issue (HEALTH.md # Detector catalog, locus C): a result
// other than "ok" raises it, "ok" clears it. The result is authoritative and
// 1-shot — corruption isn't a noisy threshold sample, so there's no debounce.
// A query error is best-effort: it can't conclude corrupt *or* sound, so it
// logs and leaves the issue state untouched (a transient blip neither raises a
// false banner nor clears a real one). Transitions are audited per issue and
// fan out to the notification center, mirroring pullStorageHealth.
func checkBrainDBIntegrity(ctx context.Context, db integrityChecker, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus) {
	result, err := db.IntegrityCheck()
	if err != nil {
		slog.Warn("integrity check: query failed; leaving brain-db-corrupt unchanged", "err", err)
		return
	}
	var raised, cleared []health.IssueKey
	if result != "ok" {
		details := fmt.Sprintf("SQLite integrity check failed:\n%s", result)
		if healthMgr.Raise("brain-db-corrupt", "", details) {
			raised = []health.IssueKey{{ID: "brain-db-corrupt"}}
			slog.Error("integrity check: brain database is corrupt", "output", result)
		}
	} else if healthMgr.Clear("brain-db-corrupt", "") {
		cleared = []health.IssueKey{{ID: "brain-db-corrupt"}}
		slog.Info("integrity check: brain database recovered; cleared brain-db-corrupt")
	}
	emitHealthTransitions(ctx, auditor, bus, raised, cleared)
	emitHealthNotifications(notifier, healthMgr, raised, cleared)
}

// brainDBIntegrityLoop runs the boot integrity check and then re-checks on the
// 6h cadence. The boot run lives *inside* this goroutine (not synchronously in
// main before serving) on purpose — see the call site: the check must never
// gate brain startup.
func brainDBIntegrityLoop(ctx context.Context, db integrityChecker, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus, interval time.Duration) {
	checkBrainDBIntegrity(ctx, db, healthMgr, auditor, notifier, bus) // boot run
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			checkBrainDBIntegrity(ctx, db, healthMgr, auditor, notifier, bus)
		}
	}
}

const (
	// restartLoopThreshold / restartLoopWindow define the container-restart-loop
	// trip condition: more than 3 container restarts within a 5-minute sliding
	// window. Conservative defaults — HEALTH.md pins the detector's shape, not
	// the constants (issue #35); tune at first soak if they prove noisy.
	restartLoopThreshold = 3
	restartLoopWindow    = 5 * time.Minute
)

// restartCountReader is the brain's consumer-side slice of the Docker seam:
// just the per-instance restart counts the loop detector needs.
// lifecycle.DockerDriver satisfies it; tests pass a fake (CLAUDE.md:
// consumer-side interfaces).
type restartCountReader interface {
	RestartCounts(ctx context.Context) (map[string]int, error)
}

// restartSample is one (time, cumulative RestartCount) reading for an instance.
type restartSample struct {
	t     time.Time
	count int
}

// restartLoopDetector samples managed containers' cumulative RestartCount and
// reconciles the per-instance container-restart-loop health issue (HEALTH.md #
// Detector catalog, locus D). Not thread-safe: it holds per-instance sample
// history and is driven from a single poll goroutine (run).
type restartLoopDetector struct {
	docker    restartCountReader
	healthMgr *health.Manager
	auditor   *audit.Recorder
	notifier  *notify.Notifier
	bus       *events.Bus
	window    time.Duration
	threshold int
	now       func() time.Time
	history   map[string][]restartSample // instance_id -> samples within the window
}

func newRestartLoopDetector(docker restartCountReader, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus) *restartLoopDetector {
	return &restartLoopDetector{
		docker:    docker,
		healthMgr: healthMgr,
		auditor:   auditor,
		notifier:  notifier,
		bus:       bus,
		window:    restartLoopWindow,
		threshold: restartLoopThreshold,
		now:       func() time.Time { return time.Now().UTC() },
		history:   map[string][]restartSample{},
	}
}

// appRuntimeHealthLoop drives the app-runtime health detectors (container-
// restart-loop then app-unresponsive) on one ticker — issue #54's "reuse the
// timer, no parallel goroutine." Each check runs once immediately (the
// restart-loop detector establishes its RestartCount baseline on the first
// sample) then re-runs every interval, in the given order. Locus-D is reactive
// in spirit, but the socket-proxy allowlist grants CONTAINERS, not EVENTS
// (CONTROL_PLANE.md), so polling is the no-proxy-change path.
func appRuntimeHealthLoop(ctx context.Context, interval time.Duration, checks ...func(context.Context)) {
	runAll := func() {
		for _, c := range checks {
			c(ctx)
		}
	}
	runAll()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runAll()
		}
	}
}

// check samples every managed container's RestartCount once and reconciles the
// container-restart-loop issue per instance: raise where the within-window
// restart delta exceeds the threshold, clear where it no longer does (the app
// stabilized) or the instance is gone (uninstalled). RestartCount is cumulative
// since container creation, so we threshold the delta over a sliding window, not
// the raw counter — a container that crashed a lot yesterday but is quiet now
// reads as zero recent restarts.
func (d *restartLoopDetector) check(ctx context.Context) {
	counts, err := d.docker.RestartCounts(ctx)
	if err != nil {
		slog.Warn("restart-loop check: docker unreachable; skipping", "err", err)
		return
	}
	now := d.now()
	cutoff := now.Add(-d.window)

	looping := map[string]bool{}
	var raised []health.IssueKey
	for id, count := range counts {
		prev := d.history[id]
		// Container recreation (app update / reinstall) resets RestartCount to 0.
		// A count below the last sample means a fresh container — restart the
		// window from here so a stale high baseline can't mask the new container's
		// restarts (negative delta) or distort the count.
		if n := len(prev); n > 0 && count < prev[n-1].count {
			prev = nil
		}
		var samples []restartSample
		for _, s := range prev {
			if !s.t.Before(cutoff) {
				samples = append(samples, s)
			}
		}
		samples = append(samples, restartSample{t: now, count: count})
		d.history[id] = samples

		// Baseline is the oldest sample still inside the window; the delta against
		// it is how many times this container restarted within the window. The
		// first sample for an instance has delta 0 — no false raise from history.
		if count-samples[0].count > d.threshold {
			looping[id] = true
			details := fmt.Sprintf("This app's container restarted %d times in about %d minutes.",
				count-samples[0].count, int(d.window.Minutes()))
			if d.healthMgr.Raise("container-restart-loop", id, details) {
				raised = append(raised, health.IssueKey{ID: "container-restart-loop", InstanceKey: id})
			}
		}
	}

	// Forget instances Docker no longer reports (stopped / uninstalled) so the
	// history map can't grow without bound.
	for id := range d.history {
		if _, ok := counts[id]; !ok {
			delete(d.history, id)
		}
	}

	// Clear any active restart-loop whose instance isn't looping this poll — it
	// stabilized, or its app was uninstalled (absent from counts entirely).
	var cleared []health.IssueKey
	for _, iss := range d.healthMgr.List() {
		if iss.ID != "container-restart-loop" || looping[iss.InstanceKey] {
			continue
		}
		if d.healthMgr.Clear("container-restart-loop", iss.InstanceKey) {
			cleared = append(cleared, health.IssueKey{ID: "container-restart-loop", InstanceKey: iss.InstanceKey})
		}
	}

	// Stable order (all keys share the ID) so the per-issue audit records and
	// tests see a deterministic sequence.
	sortByInstanceKey(raised)
	sortByInstanceKey(cleared)
	emitHealthTransitions(ctx, d.auditor, d.bus, raised, cleared)
	emitHealthNotifications(d.notifier, d.healthMgr, raised, cleared)
}

func sortByInstanceKey(ks []health.IssueKey) {
	sort.Slice(ks, func(i, j int) bool { return ks[i].InstanceKey < ks[j].InstanceKey })
}

const (
	// appProbeRaiseThreshold is the cross-cutting debounce (HEALTH.md
	// # Cross-cutting detector policy) for app-unresponsive: raise only on the
	// 2nd consecutive failed probe; one good probe clears.
	appProbeRaiseThreshold = 2
	// appProbeTimeout bounds a single probe request. The spec leaves the exact
	// value open (issue #54); 5s matches the brain's other host-call timeouts and
	// sits well under the 60s tick. Tune at first soak.
	appProbeTimeout = 5 * time.Second
)

// managedContainerReader is the brain's consumer-side slice of the Docker seam
// the probe needs: per-instance container service / running / StartedAt.
// lifecycle.DockerDriver satisfies it.
type managedContainerReader interface {
	ManagedContainers(ctx context.Context) ([]lifecycle.ManagedContainer, error)
}

// instanceLister enumerates installed instances (id + slug + mDNS host).
// *store.Store satisfies it.
type instanceLister interface {
	List() ([]store.Instance, error)
}

// instanceManifestLoader loads an instance's persisted manifest (main_service +
// health_probe). *lifecycle.Manager satisfies it.
type instanceManifestLoader interface {
	InstanceManifest(id string) (*manifest.Manifest, error)
}

// appProbeDetector is the locus-C app-unresponsive detector (HEALTH.md #
// Detector catalog). For each steady-running instance that declares a
// health_probe (APP_MANIFEST.md # B), it GETs the probe path *through the app's
// Caddy route* (Host header, never dialing the container — THREAT_MODEL.md # B2)
// and reconciles the per-instance app-unresponsive issue. Not thread-safe: it
// holds per-instance bad-sample counters and is driven from the single
// app-runtime poll goroutine (appRuntimeHealthLoop), after the restart-loop
// detector so a crash-looper surfaces as container-restart-loop, not here.
type appProbeDetector struct {
	docker    managedContainerReader
	instances instanceLister
	manifests instanceManifestLoader
	healthMgr *health.Manager
	auditor   *audit.Recorder
	notifier  *notify.Notifier
	bus       *events.Bus
	client    *http.Client
	baseURL   string // Caddy site-listener base, e.g. http://127.0.0.1:80
	now       func() time.Time
	bad       map[string]int // instance_id -> consecutive failed-probe count

	// profile + boxID select the probe's Host header to match the Caddy route
	// (the same scheme the lifecycle keys routes on and the API surfaces). On
	// hosted there is no mDNS, so the route host is "<slug>.<box-id>.onmoose.network"
	// and probing "<slug>.local" hits Caddy's catch-all 404 — every probed app
	// would flap to app-unresponsive. Set once at startup via SetEnvironment; the
	// empty default keeps the appliance ".local" path.
	profile profile.Profile
	boxID   string
}

// SetEnvironment records the environment profile and (on hosted) the box-id, so
// the probe addresses each app at its real Caddy route host. Mirrors the
// lifecycle and API seams; cmd/brain wires it from the resolved profile + the
// ingested seed. Appliance passes an empty box-id and the ".local" path stands.
func (d *appProbeDetector) SetEnvironment(prof profile.Profile, boxID string) {
	d.profile = prof
	d.boxID = boxID
}

func newAppProbeDetector(docker managedContainerReader, instances instanceLister, manifests instanceManifestLoader, baseURL string, healthMgr *health.Manager, auditor *audit.Recorder, notifier *notify.Notifier, bus *events.Bus) *appProbeDetector {
	return &appProbeDetector{
		docker:    docker,
		instances: instances,
		manifests: manifests,
		healthMgr: healthMgr,
		auditor:   auditor,
		notifier:  notifier,
		bus:       bus,
		client:    &http.Client{Timeout: appProbeTimeout},
		baseURL:   baseURL,
		now:       func() time.Time { return time.Now().UTC() },
		bad:       map[string]int{},
	}
}

// check probes every eligible instance once and reconciles app-unresponsive.
// Eligible = declares a health_probe, isn't already flagged
// container-restart-loop, has its main_service container running, and is past
// the start-period grace. A failed probe raises on the 2nd consecutive failure
// (or immediately while already active); one good probe — or losing eligibility
// (stopped, crash-looping, uninstalled) — clears.
func (d *appProbeDetector) check(ctx context.Context) {
	instances, err := d.instances.List()
	if err != nil {
		slog.Warn("app-probe check: list instances; skipping", "err", err)
		return
	}
	containers, err := d.docker.ManagedContainers(ctx)
	if err != nil {
		slog.Warn("app-probe check: docker unreachable; skipping", "err", err)
		return
	}
	byInstance := map[string]map[string]lifecycle.ManagedContainer{}
	for _, c := range containers {
		if byInstance[c.InstanceID] == nil {
			byInstance[c.InstanceID] = map[string]lifecycle.ManagedContainer{}
		}
		byInstance[c.InstanceID][c.Service] = c
	}

	now := d.now()
	probed := map[string]bool{}    // instances probed this tick
	unhealthy := map[string]bool{} // instances holding/raising app-unresponsive this tick
	var raised []health.IssueKey

	for _, inst := range instances {
		man, err := d.manifests.InstanceManifest(inst.ID)
		if err != nil || man.HealthProbe == nil {
			continue // unloadable, or opt-out → never probed
		}
		// Defer to container-restart-loop: a crash-looper is its domain, not ours
		// (HEALTH.md # app-unresponsive anti-flap — don't double-banner).
		if _, looping := d.healthMgr.Get("container-restart-loop", inst.ID); looping {
			continue
		}
		main, ok := byInstance[inst.ID][man.MainService]
		if !ok || !main.Running {
			continue // not steady-running
		}
		if now.Sub(main.StartedAt) < man.HealthProbe.StartPeriod {
			continue // start-period grace: warming up, don't count failures yet
		}
		probed[inst.ID] = true
		// Address the app at its real Caddy route host (mirrors api getAppURL):
		// hosted's public "<slug>.<box-id>.onmoose.network" takes precedence, then
		// the announced mDNS name, then the reconstructed "<slug>.local".
		var host string
		switch {
		case d.profile == profile.Hosted && d.boxID != "" && inst.Slug != "":
			host = profile.HostedAppHost(d.boxID, inst.Slug)
		case inst.MDNSName != "":
			host = inst.MDNSName
		default:
			host = inst.Slug + protocol.AppHostSuffix
		}
		if d.probe(ctx, host, man.HealthProbe) {
			d.bad[inst.ID] = 0
			continue // good sample → clear handled in the sweep below
		}
		d.bad[inst.ID]++
		_, active := d.healthMgr.Get("app-unresponsive", inst.ID)
		if active || d.bad[inst.ID] >= appProbeRaiseThreshold {
			unhealthy[inst.ID] = true
			details := "This app is running but didn't respond to its health check."
			if d.healthMgr.Raise("app-unresponsive", inst.ID, details) {
				raised = append(raised, health.IssueKey{ID: "app-unresponsive", InstanceKey: inst.ID})
			}
		}
	}

	// Drop bad-sample counters for instances not probed this tick (stopped, in
	// grace, crash-looping, uninstalled) so a stale count can't carry across a
	// gap — "2 consecutive" means consecutive probes.
	for id := range d.bad {
		if !probed[id] {
			delete(d.bad, id)
		}
	}

	// Clear any active app-unresponsive not held unhealthy this tick: a good
	// sample, or the instance lost eligibility (stopped / now crash-looping /
	// uninstalled) — all "no longer app-unresponsive".
	var cleared []health.IssueKey
	for _, iss := range d.healthMgr.List() {
		if iss.ID != "app-unresponsive" || unhealthy[iss.InstanceKey] {
			continue
		}
		if d.healthMgr.Clear("app-unresponsive", iss.InstanceKey) {
			cleared = append(cleared, health.IssueKey{ID: "app-unresponsive", InstanceKey: iss.InstanceKey})
		}
	}

	sortByInstanceKey(raised)
	sortByInstanceKey(cleared)
	emitHealthTransitions(ctx, d.auditor, d.bus, raised, cleared)
	emitHealthNotifications(d.notifier, d.healthMgr, raised, cleared)
}

// probe GETs the app's probe path through Caddy with Host: <route host> —
// exactly the request a browser makes, never the brain dialing the container
// (THREAT_MODEL.md # B2). Healthy = status in the app's healthy set. A timeout,
// connection failure, or Caddy 502 (dead upstream) is unhealthy.
func (d *appProbeDetector) probe(ctx context.Context, host string, p *manifest.HealthProbe) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+p.Path, nil)
	if err != nil {
		slog.Warn("app-probe: build request", "host", host, "err", err)
		return false
	}
	req.Host = host // route by Host through Caddy
	resp, err := d.client.Do(req)
	if err != nil {
		return false // timeout / connection failure = unhealthy
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the keep-alive can be reused
	return probeHealthy(resp.StatusCode, p.HealthyStatus)
}

// probeHealthy classifies a probe response. Empty allowed set ⇒ the default
// "any status < 500" (the server answered coherently; 401/403/404 still count
// as responding). A non-empty set is exact-match.
func probeHealthy(status int, allowed []int) bool {
	if len(allowed) == 0 {
		return status < 500
	}
	for _, s := range allowed {
		if s == status {
			return true
		}
	}
	return false
}
