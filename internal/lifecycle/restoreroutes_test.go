package lifecycle

// RestoreRoutes (#613): after Caddy lost its config while the brain runs, the
// health tick writes every app's route again, the same routes the startup
// pass writes, without fighting an operation that holds the app.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"
)

// installApp installs a running copy of whoami under its own catalog id, so
// one test can hold several apps (an instance id is the manifest id plus the
// time to the second).
func installApp(t *testing.T, e *testEnv, id string) store.Instance {
	t.Helper()
	man := strings.NewReplacer("id: whoami", "id: "+id, "[whoami]", "["+id+"]").Replace(whoamiManifest(testDigest))
	e.writeCatalogApp(t, id, whoamiCompose, man)
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install %s: %v", id, err)
	}
	return inst
}

// loseRoutes is what a Caddy recreate does to the routes.
func (c *fakeCaddy) loseRoutes() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.routes = map[string]string{}
}

func (c *fakeCaddy) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, k := range c.calls {
		if k.method == method {
			n++
		}
	}
	return n
}

func restore(t *testing.T, e *testEnv, only map[string]bool) map[string]bool {
	t.Helper()
	owed, err := e.m.RestoreRoutes(context.Background(), only)
	if err != nil {
		t.Fatalf("RestoreRoutes: %v", err)
	}
	return owed
}

// A running app gets its app route back, with the same policy the install
// wrote; a stopped and a failed app get their splash. Nothing is published
// and no compose runs.
func TestRestoreRoutesWritesEveryApp(t *testing.T) {
	e := newTestEnv(t)
	run := installApp(t, e, "runapp")
	want := e.caddy.config(run.ID)
	stopped := installApp(t, e, "stopapp")
	if err := e.m.Stop(context.Background(), stopped.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	failed := installApp(t, e, "failapp")
	if err := e.store.SetState(failed.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	pubs := e.host.publishCount(run.Slug)
	dockerCalls := len(e.docker.methods())
	e.caddy.loseRoutes()

	if owed := restore(t, e, nil); len(owed) != 0 {
		t.Fatalf("owed = %v, want none", owed)
	}
	if got := e.caddy.route(run.ID); got != "upstream:"+want.Upstream {
		t.Errorf("running app route = %q, want upstream:%s", got, want.Upstream)
	}
	if got := e.caddy.config(run.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("restored route config = %+v, want the install's %+v", got, want)
	}
	if got := e.caddy.route(stopped.ID); got != "splash:stopped" {
		t.Errorf("stopped app route = %q, want splash:stopped", got)
	}
	if got := e.caddy.route(failed.ID); got != "splash:failed" {
		t.Errorf("failed app route = %q, want splash:failed", got)
	}
	if got := e.host.publishCount(run.Slug); got != pubs {
		t.Errorf("publish count = %d, want %d: a lost Caddy config does not touch mDNS", got, pubs)
	}
	if got := e.docker.methods()[dockerCalls:]; len(got) != 0 {
		t.Errorf("docker called: %v", got)
	}
}

// An app whose lock is held is owed, not written, and is written on the next
// call once the lock is free. That call writes only the owed apps.
func TestRestoreRoutesOwesALockedApp(t *testing.T) {
	e := newTestEnv(t)
	busy := installApp(t, e, "busyapp")
	other := installApp(t, e, "freeapp")
	e.caddy.loseRoutes()

	unlock := e.m.lockInstance(busy.ID)
	owed := restore(t, e, nil)
	if !reflect.DeepEqual(owed, map[string]bool{busy.ID: true}) {
		t.Fatalf("owed = %v, want only %s", owed, busy.ID)
	}
	if got := e.caddy.route(busy.ID); got != "" {
		t.Fatalf("a locked app's route was written: %q", got)
	}
	if got := e.caddy.route(other.ID); got == "" {
		t.Fatal("the free app's route was not written")
	}
	unlock()

	adds := e.caddy.count("AddRoute")
	if owed := restore(t, e, owed); len(owed) != 0 {
		t.Fatalf("owed after the lock was freed = %v, want none", owed)
	}
	if got := e.caddy.route(busy.ID); got == "" {
		t.Fatal("the owed app's route was not written once its lock was free")
	}
	if got := e.caddy.count("AddRoute") - adds; got != 1 {
		t.Errorf("AddRoute calls = %d, want 1: only the owed app is written again", got)
	}
}

// An app being installed is owed until its row settles; an app that is gone
// drops out.
func TestRestoreRoutesWaitsForAnInstall(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	if err := e.store.SetState(inst.ID, "installing"); err != nil {
		t.Fatal(err)
	}
	e.caddy.loseRoutes()
	owed := restore(t, e, nil)
	if !owed[inst.ID] {
		t.Fatalf("an installing app is not owed: %v", owed)
	}
	if got := e.caddy.route(inst.ID); got != "" {
		t.Fatalf("an installing app's route was written: %q", got)
	}
	if err := e.store.SetState(inst.ID, "running"); err != nil {
		t.Fatal(err)
	}
	if owed := restore(t, e, owed); len(owed) != 0 {
		t.Fatalf("owed = %v, want none once the app runs", owed)
	}
	if got := e.caddy.route(inst.ID); got == "" {
		t.Fatal("the route was not written once the app runs")
	}

	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if owed := restore(t, e, map[string]bool{inst.ID: true}); len(owed) != 0 {
		t.Fatalf("an uninstalled app is still owed: %v", owed)
	}
}

// A failed write is owed and written on the next call.
func TestRestoreRoutesOwesAFailedWrite(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	e.caddy.loseRoutes()
	e.caddy.mu.Lock()
	e.caddy.writeErr = errors.New("caddy admin unreachable")
	e.caddy.mu.Unlock()
	owed := restore(t, e, nil)
	if !owed[inst.ID] {
		t.Fatalf("a failed write is not owed: %v", owed)
	}
	e.caddy.mu.Lock()
	e.caddy.writeErr = nil
	e.caddy.mu.Unlock()
	if owed := restore(t, e, owed); len(owed) != 0 {
		t.Fatalf("owed = %v, want none", owed)
	}
	if got := e.caddy.route(inst.ID); got == "" {
		t.Fatal("the route was not written on the second call")
	}
}
