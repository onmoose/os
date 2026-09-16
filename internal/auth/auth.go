// Package auth owns dashboard sessions: opaque random tokens issued by the
// brain, validated on every authenticated request, and revoked on logout.
// The credential itself never lives here — PAM (via host-agent) is the
// source of truth for passwords (AUTH.md # Identity primitive). This package
// owns the session lifecycle and the cookie shape; it does not implement the
// HTTP middleware (that lives in internal/api so it can use huma's types).
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/onmoose/os/internal/store"
)

// CookieName is the dashboard's session cookie. AUTH.md # Sessions.
const CookieName = "moose_session"

// ForwardAuthCookieName is the hosted per-app forward-auth cookie (issue #305).
// Unlike CookieName it is Domain-scoped to the box apex so the browser sends it
// to app subdomains, where the box Caddy validates it against the brain before
// proxying (the standard forward_auth shape). It is a strictly lower-privilege
// credential: it proves a valid box session exists, but its value is a distinct
// random token stored in a distinct column, so replaying it as CookieName never
// resolves to a dashboard session. Hosted-only; the appliance never mints one.
const ForwardAuthCookieName = "moose_forward_auth"

// tokenBytes is the entropy of one session token. 32 bytes = 256 bits;
// base64url-encoded that's 43 chars. Server-side validated, so length here
// is for collision resistance, not guess resistance.
const tokenBytes = 32

// Session lifetime constants (AUTH.md # Lifetime).
const (
	// SessionIdleWindow is the rolling idle timeout. A session that hasn't
	// been seen within this window is treated as expired.
	SessionIdleWindow = 30 * 24 * time.Hour

	// SessionHardCap is the absolute maximum lifetime of a session. Even a
	// constantly-active session is invalidated after this duration.
	SessionHardCap = 90 * 24 * time.Hour

	// ElevationWindow is the duration a session stays elevated after a
	// successful POST /api/v1/auth/elevate (USERS_AND_GROUPS.md # Elevation).
	ElevationWindow = 5 * time.Minute
)

// ErrInvalidSession is returned when a token can't be resolved to a live
// user. The HTTP layer maps it to 401.
var ErrInvalidSession = errors.New("invalid session")

// SessionStore is the persistence surface auth needs. Declared here, not in
// internal/store, so auth doesn't depend on the store's full API
// (consumer-side interface rule from CLAUDE.md).
type SessionStore interface {
	CreateSession(store.Session) error
	GetSession(token string) (store.Session, error)
	TouchSession(token string, at time.Time) error
	DeleteSession(token string) error
	SetElevatedUntil(token string, until time.Time) error
	GetUser(id string) (store.User, error)
	// Forward-auth (hosted, #305): stamp a session with its forward-auth token
	// and resolve a session back from it. The reverse lookup backs the per-app
	// verify endpoint.
	SetSessionForwardAuthToken(sessionToken, faToken string) error
	GetSessionByForwardAuthToken(faToken string) (store.Session, error)
}

// Identity is the authenticated principal attached to a request context by
// the API's auth middleware. The raw session token is not part of Identity —
// handlers shouldn't be able to forward it.
type Identity struct {
	User     store.User
	Session  store.Session
	elevated bool // computed by Validate at the Manager's clock; use IsElevated()
}

// IsAdmin is a convenience for role-gated handlers.
func (id Identity) IsAdmin() bool { return id.User.Role == store.RoleAdmin }

// IsElevated reports whether the session was in the 5-minute elevation window
// at the time Validate ran (USERS_AND_GROUPS.md # Elevation in the UI).
func (id Identity) IsElevated() bool { return id.elevated }

// Manager issues, validates, and revokes sessions. One per brain.
type Manager struct {
	store SessionStore
	// Clock returns "now". Injected for deterministic tests.
	Clock func() time.Time
	// SecureCookies sets the Secure attribute on issued cookies. True only
	// when the brain is serving HTTPS — Caddy fronts us in prod, but in dev
	// the brain listens on plain HTTP and Secure would make the browser drop
	// the cookie.
	SecureCookies bool
	// ForwardAuthDomain is the Domain attribute stamped on the hosted
	// forward-auth cookie (issue #305): the box apex "<box-id>.onmoose.network",
	// so the browser sends the cookie to every app subdomain
	// "<slug>.<box-id>.onmoose.network" as well as the dashboard host. Empty on
	// appliance (and any box with no box-id), which disables minting — the
	// appliance never issues a forward-auth cookie.
	ForwardAuthDomain string
}

func NewManager(s SessionStore) *Manager {
	return &Manager{store: s, Clock: time.Now}
}

// Issue mints a new session for userID. ExpiresAt is set to now + SessionHardCap.
// Caller sets the resulting cookie on the response.
func (m *Manager) Issue(userID string) (store.Session, error) {
	token, err := newToken()
	if err != nil {
		return store.Session{}, err
	}
	now := m.Clock()
	sess := store.Session{
		Token:      token,
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(SessionHardCap),
	}
	if err := m.store.CreateSession(sess); err != nil {
		return store.Session{}, err
	}
	return sess, nil
}

// Validate resolves a token to an Identity and bumps last_seen_at. Unknown
// token → ErrInvalidSession. Expired sessions (idle window or hard cap) are
// deleted and return ErrInvalidSession. A session whose user has been removed
// is also ErrInvalidSession (defensive — the FK cascade should have killed
// the session row already, but we don't want to leak a deleted user back to a
// handler under any circumstance).
func (m *Manager) Validate(token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrInvalidSession
	}
	sess, err := m.store.GetSession(token)
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, ErrInvalidSession
	}
	if err != nil {
		return Identity{}, err
	}
	return m.resolveSession(sess, true)
}

// ValidateForwardAuth resolves the hosted forward-auth token (issue #305) to an
// Identity, running the same lifetime + user checks as Validate against the
// session the token belongs to. Unlike Validate it does NOT bump last_seen_at:
// the box Caddy calls the verify endpoint on a per-request forward_auth
// subrequest — potentially once per asset — so touching the row there would be
// write amplification for no benefit. The dashboard session stays the liveness
// authority; app traffic rides its rolling window rather than extending it.
// Owner-only policy (v1) is enforced by the caller — this layer only proves the
// session behind the token is live.
func (m *Manager) ValidateForwardAuth(faToken string) (Identity, error) {
	// Shape-check before the store. The verify endpoint is a public, internet-
	// reachable path on a hosted box and the brain serializes every query through
	// a single SQLite connection (store.Open: SetMaxOpenConns(1)), so a token that
	// cannot possibly be one we minted must be rejected without a DB round-trip —
	// otherwise anonymous garbage-cookie traffic queues in front of every other
	// brain query (login, dashboard, installs), turning an endpoint-local flood
	// into a whole-brain stall. Only a well-formed token reaches the reverse lookup.
	if !WellFormedToken(faToken) {
		return Identity{}, ErrInvalidSession
	}
	sess, err := m.store.GetSessionByForwardAuthToken(faToken)
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, ErrInvalidSession
	}
	if err != nil {
		return Identity{}, err
	}
	return m.resolveSession(sess, false)
}

// resolveSession enforces session lifetime (AUTH.md # Lifetime) and resolves the
// user for a session already fetched — by session token (Validate) or by
// forward-auth token (ValidateForwardAuth). touch controls the last_seen_at
// bump: the dashboard path keeps the rolling idle window alive, the forward-auth
// path deliberately does not. An expired session is deleted and rejected either
// way; a session whose user has vanished is rejected defensively.
func (m *Manager) resolveSession(sess store.Session, touch bool) (Identity, error) {
	now := m.Clock()
	idleExpiry := sess.LastSeenAt.Add(SessionIdleWindow)
	hardExpiry := sess.ExpiresAt
	if now.After(idleExpiry) || (!hardExpiry.IsZero() && now.After(hardExpiry)) {
		// Session is expired — delete it and reject.
		_ = m.store.DeleteSession(sess.Token)
		return Identity{}, ErrInvalidSession
	}

	user, err := m.store.GetUser(sess.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, ErrInvalidSession
	}
	if err != nil {
		return Identity{}, err
	}
	if touch {
		if err := m.store.TouchSession(sess.Token, now); err != nil {
			return Identity{}, err
		}
		sess.LastSeenAt = now
	}
	elevated := !sess.ElevatedUntil.IsZero() && now.Before(sess.ElevatedUntil)
	return Identity{User: user, Session: sess, elevated: elevated}, nil
}

// Elevate marks the session elevated for ElevationWindow. Used by the elevate
// handler; the updated ElevatedUntil is reflected on the next Validate call.
func (m *Manager) Elevate(token string) error {
	until := m.Clock().Add(ElevationWindow)
	return m.store.SetElevatedUntil(token, until)
}

// Revoke deletes the session. Idempotent: unknown token returns nil.
func (m *Manager) Revoke(token string) error {
	return m.store.DeleteSession(token)
}

// Cookie returns the http.Cookie that carries `token` to the browser.
// HttpOnly + SameSite=Lax + Path=/. Secure follows m.SecureCookies.
func (m *Manager) Cookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCookie returns a cookie that tells the browser to drop the session
// cookie. Used on logout.
func (m *Manager) ClearCookie() *http.Cookie {
	c := m.Cookie("")
	c.MaxAge = -1
	return c
}

// IssueForwardAuth mints a fresh forward-auth token for an existing session
// (issue #305), persists it on the session row so its lifetime is the session's,
// and returns the Domain-scoped cookie the caller sets alongside the dashboard
// session cookie. Hosted-only: the caller gates on profile == Hosted, and the
// cookie is meaningless without ForwardAuthDomain set.
func (m *Manager) IssueForwardAuth(sessionToken string) (*http.Cookie, error) {
	faToken, err := newToken()
	if err != nil {
		return nil, err
	}
	if err := m.store.SetSessionForwardAuthToken(sessionToken, faToken); err != nil {
		return nil, err
	}
	return m.ForwardAuthCookie(faToken), nil
}

// ForwardAuthCookie returns the http.Cookie carrying a forward-auth token to the
// browser. Same hardening as the session cookie (HttpOnly, SameSite=Lax, Secure
// per SecureCookies) but with Domain set to ForwardAuthDomain so it is sent to
// every app subdomain under the box apex — the deliberate difference from the
// host-only session cookie.
func (m *Manager) ForwardAuthCookie(faToken string) *http.Cookie {
	return &http.Cookie{
		Name:     ForwardAuthCookieName,
		Value:    faToken,
		Path:     "/",
		Domain:   m.ForwardAuthDomain,
		HttpOnly: true,
		Secure:   m.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearForwardAuthCookie returns a cookie that tells the browser to drop the
// forward-auth cookie on logout. It carries the same Domain as the issued cookie
// so the browser matches and expires the right one.
func (m *Manager) ClearForwardAuthCookie() *http.Cookie {
	c := m.ForwardAuthCookie("")
	c.MaxAge = -1
	return c
}

// TokenFromRequest extracts the session token from the request's cookie. ""
// when no cookie is set — callers treat that as "no session", not an error.
func TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// ForwardAuthTokenFromRequest extracts the forward-auth token from the request's
// cookie (issue #305). "" when absent — the verify endpoint treats that as "no
// forward-auth session", i.e. 401, not an error.
func ForwardAuthTokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(ForwardAuthCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

type ctxKey struct{}

// WithIdentity attaches id to ctx. The auth middleware does this once per
// authenticated request; handlers read it with FromContext.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the identity attached by WithIdentity, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenLen is the length of a newToken() value: 32 bytes, raw-base64url-encoded.
const tokenLen = (tokenBytes*8 + 5) / 6 // 43

// WellFormedToken reports whether s could have been produced by newToken — the
// exact length and alphabet of a raw-base64url 256-bit token. It says nothing
// about whether the token exists; it is a pure syntax gate that lets a caller
// reject impossible credentials without touching the store. That matters on the
// hosted forward-auth verify path, which is public, internet-reachable, and
// called by Caddy per app asset, in front of a store with a single SQLite
// connection: a garbage cookie must cost a string scan, not a DB round-trip.
func WellFormedToken(s string) bool {
	if len(s) != tokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
