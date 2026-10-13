package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// probeAdmin is a fake Caddy admin API with the moose server's route list and
// the moose-probe server (#610). It answers the calls the client makes the
// way Caddy does (checked against caddy 2.11.4): a PUT into the route list of
// a server that does not exist is refused, GET /id/<id> answers 404 for an
// unknown @id, and POST on a server path creates or replaces the server.
type probeAdmin struct {
	mu     sync.Mutex
	routes []map[string]any // the moose server's routes
	// probe is the moose-probe server without its routes, and nil when the
	// server does not exist. probeRts are its routes.
	probe    map[string]any
	probeRts []map[string]any
	calls    []string
	// failPrefix makes every call whose "METHOD path" starts with it answer
	// 500 and change nothing.
	failPrefix string
}

func newProbeAdmin(t *testing.T, withProbeServer bool) (*Client, *probeAdmin) {
	t.Helper()
	a := &probeAdmin{routes: []map[string]any{catchAllRoute()}}
	c := New("")
	if withProbeServer {
		c.EnableProbeServer("unix//run/moose-caddy/probe.sock")
		a.setProbeServer(roundTrip(t, c.probeServer()))
	}
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	c.admin = srv.URL
	c.retryDelay = 0
	return c, a
}

// roundTrip passes v through JSON, so the fake holds what Caddy would get.
func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (a *probeAdmin) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	a.calls = append(a.calls, call)
	if a.failPrefix != "" && strings.HasPrefix(call, a.failPrefix) {
		http.Error(w, `{"error":"injected failure"}`, http.StatusInternalServerError)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/id/")
	switch {
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/id/"):
		if list, i := a.find(id); list == nil || i < 0 {
			http.Error(w, `{"error":"unknown object ID"}`, http.StatusNotFound)
		}
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/id/"):
		list, i := a.find(id)
		if list == nil {
			http.Error(w, `{"error":"unknown object ID"}`, http.StatusNotFound)
			return
		}
		var route map[string]any
		_ = json.Unmarshal(b, &route)
		(*list)[i] = route
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/id/"):
		list, i := a.find(id)
		if list == nil {
			http.Error(w, `{"error":"unknown object ID"}`, http.StatusNotFound)
			return
		}
		*list = append((*list)[:i], (*list)[i+1:]...)
	case r.Method == "PUT" && (r.URL.Path == routesPath+"/0" || r.URL.Path == probeRoutesPath+"/0"):
		var route map[string]any
		_ = json.Unmarshal(b, &route)
		if r.URL.Path == probeRoutesPath+"/0" && a.probe == nil {
			// What Caddy answers for a PUT into a server that is not there.
			http.Error(w, `{"error":"loading new config: ... cannot unmarshal object into Go struct field Server.servers.routes"}`, http.StatusInternalServerError)
			return
		}
		if l, _ := a.find(route["@id"].(string)); l != nil {
			http.Error(w, `{"error":"duplicate ID"}`, http.StatusBadRequest)
			return
		}
		if r.URL.Path == routesPath+"/0" {
			a.routes = append([]map[string]any{route}, a.routes...)
		} else {
			a.probeRts = append([]map[string]any{route}, a.probeRts...)
		}
	case r.Method == "POST" && r.URL.Path == probeServerPath:
		var srv map[string]any
		_ = json.Unmarshal(b, &srv)
		a.setProbeServer(srv)
	case r.Method == "POST" && r.URL.Path == "/config/apps/http/servers/moose/trusted_proxies":
	case r.Method == "PATCH" && r.URL.Path == routesPath:
		a.routes = nil
	default:
		http.Error(w, "unexpected call "+call, http.StatusTeapot)
	}
}

// setProbeServer stores a whole probe server, as POST on its path does.
func (a *probeAdmin) setProbeServer(srv map[string]any) {
	a.probeRts = nil
	raw, _ := srv["routes"].([]any)
	for _, r := range raw {
		a.probeRts = append(a.probeRts, r.(map[string]any))
	}
	delete(srv, "routes")
	a.probe = srv
}

// dropProbeServer is what a Caddy recreate does to the probe server.
func (a *probeAdmin) dropProbeServer() { a.probe, a.probeRts = nil, nil }

// find returns the list holding the route with this @id and its index, or a
// nil list.
func (a *probeAdmin) find(id string) (*[]map[string]any, int) {
	for _, list := range []*[]map[string]any{&a.routes, &a.probeRts} {
		for i, r := range *list {
			if r["@id"] == id {
				return list, i
			}
		}
	}
	return nil, -1
}

func (a *probeAdmin) probeRoute(id string) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.probeRts {
		if r["@id"] == id {
			return r
		}
	}
	return nil
}

func (a *probeAdmin) probeIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for _, r := range a.probeRts {
		ids = append(ids, r["@id"].(string))
	}
	return ids
}

func (a *probeAdmin) sentTo(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.calls {
		if strings.Contains(c, path) {
			return true
		}
	}
	return false
}

func gatedRoute() RouteConfig {
	return RouteConfig{
		InstanceID:      "abc",
		Host:            "notes.cindy-fox.onmoose.io",
		Upstream:        "moose-abc-web:80",
		StripCookieName: "moose_forward_auth",
		ScrubHeaders:    []string{"X-Moose-User"},
		ForwardAuth: &ForwardAuthConfig{
			Upstream: "moose-brain:8080", VerifyPath: "/api/v1/auth/forward", LoginURL: "https://cindy-fox.onmoose.io/",
		},
		PublicPaths: []string{"/v1/*"},
	}
}

// EnsureServer writes the whole probe server: the socket listener, no
// automatic HTTPS, and only the marked catch-all.
func TestEnsureServerWritesProbeServer(t *testing.T) {
	c, a := newProbeAdmin(t, false)
	c.EnableProbeServer("unix//run/moose-caddy/probe.sock")
	if err := c.EnsureServer(context.Background()); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	if a.probe == nil {
		t.Fatal("EnsureServer did not write the probe server")
	}
	if l, _ := a.probe["listen"].([]any); len(l) != 1 || l[0] != "unix//run/moose-caddy/probe.sock" {
		t.Errorf("probe listen = %v, want only the unix socket (no TCP listener an app could reach)", a.probe["listen"])
	}
	if ah, _ := a.probe["automatic_https"].(map[string]any); ah["disable"] != true {
		t.Errorf("automatic_https = %v, want disable: true", a.probe["automatic_https"])
	}
	if ids := a.probeIDs(); len(ids) != 1 || ids[0] != probeCatchAllID {
		t.Fatalf("probe routes = %v, want only the catch-all", ids)
	}
	h := a.probeRoute(probeCatchAllID)["handle"].([]any)[0].(map[string]any)
	hdr := h["headers"].(map[string]any)[ProbeHeader].([]any)
	if hdr[0] != ProbeNoRoute {
		t.Errorf("catch-all mark = %v, want %s: %s", hdr, ProbeHeader, ProbeNoRoute)
	}
}

// Without EnableProbeServer the client never touches a probe path: the dev
// loop, and a box whose admin API is still on TCP, where Caddy cannot open
// the socket and would refuse every config with the server in it.
func TestProbeServerOffByDefault(t *testing.T) {
	c, a := newProbeAdmin(t, false)
	ctx := context.Background()
	if err := c.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	if err := c.AddSplashRoute(ctx, "abc", "notes.local", "Notes", "starting"); err != nil {
		t.Fatalf("AddSplashRoute: %v", err)
	}
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := c.RemoveRoute(ctx, "abc"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	if a.sentTo("moose-probe") {
		t.Fatalf("calls %v touch the probe server with it off", a.calls)
	}
}

// The probe's copy of a gated app route has the same host and upstream, and no
// gate: no forward_auth, no public-path subroute. It also deletes the mark
// from the app's answer, so an app cannot pass for "no route".
func TestAddRouteWritesUngatedProbeCopy(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	if err := c.AddRoute(context.Background(), gatedRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	r := a.probeRoute(probeRouteID("abc"))
	if r == nil {
		t.Fatalf("no probe route; probe routes = %v", a.probeIDs())
	}
	if ids := a.probeIDs(); ids[len(ids)-1] != probeCatchAllID {
		t.Errorf("probe routes = %v, want the catch-all last", ids)
	}
	b, _ := json.Marshal(r)
	for _, bad := range []string{"forward", "subroute", "/v1/*", "moose-brain:8080"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("probe route has %q; it must have no gate: %s", bad, b)
		}
	}
	if !strings.Contains(string(b), `"host":["notes.cindy-fox.onmoose.io"]`) {
		t.Errorf("probe route does not match the app host: %s", b)
	}
	h := r["handle"].([]any)
	if len(h) != 1 {
		t.Fatalf("probe handle = %s, want one reverse_proxy", b)
	}
	proxy := h[0].(map[string]any)
	if proxy["handler"] != "reverse_proxy" || !strings.Contains(string(b), `"dial":"moose-abc-web:80"`) {
		t.Errorf("probe handle = %s, want reverse_proxy to moose-abc-web:80", b)
	}
	del := proxy["headers"].(map[string]any)["response"].(map[string]any)["delete"].([]any)
	if len(del) != 1 || del[0] != ProbeHeader {
		t.Errorf("probe proxy deletes %v from the answer, want %s", del, ProbeHeader)
	}
}

// A splash is copied as it is, and the flip to the app replaces the copy in
// place, as it does the app route.
func TestSplashThenAppReplacesProbeCopy(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	ctx := context.Background()
	if err := c.AddSplashRoute(ctx, "abc", testHost, "Notes", "starting"); err != nil {
		t.Fatalf("AddSplashRoute: %v", err)
	}
	h := a.probeRoute(probeRouteID("abc"))["handle"].([]any)[0].(map[string]any)
	if h["handler"] != "static_response" || !strings.Contains(h["body"].(string), "starting") {
		t.Fatalf("probe copy of the splash = %v, want the same splash", h)
	}
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if ids := a.probeIDs(); len(ids) != 2 {
		t.Fatalf("probe routes = %v, want the app and the catch-all", ids)
	}
	h = a.probeRoute(probeRouteID("abc"))["handle"].([]any)[0].(map[string]any)
	if h["handler"] != "reverse_proxy" {
		t.Errorf("after the flip the probe copy is %v, want reverse_proxy", h["handler"])
	}
}

// A Caddy recreate loses the probe server (#613). The next route write puts it
// back, catch-all included, then writes the app's copy.
func TestRouteWriteRestoresMissingProbeServer(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	a.dropProbeServer()
	if err := c.AddRoute(context.Background(), testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	ids := a.probeIDs()
	if len(ids) != 2 || ids[0] != probeRouteID("abc") || ids[1] != probeCatchAllID {
		t.Fatalf("probe routes = %v, want [%s %s]", ids, probeRouteID("abc"), probeCatchAllID)
	}
	if l, _ := a.probe["listen"].([]any); len(l) != 1 || l[0] != "unix//run/moose-caddy/probe.sock" {
		t.Errorf("restored probe listen = %v", a.probe["listen"])
	}
}

// A failed probe copy does not fail the app route write: the app route is what
// users get, and an install must not fail over the probe.
func TestProbeCopyFailureDoesNotFailRouteWrite(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	a.failPrefix = "PUT " + probeRoutesPath
	if err := c.AddRoute(context.Background(), testRoute()); err != nil {
		t.Fatalf("AddRoute failed over the probe copy: %v", err)
	}
	if a.routes[0]["@id"] != routeID("abc") {
		t.Errorf("app route not written: %v", a.routes[0]["@id"])
	}
	if a.probeRoute(probeRouteID("abc")) != nil {
		t.Error("probe copy written despite the injected failure")
	}
}

// When the app route write fails, the probe copy is not written: it must not
// run ahead of what browsers get.
func TestFailedRouteWriteSkipsProbeCopy(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	a.failPrefix = "PUT " + routesPath
	if err := c.AddRoute(context.Background(), testRoute()); err == nil {
		t.Fatal("AddRoute: want the app route's error")
	}
	if a.probeRoute(probeRouteID("abc")) != nil {
		t.Error("probe copy written although the app route write failed")
	}
}

// Removing an app's route removes its probe copy too.
func TestRemoveRouteRemovesProbeCopy(t *testing.T) {
	c, a := newProbeAdmin(t, true)
	ctx := context.Background()
	if err := c.AddRoute(ctx, testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if err := c.RemoveRoute(ctx, "abc"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	if ids := a.probeIDs(); len(ids) != 1 || ids[0] != probeCatchAllID {
		t.Errorf("probe routes after remove = %v, want only the catch-all", ids)
	}
	if a.routes[0]["@id"] != "moose-catchall" {
		t.Errorf("app route not removed: %v", a.routes[0]["@id"])
	}
}
