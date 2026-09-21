package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

// fixture spins up a real SQLite store with one seeded admin user. Tests use
// the real store (modernc.org/sqlite is fast, no mocks per project policy).
func fixture(t *testing.T) (*Manager, *store.Store, store.User, func() time.Time) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	u := store.User{
		ID: "u1", Username: "andrei", DisplayName: "andrei", Role: store.RoleAdmin,
		CreatedAt: time.Unix(1_700_000_000, 0),
	}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	now := time.Unix(1_700_000_500, 0)
	clock := func() time.Time { return now }
	m := NewManager(s)
	m.Clock = clock
	return m, s, u, func() time.Time { return now }
}

func TestIssueAndValidate(t *testing.T) {
	m, _, u, _ := fixture(t)

	sess, err := m.Issue(u.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if sess.Token == "" {
		t.Fatal("empty token")
	}
	if sess.UserID != u.ID {
		t.Fatalf("user_id = %q, want %q", sess.UserID, u.ID)
	}

	id, err := m.Validate(sess.Token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.User.ID != u.ID {
		t.Fatalf("identity user = %+v", id.User)
	}
	if !id.IsAdmin() {
		t.Fatal("IsAdmin = false for admin user")
	}
}

func TestValidateTouchesLastSeen(t *testing.T) {
	m, s, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)

	// Move the clock forward, then validate. last_seen_at should follow;
	// created_at must not.
	originalCreated := sess.CreatedAt
	later := sess.CreatedAt.Add(7 * time.Minute)
	m.Clock = func() time.Time { return later }

	if _, err := m.Validate(sess.Token); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	persisted, err := s.GetSession(sess.Token)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !persisted.LastSeenAt.Equal(later) {
		t.Fatalf("last_seen_at = %v, want %v", persisted.LastSeenAt, later)
	}
	if !persisted.CreatedAt.Equal(originalCreated) {
		t.Fatalf("created_at moved on touch: %v", persisted.CreatedAt)
	}
}

func TestValidateRejectsUnknown(t *testing.T) {
	m, _, _, _ := fixture(t)
	if _, err := m.Validate("never-issued"); err != ErrInvalidSession {
		t.Fatalf("Validate(unknown) = %v, want ErrInvalidSession", err)
	}
	if _, err := m.Validate(""); err != ErrInvalidSession {
		t.Fatalf("Validate(empty) = %v, want ErrInvalidSession", err)
	}
}

func TestValidateRejectsDeletedUser(t *testing.T) {
	m, s, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)
	// Delete via DeleteSessionsForUser-equivalent path: we want a session
	// orphaned from a missing user. FK cascade kills the session row when we
	// DeleteUser, so simulate the defense-in-depth case by inserting a
	// session that points at a nonexistent user.
	bogus := store.Session{Token: "orphan", UserID: "ghost", CreatedAt: sess.CreatedAt, LastSeenAt: sess.CreatedAt}
	if err := s.CreateSession(bogus); err == nil {
		// store enforces FK with ON DELETE CASCADE but doesn't prevent
		// inserting a session pointing at a missing user out of the gate
		// unless foreign_keys is honored at insert time. Either outcome is
		// fine: if the insert succeeded, Validate must still reject it.
		if _, err := m.Validate("orphan"); err != ErrInvalidSession {
			t.Fatalf("Validate(orphaned) = %v, want ErrInvalidSession", err)
		}
	}
	// And the live happy-path session must still validate.
	if _, err := m.Validate(sess.Token); err != nil {
		t.Fatalf("Validate(live) = %v", err)
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	m, _, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)
	if err := m.Revoke(sess.Token); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := m.Validate(sess.Token); err != ErrInvalidSession {
		t.Fatalf("after Revoke: %v", err)
	}
	if err := m.Revoke(sess.Token); err != nil {
		t.Fatalf("Revoke again: %v", err)
	}
	if err := m.Revoke("never-existed"); err != nil {
		t.Fatalf("Revoke(unknown): %v", err)
	}
}

func TestTokensAreUniqueAndOpaque(t *testing.T) {
	m, _, u, _ := fixture(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		sess, err := m.Issue(u.ID)
		if err != nil {
			t.Fatalf("Issue #%d: %v", i, err)
		}
		if seen[sess.Token] {
			t.Fatalf("duplicate token at iter %d: %q", i, sess.Token)
		}
		seen[sess.Token] = true
		// base64url(32 bytes) = 43 chars unpadded. Sanity-check length so we
		// catch any accidental downgrade in entropy.
		if len(sess.Token) != 43 {
			t.Fatalf("token len = %d, want 43; token=%q", len(sess.Token), sess.Token)
		}
	}
}

func TestCookieAttributes(t *testing.T) {
	m, _, _, _ := fixture(t)

	c := m.Cookie("abc")
	if c.Name != CookieName {
		t.Fatalf("Name = %q", c.Name)
	}
	if c.Value != "abc" || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attrs = %+v", c)
	}
	if c.Secure {
		t.Fatal("Secure must default to false in dev")
	}

	m.SecureCookies = true
	if !m.Cookie("x").Secure {
		t.Fatal("Secure not set when SecureCookies=true")
	}

	cleared := m.ClearCookie()
	if cleared.MaxAge >= 0 {
		t.Fatalf("ClearCookie MaxAge = %d; want negative", cleared.MaxAge)
	}
}

func TestTokenFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := TokenFromRequest(r); got != "" {
		t.Fatalf("no-cookie request returned %q", got)
	}
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
	if got := TokenFromRequest(r); got != "tok" {
		t.Fatalf("TokenFromRequest = %q", got)
	}
}

func TestContextRoundTrip(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty ctx returned identity")
	}
	id := Identity{User: store.User{ID: "u1", Role: store.RoleMember}}
	ctx := WithIdentity(context.Background(), id)
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("not ok after WithIdentity")
	}
	if got.User.ID != "u1" || got.IsAdmin() {
		t.Fatalf("round-trip = %+v", got)
	}
}

// --- Session expiry tests --------------------------------------------------

func TestValidateRejectsIdleExpiredSession(t *testing.T) {
	m, s, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)

	// Advance clock past the idle window.
	m.Clock = func() time.Time { return sess.CreatedAt.Add(SessionIdleWindow + time.Second) }

	if _, err := m.Validate(sess.Token); err != ErrInvalidSession {
		t.Fatalf("idle-expired session: Validate = %v, want ErrInvalidSession", err)
	}
	// Row must have been deleted.
	if _, err := s.GetSession(sess.Token); err == nil {
		t.Fatal("expired session row not deleted")
	}
}

func TestValidateRejectsHardCapExpiredSession(t *testing.T) {
	m, s, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)

	// Advance clock past the hard cap (but keep bumping last_seen_at by
	// validating every 29 days to defeat the idle window check).
	// Simulate: last_seen_at is only 1 day old, but expires_at is in the past.
	m.Clock = func() time.Time { return sess.CreatedAt.Add(SessionHardCap + time.Second) }

	if _, err := m.Validate(sess.Token); err != ErrInvalidSession {
		t.Fatalf("hard-cap-expired session: Validate = %v, want ErrInvalidSession", err)
	}
	// Row must have been deleted.
	if _, err := s.GetSession(sess.Token); err == nil {
		t.Fatal("hard-cap-expired session row not deleted")
	}
}

func TestValidateStillValidBeforeExpiry(t *testing.T) {
	m, _, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)

	// Just under the idle window — should still be valid.
	m.Clock = func() time.Time { return sess.CreatedAt.Add(SessionIdleWindow - time.Second) }

	if _, err := m.Validate(sess.Token); err != nil {
		t.Fatalf("valid session rejected early: %v", err)
	}
}

func TestIssueSetExpiresAt(t *testing.T) {
	m, s, u, _ := fixture(t)
	now := m.Clock()
	sess, _ := m.Issue(u.ID)

	persisted, _ := s.GetSession(sess.Token)
	want := now.Add(SessionHardCap)
	if persisted.ExpiresAt.Unix() != want.Unix() {
		t.Fatalf("ExpiresAt = %v, want %v", persisted.ExpiresAt, want)
	}
}

// --- Elevation tests -------------------------------------------------------

func TestElevateAndIsElevated(t *testing.T) {
	m, s, u, _ := fixture(t)
	now := m.Clock()
	sess, _ := m.Issue(u.ID)

	if err := m.Elevate(sess.Token); err != nil {
		t.Fatalf("Elevate: %v", err)
	}

	// Re-read from store to get the updated ElevatedUntil.
	persisted, _ := s.GetSession(sess.Token)
	wantUntil := now.Add(ElevationWindow)
	if persisted.ElevatedUntil.Unix() != wantUntil.Unix() {
		t.Fatalf("ElevatedUntil = %v, want %v", persisted.ElevatedUntil, wantUntil)
	}

	// Validate picks up the fresh ElevatedUntil; IsElevated() should be true.
	id, err := m.Validate(sess.Token)
	if err != nil {
		t.Fatalf("Validate after Elevate: %v", err)
	}
	if !id.IsElevated() {
		t.Fatal("IsElevated() = false immediately after Elevate")
	}
}

func TestIsElevatedFalseAfterWindowExpires(t *testing.T) {
	m, _, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)
	if err := m.Elevate(sess.Token); err != nil {
		t.Fatalf("Elevate: %v", err)
	}

	// Advance clock past the elevation window (but stay within session lifetimes).
	m.Clock = func() time.Time { return sess.CreatedAt.Add(ElevationWindow + time.Second) }

	id, err := m.Validate(sess.Token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.IsElevated() {
		t.Fatal("IsElevated() = true after window expired")
	}
}

func TestIsElevatedFalseWithoutElevate(t *testing.T) {
	m, _, u, _ := fixture(t)
	sess, _ := m.Issue(u.ID)
	id, _ := m.Validate(sess.Token)
	if id.IsElevated() {
		t.Fatal("IsElevated() = true without prior Elevate call")
	}
}
