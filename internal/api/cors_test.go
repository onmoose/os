package api

import (
	"net/http"
	"testing"
)

// The brain attaches no CORS headers, and that is a security property rather
// than a missing feature (api.go # Handler).
//
// Nothing calls this API cross-origin: the dashboard fetches relative paths,
// Caddy serves it and the brain on one host in production, and the Vite dev
// server proxies /api to the brain so the browser sees one origin there too.
// What a reflected Origin plus Access-Control-Allow-Credentials *would* buy is
// an attacker: on hosted, apps live at <slug>.<box-id>.onmoose.io and the
// dashboard at <box-id>.onmoose.io, which are same-site under a registrable
// domain that is not on the Public Suffix List. The owner's SameSite=Lax
// session cookie therefore rides a fetch from any app to the dashboard host,
// and a reflected header would let the app read the answer.
//
// AUTH.md # Re-authentication and confirm.go both state that a cross-origin
// page cannot make an authenticated JSON POST here. These two tests are what
// makes that true, so a CORS layer cannot be reintroduced without going red.

// A foreign Origin on a real, succeeding request gets nothing back. The
// appliance login picker is used deliberately: it answers 200 without a
// session, so this asserts the absence on a success rather than on a 401,
// which could hide a header attached further down the chain.
func TestNoCORSHeadersOnAPIResponse(t *testing.T) {
	h := newHarness(t)

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/auth/users", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "https://photos.box-1.onmoose.io")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/auth/users: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the picker is public on the appliance", resp.StatusCode)
	}
	for _, k := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"Access-Control-Allow-Headers",
		"Access-Control-Allow-Methods",
	} {
		if got := resp.Header.Get(k); got != "" {
			t.Errorf("%s = %q, want absent — a same-site app origin could then read this reply as the owner", k, got)
		}
	}
}

// The preflight is refused too.
//
// Note what this does and does not cover. Minting a challenge is a POST with no
// body and no required content type, so it is a *simple* request that no
// browser preflights -- a same-site app page can send it, and the test above is
// what keeps the app from reading the answer. The preflight matters for the
// requests that do trigger one: every JSON PUT and PATCH on this API, the SSH
// and mail writes included. Answering those 204 with a reflected Origin is what
// would let an app drive them with the owner's cookie.
func TestPreflightIsNotAnswered(t *testing.T) {
	h := newHarness(t)

	req, err := http.NewRequest(http.MethodOptions, h.srv.URL+"/api/v1/auth/elevate/challenge", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "https://photos.box-1.onmoose.io")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS /api/v1/auth/elevate/challenge: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		t.Errorf("status = 204 — the preflight was answered, so the browser would let a cross-origin POST through")
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want absent", got)
	}
}
