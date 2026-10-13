package caddy

import (
	"context"
	"testing"
)

// recreate makes the fake look like a Caddy that was just recreated: it runs
// on the staged caddy.json alone, so the moose server has its catch-all and the
// trusted proxies with no mark, and there is no probe server (#613).
func (a *probeAdmin) recreate() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.routes = []map[string]any{catchAllRoute()}
	a.trusted = trustedProxies()
	a.probe, a.probeRts = nil, nil
}

func (a *probeAdmin) routeIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for _, r := range a.routes {
		ids = append(ids, r["@id"].(string))
	}
	return ids
}

func configured(t *testing.T, c *Client) bool {
	t.Helper()
	ok, err := c.Configured(context.Background())
	if err != nil {
		t.Fatalf("Configured: %v", err)
	}
	return ok
}

// mark runs BeginConfig then MarkConfigured, as the brain does around a full
// write.
func mark(t *testing.T, c *Client) {
	t.Helper()
	tok, err := c.BeginConfig(context.Background())
	if err != nil {
		t.Fatalf("BeginConfig: %v", err)
	}
	if err := c.MarkConfigured(context.Background(), tok); err != nil {
		t.Fatalf("MarkConfigured: %v", err)
	}
}

// The mark is there only after MarkConfigured, and every write of the trusted
// proxies without it takes it away: the startup reset, the start of a new
// write, and a Caddy recreate.
func TestConfiguredMark(t *testing.T) {
	ctx := context.Background()
	c, a := newProbeAdmin(t, true)
	a.recreate()
	if configured(t, c) {
		t.Fatal("a Caddy on the staged caddy.json reads as configured")
	}
	mark(t, c)
	if !configured(t, c) {
		t.Fatal("no mark after MarkConfigured")
	}
	if a.trusted["source"] != "static" || len(a.trusted["ranges"].([]any)) != 0 {
		t.Errorf("the mark changed the trusted proxies: %v", a.trusted)
	}
	if err := c.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	if configured(t, c) {
		t.Error("the mark is still there after EnsureServer reset the routes")
	}
	mark(t, c)
	if _, err := c.BeginConfig(ctx); err != nil {
		t.Fatalf("BeginConfig: %v", err)
	}
	if configured(t, c) {
		t.Error("the mark is still there after BeginConfig started a new write")
	}
	mark(t, c)
	a.recreate()
	if configured(t, c) {
		t.Error("a recreated Caddy reads as configured")
	}
}

// The mark goes only on the Caddy that got the writes. Writes in between keep
// the token good; a recreate in between makes MarkConfigured refuse, so the
// brain never marks a Caddy that lost the config while it was being written.
func TestMarkConfiguredRefusesARecreatedCaddy(t *testing.T) {
	ctx := context.Background()
	c, a := newProbeAdmin(t, true)
	a.recreate()
	tok, err := c.BeginConfig(ctx)
	if err != nil {
		t.Fatalf("BeginConfig: %v", err)
	}
	if err := c.RestoreServer(ctx); err != nil {
		t.Fatalf("RestoreServer: %v", err)
	}
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	a.recreate()
	if err := c.MarkConfigured(ctx, tok); err == nil {
		t.Fatal("MarkConfigured marked a Caddy that was recreated after BeginConfig")
	}
	if configured(t, c) {
		t.Fatal("marked after a refused MarkConfigured")
	}

	tok, err = c.BeginConfig(ctx)
	if err != nil {
		t.Fatalf("BeginConfig: %v", err)
	}
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := c.MarkConfigured(ctx, tok); err != nil {
		t.Fatalf("MarkConfigured after writes on the same Caddy: %v", err)
	}
	if !configured(t, c) {
		t.Fatal("no mark")
	}
}

// An answer that is neither "there" nor "not there" is an error, so the
// brain never rewrites a config over a Caddy that is only having trouble.
func TestConfiguredUnclearAnswerIsAnError(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	a.failPrefix = "GET /id/" + configuredID
	if _, err := c.Configured(context.Background()); err == nil {
		t.Fatal("a 500 read as a clear answer")
	}
}

// After a recreate, RestoreServer puts back the probe server and keeps the
// one catch-all at the end. It never resets the route list.
func TestRestoreServerAfterRecreate(t *testing.T) {
	ctx := context.Background()
	c, a := newProbeAdmin(t, true)
	a.recreate()
	// An install that wrote its route between the recreate and the restore.
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := c.RestoreServer(ctx); err != nil {
		t.Fatalf("RestoreServer: %v", err)
	}
	if ids := a.routeIDs(); len(ids) != 2 || ids[0] != routeID("abc") || ids[1] != "moose-catchall" {
		t.Fatalf("routes = %v, want [%s moose-catchall]", ids, routeID("abc"))
	}
	if a.probe == nil {
		t.Fatal("RestoreServer did not write the probe server")
	}
	if ids := a.probeIDs(); len(ids) != 2 || ids[0] != probeRouteID("abc") || ids[1] != probeCatchAllID {
		t.Fatalf("probe routes = %v, want [%s %s]", ids, probeRouteID("abc"), probeCatchAllID)
	}
	if a.sentTo("PATCH " + routesPath) {
		t.Error("RestoreServer reset the route list")
	}
}

// On a Caddy that still has everything, RestoreServer changes nothing: no
// route and no probe copy is lost, and no second catch-all is added.
func TestRestoreServerKeepsWhatIsThere(t *testing.T) {
	ctx := context.Background()
	c, a := newProbeAdmin(t, true)
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := c.RestoreServer(ctx); err != nil {
			t.Fatalf("RestoreServer: %v", err)
		}
	}
	if ids := a.routeIDs(); len(ids) != 2 || ids[1] != "moose-catchall" {
		t.Fatalf("routes = %v, want the app route and one catch-all", ids)
	}
	if ids := a.probeIDs(); len(ids) != 2 || ids[0] != probeRouteID("abc") {
		t.Fatalf("probe routes = %v, want the copy kept", ids)
	}
	if a.sentTo("POST " + probeServerPath) {
		t.Error("RestoreServer wrote the whole probe server over one that was there")
	}
}

// A catch-all that is missing is appended, so it stays behind every app route.
func TestRestoreServerAppendsMissingCatchAll(t *testing.T) {
	ctx := context.Background()
	c, a := newProbeAdmin(t, true)
	a.routes = nil
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := c.RestoreServer(ctx); err != nil {
		t.Fatalf("RestoreServer: %v", err)
	}
	if ids := a.routeIDs(); len(ids) != 2 || ids[1] != "moose-catchall" {
		t.Fatalf("routes = %v, want the catch-all last", ids)
	}
}

// With the probe server off, RestoreServer never touches a probe path.
func TestRestoreServerWithoutProbe(t *testing.T) {
	c, a := newProbeAdmin(t, false)
	a.recreate()
	if err := c.RestoreServer(context.Background()); err != nil {
		t.Fatalf("RestoreServer: %v", err)
	}
	if a.sentTo("moose-probe") {
		t.Error("RestoreServer touched the probe server while it is off")
	}
}

// A failed step is returned, so the caller does not mark the config written.
func TestRestoreServerReturnsAFailedStep(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	a.recreate()
	a.failPrefix = "POST " + probeServerPath
	if err := c.RestoreServer(context.Background()); err == nil {
		t.Fatal("a failed probe server write was not returned")
	}
}
