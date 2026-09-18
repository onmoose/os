package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingAdmin is a stand-in for Caddy's admin API: it records the method +
// path + decoded body of each request and answers a programmable status per
// path so the client's request shaping can be asserted without a real Caddy.
type recordingAdmin struct {
	mu     sync.Mutex
	calls  []adminCall
	status map[string]int // path → status to return on GET (default 200)
}

type adminCall struct {
	method string
	path   string
	body   map[string]any
}

func (a *recordingAdmin) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
		}
		a.mu.Lock()
		a.calls = append(a.calls, adminCall{method: r.Method, path: r.URL.Path, body: body})
		st := http.StatusOK
		if s, ok := a.status[r.URL.Path]; ok {
			st = s
		}
		a.mu.Unlock()
		w.WriteHeader(st)
	})
}

func (a *recordingAdmin) find(method, pathContains string) *adminCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.calls {
		c := a.calls[i]
		if c.method == method && strings.Contains(c.path, pathContains) {
			return &c
		}
	}
	return nil
}

func TestEnsureDashboardInstallsSplitRoute(t *testing.T) {
	admin := &recordingAdmin{}
	srv := httptest.NewServer(admin.handler())
	defer srv.Close()
	c := New(srv.URL)

	if err := c.EnsureDashboard(context.Background(), "moose.local", "moose-brain:8080", "moose-ui:80"); err != nil {
		t.Fatalf("EnsureDashboard: %v", err)
	}

	// Idempotency: it deletes any prior route by @id before inserting.
	if admin.find("DELETE", "/id/"+dashboardRouteID) == nil {
		t.Error("expected a DELETE of the dashboard route @id before insert")
	}
	// The route is PUT at index 0 so it sorts before the catch-all.
	put := admin.find("PUT", "/routes/0")
	if put == nil {
		t.Fatal("expected a PUT to routes/0 for the dashboard route")
	}
	if put.body["@id"] != dashboardRouteID {
		t.Errorf("route @id = %v, want %s", put.body["@id"], dashboardRouteID)
	}

	// Walk into the subroute and assert the /api leg targets the brain with
	// buffering disabled and the fallback leg targets the UI.
	handle := put.body["handle"].([]any)[0].(map[string]any)
	if handle["handler"] != "subroute" {
		t.Fatalf("dashboard handler = %v, want subroute", handle["handler"])
	}
	routes := handle["routes"].([]any)
	apiLeg := routes[0].(map[string]any)
	apiMatch := apiLeg["match"].([]any)[0].(map[string]any)
	apiPaths := apiMatch["path"].([]any)
	if len(apiPaths) != 2 || apiPaths[0] != "/api/*" || apiPaths[1] != "/_moose/*" {
		t.Errorf("api leg paths = %v, want [/api/* /_moose/*]", apiPaths)
	}
	apiProxy := apiLeg["handle"].([]any)[0].(map[string]any)
	if apiProxy["handler"] != "reverse_proxy" {
		t.Errorf("api leg handler = %v", apiProxy["handler"])
	}
	if apiProxy["flush_interval"].(float64) != -1 {
		t.Errorf("api leg flush_interval = %v, want -1 (SSE buffering off)", apiProxy["flush_interval"])
	}
	if dial := apiProxy["upstreams"].([]any)[0].(map[string]any)["dial"]; dial != "moose-brain:8080" {
		t.Errorf("api leg dial = %v, want moose-brain:8080", dial)
	}
	uiLeg := routes[1].(map[string]any)
	if _, hasMatch := uiLeg["match"]; hasMatch {
		t.Error("ui fallback leg should have no match (catch-all within the host)")
	}
	uiProxy := uiLeg["handle"].([]any)[0].(map[string]any)
	if dial := uiProxy["upstreams"].([]any)[0].(map[string]any)["dial"]; dial != "moose-ui:80" {
		t.Errorf("ui leg dial = %v, want moose-ui:80", dial)
	}
}

func TestEnsureWildcardTLS(t *testing.T) {
	admin := &recordingAdmin{}
	srv := httptest.NewServer(admin.handler())
	defer srv.Close()
	c := New(srv.URL)

	subjects := []string{"cindy-fox.onmoose.io", "*.cindy-fox.onmoose.io"}
	enr := EnrollmentCredentials{Subdomain: "abc-123", Username: "u", Password: "p"}
	if err := c.EnsureWildcardTLS(context.Background(), subjects, "https://auth.onmoose.io", enr); err != nil {
		t.Fatalf("EnsureWildcardTLS: %v", err)
	}

	// Idempotent remove-then-put of the tls app.
	if admin.find("DELETE", "/config/apps/tls") == nil {
		t.Error("expected a DELETE of the tls app before PUT")
	}

	put := admin.find("PUT", "/config/apps/tls")
	if put == nil {
		t.Fatal("expected a PUT to /config/apps/tls")
	}

	// The wildcard must be listed in certificates.automate — the whole fix. A
	// policy alone only says *how* to manage a matching name; without an automate
	// entry Caddy never places the wildcard order (the live #278 symptom).
	automate := put.body["certificates"].(map[string]any)["automate"].([]any)
	if len(automate) != 1 || automate[0] != "*.cindy-fox.onmoose.io" {
		t.Errorf("certificates.automate = %v, want [*.cindy-fox.onmoose.io]", automate)
	}

	// Exactly one automation policy — the wildcard, pinned to the acme-dns issuer.
	policies := put.body["automation"].(map[string]any)["policies"].([]any)
	if len(policies) != 1 {
		t.Fatalf("policies = %v, want exactly 1 (wildcard only)", policies)
	}
	policy := policies[0].(map[string]any)
	gotSubjects := policy["subjects"].([]any)
	if len(gotSubjects) != 1 || gotSubjects[0] != "*.cindy-fox.onmoose.io" {
		t.Errorf("policy subjects = %v, want [*.cindy-fox.onmoose.io]", gotSubjects)
	}
	assertACMEIssuer(t, policy, "https://auth.onmoose.io", "u", "p", "abc-123")

	// The :443 listener is added alongside :80.
	if admin.find("PATCH", "/servers/moose/listen") == nil {
		t.Fatal("expected a PATCH of the server listen array")
	}

	// The base subject is NOT routed through acme-dns: it is served by Caddy's
	// default issuer (tls-alpn-01/http-01), so no base policy is ever POSTed and
	// only one acme-dns order (the wildcard's) ever runs.
	if admin.find("POST", "/config/apps/tls/automation/policies") != nil {
		t.Error("did not expect a base-subject policy POST (base is served via the default issuer)")
	}
}

// routeHandle PUTs a RouteConfig and returns the route's decoded "handle" array.
func routeHandle(t *testing.T, cfg RouteConfig) []any {
	t.Helper()
	admin := &recordingAdmin{}
	srv := httptest.NewServer(admin.handler())
	defer srv.Close()
	if err := New(srv.URL).AddRoute(context.Background(), cfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	put := admin.find("PUT", "/routes/0")
	if put == nil {
		t.Fatal("expected a PUT to routes/0 for the app route")
	}
	return put.body["handle"].([]any)
}

// Appliance / no-policy: the emitted route is the plain reverse_proxy, byte for
// byte — no headers manipulation (no Cookie strip), no forward_auth wrap.
func TestAddRoute_Plain(t *testing.T) {
	handle := routeHandle(t, RouteConfig{InstanceID: "i1", Host: "whoami.local", Upstream: "app:80"})
	if len(handle) != 1 {
		t.Fatalf("plain route must be a single reverse_proxy, got %d handlers", len(handle))
	}
	h := handle[0].(map[string]any)
	if h["handler"] != "reverse_proxy" {
		t.Errorf("handler = %v, want reverse_proxy", h["handler"])
	}
	if _, ok := h["headers"]; ok {
		t.Error("plain route must not touch headers (no Cookie strip)")
	}
	if dial := h["upstreams"].([]any)[0].(map[string]any)["dial"]; dial != "app:80" {
		t.Errorf("dial = %v, want app:80", dial)
	}
}

// Public hosted app: plain reverse_proxy but with the forward-auth cookie
// stripped, so the box's Domain-scoped cookie never reaches the app upstream.
func TestAddRoute_StripCookieOnly(t *testing.T) {
	handle := routeHandle(t, RouteConfig{InstanceID: "i1", Host: "h", Upstream: "app:80", StripCookieName: "moose_forward_auth"})
	if len(handle) != 1 {
		t.Fatalf("public route must be a single reverse_proxy, got %d", len(handle))
	}
	assertCookieStripped(t, handle[0].(map[string]any))
}

// The strip is name-scoped, so its correctness is entirely in the regex. This is
// the table that holds it: every case is a Cookie header a browser could really
// send to a hosted app, including the ones an app can arrange for itself by
// setting cookies on its own subdomain. Both halves of the invariant are
// asserted for each: the token never survives, and nothing else is touched.
func TestAddRoute_CookieStripBehaviour(t *testing.T) {
	const secret = "REAL_TOKEN"
	handle := routeHandle(t, RouteConfig{InstanceID: "i1", Host: "h", Upstream: "app:80", StripCookieName: "moose_forward_auth"})
	proxy := handle[0].(map[string]any)

	cases := []struct{ name, in, want string }{
		{"typical", "app_sess=abc; moose_forward_auth=" + secret + "; other=1", "app_sess=abc; other=1"},
		{"token first", "moose_forward_auth=" + secret + "; app_sess=abc", "app_sess=abc"},
		{"token last", "app_sess=abc; moose_forward_auth=" + secret, "app_sess=abc"},
		{"token only", "moose_forward_auth=" + secret, ""},
		{"empty value", "moose_forward_auth=; app_sess=abc", "app_sess=abc"},
		// An empty-but-present header. A request with no Cookie header at all
		// never reaches the replacement (verified against real Caddy: the header
		// stays absent rather than being created empty).
		{"empty header", "", ""},
		{"nothing to strip", "app_sess=abc; other=1", "app_sess=abc; other=1"},
		// An app can set its own host-only cookie of the same name on its
		// subdomain, so the browser sends the name twice and the app controls the
		// ordering. A first-match-only strip would hand it the real token.
		{"duplicate names, real one second", "moose_forward_auth=junk; moose_forward_auth=" + secret + "; a=1", "a=1"},
		{"duplicate names, real one first", "moose_forward_auth=" + secret + "; moose_forward_auth=junk; a=1", "a=1"},
		// Cookies whose names merely contain the forward-auth name must survive
		// untouched. An unanchored regex silently mangles these.
		{"prefixed name", "evil_moose_forward_auth=junk; app_sess=abc", "evil_moose_forward_auth=junk; app_sess=abc"},
		{"suffixed name", "moose_forward_auth_x=junk; app_sess=abc", "moose_forward_auth_x=junk; app_sess=abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyEmittedCookieStrip(t, proxy, tc.in)
			if got != tc.want {
				t.Errorf("Cookie at app upstream = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, secret) {
				t.Errorf("forward-auth token reached the app upstream: %q", got)
			}
		})
	}
}

// A name with regex metacharacters must not compile into a pattern that matches
// more than itself (regexp.QuoteMeta). The only caller passes a Go constant
// today, but the strip is a security control and the next caller might not.
func TestAddRoute_CookieStripQuotesMetacharacters(t *testing.T) {
	handle := routeHandle(t, RouteConfig{InstanceID: "i1", Host: "h", Upstream: "app:80", StripCookieName: "a.b"})
	got := applyEmittedCookieStrip(t, handle[0].(map[string]any), "axb=keepme; a.b=drop; c=1")
	if want := "axb=keepme; c=1"; got != want {
		t.Errorf("Cookie at app upstream = %q, want %q (a.b must not match axb)", got, want)
	}
}

// Restricted hosted app: the forward_auth gate in front of the (Cookie-stripped)
// app reverse_proxy. This asserts the full native-JSON shape of the Caddyfile
// forward_auth directive plus the added 401→login redirect block.
func TestAddRoute_ForwardAuthGate(t *testing.T) {
	handle := routeHandle(t, RouteConfig{
		InstanceID: "i1", Host: "h", Upstream: "app:80", StripCookieName: "moose_forward_auth",
		ForwardAuth: &ForwardAuthConfig{
			Upstream:    "moose-brain:8080",
			VerifyPath:  "/_moose/forward-auth/verify",
			CopyHeaders: []string{"X-Moose-User", "X-Moose-User-Id"},
			LoginURL:    "https://cindy-fox.onmoose.io/",
		},
	})
	if len(handle) != 2 {
		t.Fatalf("restricted route = forward_auth + app proxy, got %d handlers", len(handle))
	}

	// [0] the forward_auth reverse_proxy to the brain verify endpoint.
	auth := handle[0].(map[string]any)
	if auth["handler"] != "reverse_proxy" {
		t.Fatalf("auth handler = %v, want reverse_proxy", auth["handler"])
	}
	if dial := auth["upstreams"].([]any)[0].(map[string]any)["dial"]; dial != "moose-brain:8080" {
		t.Errorf("verify dial = %v, want moose-brain:8080", dial)
	}
	rw := auth["rewrite"].(map[string]any)
	if rw["method"] != "GET" || rw["uri"] != "/_moose/forward-auth/verify" {
		t.Errorf("rewrite = %v, want GET /_moose/forward-auth/verify", rw)
	}
	hr := auth["handle_response"].([]any)
	if len(hr) != 2 {
		t.Fatalf("want 2 handle_response blocks (2xx copy + catch-all redirect), got %d", len(hr))
	}
	// Block 0: a 2xx match scrubs any caller-supplied identity headers, THEN sets
	// them from the verify response — the delete must precede the set so a client
	// can't forge X-Moose-User.
	b0 := hr[0].(map[string]any)
	if sc := b0["match"].(map[string]any)["status_code"].([]any); len(sc) != 1 || sc[0].(float64) != 2 {
		t.Errorf("first block match = %v, want status_code [2] (2xx)", b0["match"])
	}
	twoxxHandle := b0["routes"].([]any)[0].(map[string]any)["handle"].([]any)
	if len(twoxxHandle) != 2 {
		t.Fatalf("2xx handle = %d handlers, want 2 (delete then set)", len(twoxxHandle))
	}
	del := twoxxHandle[0].(map[string]any)["request"].(map[string]any)["delete"].([]any)
	set := twoxxHandle[1].(map[string]any)["request"].(map[string]any)["set"].(map[string]any)
	for _, h := range []string{"X-Moose-User", "X-Moose-User-Id"} {
		if _, ok := set[h]; !ok {
			t.Errorf("identity header %s not set from the verify response", h)
		}
		found := false
		for _, d := range del {
			if d == h {
				found = true
			}
		}
		if !found {
			t.Errorf("identity header %s not scrubbed before set (forgery guard)", h)
		}
	}
	// Block 1: no matcher (catch-all) → 302 redirect to the box login on any
	// non-2xx (the brain returns 401 when the forward-auth cookie is missing).
	b1 := hr[1].(map[string]any)
	if _, ok := b1["match"]; ok {
		t.Error("redirect block must have no matcher (catch-all for non-2xx)")
	}
	redir := b1["routes"].([]any)[0].(map[string]any)["handle"].([]any)[0].(map[string]any)
	if redir["handler"] != "static_response" || redir["status_code"].(float64) != 302 {
		t.Errorf("redirect = %v, want static_response 302", redir)
	}
	if loc := redir["headers"].(map[string]any)["Location"].([]any); len(loc) != 1 || loc[0] != "https://cindy-fox.onmoose.io/" {
		t.Errorf("redirect Location = %v", loc)
	}

	// [1] the app reverse_proxy: real upstream, Cookie header stripped.
	app := handle[1].(map[string]any)
	if dial := app["upstreams"].([]any)[0].(map[string]any)["dial"]; dial != "app:80" {
		t.Errorf("app dial = %v, want app:80", dial)
	}
	assertCookieStripped(t, app)
}

// Path-scoped exposure (#415): a restricted app that declares public paths must
// emit ONE route whose subroute proxies those paths straight through and gates
// everything else. One route and one @id, so the insert-at-0 ordering against
// the catch-all is unchanged.
func TestAddRoute_PublicPathsCarveOutOfTheGate(t *testing.T) {
	handle := routeHandle(t, RouteConfig{
		InstanceID: "i1", Host: "h", Upstream: "app:80",
		StripCookieName: "moose_forward_auth",
		PublicPaths:     []string{"/v1", "/v1/*"},
		ScrubHeaders:    []string{"X-Moose-User", "X-Moose-User-Id"},
		ForwardAuth: &ForwardAuthConfig{
			Upstream: "moose-brain:8080", VerifyPath: "/_moose/forward-auth/verify",
			CopyHeaders: []string{"X-Moose-User", "X-Moose-User-Id"},
			LoginURL:    "https://cindy-fox.onmoose.io/",
		},
	})
	if len(handle) != 2 {
		t.Fatalf("want [scrub, subroute], got %d handlers", len(handle))
	}
	sub := handle[1].(map[string]any)
	if sub["handler"] != "subroute" {
		t.Fatalf("second handler = %v, want subroute", sub["handler"])
	}
	routes := sub["routes"].([]any)
	if len(routes) != 2 {
		t.Fatalf("want 2 subroutes (public paths, then the gated rest), got %d", len(routes))
	}

	// Subroute 0: the declared paths, verbatim, straight to the app with no gate.
	pub := routes[0].(map[string]any)
	match := pub["match"].([]any)[0].(map[string]any)["path"].([]any)
	if len(match) != 2 || match[0] != "/v1" || match[1] != "/v1/*" {
		t.Errorf("public matcher = %v, want the declared paths verbatim", match)
	}
	pubHandle := pub["handle"].([]any)
	if len(pubHandle) != 1 {
		t.Fatalf("public branch = %d handlers, want the app proxy alone (no gate)", len(pubHandle))
	}
	pubProxy := pubHandle[0].(map[string]any)
	if pubProxy["handler"] != "reverse_proxy" {
		t.Errorf("public branch handler = %v, want reverse_proxy", pubProxy["handler"])
	}
	// The #335 invariant holds on the public branch too: it is the same proxy.
	assertCookieStripped(t, pubProxy)

	// Subroute 1: no matcher ⇒ everything else, gated exactly as before.
	rest := routes[1].(map[string]any)
	if _, ok := rest["match"]; ok {
		t.Error("the gated branch must have no matcher (catch-all for every other path)")
	}
	restHandle := rest["handle"].([]any)
	if len(restHandle) != 2 {
		t.Fatalf("gated branch = %d handlers, want [forward_auth, proxy]", len(restHandle))
	}
	if rw := restHandle[0].(map[string]any)["rewrite"].(map[string]any); rw["uri"] != "/_moose/forward-auth/verify" {
		t.Errorf("gated branch is not the forward_auth gate: %v", restHandle[0])
	}
	assertCookieStripped(t, restHandle[1].(map[string]any))
}

// The identity scrub is the outer guarantee forward_auth cannot give: it must be
// the FIRST handler, so it covers the public branch (where no gate runs), the
// gated branch, and the verify subrequest itself. A caller-forged X-Moose-User
// must never reach an app on any path (#415).
func TestAddRoute_ScrubsIdentityHeadersFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RouteConfig
	}{
		{"public app, no gate", RouteConfig{
			InstanceID: "i1", Host: "h", Upstream: "app:80",
			StripCookieName: "moose_forward_auth",
			ScrubHeaders:    []string{"X-Moose-User", "X-Moose-User-Id"},
		}},
		{"restricted app", RouteConfig{
			InstanceID: "i1", Host: "h", Upstream: "app:80",
			StripCookieName: "moose_forward_auth",
			ScrubHeaders:    []string{"X-Moose-User", "X-Moose-User-Id"},
			ForwardAuth:     &ForwardAuthConfig{Upstream: "b:8080", VerifyPath: "/v", LoginURL: "https://l/"},
		}},
		{"restricted app with public paths", RouteConfig{
			InstanceID: "i1", Host: "h", Upstream: "app:80",
			StripCookieName: "moose_forward_auth",
			ScrubHeaders:    []string{"X-Moose-User", "X-Moose-User-Id"},
			PublicPaths:     []string{"/v1/*"},
			ForwardAuth:     &ForwardAuthConfig{Upstream: "b:8080", VerifyPath: "/v", LoginURL: "https://l/"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle := routeHandle(t, tc.cfg)
			first := handle[0].(map[string]any)
			if first["handler"] != "headers" {
				t.Fatalf("first handler = %v, want the identity scrub", first["handler"])
			}
			del := first["request"].(map[string]any)["delete"].([]any)
			for _, want := range []string{"X-Moose-User", "X-Moose-User-Id"} {
				found := false
				for _, d := range del {
					if d == want {
						found = true
					}
				}
				if !found {
					t.Errorf("%s is not scrubbed, so a caller can forge it", want)
				}
			}
		})
	}
}

// PublicPaths without a gate is meaningless — a public app has nothing to carve
// out of — and must not turn the route into a subroute.
func TestAddRoute_PublicPathsIgnoredWithoutGate(t *testing.T) {
	handle := routeHandle(t, RouteConfig{
		InstanceID: "i1", Host: "h", Upstream: "app:80",
		StripCookieName: "moose_forward_auth",
		PublicPaths:     []string{"/v1/*"},
	})
	if len(handle) != 1 || handle[0].(map[string]any)["handler"] != "reverse_proxy" {
		t.Fatalf("public app route must stay the plain proxy, got %v", handle)
	}
}

// applyEmittedCookieStrip runs the route's own emitted Cookie replacements over
// a real Cookie header, exactly as Caddy does: each search_regexp is compiled
// with Go's regexp and applied with ReplaceAllString, in the emitted order.
//
// Driving the *emitted* config rather than a copy of the pattern is the point.
// The regex lives in one place (stripCookieReplacements), and a change to it is
// a change these tests follow automatically instead of silently diverging from.
func applyEmittedCookieStrip(t *testing.T, proxy map[string]any, cookie string) string {
	t.Helper()
	headers, ok := proxy["headers"].(map[string]any)
	if !ok {
		t.Fatal("route emits no headers block, so nothing strips the forward-auth cookie")
	}
	request, ok := headers["request"].(map[string]any)
	if !ok {
		t.Fatal("route emits no request-header block")
	}
	// Comma-ok every step: a regression back to a whole-header `delete` is exactly
	// what this helper exists to catch, and it must report that, not panic on the
	// missing `replace` key.
	replace, ok := request["replace"].(map[string]any)
	if !ok {
		t.Fatalf("route emits no header replacement, got request block %v", request)
	}
	reps, ok := replace["Cookie"].([]any)
	if !ok {
		t.Fatal("route emits no Cookie header replacement")
	}
	for _, r := range reps {
		m := r.(map[string]any)
		re, err := regexp.Compile(m["search_regexp"].(string))
		if err != nil {
			t.Fatalf("emitted search_regexp %q does not compile: %v", m["search_regexp"], err)
		}
		cookie = re.ReplaceAllString(cookie, m["replace"].(string))
	}
	return cookie
}

// assertCookieStripped checks the invariant that actually matters: the app
// upstream never receives moose_forward_auth, and every other cookie survives
// byte-for-byte. The strip is a regex now (#335), so asserting the emitted JSON
// shape cannot prove it correct: a config-shape assertion cannot see a pattern
// that strips the wrong thing, or mangles a cookie the app needed to log its
// user in. Assert on the header the app would actually receive.
func assertCookieStripped(t *testing.T, proxy map[string]any) {
	t.Helper()
	got := applyEmittedCookieStrip(t, proxy, "app_sess=abc; moose_forward_auth=SECRET; other=1")
	if want := "app_sess=abc; other=1"; got != want {
		t.Errorf("Cookie at app upstream = %q, want %q", got, want)
	}
}

// assertACMEIssuer checks the shared acme-dns issuer shape embedded in a
// single-subject policy body.
func assertACMEIssuer(t *testing.T, policy map[string]any, wantEndpoint, wantUser, wantPass, wantSubdomain string) {
	t.Helper()
	issuer := policy["issuers"].([]any)[0].(map[string]any)
	if issuer["module"] != "acme" {
		t.Errorf("issuer module = %v, want acme", issuer["module"])
	}
	provider := issuer["challenges"].(map[string]any)["dns"].(map[string]any)["provider"].(map[string]any)
	if provider["name"] != "acmedns" {
		t.Errorf("dns provider = %v, want acmedns", provider["name"])
	}
	if provider["server_url"] != wantEndpoint {
		t.Errorf("provider server_url = %v, want %s", provider["server_url"], wantEndpoint)
	}
	if provider["username"] != wantUser || provider["password"] != wantPass || provider["subdomain"] != wantSubdomain {
		t.Errorf("provider creds = %v, want {%s %s %s}", provider, wantSubdomain, wantUser, wantPass)
	}
}

func TestWaitReadyReturnsWhenAdminAnswers(t *testing.T) {
	admin := &recordingAdmin{}
	srv := httptest.NewServer(admin.handler())
	defer srv.Close()
	c := New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady against a live admin: %v", err)
	}
}

func TestWaitReadyTimesOutWhenAdminDown(t *testing.T) {
	// Point at a closed server so every probe fails the transport dial.
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close()
	c := New(url)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.WaitReady(ctx); err == nil {
		t.Fatal("want a timeout error when the admin API never answers")
	}
}

// TestSplitCertSubjects covers both the happy path (order-independent) and the
// error path EnsureWildcardTLS relies on to reject a malformed subjects list
// before configuring anything.
func TestSplitCertSubjects(t *testing.T) {
	cases := []struct {
		name         string
		subjects     []string
		wantWildcard string
		wantBase     string
		wantErr      bool
	}{
		{
			name:         "base then wildcard",
			subjects:     []string{"cindy-fox.onmoose.io", "*.cindy-fox.onmoose.io"},
			wantWildcard: "*.cindy-fox.onmoose.io",
			wantBase:     "cindy-fox.onmoose.io",
		},
		{
			name:         "wildcard then base",
			subjects:     []string{"*.cindy-fox.onmoose.io", "cindy-fox.onmoose.io"},
			wantWildcard: "*.cindy-fox.onmoose.io",
			wantBase:     "cindy-fox.onmoose.io",
		},
		{
			name:     "no wildcard",
			subjects: []string{"cindy-fox.onmoose.io"},
			wantErr:  true,
		},
		{
			name:     "no base",
			subjects: []string{"*.cindy-fox.onmoose.io"},
			wantErr:  true,
		},
		{
			name:     "empty",
			subjects: nil,
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wildcard, base, err := splitCertSubjects(tc.subjects)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitCertSubjects(%v) = %q, %q, nil; want an error", tc.subjects, wildcard, base)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitCertSubjects(%v): %v", tc.subjects, err)
			}
			if wildcard != tc.wantWildcard || base != tc.wantBase {
				t.Errorf("splitCertSubjects(%v) = %q, %q; want %q, %q", tc.subjects, wildcard, base, tc.wantWildcard, tc.wantBase)
			}
		})
	}
}

// TestEnsureServerDeclaresEmptyTrustedProxies pins the Caddy half of #329: the
// box's edge trusts no client to have sent an honest X-Forwarded-For, so a
// forged chain is replaced with the address Caddy actually saw and cannot reach
// the brain. POST, not PUT — the bootstrap caddy.json already declares the key
// and Caddy's admin API rejects a PUT onto an existing object key.
func TestEnsureServerDeclaresEmptyTrustedProxies(t *testing.T) {
	admin := &recordingAdmin{}
	srv := httptest.NewServer(admin.handler())
	defer srv.Close()
	c := New(srv.URL)

	if err := c.EnsureServer(context.Background()); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	post := admin.find("POST", "/trusted_proxies")
	if post == nil {
		t.Fatal("EnsureServer must declare the server's trusted_proxies")
	}
	if post.body["source"] != "static" {
		t.Errorf("trusted_proxies source = %v, want static", post.body["source"])
	}
	ranges, ok := post.body["ranges"].([]any)
	if !ok || len(ranges) != 0 {
		t.Errorf("trusted_proxies ranges = %v, want an empty list (trust no client)", post.body["ranges"])
	}
	if admin.find("PATCH", "/routes") == nil {
		t.Error("EnsureServer must still reset the route list")
	}
}

// TestEnsureServerFailsClosedOnTrustedProxyError makes sure a Caddy that
// rejected the trusted-proxy declaration is reported rather than silently
// serving routes under an unknown trust model.
func TestEnsureServerFailsClosedOnTrustedProxyError(t *testing.T) {
	admin := &recordingAdmin{status: map[string]int{
		"/config/apps/http/servers/moose/trusted_proxies": http.StatusBadRequest,
	}}
	srv := httptest.NewServer(admin.handler())
	defer srv.Close()
	c := New(srv.URL)

	if err := c.EnsureServer(context.Background()); err == nil {
		t.Fatal("EnsureServer: want an error when the trusted-proxy declaration is rejected")
	}
	if admin.find("PATCH", "/routes") != nil {
		t.Error("route reset should not run once the trust model could not be applied")
	}
}
