package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/store"
)

// The hosted confirm step (issue #469): a hosted owner has no box password, so
// the portal round-trip is what opens the five-minute elevation window. These
// tests cover the two halves — the return path (an open redirect here would hand
// out a live session) and the challenge that keeps a cross-site page from arming
// the window on its own.

// ssoReturn drives the SSO landing with a token and a `return` query param, the
// shape the portal produces once it forwards the dashboard's return path.
func (h *harness) ssoReturn(token, ret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		"/_moose/sso?token="+token+"&return="+url.QueryEscape(ret), nil)
	rec := httptest.NewRecorder()
	h.apiSrv.ssoLanding(rec, req)
	return rec
}

// adoptCookies moves the cookies an SSO landing set into the harness jar, so the
// following h.do calls ride the session the landing just minted.
func (h *harness) adoptCookies(rec *httptest.ResponseRecorder) {
	h.t.Helper()
	u, err := url.Parse(h.srv.URL)
	if err != nil {
		h.t.Fatalf("parse harness url: %v", err)
	}
	h.jar.SetCookies(u, rec.Result().Cookies())
}

// mintChallenge asks the box for a confirm challenge over the wire, as the
// dashboard does before sending the browser to the portal.
func (h *harness) mintChallenge() string {
	h.t.Helper()
	resp := h.do("POST", "/api/v1/auth/elevate/challenge", struct{}{})
	if resp.StatusCode != 200 {
		h.t.Fatalf("mint challenge = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Challenge string `json:"challenge"`
		ExpiresAt int64  `json:"expires_at"`
	}](h.t, resp)
	if body.Challenge == "" || body.ExpiresAt == 0 {
		h.t.Fatalf("challenge response is incomplete: %+v", body)
	}
	return body.Challenge
}

// createSecondUser drives one elevation-class write and returns its status.
func (h *harness) createSecondUser(username string) int {
	h.t.Helper()
	resp := h.do("POST", "/api/v1/users", map[string]string{
		"display_name": username, "password": "hunter2hunter2",
	})
	resp.Body.Close()
	return resp.StatusCode
}

// signIn completes one portal handshake and adopts the session it mints.
func (h *harness) signIn(token string) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := h.sso(token)
	if rec.Code != http.StatusSeeOther {
		h.t.Fatalf("sso sign-in = %d; want 303", rec.Code)
	}
	h.adoptCookies(rec)
	return rec
}

func (h *harness) elevateAuditCount(t *testing.T, action string) int {
	t.Helper()
	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// returnTarget is the open-redirect guard. Everything that is not a plain
// relative path on this box must fall back to the box's own front page.
func TestReturnTarget(t *testing.T) {
	long := "/" + strings.Repeat("a", maxReturnPathLen)
	cases := []struct {
		name, in, wantTarget, wantChallenge string
	}{
		{"empty", "", "/", ""},
		{"plain path", "/settings/users", "/settings/users", ""},
		{"path with query", "/settings/users?tab=list", "/settings/users?tab=list", ""},
		{"confirm is stripped", "/settings/users?confirm=abc123", "/settings/users", "abc123"},
		{"confirm stripped, rest kept", "/settings?tab=list&confirm=abc", "/settings?tab=list", "abc"},
		{"absolute https url", "https://evil.example/x", "/", ""},
		{"scheme-relative", "//evil.example/x", "/", ""},
		{"backslash host", "/\\evil.example", "/", ""},
		{"backslash anywhere", "/settings\\x", "/", ""},
		{"userinfo", "https://user@evil.example/", "/", ""},
		{"javascript scheme", "javascript:alert(1)", "/", ""},
		{"not rooted", "settings/users", "/", ""},
		{"newline", "/settings\nx", "/", ""},
		{"tab", "/settings\tx", "/", ""},
		{"over the length cap", long, "/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, challenge := returnTarget(tc.in)
			if target != tc.wantTarget {
				t.Errorf("target = %q; want %q", target, tc.wantTarget)
			}
			if challenge != tc.wantChallenge {
				t.Errorf("challenge = %q; want %q", challenge, tc.wantChallenge)
			}
		})
	}
}

// The whole point of the issue: an owner who arrived through the portal can drive
// an elevation-class write without ever typing a password.
func TestSSO_ConfirmChallengeElevates(t *testing.T) {
	h, priv := ssoHarness(t)

	// 1. Plain sign-in. The owner is an admin but not elevated, so the write is
	//    refused — the state a hosted box could never leave before this change.
	h.signIn(mint(t, priv, ownerClaims()))
	if code := h.createSecondUser("alice"); code != http.StatusForbidden {
		t.Fatalf("create-user right after sign-in = %d; want 403 (not elevated)", code)
	}

	// 2. The dashboard mints a confirm challenge and hands it to the portal as
	//    part of its return path.
	challenge := h.mintChallenge()

	// 3. The portal sends the owner back with a fresh assertion and that return
	//    path. The box lands them where they were, minus the spent challenge.
	rec := h.ssoReturn(mint(t, priv, ownerClaims()), "/settings/users?confirm="+challenge)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("confirm landing = %d; want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/users" {
		t.Fatalf("confirm landing redirect = %q; want /settings/users with the confirm stripped", loc)
	}
	h.adoptCookies(rec)

	// 4. The same write now passes.
	if code := h.createSecondUser("alice"); code != 200 {
		t.Fatalf("create-user after the portal confirm = %d; want 200", code)
	}
	if n := h.elevateAuditCount(t, audit.ActionElevateSuccess); n != 1 {
		t.Fatalf("auth.elevate.success rows = %d; want 1", n)
	}
}

// A landing with no confirm challenge is a plain sign-in. It must not open the
// window — otherwise a cross-site page that can drive the portal's open-box
// navigation would arm five privileged minutes on the victim's own box.
func TestSSO_SignInWithoutChallengeDoesNotElevate(t *testing.T) {
	h, priv := ssoHarness(t)
	h.signIn(mint(t, priv, ownerClaims()))

	rec := h.ssoReturn(mint(t, priv, ownerClaims()), "/settings/users")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("landing = %d; want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/users" {
		t.Fatalf("redirect = %q; want /settings/users", loc)
	}
	h.adoptCookies(rec)

	if code := h.createSecondUser("alice"); code != http.StatusForbidden {
		t.Fatalf("create-user after a challenge-less landing = %d; want 403", code)
	}
	if n := h.elevateAuditCount(t, audit.ActionElevateSuccess); n != 0 {
		t.Fatalf("auth.elevate.success rows = %d; want 0", n)
	}
}

// The challenge is single-use: replaying a captured return URL with a second
// assertion signs the owner in again but opens no window.
func TestSSO_ConfirmChallengeIsSingleUse(t *testing.T) {
	h, priv := ssoHarness(t)
	h.signIn(mint(t, priv, ownerClaims()))
	challenge := h.mintChallenge()
	ret := "/settings/users?confirm=" + challenge

	if rec := h.ssoReturn(mint(t, priv, ownerClaims()), ret); rec.Code != http.StatusSeeOther {
		t.Fatalf("first confirm landing = %d; want 303", rec.Code)
	}
	rec := h.ssoReturn(mint(t, priv, ownerClaims()), ret)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("replayed confirm landing = %d; want 303 (sign-in still works)", rec.Code)
	}
	h.adoptCookies(rec)

	if code := h.createSecondUser("alice"); code != http.StatusForbidden {
		t.Fatalf("create-user after a replayed challenge = %d; want 403", code)
	}
	if n := h.elevateAuditCount(t, audit.ActionElevateFailure); n != 1 {
		t.Fatalf("auth.elevate.failure rows = %d; want 1 (the replay)", n)
	}
}

// A challenge minted for one user must not elevate another. v1 is owner-only, so
// this is a guard against a later multi-user box, not a reachable path today.
func TestSSO_ConfirmChallengeBoundToItsUser(t *testing.T) {
	h, priv := ssoHarness(t)
	h.signIn(mint(t, priv, ownerClaims()))

	h.addMember("u_member", "bob", "hunter2hunter2")
	challenge, err := newChallengeID()
	if err != nil {
		t.Fatalf("new challenge: %v", err)
	}
	now := time.Now()
	if err := h.st.CreateElevationChallenge(challenge, "u_member", now.Add(elevationChallengeTTL), now); err != nil {
		t.Fatalf("create challenge: %v", err)
	}

	rec := h.ssoReturn(mint(t, priv, ownerClaims()), "/settings/users?confirm="+challenge)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("landing = %d; want 303", rec.Code)
	}
	h.adoptCookies(rec)
	if code := h.createSecondUser("alice"); code != http.StatusForbidden {
		t.Fatalf("create-user with another user's challenge = %d; want 403", code)
	}
}

// A return path that names another host is refused and the owner still lands on
// the box's own front page — with a valid session, which is exactly why the
// redirect must never leave the box.
func TestSSO_OffBoxReturnPathLandsOnRoot(t *testing.T) {
	for _, ret := range []string{"https://evil.example/steal", "//evil.example/steal", "/\\evil.example"} {
		h, priv := ssoHarness(t)
		rec := h.ssoReturn(mint(t, priv, ownerClaims()), ret)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("landing with return %q = %d; want 303", ret, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("return %q redirected to %q; want /", ret, loc)
		}
		if c := findCookie(rec.Result().Cookies(), auth.CookieName); c == nil {
			t.Fatalf("return %q: sign-in should still succeed", ret)
		}
	}
}

// The challenge route is hosted-only, like the SSO landing itself: the appliance
// confirm step is the password prompt and nothing else.
func TestElevateChallenge_ApplianceReturns404(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("andrei", "hunter2hunter2")
	resp := h.do("POST", "/api/v1/auth/elevate/challenge", struct{}{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("appliance challenge = %d; want 404", resp.StatusCode)
	}
}

// Only the owner can mint. Nobody else can redeem a challenge — the landing
// refuses one belonging to another user — so for them the route does not exist.
func TestElevateChallenge_NonOwnerReturns404(t *testing.T) {
	h, priv := ssoHarness(t)
	h.signIn(mint(t, priv, ownerClaims()))
	h.addMember("u_member", "bob", "hunter2hunter2")
	h.loginAs("bob", "hunter2hunter2")

	resp := h.do("POST", "/api/v1/auth/elevate/challenge", struct{}{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-owner challenge = %d; want 404", resp.StatusCode)
	}
}

// A hosted box with no owner recorded yet mints nothing: fail closed rather than
// hand out a challenge no landing could be trusted to match.
func TestElevateChallenge_NoOwnerRecordedReturns404(t *testing.T) {
	h, _ := ssoHarness(t)
	h.addMember("u_member", "bob", "hunter2hunter2")
	h.loginAs("bob", "hunter2hunter2")

	resp := h.do("POST", "/api/v1/auth/elevate/challenge", struct{}{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("challenge on an ownerless box = %d; want 404", resp.StatusCode)
	}
}

// No session, no challenge — the route sits behind the normal auth middleware.
func TestElevateChallenge_RequiresSession(t *testing.T) {
	h, _ := ssoHarness(t)
	resp := h.do("POST", "/api/v1/auth/elevate/challenge", struct{}{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated challenge = %d; want 401", resp.StatusCode)
	}
}

// The dashboard picks the confirm step from the `owner` flag on /me: only the
// hosted owner has no box password. A box user the owner created keeps the
// password prompt, and an appliance never sees the field at all.
func TestMe_OwnerFlag(t *testing.T) {
	h, priv := ssoHarness(t)
	h.signIn(mint(t, priv, ownerClaims()))

	resp := h.do("GET", "/api/v1/me", nil)
	me := decodeJSON[UserDTO](t, resp)
	if !me.Owner {
		t.Fatalf("hosted owner /me owner = false; want true")
	}

	// A second hosted account is not the owner.
	h.addMember("u_member", "bob", "hunter2hunter2")
	h.loginAs("bob", "hunter2hunter2")
	resp = h.do("GET", "/api/v1/me", nil)
	member := decodeJSON[UserDTO](t, resp)
	if member.Owner {
		t.Fatalf("hosted member /me owner = true; want false")
	}
}

func TestMe_ApplianceOmitsOwnerFlag(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("andrei", "hunter2hunter2")
	resp := h.do("GET", "/api/v1/me", nil)
	raw := decodeJSON[map[string]any](t, resp)
	if _, ok := raw["owner"]; ok {
		t.Fatalf("appliance /me carries an owner field: %v", raw)
	}
}
