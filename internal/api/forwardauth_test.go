package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/profile"
	"github.com/onmoose/moose/internal/store"
)

// Hosted per-app forward-auth verify endpoint + cookie minting (issue #305).

// ssoOwnerBox provisions a hosted box, drives the SSO handshake to create the
// owner + session, and returns the harness, the owner's forward-auth token (the
// value the box Caddy would forward to the verify endpoint), and the owner row.
func ssoOwnerBox(t *testing.T) (*harness, string, store.User) {
	t.Helper()
	h, priv := ssoHarness(t)
	rec := h.sso(mint(t, priv, ownerClaims()))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sso setup = %d; want 303", rec.Code)
	}
	fa := findCookie(rec.Result().Cookies(), auth.ForwardAuthCookieName)
	if fa == nil {
		t.Fatal("sso did not set a forward-auth cookie")
	}
	owner, err := h.st.GetUserByUsername("owner")
	if err != nil {
		t.Fatalf("owner not created: %v", err)
	}
	return h, fa.Value, owner
}

// verify drives the raw forward-auth verify handler with the given forward-auth
// token (empty ⇒ no cookie), returning the recorder.
func (h *harness) verify(faToken string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, forwardAuthVerifyPath, nil)
	if faToken != "" {
		req.AddCookie(&http.Cookie{Name: auth.ForwardAuthCookieName, Value: faToken})
	}
	rec := httptest.NewRecorder()
	h.apiSrv.forwardAuthVerify(rec, req)
	return rec
}

// SSO mints the Domain-scoped forward-auth cookie alongside the host-only
// session cookie, so a click-through to an app carries a credential.
func TestSSO_SetsForwardAuthCookie(t *testing.T) {
	h, priv := ssoHarness(t)
	rec := h.sso(mint(t, priv, ownerClaims()))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sso = %d; want 303", rec.Code)
	}
	fa := findCookie(rec.Result().Cookies(), auth.ForwardAuthCookieName)
	if fa == nil {
		t.Fatal("no forward-auth cookie set")
	}
	if fa.Domain != profile.HostedDashboardHost("cindy-fox") {
		t.Fatalf("forward-auth cookie Domain = %q; want the box apex", fa.Domain)
	}
	// It must be a distinct value from the host-only session cookie.
	sess := findCookie(rec.Result().Cookies(), auth.CookieName)
	if sess == nil || sess.Value == fa.Value {
		t.Fatalf("forward-auth cookie must differ from the session cookie (sess=%v)", sess)
	}
}

func TestForwardAuthVerify_OwnerAllowed(t *testing.T) {
	h, faToken, owner := ssoOwnerBox(t)
	rec := h.verify(faToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify(owner) = %d; want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Moose-User"); got != owner.Username {
		t.Fatalf("X-Moose-User = %q; want %q", got, owner.Username)
	}
	if got := rec.Header().Get("X-Moose-User-Id"); got != owner.ID {
		t.Fatalf("X-Moose-User-Id = %q; want %q", got, owner.ID)
	}
}

func TestForwardAuthVerify_NoCookieRejected(t *testing.T) {
	h, _, _ := ssoOwnerBox(t)
	if rec := h.verify(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify(no cookie) = %d; want 401", rec.Code)
	}
}

func TestForwardAuthVerify_BadCookieRejected(t *testing.T) {
	h, _, _ := ssoOwnerBox(t)
	if rec := h.verify("not-a-real-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify(bad cookie) = %d; want 401", rec.Code)
	}
}

// A live session that is not the box owner must not validate (owner-only, v1).
func TestForwardAuthVerify_NonOwnerRejected(t *testing.T) {
	h, _, _ := ssoOwnerBox(t)

	// A second, non-owner user with a live session + forward-auth token.
	other := store.User{ID: "u_other", Username: "cindy", Role: store.RoleMember, CreatedAt: time.Now()}
	if err := h.st.CreateUser(other); err != nil {
		t.Fatalf("create non-owner: %v", err)
	}
	sess, err := h.apiSrv.auth.Issue(other.ID)
	if err != nil {
		t.Fatalf("issue non-owner session: %v", err)
	}
	fa, err := h.apiSrv.auth.IssueForwardAuth(sess.Token)
	if err != nil {
		t.Fatalf("issue non-owner forward-auth: %v", err)
	}
	if rec := h.verify(fa.Value); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify(non-owner) = %d; want 401", rec.Code)
	}
}

// Fail closed: a live session on a hosted box whose owner has not been recorded
// yet (no SSO handshake has completed) must be rejected, never allowed through.
func TestForwardAuthVerify_UnprovisionedOwnerRejected(t *testing.T) {
	h := hostedHarness(t) // hosted, but no owner meta written
	other := store.User{ID: "u_x", Username: "andrei", Role: store.RoleAdmin, CreatedAt: time.Now()}
	if err := h.st.CreateUser(other); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess, err := h.apiSrv.auth.Issue(other.ID)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	fa, err := h.apiSrv.auth.IssueForwardAuth(sess.Token)
	if err != nil {
		t.Fatalf("issue forward-auth: %v", err)
	}
	if rec := h.verify(fa.Value); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify(unprovisioned owner) = %d; want 401", rec.Code)
	}
}

// On the appliance the endpoint does not exist as far as a caller is concerned.
func TestForwardAuthVerify_ApplianceNotFound(t *testing.T) {
	h := newHarness(t) // zero-valued env ⇒ appliance
	if rec := h.verify("anything"); rec.Code != http.StatusNotFound {
		t.Fatalf("verify on appliance = %d; want 404", rec.Code)
	}
}

// The load-bearing invariant: the forward-auth token, replayed as the dashboard
// session cookie, must not authenticate the admin API.
func TestForwardAuthToken_CannotUpgradeToDashboardSession(t *testing.T) {
	h, faToken, _ := ssoOwnerBox(t)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: faToken})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me with forward-auth token as session = %d; want 401", resp.StatusCode)
	}
}

// verifyOverHTTP drives the verify endpoint through the full middleware chain
// (auth middleware + rate limiter), which is where the exemption lives. cookie ==
// "" sends no forward-auth cookie at all.
func (h *harness) verifyOverHTTP(t *testing.T, cookie string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+forwardAuthVerifyPath, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.ForwardAuthCookieName, Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A request carrying a well-formed forward-auth token is exempt from the per-IP
// bucket: the box Caddy calls verify on a forward_auth subrequest for every asset
// of a restricted app, so throttling it at 30/min/IP would throttle legitimate app
// traffic. Well past the allowlist budget, none of these may 429.
func TestForwardAuthVerify_ValidTokenNotRateLimited(t *testing.T) {
	h, faToken, _ := ssoOwnerBox(t)
	for i := 0; i < ipRateBurst+10; i++ {
		if code := h.verifyOverHTTP(t, faToken); code == http.StatusTooManyRequests {
			t.Fatalf("verify call %d with a valid token was rate-limited (429); the gated path must be exempt", i)
		} else if code != http.StatusOK {
			t.Fatalf("verify call %d = %d; want 200", i, code)
		}
	}
}

// The property that matters (and that the old blanket exemption did not have): an
// anonymous flood of garbage cookies is BOUNDED, and never reaches the store. The
// brain serializes every query through one SQLite connection, so unthrottled
// anonymous DB load on this public path would stall the whole brain, not just this
// endpoint. Malformed tokens are rejected by the shape gate (no store lookup) and
// stay on the normal per-IP bucket, so the flood 429s once the budget is spent.
func TestForwardAuthVerify_GarbageCookieFloodIsThrottled(t *testing.T) {
	h, _, _ := ssoOwnerBox(t)
	throttled := false
	for i := 0; i < ipRateBurst+10; i++ {
		code := h.verifyOverHTTP(t, "garbage-not-a-token")
		if code == http.StatusTooManyRequests {
			throttled = true
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("verify call %d with a garbage cookie = %d; want 401 (or 429)", i, code)
		}
	}
	if !throttled {
		t.Fatalf("a garbage-cookie flood of %d requests was never rate-limited; the exemption must not cover malformed credentials", ipRateBurst+10)
	}
}

// Same for a request with no forward-auth cookie at all — it is not app traffic,
// so it draws from the normal per-IP bucket.
func TestForwardAuthVerify_NoCookieFloodIsThrottled(t *testing.T) {
	h, _, _ := ssoOwnerBox(t)
	throttled := false
	for i := 0; i < ipRateBurst+10; i++ {
		if h.verifyOverHTTP(t, "") == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("a no-cookie flood was never rate-limited")
	}
}

// Hosted login sets both cookies: the host-only session and the Domain-scoped
// forward-auth cookie.
func TestHostedLogin_SetsBothCookies(t *testing.T) {
	h := hostedHarness(t)
	ctx := context.Background()
	if err := h.st.CreateUser(store.User{ID: "u1", Username: "andrei", Role: store.RoleAdmin, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.apiSrv.host.SetPassword(ctx, "andrei", "hunter2"); err != nil {
		t.Fatalf("set password: %v", err)
	}

	resp := h.do("POST", "/api/v1/login", map[string]string{"username": "andrei", "password": "hunter2"})
	if resp.StatusCode != 200 {
		t.Fatalf("hosted login = %d; want 200", resp.StatusCode)
	}
	resp.Body.Close()

	sess := findCookie(resp.Cookies(), auth.CookieName)
	if sess == nil || sess.Domain != "" {
		t.Fatalf("session cookie must be present and host-only (empty Domain); got %v", sess)
	}
	fa := findCookie(resp.Cookies(), auth.ForwardAuthCookieName)
	if fa == nil {
		t.Fatal("hosted login did not set a forward-auth cookie")
	}
	if fa.Domain != profile.HostedDashboardHost("cindy-fox") {
		t.Fatalf("forward-auth cookie Domain = %q; want the box apex", fa.Domain)
	}
}

// The appliance login path is unchanged: exactly one cookie, no forward-auth one.
func TestApplianceLogin_NoForwardAuthCookie(t *testing.T) {
	h := newHarness(t) // appliance
	ctx := context.Background()
	if err := h.st.CreateUser(store.User{ID: "u1", Username: "andrei", Role: store.RoleAdmin, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.apiSrv.host.SetPassword(ctx, "andrei", "hunter2"); err != nil {
		t.Fatalf("set password: %v", err)
	}

	resp := h.do("POST", "/api/v1/login", map[string]string{"username": "andrei", "password": "hunter2"})
	if resp.StatusCode != 200 {
		t.Fatalf("appliance login = %d; want 200", resp.StatusCode)
	}
	resp.Body.Close()

	if fa := findCookie(resp.Cookies(), auth.ForwardAuthCookieName); fa != nil {
		t.Fatalf("appliance login set a forward-auth cookie: %v", fa)
	}
	if sess := findCookie(resp.Cookies(), auth.CookieName); sess == nil {
		t.Fatal("appliance login did not set the session cookie")
	}
}

// Hosted logout clears both cookies; the forward-auth cookie's clear carries the
// box-apex Domain so the browser expires the right one.
func TestHostedLogout_ClearsBothCookies(t *testing.T) {
	h, _, _ := ssoOwnerBox(t)
	// Log in the owner over HTTP so the request carries a session the logout can
	// revoke. The SSO recorder issued cookies out-of-band, so drive a fresh login
	// via a second user instead: create + authenticate a hosted account.
	ctx := context.Background()
	if err := h.st.CreateUser(store.User{ID: "u_login", Username: "dana", Role: store.RoleAdmin, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.apiSrv.host.SetPassword(ctx, "dana", "hunter2"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if resp := h.do("POST", "/api/v1/login", map[string]string{"username": "dana", "password": "hunter2"}); resp.StatusCode != 200 {
		t.Fatalf("login = %d; want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp := h.do("POST", "/api/v1/logout", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("logout = %d; want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Both clears present; the forward-auth clear is Domain-scoped and expiring.
	fa := findCookie(resp.Cookies(), auth.ForwardAuthCookieName)
	if fa == nil {
		t.Fatal("logout did not clear the forward-auth cookie")
	}
	if fa.MaxAge >= 0 || fa.Domain != profile.HostedDashboardHost("cindy-fox") {
		t.Fatalf("forward-auth clear cookie = %+v; want expiring + box-apex Domain", fa)
	}
	if sess := findCookie(resp.Cookies(), auth.CookieName); sess == nil || sess.MaxAge >= 0 {
		t.Fatalf("session clear cookie = %+v; want expiring", sess)
	}
}
