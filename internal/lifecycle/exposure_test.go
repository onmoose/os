package lifecycle

// Per-app exposure → Caddy route policy (#306). These lock the "one central route
// builder is the safety boundary" invariant (ENVIRONMENT.md # Public-by-default):
// every hosted app route strips the box's Domain-scoped forward-auth cookie (so
// it never reaches an app upstream) and nothing else, a restricted app is
// forward-auth gated, and the appliance route stays the plain reverse_proxy.
//
// These assert the *policy* the route builder resolves. That the emitted config
// actually removes that cookie and leaves every other one intact is asserted
// behaviourally in internal/caddy (#335).

import (
	"context"
	"testing"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

func installWhoami(t *testing.T, e *testEnv) store.Instance {
	t.Helper()
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst
}

// Appliance: the route must be byte-for-byte the plain reverse_proxy — no strip,
// no gate — and the app is created public.
func TestInstall_Appliance_PlainRoute(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)

	if row, _ := e.store.Get(inst.ID); row.Exposure != store.ExposurePublic {
		t.Fatalf("appliance exposure = %q, want public", row.Exposure)
	}
	cfg := e.caddy.config(inst.ID)
	if cfg.StripCookieName != "" {
		t.Errorf("appliance route must not strip any cookie, got %q", cfg.StripCookieName)
	}
	if cfg.ForwardAuth != nil {
		t.Error("appliance route must not be forward-auth gated")
	}
}

// Hosted: a fresh install defaults to restricted (the #306 flip), so its route
// strips the Cookie header and is wrapped in the forward_auth gate pointed at the
// brain verify endpoint.
func TestInstall_Hosted_DefaultRestrictedStripsAndGates(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoami(t, e)

	if row, _ := e.store.Get(inst.ID); row.Exposure != store.ExposureRestricted {
		t.Fatalf("hosted default exposure = %q, want restricted", row.Exposure)
	}
	cfg := e.caddy.config(inst.ID)
	if cfg.StripCookieName != auth.ForwardAuthCookieName {
		t.Fatalf("hosted route strips %q, want %q so the forward-auth cookie never reaches the app", cfg.StripCookieName, auth.ForwardAuthCookieName)
	}
	if cfg.ForwardAuth == nil {
		t.Fatal("restricted app must be forward-auth gated")
	}
	fa := cfg.ForwardAuth
	if fa.Upstream != "moose-brain:8080" {
		t.Errorf("verify upstream = %q, want moose-brain:8080", fa.Upstream)
	}
	if fa.VerifyPath != profile.ForwardAuthVerifyPath {
		t.Errorf("verify path = %q, want %q", fa.VerifyPath, profile.ForwardAuthVerifyPath)
	}
	if fa.LoginURL != "https://cindy-fox.onmoose.io/" {
		t.Errorf("login URL = %q, want the box dashboard root", fa.LoginURL)
	}
	if len(fa.CopyHeaders) == 0 {
		t.Error("expected identity CopyHeaders forwarded to the app on allow")
	}
}

// The load-bearing invariant: NO hosted app route ever forwards the forward-auth
// cookie to an app upstream. Flipping to public drops the gate but keeps the
// strip — the cookie is Domain-scoped to every "<slug>.<box-id>" subdomain, so a
// public app would otherwise receive it.
func TestSetExposure_HostedPublicStillStripsCookie(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoami(t, e)

	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposurePublic); err != nil {
		t.Fatalf("set public: %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.Exposure != store.ExposurePublic {
		t.Fatalf("exposure = %q, want public", row.Exposure)
	}
	cfg := e.caddy.config(inst.ID)
	if cfg.StripCookieName != auth.ForwardAuthCookieName {
		t.Fatalf("a public hosted app must STILL strip %q, got %q", auth.ForwardAuthCookieName, cfg.StripCookieName)
	}
	if cfg.ForwardAuth != nil {
		t.Fatal("a public app must not be forward-auth gated")
	}

	// Flip back to restricted re-applies the gate.
	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposureRestricted); err != nil {
		t.Fatalf("set restricted: %v", err)
	}
	if cfg := e.caddy.config(inst.ID); cfg.ForwardAuth == nil {
		t.Fatal("flipping back to restricted must re-apply the forward-auth gate")
	}
}

// SetExposure on a stopped app persists desired state but touches no route (a
// stopped app shows only a splash); the change is picked up at the next start.
func TestSetExposure_Stopped_PersistsOnly(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoami(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	e.caddy.calls = nil

	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposurePublic); err != nil {
		t.Fatalf("set public: %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.Exposure != store.ExposurePublic {
		t.Fatalf("exposure = %q, want public", row.Exposure)
	}
	if e.caddy.called("AddRoute") {
		t.Error("SetExposure on a stopped app must not re-apply a route")
	}
}

// SetBrainUpstream overrides the forward_auth verify dial; an empty value keeps
// the current one so a misconfiguration can't blank the upstream.
func TestSetBrainUpstream(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	e.m.SetBrainUpstream("brain.internal:9999")
	inst := installWhoami(t, e)
	if fa := e.caddy.config(inst.ID).ForwardAuth; fa == nil || fa.Upstream != "brain.internal:9999" {
		t.Fatalf("forward-auth upstream = %+v, want brain.internal:9999", fa)
	}

	e.m.SetBrainUpstream("") // empty must not blank the override
	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposureRestricted); err != nil {
		t.Fatalf("set exposure: %v", err)
	}
	if fa := e.caddy.config(inst.ID).ForwardAuth; fa == nil || fa.Upstream != "brain.internal:9999" {
		t.Fatalf("empty SetBrainUpstream blanked the upstream: %+v", fa)
	}
}

func TestSetExposure_MissingInstance(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	if err := e.m.SetExposure(context.Background(), "nope", store.ExposureRestricted); err == nil {
		t.Fatal("SetExposure on a missing instance must error")
	}
}

// A restart (reconcile) must rebuild the route from stored exposure — it must not
// drop the Cookie strip or the forward-auth gate of a restricted hosted app.
func TestReconcile_Hosted_ReassertKeepsGate(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoami(t, e)

	// Drift: SQLite says running, Docker has no containers — reconcile restarts it.
	e.docker.psManaged = map[string]bool{}
	e.caddy.calls = nil

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !e.caddy.called("AddRoute") {
		t.Fatalf("reconcile must reassert the route: %v", e.caddy.calls)
	}
	cfg := e.caddy.config(inst.ID)
	if cfg.StripCookieName != auth.ForwardAuthCookieName || cfg.ForwardAuth == nil {
		t.Fatalf("a restart must not drop the gate or strip: %+v", cfg)
	}
}

// --- path-scoped exposure (#415) ------------------------------------------

// installWhoamiWithPublicPaths installs the same test app with an
// `access.public_paths` block, so the route builder has a manifest that declares
// one.
func installWhoamiWithPublicPaths(t *testing.T, e *testEnv, paths string) store.Instance {
	t.Helper()
	man := whoamiManifest(testDigest) + "access:\n  public_paths: " + paths + "\n"
	e.writeCatalogApp(t, "whoami", whoamiCompose, man)
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst
}

// A restricted hosted app carries its manifest's public paths into the route, so
// the token-authed API answers anonymously while the UI keeps the box login.
func TestInstall_Hosted_RestrictedCarriesPublicPaths(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoamiWithPublicPaths(t, e, `["/v1/*"]`)

	cfg := e.caddy.config(inst.ID)
	if cfg.ForwardAuth == nil {
		t.Fatal("the app must still be gated — public paths are an exception, not an off switch")
	}
	if len(cfg.PublicPaths) != 1 || cfg.PublicPaths[0] != "/v1/*" {
		t.Fatalf("PublicPaths = %v, want the manifest's declaration", cfg.PublicPaths)
	}
}

// Every hosted app route scrubs the vouched identity headers, in every exposure.
// The gate scrubs only where it runs, and it does not run on a public app or on
// a public path — so without this a caller could forge X-Moose-User there.
func TestHosted_AllExposuresScrubIdentityHeaders(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoamiWithPublicPaths(t, e, `["/v1/*"]`)

	assertScrubbed := func(what string) {
		t.Helper()
		cfg := e.caddy.config(inst.ID)
		if len(cfg.ScrubHeaders) == 0 {
			t.Fatalf("%s: route scrubs no identity headers", what)
		}
		for _, want := range []string{"X-Moose-User", "X-Moose-User-Id"} {
			found := false
			for _, h := range cfg.ScrubHeaders {
				if h == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: %s is not scrubbed, so a caller can forge it", what, want)
			}
		}
	}
	assertScrubbed("restricted with public paths")

	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposurePublic); err != nil {
		t.Fatalf("set public: %v", err)
	}
	assertScrubbed("public")
}

// A public app has no gate, so its manifest's public paths are meaningless and
// must not reach the route: nothing to carve out of.
func TestSetExposure_PublicDropsPublicPaths(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoamiWithPublicPaths(t, e, `["/v1/*"]`)
	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposurePublic); err != nil {
		t.Fatalf("set public: %v", err)
	}
	cfg := e.caddy.config(inst.ID)
	if cfg.ForwardAuth != nil {
		t.Fatal("a public app must not be gated")
	}
	if len(cfg.PublicPaths) != 0 {
		t.Fatalf("PublicPaths = %v, want none on a public app", cfg.PublicPaths)
	}
}

// The appliance route stays exactly what it was: no scrub, no strip, no gate, no
// public paths — even for a manifest that declares them.
func TestInstall_Appliance_IgnoresPublicPaths(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoamiWithPublicPaths(t, e, `["/v1/*"]`)
	cfg := e.caddy.config(inst.ID)
	if cfg.StripCookieName != "" || cfg.ForwardAuth != nil || len(cfg.PublicPaths) != 0 || len(cfg.ScrubHeaders) != 0 {
		t.Fatalf("appliance route must stay the plain reverse_proxy, got %+v", cfg)
	}
}

// A restart rebuilds the route from the instance's own manifest copy, so the
// carve-out survives a reboot instead of quietly closing (or quietly widening).
func TestReconcile_Hosted_ReassertKeepsPublicPaths(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installWhoamiWithPublicPaths(t, e, `["/v1/*"]`)

	e.docker.psManaged = map[string]bool{}
	e.caddy.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cfg := e.caddy.config(inst.ID)
	if cfg.ForwardAuth == nil || len(cfg.PublicPaths) != 1 || cfg.PublicPaths[0] != "/v1/*" {
		t.Fatalf("a restart must rebuild the same carve-out, got %+v", cfg)
	}
}
