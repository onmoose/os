package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// routeAdmin is a fake Caddy admin API that keeps a real route list, so a test
// can ask "which route serves this host now?" after a write fails (#520). It
// models the calls the route writes use, the way Caddy answers them (checked
// against caddy:2.11.4):
//
//   - PATCH /id/<id> replaces that route in place, or answers 404 when no
//     route has the @id.
//   - PUT <routesPath>/0 inserts at the front, or answers 400 when the @id is
//     already in the list (Caddy refuses a duplicate ID).
//   - DELETE /id/<id> removes the route.
//
// A call can be made to fail with no HTTP answer (the connection is reset, as
// in CI run 36490829133) or with a 500 answer, and when it fails the list is
// left as it was, the way Caddy rolls back a config it could not load. With
// failAfterApply the call changes the list first and then resets, which is a
// write Caddy applied whose answer was lost.
type routeAdmin struct {
	mu     sync.Mutex
	routes []map[string]any
	calls  []string // "METHOD path", in order

	// fail decides whether call n (counted from 1 over the admin's life)
	// fails. nil means no call fails.
	fail func(n int) bool
	// failReset makes a failing call reset the connection instead of
	// answering 500.
	failReset bool
	// failAfterApply makes a failing call apply its change, then reset.
	failAfterApply bool
	// delay makes every call wait this long before it answers.
	delay time.Duration
}

func newRouteAdmin() *routeAdmin {
	return &routeAdmin{routes: []map[string]any{catchAllRoute()}}
}

func (a *routeAdmin) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if a.delay > 0 {
			time.Sleep(a.delay)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		failing := a.fail != nil && a.fail(len(a.calls))
		if failing && !a.failAfterApply && !a.failReset {
			http.Error(w, `{"error":"injected failure"}`, http.StatusInternalServerError)
			return
		}
		status, msg := http.StatusOK, ""
		if !failing || a.failAfterApply {
			status, msg = a.apply(r, b)
		}
		if failing {
			resetConn(w)
			return
		}
		if status != http.StatusOK {
			http.Error(w, msg, status)
		}
	})
}

// resetConn closes the client's connection with a TCP reset and no answer.
func resetConn(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) // close with RST, not FIN
	}
	_ = conn.Close()
}

// apply makes the change one call asks for and returns Caddy's answer.
func (a *routeAdmin) apply(r *http.Request, body []byte) (int, string) {
	var route map[string]any
	_ = json.Unmarshal(body, &route)
	switch {
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/id/"):
		i := a.index(strings.TrimPrefix(r.URL.Path, "/id/"))
		if i < 0 {
			return http.StatusNotFound, `{"error":"unknown object ID"}`
		}
		a.routes[i] = route
	case r.Method == "PUT" && r.URL.Path == routesPath+"/0":
		if id, _ := route["@id"].(string); a.index(id) >= 0 {
			return http.StatusBadRequest, `{"error":"duplicate ID"}`
		}
		a.routes = append([]map[string]any{route}, a.routes...)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/id/"):
		i := a.index(strings.TrimPrefix(r.URL.Path, "/id/"))
		if i < 0 {
			return http.StatusNotFound, `{"error":"unknown object ID"}`
		}
		a.routes = append(a.routes[:i], a.routes[i+1:]...)
	default:
		return http.StatusTeapot, "unexpected call"
	}
	return http.StatusOK, ""
}

func (a *routeAdmin) index(id string) int {
	for i, r := range a.routes {
		if r["@id"] == id {
			return i
		}
	}
	return -1
}

// serving walks the routes in order, like Caddy, and names what answers a
// request for host: "splash", "proxy", or "catch-all".
func (a *routeAdmin) serving(host string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.routes {
		if r["@id"] == "moose-catchall" {
			return "catch-all"
		}
		m := r["match"].([]any)[0].(map[string]any)
		hosts, _ := m["host"].([]any)
		for _, h := range hosts {
			if h != host {
				continue
			}
			handle := r["handle"].([]any)
			last := handle[len(handle)-1].(map[string]any)
			switch last["handler"] {
			case "static_response":
				return "splash"
			case "reverse_proxy":
				return "proxy"
			}
			return "unknown handler"
		}
	}
	return "no catch-all"
}

func (a *routeAdmin) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *routeAdmin) sent(prefix string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

const testHost = "notes.local"

func testRoute() RouteConfig {
	return RouteConfig{InstanceID: "abc", Host: testHost, Upstream: "moose-abc-web:80"}
}

// startSplash returns a client and a fake admin where the app already has its
// "starting" splash route, which is what the flip to the app replaces.
func startSplash(t *testing.T) (*Client, *routeAdmin) {
	t.Helper()
	admin := newRouteAdmin()
	srv := httptest.NewServer(admin.handler())
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	c.retryDelay = 0
	if err := c.AddSplashRoute(context.Background(), "abc", testHost, "Notes", "starting"); err != nil {
		t.Fatalf("AddSplashRoute: %v", err)
	}
	if got := admin.serving(testHost); got != "splash" {
		t.Fatalf("after splash: %s serves %q, want splash", testHost, got)
	}
	return c, admin
}

// The #520 case: the flip from splash to app fails. The splash must keep
// serving, the caller must get the error, and nothing may delete the route
// first.
func TestAddRouteFailedWriteKeepsOldRoute(t *testing.T) {
	for _, reset := range []bool{true, false} {
		name := "500 answer"
		if reset {
			name = "connection reset"
		}
		t.Run(name, func(t *testing.T) {
			c, admin := startSplash(t)
			base := admin.callCount()
			admin.failReset = reset
			admin.fail = func(n int) bool { return n > base }

			err := c.AddRoute(context.Background(), testRoute())
			if err == nil {
				t.Fatal("AddRoute: want an error when every admin call fails")
			}
			if got := admin.serving(testHost); got != "splash" {
				t.Errorf("after failed flip: %s serves %q, want the old splash", testHost, got)
			}
			if admin.sent("DELETE") {
				t.Error("the route write sent a DELETE; a failed write must not remove the old route")
			}
		})
	}
}

// Whichever single call of the flip fails, the host is never left without a
// route: it serves the old splash (and the caller gets an error) or the new
// app. The old remove-then-add failed this at its second call, the PUT.
func TestAddRouteOneFailedCallNeverLeavesNoRoute(t *testing.T) {
	for _, reset := range []bool{true, false} {
		for failAt := 1; failAt <= 4; failAt++ {
			c, admin := startSplash(t)
			base := admin.callCount()
			admin.failReset = reset
			admin.fail = func(n int) bool { return n == base+failAt }

			err := c.AddRoute(context.Background(), testRoute())
			got := admin.serving(testHost)
			switch {
			case err != nil && got != "splash":
				t.Errorf("reset=%v failAt=%d: AddRoute failed (%v) and %s serves %q, want the old splash", reset, failAt, err, testHost, got)
			case err == nil && got != "proxy":
				t.Errorf("reset=%v failAt=%d: AddRoute succeeded and %s serves %q, want the app", reset, failAt, testHost, got)
			}
		}
	}
}

// A call that got no answer is tried again, so one reset connection does not
// strand the app on its splash.
func TestAddRouteRetriesAWriteWithNoAnswer(t *testing.T) {
	c, admin := startSplash(t)
	base := admin.callCount()
	admin.failReset = true
	admin.fail = func(n int) bool { return n == base+1 }

	if err := c.AddRoute(context.Background(), testRoute()); err != nil {
		t.Fatalf("AddRoute after one reset: %v", err)
	}
	if got := admin.serving(testHost); got != "proxy" {
		t.Errorf("%s serves %q, want the app", testHost, got)
	}
	if n := admin.callCount() - base; n != 2 {
		t.Errorf("admin calls for the flip = %d, want 2 (the reset PATCH and its retry)", n)
	}
}

// A write Caddy answered with an error is not repeated: Caddy would refuse
// the same config again.
func TestAddRouteDoesNotRetryAnAnsweredError(t *testing.T) {
	c, admin := startSplash(t)
	base := admin.callCount()
	admin.fail = func(n int) bool { return n > base }

	if err := c.AddRoute(context.Background(), testRoute()); err == nil {
		t.Fatal("AddRoute: want the 500 as an error")
	}
	if n := admin.callCount() - base; n != 1 {
		t.Errorf("admin calls = %d, want 1", n)
	}
}

// The first write for an app inserts its route ahead of the catch-all, and a
// later write replaces it in place: one route per app, catch-all still last.
func TestAddRouteInsertsOnceThenReplacesInPlace(t *testing.T) {
	admin := newRouteAdmin()
	srv := httptest.NewServer(admin.handler())
	defer srv.Close()
	c := New(srv.URL)

	if err := c.AddSplashRoute(context.Background(), "abc", testHost, "Notes", "starting"); err != nil {
		t.Fatalf("AddSplashRoute: %v", err)
	}
	if !admin.sent("PUT " + routesPath + "/0") {
		t.Error("first write: expected the PATCH's 404 to fall back to a PUT at index 0")
	}
	if err := c.AddRoute(context.Background(), testRoute()); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if len(admin.routes) != 2 {
		t.Fatalf("routes = %d, want 2 (the app and the catch-all)", len(admin.routes))
	}
	if admin.routes[0]["@id"] != routeID("abc") || admin.routes[1]["@id"] != "moose-catchall" {
		t.Errorf("route order = [%v %v], want [%s moose-catchall]", admin.routes[0]["@id"], admin.routes[1]["@id"], routeID("abc"))
	}
	if got := admin.serving(testHost); got != "proxy" {
		t.Errorf("%s serves %q, want the app", testHost, got)
	}
}

// A write Caddy applied but whose answer was lost is retried safely: the
// retry finds the route already there and replaces it again, and no duplicate
// is added. Covers both the PATCH (a flip) and the PUT (a first write).
func TestAddRouteRetryAfterAnAppliedWrite(t *testing.T) {
	t.Run("flip", func(t *testing.T) {
		c, admin := startSplash(t)
		base := admin.callCount()
		admin.failReset, admin.failAfterApply = true, true
		admin.fail = func(n int) bool { return n == base+1 }

		if err := c.AddRoute(context.Background(), testRoute()); err != nil {
			t.Fatalf("AddRoute: %v", err)
		}
		if got := admin.serving(testHost); got != "proxy" {
			t.Errorf("%s serves %q, want the app", testHost, got)
		}
		if len(admin.routes) != 2 {
			t.Errorf("routes = %d, want 2 (the app and the catch-all)", len(admin.routes))
		}
	})
	t.Run("first write", func(t *testing.T) {
		admin := newRouteAdmin()
		srv := httptest.NewServer(admin.handler())
		defer srv.Close()
		c := New(srv.URL)
		c.retryDelay = 0
		// Call 1 is the PATCH (404), call 2 the PUT: it lands, then resets.
		admin.failReset, admin.failAfterApply = true, true
		admin.fail = func(n int) bool { return n == 2 }

		if err := c.AddRoute(context.Background(), testRoute()); err != nil {
			t.Fatalf("AddRoute: %v", err)
		}
		if len(admin.routes) != 2 || admin.routes[0]["@id"] != routeID("abc") {
			t.Errorf("routes = %v, want [%s moose-catchall]", len(admin.routes), routeID("abc"))
		}
		if got := admin.serving(testHost); got != "proxy" {
			t.Errorf("%s serves %q, want the app", testHost, got)
		}
	})
}

// A call that timed out is not repeated: each try would cost the full client
// timeout, and the startup pass writes every route under one deadline.
func TestAddRouteDoesNotRetryATimeout(t *testing.T) {
	c, admin := startSplash(t)
	base := admin.callCount()
	c.http.Timeout = 20 * time.Millisecond
	admin.delay = 200 * time.Millisecond

	if err := c.AddRoute(context.Background(), testRoute()); err == nil {
		t.Fatal("AddRoute: want the timeout as an error")
	}
	time.Sleep(250 * time.Millisecond) // let the slow handler record its call
	if n := admin.callCount() - base; n != 1 {
		t.Errorf("admin calls = %d, want 1", n)
	}
}
