package lifecycle

// Stop/Start scenarios (APP_LIFECYCLE.md # stop, start, uninstall). Each installs
// a real instance through the happy path, then drives Stop/Start against the
// fakes and asserts the persisted state, the Caddy route variant, and the guard
// errors for illegal transitions.

import (
	"context"
	"errors"
	"testing"

	"github.com/onmoose/moose/internal/store"
)

// install a running whoami instance for the stop/start tests to act on.
func installRunning(t *testing.T, e *testEnv) store.Instance {
	t.Helper()
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst
}

func TestStopThenStart(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)

	// Stop: compose stop, state stopped, route flips to the stopped splash.
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !e.docker.called("ComposeStop") {
		t.Fatalf("ComposeStop not called")
	}
	if row, _ := e.store.Get(inst.ID); row.State != "stopped" {
		t.Fatalf("state after stop = %q, want stopped", row.State)
	}
	if got := e.caddy.route(inst.ID); got != "splash:stopped" {
		t.Fatalf("route after stop = %q, want splash:stopped", got)
	}

	// Start: compose up, healthy, route flips back to the real upstream.
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.State != "running" {
		t.Fatalf("state after start = %q, want running", row.State)
	}
	if got := e.caddy.route(inst.ID); len(got) < 9 || got[:9] != "upstream:" {
		t.Fatalf("route after start = %q, want upstream:…", got)
	}
}

func TestStopGuardRejectsNonRunning(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	// Stopping an already-stopped app is an illegal transition.
	err := e.m.Stop(context.Background(), inst.ID)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second stop err = %v, want ErrNotRunning", err)
	}
}

// TestStartReassertsMDNSName guards #153: Start must re-announce the app's mDNS
// name, not only re-register the Caddy route. An app recovered via Start — after
// the host-agent dropped its process-local Avahi entry groups, or a prior
// install/start failed before publishing — was reachable by Host-header proxy
// but its <slug>.local stayed dark until the next brain reboot's reconcile pass.
// Stop must NOT re-publish: the stopped splash keeps resolving on the
// install-time announcement, which is intended (APP_LIFECYCLE.md # stop).
func TestStartReassertsMDNSName(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)

	// Install published the name exactly once.
	if got := e.host.publishCount(inst.Slug); got != 1 {
		t.Fatalf("publish count after install = %d, want 1", got)
	}

	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := e.host.publishCount(inst.Slug); got != 1 {
		t.Fatalf("publish count after stop = %d, want 1 (stop must not re-publish)", got)
	}

	// The bug's trigger: the host-agent restarted and lost its entry group, so
	// the name is dark while the SQLite row still expects the app to be there.
	e.host.dropPublished(inst.Slug)
	if e.host.isPublished(inst.Slug) {
		t.Fatalf("precondition: name should be dropped before Start")
	}

	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Start re-asserts both: a second Publish for the slug...
	if got := e.host.publishCount(inst.Slug); got != 2 {
		t.Fatalf("publish count after start = %d, want 2 (start must re-publish)", got)
	}
	// ...so the dropped name is announced again without a brain restart...
	if !e.host.isPublished(inst.Slug) {
		t.Fatalf("name not re-announced after start")
	}
	// ...and the stored MDNSName stays the announced name (re-asserted, in sync).
	if row, _ := e.store.Get(inst.ID); row.MDNSName != inst.Slug+".local" {
		t.Fatalf("MDNSName after start = %q, want %q", row.MDNSName, inst.Slug+".local")
	}
}

// TestPublishHostBranches exercises publishHost's non-happy paths directly: the
// mDNS-down fallback (publish fails → reconstructed primary, avahi not-OK,
// nothing persisted) and the collision-fallback persist (a published name that
// differs from the stored one is written through so Caddy and the URL follow it).
func TestPublishHostBranches(t *testing.T) {
	e := newTestEnv(t)

	// Publish fails and there's no stored name → fall back to <slug>.local,
	// report avahi not-OK, persist nothing.
	e.host.publishErr = errors.New("avahi down")
	host, ok := e.m.publishHost(context.Background(), store.Instance{ID: "i_x", Slug: "demo"})
	if ok {
		t.Fatalf("avahiOK = true, want false on publish error")
	}
	if host != "demo.local" {
		t.Fatalf("host = %q, want demo.local (reconstructed fallback)", host)
	}
	e.host.publishErr = nil

	// Publish returns a box-qualified collision fallback differing from the
	// stored name → persist the new name and key the route on it.
	inst := installRunning(t, e) // slug "whoami", MDNSName "whoami.local"
	e.host.publishName = "whoami-box.local"
	host, ok = e.m.publishHost(context.Background(), inst)
	if !ok || host != "whoami-box.local" {
		t.Fatalf("publishHost = (%q,%v), want (whoami-box.local, true)", host, ok)
	}
	if row, _ := e.store.Get(inst.ID); row.MDNSName != "whoami-box.local" {
		t.Fatalf("persisted MDNSName = %q, want whoami-box.local", row.MDNSName)
	}
}

func TestStartGuardRejectsNonStopped(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	// The instance is running, so Start must reject.
	err := e.m.Start(context.Background(), inst.ID)
	if !errors.Is(err, ErrNotStartable) {
		t.Fatalf("start err = %v, want ErrNotStartable", err)
	}
}

// TestRetryFromFailed guards #154: a `failed` instance is recoverable via the
// same Start path as `stopped` (click-to-retry). Start must be legal from
// `failed`, re-run the full healthy transition, re-assert the mDNS name (#153 —
// the reason <slug>.local returns on retry), and land back in `running` on the
// real upstream — no SQLite editing, no reinstall.
func TestRetryFromFailed(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)

	// Drive the instance into `failed`: stop, then a start whose main_service
	// never goes healthy (the install-health-timeout shape, APP_LIFECYCLE.md).
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	e.docker.inspect = func(_, _ string) (bool, string, error) { return true, "unhealthy", nil }
	if err := e.m.Start(context.Background(), inst.ID); err == nil {
		t.Fatalf("start: expected the health timeout to mark the instance failed")
	}
	if row, _ := e.store.Get(inst.ID); row.State != "failed" {
		t.Fatalf("precondition: state = %q, want failed", row.State)
	}

	// The failed instance's name may have gone dark (a host-agent restart, or the
	// failing start published before timing out) — simulate it so the retry's
	// re-publish is observable, mirroring TestStartReassertsMDNSName.
	e.host.dropPublished(inst.Slug)
	before := e.host.publishCount(inst.Slug)

	// Retry is just Start from `failed`: legal now, and the app comes healthy this
	// time (default inspect).
	e.docker.inspect = nil
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("retry (start from failed): %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.State != "running" {
		t.Fatalf("state after retry = %q, want running", row.State)
	}
	if got := e.caddy.route(inst.ID); len(got) < 9 || got[:9] != "upstream:" {
		t.Fatalf("route after retry = %q, want upstream:…", got)
	}
	// The retry re-asserted the mDNS name: one fresh Publish, so <slug>.local
	// resolves again without a brain reboot.
	if got := e.host.publishCount(inst.Slug); got != before+1 {
		t.Fatalf("publish count after retry = %d, want %d (retry must re-publish)", got, before+1)
	}
	if !e.host.isPublished(inst.Slug) {
		t.Fatalf("name not re-announced after retry")
	}
}

func TestStartHealthFailureMarksFailed(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// main_service never goes healthy on the way back up.
	e.docker.inspect = func(_, _ string) (bool, string, error) {
		return true, "unhealthy", nil
	}
	if err := e.m.Start(context.Background(), inst.ID); err == nil {
		t.Fatalf("start: expected health failure error")
	}
	if row, _ := e.store.Get(inst.ID); row.State != "failed" {
		t.Fatalf("state after failed start = %q, want failed", row.State)
	}
	if got := e.caddy.route(inst.ID); got != "splash:failed" {
		t.Fatalf("route after failed start = %q, want splash:failed", got)
	}
}
