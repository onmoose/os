package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onmoose/os/internal/assertion"
	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// Portal-to-box SSO handshake, box side (issue #275). These drive the
// /_moose/sso handler directly (httptest recorder) rather than over the harness's
// redirect-following HTTP client, since the handler 303s to an absolute https
// dashboard URL.

// ssoHarness provisions a hosted box trusting a freshly minted portal keypair,
// and returns the private key the tests sign assertions with.
func ssoHarness(t *testing.T) (*harness, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	h := newHarness(t, func(s *Server) {
		s.SetEnvironment(profile.Hosted, "cindy-fox", pub)
	})
	return h, priv
}

// ownerClaims is a valid owner assertion for the ssoHarness box, ~60s-lived.
func ownerClaims() assertion.Claims {
	now := time.Now()
	return assertion.Claims{
		Iss:   profile.NetworkApex,
		Sub:   "acct_owner_1",
		Email: "Owner@Example.com",
		Box:   "cindy-fox",
		Iat:   now.Unix(),
		Exp:   now.Add(60 * time.Second).Unix(),
		JTI:   newJTI(),
		KID:   "v1",
	}
}

// newJTI returns a fresh 128-bit nonce. crypto/rand.Read never fails in practice;
// a panic here would just fail the test, which is the intent.
func newJTI() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// mint signs claims into the wire token shape the box verifies:
// base64url(claims) "." base64url(ed25519-sig over the first segment bytes).
func mint(t *testing.T, priv ed25519.PrivateKey, c assertion.Claims) string {
	t.Helper()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// sso drives the SSO landing handler with the given token and returns the
// recorder. The handler never follows the redirect itself.
func (h *harness) sso(token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/_moose/sso?token="+token, nil)
	rec := httptest.NewRecorder()
	h.apiSrv.ssoLanding(rec, req)
	return rec
}

func (h *harness) ssoFailureCount(t *testing.T) int {
	t.Helper()
	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 200})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Action == audit.ActionSSOFailure && !e.Success {
			n++
		}
	}
	return n
}

func TestSSO_ValidAssertionCreatesOwnerAndSession(t *testing.T) {
	h, priv := ssoHarness(t)
	rec := h.sso(mint(t, priv, ownerClaims()))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sso = %d; want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("redirect = %q; want the box dashboard root", loc)
	}

	// The owner admin was auto-created from the email local-part.
	u, err := h.st.GetUserByUsername("owner")
	if err != nil {
		t.Fatalf("owner user not created: %v", err)
	}
	if u.Role != store.RoleAdmin {
		t.Fatalf("owner role = %q; want admin", u.Role)
	}

	// A box session cookie was issued, host-only (no Domain attribute) so it never
	// reaches app subdomains.
	cookie := findCookie(rec.Result().Cookies(), auth.CookieName)
	if cookie == nil {
		t.Fatal("no session cookie set")
	}
	if cookie.Domain != "" {
		t.Fatalf("session cookie Domain = %q; want host-only (empty)", cookie.Domain)
	}
	if _, err := h.st.GetSession(cookie.Value); err != nil {
		t.Fatalf("issued session not persisted: %v", err)
	}
}

func TestSSO_SecondOwnerAssertionReusesAdmin(t *testing.T) {
	h, priv := ssoHarness(t)
	if rec := h.sso(mint(t, priv, ownerClaims())); rec.Code != http.StatusSeeOther {
		t.Fatalf("first sso = %d; want 303", rec.Code)
	}
	// A second, distinct assertion from the same owner (new jti) succeeds and
	// reuses the existing admin — no second user row.
	if rec := h.sso(mint(t, priv, ownerClaims())); rec.Code != http.StatusSeeOther {
		t.Fatalf("second sso = %d; want 303", rec.Code)
	}
	n, err := h.st.UserCount()
	if err != nil {
		t.Fatalf("user count: %v", err)
	}
	if n != 1 {
		t.Fatalf("user count = %d; want 1 (admin reused)", n)
	}
}

func TestSSO_NonOwnerRejected(t *testing.T) {
	h, priv := ssoHarness(t)
	if rec := h.sso(mint(t, priv, ownerClaims())); rec.Code != http.StatusSeeOther {
		t.Fatalf("owner sso = %d; want 303", rec.Code)
	}
	other := ownerClaims()
	other.Sub = "acct_intruder"
	other.Email = "intruder@example.com"
	rec := h.sso(mint(t, priv, other))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner sso = %d; want 403", rec.Code)
	}
	if n, _ := h.st.UserCount(); n != 1 {
		t.Fatalf("user count = %d; want 1 (no second admin)", n)
	}
	if h.ssoFailureCount(t) == 0 {
		t.Fatal("non-owner rejection was not audited")
	}
}

// A signed assertion missing an identity field the box persists (sub/email/jti)
// is rejected before any owner state is written — an empty sub would otherwise
// record the owner as "" and wedge SSO for the real account.
func TestSSO_EmptyClaimFieldsRejected(t *testing.T) {
	for _, field := range []string{"sub", "email", "jti"} {
		h, priv := ssoHarness(t)
		c := ownerClaims()
		switch field {
		case "sub":
			c.Sub = ""
		case "email":
			c.Email = ""
		case "jti":
			c.JTI = ""
		}
		rec := h.sso(mint(t, priv, c))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("empty %s sso = %d; want 403", field, rec.Code)
		}
		if n, _ := h.st.UserCount(); n != 0 {
			t.Fatalf("empty %s: user count = %d; want 0 (no owner state)", field, n)
		}
		if _, err := h.st.GetBoxMeta(store.BoxMetaOwnerSub); err == nil {
			t.Fatalf("empty %s: owner sub recorded; want none", field)
		}
	}
}

func TestSSO_TamperedTokenRejected(t *testing.T) {
	h, priv := ssoHarness(t)
	token := mint(t, priv, ownerClaims())
	// Flip a byte in the payload segment so the signature no longer matches.
	bad := []byte(token)
	bad[0] ^= 0x01
	rec := h.sso(string(bad))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered sso = %d; want 401", rec.Code)
	}
	if n, _ := h.st.UserCount(); n != 0 {
		t.Fatalf("user count = %d; want 0", n)
	}
	if h.ssoFailureCount(t) == 0 {
		t.Fatal("tampered rejection was not audited")
	}
}

func TestSSO_WrongKeyRejected(t *testing.T) {
	h, _ := ssoHarness(t)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	rec := h.sso(mint(t, otherPriv, ownerClaims()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key sso = %d; want 401", rec.Code)
	}
}

func TestSSO_ExpiredRejected(t *testing.T) {
	h, priv := ssoHarness(t)
	c := ownerClaims()
	c.Iat = time.Now().Add(-2 * time.Minute).Unix()
	c.Exp = time.Now().Add(-1 * time.Minute).Unix()
	rec := h.sso(mint(t, priv, c))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired sso = %d; want 401", rec.Code)
	}
}

func TestSSO_WrongBoxRejected(t *testing.T) {
	h, priv := ssoHarness(t)
	c := ownerClaims()
	c.Box = "rocky-owl"
	rec := h.sso(mint(t, priv, c))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-box sso = %d; want 403", rec.Code)
	}
}

func TestSSO_WrongIssuerRejected(t *testing.T) {
	h, priv := ssoHarness(t)
	c := ownerClaims()
	c.Iss = "evil.example"
	rec := h.sso(mint(t, priv, c))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-issuer sso = %d; want 403", rec.Code)
	}
}

func TestSSO_ReplayRejected(t *testing.T) {
	h, priv := ssoHarness(t)
	c := ownerClaims()
	token := mint(t, priv, c)
	if rec := h.sso(token); rec.Code != http.StatusSeeOther {
		t.Fatalf("first use = %d; want 303", rec.Code)
	}
	// Same token (same jti) again — single-use enforcement rejects it.
	rec := h.sso(token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay = %d; want 401", rec.Code)
	}
	if h.ssoFailureCount(t) == 0 {
		t.Fatal("replay rejection was not audited")
	}
}

func TestSSO_UnprovisionedReturns503(t *testing.T) {
	h := newHarness(t, func(s *Server) {
		s.SetEnvironment(profile.Hosted, "cindy-fox", nil) // hosted, no key yet
	})
	// A syntactically valid token can't even be checked without a key.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	rec := h.sso(mint(t, priv, ownerClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unprovisioned sso = %d; want 503", rec.Code)
	}
}

func TestSSO_ApplianceReturns404(t *testing.T) {
	h := newHarness(t) // appliance
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	rec := h.sso(mint(t, priv, ownerClaims()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("appliance sso = %d; want 404", rec.Code)
	}
}

// The owner's account name now comes from the same derivation every other
// account uses, so this test covers only the SSO-specific half: which string
// gets fed into it. The derivation itself is tested in accountname_test.go.
func TestSSODisplayName(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  string
	}{
		{"Jane Doe", "jane.doe@example.com", "Jane Doe"}, // the portal sent a name
		{"  Jane   Doe ", "j@x.io", "Jane Doe"},          // and it gets normalized
		{"", "jane.doe@example.com", "jane.doe"},         // no name: email local part
		{"", "Owner@Example.com", "Owner"},               // case is preserved here
		{"", "@x.io", ""},                                // nothing usable at all
	}
	for _, c := range cases {
		got := ssoDisplayName(assertion.Claims{Name: c.name, Email: c.email})
		if got != c.want {
			t.Errorf("ssoDisplayName(name=%q, email=%q) = %q; want %q", c.name, c.email, got, c.want)
		}
	}
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestSSO_AdoptsAnOwnerLeftHalfCreated is the crash-retry the owner bootstrap
// has always had to survive: a prior handshake wrote the admin row and died
// before the owner meta committed, leaving an admin with no owner. The retry
// must adopt that admin.
//
// It is a regression test for a wedge this is easy to reintroduce. The account
// name is derived from a display name now, and deriving one runs the
// display-name uniqueness check. The half-created admin already holds the name
// this assertion asks for, so deriving first answers 409 and the adopt path
// below is never reached. The box would then be stuck: every retry refuses, and
// nothing else can set the owner.
func TestSSO_AdoptsAnOwnerLeftHalfCreated(t *testing.T) {
	h, priv := ssoHarness(t)

	// The wreckage of a prior attempt: an admin row, and no owner meta.
	if err := h.st.CreateFirstAdmin(store.User{
		ID: "u_half", Username: "owner", DisplayName: "Owner",
		Role: store.RoleAdmin, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed the half-created admin: %v", err)
	}

	rec := h.sso(mint(t, priv, ownerClaims()))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sso retry = %d; want 303 (the box is wedged otherwise)", rec.Code)
	}

	// It adopted the existing row rather than making a second account.
	users, err := h.st.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("box has %d users after the retry; want the one it adopted", len(users))
	}
	if users[0].ID != "u_half" {
		t.Errorf("adopted %q; want the half-created u_half", users[0].ID)
	}

	owner, err := h.st.GetBoxMeta(store.BoxMetaOwnerUserID)
	if err != nil {
		t.Fatalf("owner meta not committed by the retry: %v", err)
	}
	if owner != "u_half" {
		t.Errorf("owner meta = %q; want u_half", owner)
	}
}
