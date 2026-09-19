// Package caddy drives the Caddy admin API (CONTROL_PLANE.md: "Configured via
// Caddy's admin API on localhost:2019 ... no Caddyfile on disk"). Skeleton
// scope: per-instance reverse-proxy routes keyed by Host header, added/removed
// on install/uninstall. All ops are best-effort in dev — if Caddy is down the
// brain logs and continues so the install spine still works.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// catchAllBody is the HTML returned by the catch-all 404 route for any
// unmatched hostname. Kept as a raw string so the Go source is readable;
// serialised to a single JSON string value when sent to the Caddy admin API.
const catchAllBody = `<!doctype html><html><head><meta charset="utf-8"><title>404 — moose</title><style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:6rem auto;padding:0 1rem;color:#222}h1{margin:0 0 .5rem;font-size:1.5rem}p{color:#555;line-height:1.5}a{color:#06c}</style></head><body><h1>404 — No app at this hostname</h1><p>The address you tried doesn't match any installed app on this moose box.</p><p><a href="http://moose.local">Go to the dashboard</a></p></body></html>`

type Client struct {
	admin string
	http  *http.Client
}

func New(adminAddr string) *Client {
	return &Client{
		admin: adminAddr,
		http:  &http.Client{Timeout: 5 * time.Second},
	}
}

// routeID is the stable @id we attach to each route so we can delete it later.
func routeID(instanceID string) string { return "moose-app-" + instanceID }

// RouteConfig is the fully-resolved policy for one app's reverse-proxy route.
// The profile/exposure decision lives in the caller — internal/lifecycle is the
// single central route builder (ENVIRONMENT.md #306, "one central route builder
// is the safety boundary"); this package only renders the JSON. An appliance
// caller leaves StripCookieName empty and ForwardAuth nil, so the emitted route
// is byte-for-byte the plain reverse_proxy it has always been.
type RouteConfig struct {
	InstanceID string
	Host       string
	Upstream   string // "host:port" reachable from Caddy (the app's moose-ingress alias)
	// StripCookieName, when non-empty, removes exactly that one cookie from the
	// inbound Cookie header before proxying to the app upstream (hosted), and
	// passes every other cookie through untouched. It carries the name rather
	// than a bool so the one source of truth stays auth.ForwardAuthCookieName in
	// the caller: a rename there must not silently stop the strip and leak the
	// token to an app upstream.
	//
	// This deliberately does NOT delete the whole Cookie header. Doing so also
	// deleted the app's own session cookies, so no third-party app with a
	// cookie login could authenticate at all on hosted (#335).
	StripCookieName string
	// ForwardAuth, when non-nil, gates the route behind a Caddy forward_auth
	// subrequest to the brain verify endpoint (hosted restricted apps). nil is a
	// public app: the plain reverse_proxy with no gate.
	ForwardAuth *ForwardAuthConfig
	// PublicPaths, when non-empty alongside a non-nil ForwardAuth, carves those
	// request paths out of the gate: they proxy straight to the app with no box
	// login, while every other path stays gated (#415). It is how an app that
	// pairs a token-authed API with a session-authed UI keeps the box login in
	// front of the UI. Ignored when ForwardAuth is nil — a public app has no gate
	// to carve out of.
	PublicPaths []string
	// ScrubHeaders lists request headers deleted from EVERY request this route
	// proxies, before any gate runs. It carries moose's vouched-identity headers
	// so a caller can never supply them: the app must be able to read
	// X-Moose-User as "the brain said so", on every path and in every exposure.
	//
	// The forward_auth gate does its own delete-before-set of the same headers on
	// the allow branch, and that stays. This is the outer guarantee the gate
	// cannot give, because the gate does not run on a public app or on a public
	// path.
	ScrubHeaders []string
}

// ForwardAuthConfig parameterises the forward_auth gate placed in front of a
// restricted app's reverse_proxy.
type ForwardAuthConfig struct {
	Upstream    string   // brain address Caddy dials for the verify subrequest
	VerifyPath  string   // verify endpoint path, rewritten onto the subrequest
	CopyHeaders []string // response headers copied from a 2xx verify onto the app request (identity)
	LoginURL    string   // 302 target when verify does not return 2xx (the box login)
}

// AddRoute registers Host(cfg.Host) -> the app's reverse_proxy(cfg.Upstream),
// optionally wrapped in a forward_auth gate and/or a Cookie strip per cfg. The
// upstream is the real container alias, added once the app is healthy.
//
// The emitted handler chain, in order:
//
//  1. the identity-header scrub (cfg.ScrubHeaders), first so it covers every
//     later branch, gated or not;
//  2. either the flat [forwardAuth?, proxy], or — when cfg carves public paths
//     out of a gate — a subroute whose first route proxies those paths straight
//     through and whose second route gates everything else.
//
// The proxy handler is built once and used on both branches of the subroute, so
// the Cookie strip (#335) applies identically to gated and public paths. A route
// with no scrub, no gate and no public paths is byte-for-byte the plain
// reverse_proxy the appliance has always emitted.
func (c *Client) AddRoute(ctx context.Context, cfg RouteConfig) error {
	proxy := map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": []any{map[string]any{"dial": cfg.Upstream}},
	}
	if cfg.StripCookieName != "" {
		proxy["headers"] = map[string]any{
			"request": map[string]any{"replace": stripCookieReplacements(cfg.StripCookieName)},
		}
	}
	handle := make([]any, 0, 3)
	if len(cfg.ScrubHeaders) > 0 {
		handle = append(handle, scrubHeadersHandler(cfg.ScrubHeaders))
	}
	switch {
	case cfg.ForwardAuth != nil && len(cfg.PublicPaths) > 0:
		handle = append(handle, map[string]any{
			"handler": "subroute",
			"routes": []any{
				// Declared public paths: no gate, straight to the app. Its token
				// auth is the app's own business.
				map[string]any{
					"match":  []any{map[string]any{"path": cfg.PublicPaths}},
					"handle": []any{proxy},
				},
				// No matcher ⇒ every other path, gated exactly as a fully
				// restricted app is.
				map[string]any{
					"handle": []any{forwardAuthHandler(*cfg.ForwardAuth), proxy},
				},
			},
		})
	case cfg.ForwardAuth != nil:
		handle = append(handle, forwardAuthHandler(*cfg.ForwardAuth), proxy)
	default:
		handle = append(handle, proxy)
	}
	return c.upsertRoute(ctx, cfg.InstanceID, cfg.Host, handle)
}

// scrubHeadersHandler renders a Caddy headers handler that deletes the named
// request headers. Placed at the head of an app route it is the one guarantee
// forward_auth cannot give on its own: forward_auth scrubs only on the branch
// where it runs, and it does not run on a public app or on a public path, so
// without this a caller could hand an app a forged X-Moose-User there.
func scrubHeadersHandler(names []string) map[string]any {
	del := make([]any, 0, len(names))
	for _, n := range names {
		del = append(del, n)
	}
	return map[string]any{
		"handler": "headers",
		"request": map[string]any{"delete": del},
	}
}

// stripCookieReplacements renders the Caddy headers.request.replace body that
// removes exactly one named cookie from the inbound Cookie header, leaving every
// other cookie byte-for-byte intact. Two ordered passes, both load-bearing:
//
//  1. `(?:^|;\s*)<name>=[^;]*` removes the cookie wherever it sits in the
//     header. The leading anchor is the safety: the obvious unanchored form
//     (`<name>=[^;]*`) also matches *inside* a longer cookie name, so an app
//     cookie called `evil_moose_forward_auth` gets silently mangled into `evil_`
//     plus whatever followed it. Caddy compiles search_regexp with Go's regexp
//     and applies it with ReplaceAllString, so every occurrence goes: an app
//     that sets its own host-only copy of the name cannot push the real cookie
//     into a later position and have it survive for the app to read.
//  2. `^;\s*` removes the separator the first pass leaves behind when the
//     stripped cookie happened to be first in the header.
//
// The alternation is deliberately non-capturing: Caddy expands `${1}` in a
// replacement, so a capture here would be a live trap for any future edit that
// puts something in the (currently empty) replacement string.
//
// regexp.QuoteMeta stops a name containing regex metacharacters from compiling
// into a pattern that matches more than itself.
func stripCookieReplacements(name string) map[string]any {
	return map[string]any{
		"Cookie": []any{
			map[string]any{"search_regexp": `(?:^|;\s*)` + regexp.QuoteMeta(name) + `=[^;]*`, "replace": ""},
			map[string]any{"search_regexp": `^;\s*`, "replace": ""},
		},
	}
}

// forwardAuthHandler renders the Caddy forward_auth step — a reverse_proxy to the
// brain verify endpoint with a handle_response policy — that gates a restricted
// app. It is the native-JSON form of the Caddyfile `forward_auth` directive
// (moose drives Caddy over its admin API, so there is no Caddyfile): the
// subrequest is copied, rewritten to GET fa.VerifyPath, and dialed at
// fa.Upstream, carrying the request's cookies so the brain can read the
// forward-auth cookie. On the response:
//   - a 2xx (authorized) **deletes any client-supplied copy of fa.CopyHeaders,
//     then sets them from the verify response**, and falls through to the next
//     handler (the app reverse_proxy) — the delete-before-set is the load-bearing
//     part: the app must never receive a caller-forged identity header, only what
//     the brain vouched for (mirrors upstream Caddy's own copy_headers scrub);
//   - any other status — the brain returns 401 when the forward-auth cookie is
//     missing/invalid (#305) — is turned into a 302 redirect to fa.LoginURL.
//
// It deliberately omits the informational X-Forwarded-Method/-Uri header_up that
// stock forward_auth adds: the verify endpoint reads only the cookie (#305). The
// Cookie strip is on the app reverse_proxy (RouteConfig.StripCookieName), not
// here: this subrequest must forward the cookie to the brain to be verified.
func forwardAuthHandler(fa ForwardAuthConfig) map[string]any {
	copyHeaders := map[string]any{}
	scrub := make([]any, 0, len(fa.CopyHeaders))
	for _, h := range fa.CopyHeaders {
		copyHeaders[h] = []any{"{http.reverse_proxy.header." + h + "}"}
		scrub = append(scrub, h)
	}
	return map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": []any{map[string]any{"dial": fa.Upstream}},
		"rewrite": map[string]any{
			"method": "GET",
			"uri":    fa.VerifyPath,
		},
		"handle_response": []any{
			// 2xx: authorized — scrub any caller-supplied identity headers, then set
			// them from the verify response, then continue to the app reverse_proxy
			// (an empty final response falls through). Two ordered `headers` handlers
			// so the delete provably runs before the set.
			map[string]any{
				"match": map[string]any{"status_code": []any{2}},
				"routes": []any{map[string]any{
					"handle": []any{
						map[string]any{
							"handler": "headers",
							"request": map[string]any{"delete": scrub},
						},
						map[string]any{
							"handler": "headers",
							"request": map[string]any{"set": copyHeaders},
						},
					},
				}},
			},
			// No matcher ⇒ everything else (401 from #305, or a brain error):
			// redirect the browser to the box login instead of leaking the app.
			map[string]any{
				"routes": []any{map[string]any{
					"handle": []any{map[string]any{
						"handler":     "static_response",
						"status_code": 302,
						"headers":     map[string]any{"Location": []any{fa.LoginURL}},
					}},
				}},
			},
		},
	}
}

// AddSplashRoute registers Host(host) -> a moose-served splash page for the
// given lifecycle state (APP_LIFECYCLE.md # register early, with a splash).
// The brain owns the route from install-time so the hostname never returns
// connection-refused; the upstream is flipped to the real container once
// healthy. State is one of "starting" | "stopped" | "failed".
func (c *Client) AddSplashRoute(ctx context.Context, instanceID, host, appName, state string) error {
	return c.upsertRoute(ctx, instanceID, host, []any{
		map[string]any{
			"handler":     "static_response",
			"status_code": 200,
			"headers":     map[string]any{"Content-Type": []string{"text/html; charset=utf-8"}},
			"body":        splashHTML(appName, state),
		},
	})
}

// upsertRoute replaces any existing route for the instance (remove-then-add),
// so callers can flip a route's handler without duplicate-@id errors.
func (c *Client) upsertRoute(ctx context.Context, instanceID, host string, handle []any) error {
	_ = c.RemoveRoute(ctx, instanceID)
	route := map[string]any{
		"@id":    routeID(instanceID),
		"match":  []any{map[string]any{"host": []string{host}}},
		"handle": handle,
	}
	// Caddy admin API insert semantics: PUT to /routes/<N> inserts at that
	// index, pushing existing items down. POST to /routes/<N> appends
	// regardless of the trailing index (verified 2026-05-24). Using PUT/0
	// keeps the catch-all (which initially sits at index 0, then index 1+
	// after the first install) last in evaluation order.
	return c.put(ctx, "/config/apps/http/servers/moose/routes/0", route)
}

func splashHTML(appName, state string) string {
	var msg, refresh string
	switch state {
	case "stopped":
		msg = appName + " is stopped."
	case "failed":
		msg = appName + " failed to start."
	default: // starting
		msg = appName + " is starting up…"
		refresh = `<meta http-equiv="refresh" content="2">`
	}
	return `<!doctype html><html><head><meta charset="utf-8">` + refresh +
		`<title>` + appName + `</title>` +
		`<style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;color:#444;background:#f6f6f7}</style>` +
		`</head><body><div><h1 style="font-weight:600">` + msg + `</h1></div></body></html>`
}

func (c *Client) RemoveRoute(ctx context.Context, instanceID string) error {
	return c.RemoveRouteByID(ctx, routeID(instanceID))
}

// RemoveRouteByID deletes a route by its raw Caddy @id (idempotent: an unknown
// @id is treated as already-gone). RemoveRoute is the per-instance wrapper.
func (c *Client) RemoveRouteByID(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.admin+"/id/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 200 on delete; Caddy returns an error if the @id is unknown — treat that
	// as already-gone (idempotent uninstall).
	return nil
}

// catchAllRoute returns the map[string]any that represents the catch-all 404
// route. The @id makes it addressable via /id/moose-catchall so EnsureCatchAll
// can probe for its presence idempotently.
func catchAllRoute() map[string]any {
	return map[string]any{
		"@id":   "moose-catchall",
		"match": []any{map[string]any{}},
		"handle": []any{map[string]any{
			"handler":     "static_response",
			"status_code": 404,
			"headers":     map[string]any{"Content-Type": []any{"text/html; charset=utf-8"}},
			"body":        catchAllBody,
		}},
	}
}

// dashboardRouteID is the stable @id of the dashboard route, so EnsureDashboard
// is idempotent (remove-then-add) the same way per-app routes are.
const dashboardRouteID = "moose-dashboard"

// EnsureDashboard installs the dashboard host route (WEB_UI.md # deploy model):
// requests to the dashboard host split by path — /api/v1/* (including the SSE
// streams) reverse-proxy to the brain, everything else to the moose-ui static
// server. It is a production-only route: in dev the brain runs natively and the
// UI is served by Vite, so cmd/brain only calls this when the UI upstream is
// configured (the containerized stack). Inserted at index 0 like an app route,
// so it sorts before the catch-all; idempotent via its @id.
//
// SSE buffering: the brain's API serves the live-resources and per-app log
// streams under /api/v1/, so the brain leg sets flush_interval -1 (flush each
// write, no response buffering) — buffering those would stall the dashboard's
// live updates. It is harmless for the plain JSON requests on the same leg.
func (c *Client) EnsureDashboard(ctx context.Context, host, brainUpstream, uiUpstream string) error {
	_ = c.RemoveRouteByID(ctx, dashboardRouteID)
	route := map[string]any{
		"@id":   dashboardRouteID,
		"match": []any{map[string]any{"host": []string{host}}},
		"handle": []any{map[string]any{
			"handler": "subroute",
			"routes": []any{
				map[string]any{
					// /api/* is the brain's REST+SSE surface; /_moose/* is the brain's
					// non-API browser endpoints (today the portal-to-box SSO landing
					// GET /_moose/sso). Both leg to the brain; everything else is the
					// dashboard SPA.
					"match": []any{map[string]any{"path": []string{"/api/*", "/_moose/*"}}},
					"handle": []any{map[string]any{
						"handler":        "reverse_proxy",
						"flush_interval": -1,
						"upstreams":      []any{map[string]any{"dial": brainUpstream}},
					}},
				},
				map[string]any{
					"handle": []any{map[string]any{
						"handler":   "reverse_proxy",
						"upstreams": []any{map[string]any{"dial": uiUpstream}},
					}},
				},
			},
		}},
	}
	if err := c.put(ctx, "/config/apps/http/servers/moose/routes/0", route); err != nil {
		return fmt.Errorf("caddy: install dashboard route: %w", err)
	}
	slog.Info("caddy: dashboard route installed", "host", host, "upstream", uiUpstream)
	return nil
}

// WaitReady blocks until the Caddy admin API answers a GET on the config root,
// or ctx is done. Used at brain startup after the brain has brought Caddy up via
// compose: the one-shot route configuration (EnsureServer / EnsureCatchAll /
// EnsureDashboard) must not race the container's first second of life, since
// nothing re-runs those on a transient failure.
func (c *Client) WaitReady(ctx context.Context) error {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		if status, err := c.get(ctx, "/config/"); err == nil && status == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("caddy: admin API not ready: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// EnsureCatchAll installs the catch-all 404 route if it is not already present.
// Called at brain startup after EnsureServer resets the route list — it
// appends the catch-all at the tail of routes[] so all per-app routes inserted
// at index 0 naturally sort before it. Idempotent: probes /id/moose-catchall
// first and returns nil immediately if found.
func (c *Client) EnsureCatchAll(ctx context.Context) error {
	status, err := c.get(ctx, "/id/moose-catchall")
	if err != nil {
		return fmt.Errorf("caddy: probe catch-all: %w", err)
	}
	if status == http.StatusOK {
		slog.Info("caddy: catch-all already present")
		return nil
	}
	// Not found (404 or any non-200) — install it by appending to routes[].
	if err := c.post(ctx, "/config/apps/http/servers/moose/routes", catchAllRoute()); err != nil {
		return fmt.Errorf("caddy: install catch-all: %w", err)
	}
	slog.Info("caddy: catch-all installed")
	return nil
}

// EnsureServer resets the "moose" server's route list to empty at brain
// startup, giving the reconciler a clean slate to rebuild routes from desired
// state, and (re)declares the empty trusted-proxy set. It touches only those two
// keys, so it never changes the server's listen addr or Caddy's admin config.
// Both dev (dev/caddy.json) and production (dev/control-plane/caddy.json, staged
// by the M1b bootstrap) boot Caddy with this server and its :80 listener
// pre-declared, so the brain never sets the listen addr — there is no
// caller-supplied listen to apply.
func (c *Client) EnsureServer(ctx context.Context) error {
	if err := c.ensureTrustedProxies(ctx); err != nil {
		return err
	}
	return c.patch(ctx, "/config/apps/http/servers/moose/routes", []any{})
}

// ensureTrustedProxies declares that this Caddy trusts *no* client to have sent
// an honest X-Forwarded-For (#329). Caddy is the box's edge: its clients are LAN
// devices and, on hosted, the open internet. With an empty trusted set Caddy
// replaces any inbound X-Forwarded-For with the address it actually saw, so a
// forged chain cannot survive the hop to the brain and be used to mint a fresh
// per-IP rate-limit bucket.
//
// That is already Caddy's default (verified against caddy 2.11.4: an inbound
// "X-Forwarded-For: 1.2.3.4" arrives upstream as the peer address alone, while
// the same request from a *trusted* peer arrives with the forged hops intact).
// Declaring it explicitly pins the behaviour so a future default change or a
// hand-edited caddy.json cannot quietly re-open the hole, and states the trust
// model where someone reading the config will look for it. The brain still
// derives the client from the last untrusted hop rather than believing the
// header (internal/api/clientip.go) — either half alone leaves a gap.
//
// POST, not PUT: the bootstrap caddy.json declares this key too, and Caddy's
// admin API rejects a PUT onto an existing object key (409 "key already
// exists"), while POST creates-or-replaces. That keeps this idempotent across
// brain restarts.
func (c *Client) ensureTrustedProxies(ctx context.Context) error {
	return c.post(ctx, "/config/apps/http/servers/moose/trusted_proxies", map[string]any{
		"source": "static",
		"ranges": []any{},
	})
}

// EnrollmentCredentials is the per-box acme-dns account Caddy uses to answer the
// ACME DNS-01 challenge (consumed from the first-boot seed; C3b). Passed as
// primitives so this package stays decoupled from internal/profile.
type EnrollmentCredentials struct {
	Subdomain string
	Username  string
	Password  string
}

// EnsureWildcardTLS configures automatic HTTPS for a hosted box: it tells Caddy
// to obtain the wildcard cert "*.<box-id>.onmoose.io" via an ACME DNS-01
// challenge solved against acme-dns with the box's seeded credentials, and adds
// the :443 listener so the host-matched app routes serve over it. Hosted-only
// and always-on (ENVIRONMENT.md # Networking & discovery); the appliance path,
// which has no enrollment and serves plain-HTTP `.local`, never calls this and
// keeps its :80-only config.
//
// The wildcard must be PROACTIVELY obtained, and doing so takes two config keys,
// not one. In Caddy an automation policy only says *how* to manage a matching
// name (which issuer) — it does not by itself schedule issuance. Caddy obtains a
// cert only for a name that is either in `certificates.automate` or named by an
// HTTP route's Host matcher. So this sets BOTH: `certificates.automate` lists the
// wildcard (WHAT to obtain) and an automation policy pins that subject to the
// acme-dns DNS-01 issuer (HOW — DNS-01 is the only challenge a wildcard can use).
//
// Without the automate entry the wildcard order is never placed: the policy sits
// idle until an app route for "<slug>.<box-id>.onmoose.io" forces Caddy to try
// a cert for that *exact* name, whose DNS-01 challenge lands at
// "_acme-challenge.<slug>.<box-id>.onmoose.io" — a name that is NOT delegated
// to acme-dns (only the apex "_acme-challenge.<box-id>.onmoose.io" is), so the
// challenge can never be answered and every app fails TLS. That was the live #278
// symptom. Once the wildcard cert exists Caddy serves every "<slug>.<box-id>"
// from it and never attempts per-app issuance.
//
// The base name "<box-id>.onmoose.io" (the dashboard apex) is deliberately NOT
// routed through acme-dns: it is a real, publicly-reachable host on :443, so
// Caddy's default issuer obtains it over tls-alpn-01/http-01 the moment the
// dashboard route names it — no DNS-01, no acme-dns account write. That leaves
// exactly one acme-dns order (the wildcard's), so there is no order-vs-order
// contention on the box's shared "_acme-challenge.<box-id>" TXT store — the race
// the earlier serialize-the-two-orders change tried to manage cannot occur.
//
// The acme-dns provider sets only this box's own `_acme-challenge` TXT, so
// renewal (~every 60 days) runs box→acme-dns directly with no moose control-plane
// call (cloud specs/ARCHITECTURE.md Contract 2). The challenge `server_url` is a
// box-side constant (the same acme-dns endpoint for every box), not part of the
// seeded payload.
//
// Idempotent: it removes any prior tls app config and re-puts (the file's
// remove-then-put idiom), so a re-run at every boot converges cleanly. Synchronous
// and fast — it only applies config; Caddy obtains the cert on its own schedule
// afterwards. The DNS provider's JSON shape is pinned to the caddy-dns/acmedns
// module compiled into the hosted Caddy image; real issuance is verified on a real
// hosted box (air-gapped CI never reaches ACME). This function is only ever reached
// on the hosted + enrolled path (its one caller gates on both).
func (c *Client) EnsureWildcardTLS(ctx context.Context, subjects []string, acmeDNSEndpoint string, enr EnrollmentCredentials) error {
	// base is validated (the profile must declare both) but not configured here:
	// it is served by Caddy's default issuer, so only the wildcard needs acme-dns.
	wildcard, _, err := splitCertSubjects(subjects)
	if err != nil {
		return err
	}
	issuer := acmeIssuer(acmeDNSEndpoint, enr)

	// Remove-then-put so a re-run replaces the tls app cleanly; PUT on a fresh
	// (deleted) key creates it, matching upsertRoute's idiom. The DELETE is
	// best-effort — an absent tls app is the first-boot case.
	_ = c.del(ctx, "/config/apps/tls")

	// certificates.automate makes Caddy proactively OBTAIN the wildcard; the
	// automation policy pins that subject to the acme-dns DNS-01 issuer. Both are
	// required — a policy without an automate entry never places the order.
	tlsApp := map[string]any{
		"certificates": map[string]any{
			"automate": []any{wildcard},
		},
		"automation": map[string]any{
			"policies": []any{certPolicy(wildcard, issuer)},
		},
	}
	if err := c.put(ctx, "/config/apps/tls", tlsApp); err != nil {
		return fmt.Errorf("caddy: configure wildcard tls: %w", err)
	}
	// Add :443 alongside the existing :80 so the same host-matched routes serve
	// HTTPS. PATCH replaces the listen array the bootstrap config declared as [":80"].
	if err := c.patch(ctx, "/config/apps/http/servers/moose/listen", []any{":80", ":443"}); err != nil {
		return fmt.Errorf("caddy: add :443 listener: %w", err)
	}
	// Config applied and :443 bound; Caddy now obtains the wildcard in the
	// background on its own schedule (retrying until acme-dns/ACME succeed). This
	// is the "wildcard TLS configured" milestone; issuance is asynchronous and may
	// never complete offline, so we deliberately never wait on a cert here.
	slog.Info("caddy: wildcard TLS configured", "wildcard", wildcard)
	return nil
}

// splitCertSubjects sorts subjects (profile.CertSubjects' [base, wildcard]
// pair) into its wildcard and base names, independent of input order.
func splitCertSubjects(subjects []string) (wildcard, base string, err error) {
	for _, s := range subjects {
		if strings.HasPrefix(s, "*.") {
			wildcard = s
		} else if base == "" {
			base = s
		}
	}
	if wildcard == "" || base == "" {
		return "", "", fmt.Errorf("caddy: want exactly one wildcard and one base subject, got %v", subjects)
	}
	return wildcard, base, nil
}

// acmeIssuer builds the acme-dns ACME issuer config for the wildcard's
// automation policy.
func acmeIssuer(acmeDNSEndpoint string, enr EnrollmentCredentials) map[string]any {
	return map[string]any{
		"module": "acme",
		"challenges": map[string]any{
			"dns": map[string]any{
				"provider": map[string]any{
					"name":       "acmedns",
					"server_url": acmeDNSEndpoint,
					"username":   enr.Username,
					"password":   enr.Password,
					"subdomain":  enr.Subdomain,
				},
			},
		},
	}
}

// certPolicy builds a single-subject Caddy TLS automation policy pinning the
// subject to the given (acme-dns) issuer.
func certPolicy(subject string, issuer map[string]any) map[string]any {
	return map[string]any{
		"subjects": []any{subject},
		"issuers":  []any{issuer},
	}
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	return c.send(ctx, "POST", path, body)
}

// del issues a DELETE to a Caddy admin config path, tolerating a not-found
// (the path was never configured — an absent tls app on first boot). Transport
// failures are returned.
func (c *Client) del(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.admin+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin unreachable: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (c *Client) patch(ctx context.Context, path string, body any) error {
	return c.send(ctx, "PATCH", path, body)
}

func (c *Client) put(ctx context.Context, path string, body any) error {
	return c.send(ctx, "PUT", path, body)
}

// get issues a GET to the Caddy admin API and returns the HTTP status code.
// It never returns an error for non-2xx responses — only for transport failures.
// This lets EnsureCatchAll distinguish "not found" (404) from "unreachable".
func (c *Client) get(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.admin+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("caddy admin unreachable: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (c *Client) send(ctx context.Context, method, path string, body any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.admin+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(resp.Body)
		return fmt.Errorf("caddy admin %s %s: %s: %s", method, path, resp.Status, b.String())
	}
	return nil
}
