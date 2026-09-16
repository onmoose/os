package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/onmoose/moose/internal/assertion"
	"github.com/onmoose/moose/internal/audit"
	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/profile"
	"github.com/onmoose/moose/internal/store"
)

// errSSONotOwner marks an assertion that verified but is not from the box's
// recorded owner. v1 is owner-only (cloud specs/AUTH_AND_ACCESS.md — granting
// other accounts box access is deferred), so a valid assertion from a different
// portal account is refused rather than provisioned a second admin.
var errSSONotOwner = errors.New("sso: assertion is not from the box owner")

// ssoLanding is the box side of the portal-to-box SSO handshake (cloud
// specs/AUTH_AND_ACCESS.md # Portal-to-box SSO). The portal redirects the owner's
// browser here with a short-lived Ed25519-signed ownership assertion in the
// `token` query param; the box verifies it against the seed-delivered portal key,
// auto-creates the owner admin on the first valid assertion, mints its own
// host-only box session, and 303s to the dashboard. Hosted-only — the appliance
// has no portal and keeps its /setup bootstrap.
//
// Every assertion that fails verification or box-side policy (bad signature,
// expiry, wrong issuer/box, replay, non-owner) returns one opaque status and
// audits sso.failure, mirroring login.failure so the Activity view sees mutation
// attempts. The pre-checks that precede any credential evaluation — a missing
// token, or an un-provisioned box — return without auditing (like login's
// empty-credential 401), so an unauthenticated probe can't append audit rows. The
// precise reason is logged, never returned; the token is a credential and is never
// written to a log line.
func (s *Server) ssoLanding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Hosted-only. On appliance the route doesn't exist as far as a caller is
	// concerned — there is no portal to mint assertions.
	if s.profile != profile.Hosted {
		http.NotFound(w, r)
		return
	}
	// No verification key ⇒ the box hasn't ingested a seed yet. Like the old
	// /setup gate's 503, it never falls through to an open bootstrap.
	if s.assertionKey == nil {
		slog.Warn("sso: assertion attempted before provisioning; box has no verification key")
		http.Error(w, "box is not provisioned", http.StatusServiceUnavailable)
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	claims, err := assertion.Verify(s.assertionKey, token)
	if err != nil {
		// Don't surface the reason (tamper vs expiry) or the token to the client.
		slog.Warn("sso: assertion verification failed", "err", err)
		s.auditSSOFailure(ctx)
		http.Error(w, "invalid assertion", http.StatusUnauthorized)
		return
	}

	// Box-side policy the cloud verifier deliberately leaves to us: the assertion
	// must be minted by this box's own control plane (iss) for this box (box), and
	// must carry the identity fields the box persists — sub (owner identity), email
	// (PAM username), jti (replay key). A signed-but-empty field would otherwise
	// poison box state: an empty sub records the owner as "", after which every real
	// owner assertion is rejected as non-owner and the box can never complete SSO
	// again. Reject before the jti is recorded so an empty jti can't consume a slot.
	if claims.Iss != profile.NetworkApex || claims.Box != s.boxID ||
		claims.Sub == "" || claims.Email == "" || claims.JTI == "" {
		slog.Warn("sso: assertion not usable for this box", "iss", claims.Iss, "box_id", claims.Box)
		s.auditSSOFailure(ctx)
		http.Error(w, "invalid assertion", http.StatusForbidden)
		return
	}

	// Single-use: record the jti, rejecting a replay. Replay protection is the
	// box's job — the cloud only guarantees per-mint uniqueness.
	if err := s.store.UseAssertionJTI(claims.JTI, time.Unix(claims.Exp, 0), time.Now()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			slog.Warn("sso: assertion replay rejected")
			s.auditSSOFailure(ctx)
			http.Error(w, "assertion already used", http.StatusUnauthorized)
			return
		}
		slog.Error("sso: record assertion jti failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	user, err := s.resolveSSOOwner(ctx, claims)
	if err != nil {
		if errors.Is(err, errSSONotOwner) {
			slog.Warn("sso: non-owner assertion rejected", "sub", claims.Sub)
			s.auditSSOFailure(ctx)
			http.Error(w, "not the box owner", http.StatusForbidden)
			return
		}
		slog.Error("sso: resolve owner failed", "err", err)
		s.auditSSOFailure(ctx)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Mint the box's own host-only session. The cookie carries no Domain, so it is
	// scoped to "<box-id>.<apex>" and never sent to app subdomains
	// "<slug>.<box-id>.<apex>" (auth.Manager.Cookie; cloud specs/AUTH_AND_ACCESS.md
	// # Three identities).
	sess, err := s.auth.Issue(user.ID)
	if err != nil {
		slog.Error("sso: issue session failed", "err", err, "user_id", user.ID)
		s.auditSSOFailure(ctx)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Also mint the Domain-scoped forward-auth cookie so the owner's click-through
	// to a restricted app needs no second login (issue #305). Compute it before
	// setting any cookie so a store failure here 500s cleanly — no session cookie
	// on a 500 — and a fresh portal round-trip retries the whole exchange. This
	// handler is hosted-only (gated at the top), so no extra profile check.
	faCookie, err := s.auth.IssueForwardAuth(sess.Token)
	if err != nil {
		slog.Error("sso: issue forward-auth cookie failed", "err", err, "user_id", user.ID)
		s.auditSSOFailure(ctx)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, s.auth.Cookie(sess.Token))
	http.SetCookie(w, faCookie)
	idCtx := auth.WithIdentity(ctx, auth.Identity{User: user, Session: sess})
	s.auditor.Record(idCtx, audit.ActionSSOSuccess, audit.Target{Kind: "user", ID: user.ID}, nil, true)

	// The optional `return` param says where on this box to land, and may carry a
	// confirm challenge the dashboard minted before sending the owner to the portal
	// (confirm.go, issue #469). A landing with a valid challenge is the hosted
	// re-auth step, so it elevates the session it just minted; a landing without one
	// is a plain sign-in and elevates nothing. The challenge is spent before the
	// session is elevated, so a replayed return URL grants no second window — and
	// the assertion's own jti was already spent above, before any of this.
	target, challenge := returnTarget(r.URL.Query().Get("return"))
	if challenge != "" {
		s.elevateFromConfirm(idCtx, challenge, user, sess)
	}

	// Redirect relatively, so the browser resolves the target against the URL it
	// arrived on — the same scheme+host that just served this request. The portal
	// only ever sends the owner here over HTTPS, so an absolute
	// "https://<box-id>..." would be equivalent, but a relative target avoids
	// hardcoding the scheme and can't strand a no-cert box on an HTTPS URL it can't
	// serve. returnTarget guarantees the target is a path on this box.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// auditSSOFailure records an sso.failure with no actor (the caller is
// unauthenticated by definition). Mirrors login.failure so the Activity view can
// answer "did someone unauthorized try to enter the box?" symmetrically.
func (s *Server) auditSSOFailure(ctx context.Context) {
	s.auditor.Record(ctx, audit.ActionSSOFailure, audit.Target{Kind: "user"}, nil, false)
}

// resolveSSOOwner maps a verified owner assertion to the box's owner admin,
// creating it on the first valid assertion. On every later assertion it enforces
// owner-only (v1): the assertion's `sub` must match the recorded owner, and the
// session is issued for the same stored account rather than re-derived from the
// email.
func (s *Server) resolveSSOOwner(ctx context.Context, claims assertion.Claims) (store.User, error) {
	ownerSub, err := s.store.GetBoxMeta(store.BoxMetaOwnerSub)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return s.createSSOOwner(ctx, claims)
	case err != nil:
		return store.User{}, err
	}
	if claims.Sub != ownerSub {
		return store.User{}, errSSONotOwner
	}
	uid, err := s.store.GetBoxMeta(store.BoxMetaOwnerUserID)
	if err != nil {
		return store.User{}, err
	}
	return s.store.GetUser(uid)
}

// createSSOOwner provisions the founding admin from the owner assertion: a PAM
// account whose username is derived from the owner's email and whose password is
// random and discarded — the owner only ever authenticates via this handshake.
// It follows the brain-commits-first ordering used by /setup (CLAUDE.md): create
// the brain row, then the host account, rolling the brain row back if a host call
// fails so the next assertion can retry. The owner box-meta is written last as
// the commit marker.
//
// A CreateFirstAdmin conflict means a prior attempt created the user row but
// crashed before the owner meta committed; the username is derived
// deterministically from the email, so we adopt that existing admin rather than
// wedging the box.
func (s *Server) createSSOOwner(ctx context.Context, claims assertion.Claims) (store.User, error) {
	username := ssoUsername(claims.Email)
	password, err := randomPassword()
	if err != nil {
		return store.User{}, err
	}

	u := store.User{
		ID: newID(), Username: username, Role: store.RoleAdmin, CreatedAt: time.Now(),
	}
	if err := s.store.CreateFirstAdmin(u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return s.adoptSSOOwner(ctx, claims, username)
		}
		return store.User{}, err
	}

	if err := s.host.SetPassword(ctx, username, password); err != nil {
		s.rollbackSSOUser(ctx, u)
		return store.User{}, err
	}
	if err := s.host.SetRole(ctx, username, store.RoleAdmin); err != nil {
		s.rollbackSSOUser(ctx, u)
		return store.User{}, err
	}

	if err := s.recordSSOOwner(claims.Sub, u.ID); err != nil {
		return store.User{}, err
	}
	slog.Info("sso: owner admin auto-created", "user_id", u.ID, "username", username)
	return u, nil
}

// adoptSSOOwner reconciles a partially-created owner (brain row exists, owner
// meta does not): the existing admin under the derived username becomes the
// owner. Verifies the row is actually an admin before adopting so an unexpected
// non-admin row surfaces as an error rather than silently granting ownership.
//
// The partial state can also have lost its host account: rollbackSSOUser is
// best-effort, so a prior attempt may have deleted the PAM user but failed to
// delete the brain row. Re-running SetPassword/SetRole here (both idempotent on
// the host) re-establishes the account, so an adopted owner always has a PAM
// entry to back the box session — never a session with no underlying account.
func (s *Server) adoptSSOOwner(ctx context.Context, claims assertion.Claims, username string) (store.User, error) {
	u, err := s.store.GetUserByUsername(username)
	if err != nil {
		return store.User{}, err
	}
	if u.Role != store.RoleAdmin {
		return store.User{}, errors.New("sso: existing user for owner email is not an admin")
	}
	password, err := randomPassword()
	if err != nil {
		return store.User{}, err
	}
	if err := s.host.SetPassword(ctx, username, password); err != nil {
		return store.User{}, err
	}
	if err := s.host.SetRole(ctx, username, store.RoleAdmin); err != nil {
		return store.User{}, err
	}
	if err := s.recordSSOOwner(claims.Sub, u.ID); err != nil {
		return store.User{}, err
	}
	slog.Info("sso: adopted existing admin as owner", "user_id", u.ID, "username", username)
	return u, nil
}

// recordSSOOwner persists the owner identity (portal account id + brain user-id)
// — the commit marker for owner bootstrap. The user-id is written before the sub
// so resolveSSOOwner (which keys on the sub) never sees an owner sub without a
// resolvable user-id.
func (s *Server) recordSSOOwner(sub, userID string) error {
	if err := s.store.SetBoxMeta(store.BoxMetaOwnerUserID, userID); err != nil {
		return err
	}
	return s.store.SetBoxMeta(store.BoxMetaOwnerSub, sub)
}

// rollbackSSOUser undoes a half-created owner after a host failure: best-effort
// host delete (the Linux account may already exist) then the brain row, mirroring
// /setup's rollback so the next assertion can retry cleanly.
func (s *Server) rollbackSSOUser(ctx context.Context, u store.User) {
	if err := s.host.DeleteUser(ctx, u.Username); err != nil {
		slog.Error("sso: rollback host delete-user failed", "username", u.Username, "err", err)
	}
	if err := s.store.DeleteUser(u.ID); err != nil {
		slog.Error("sso: rollback user row failed", "user_id", u.ID, "err", err)
	}
}

// maxSSOUsernameLen caps the derived username at the conservative classic Linux
// username length, so an over-long email local-part can't produce a name useradd
// or a hardened PAM stack rejects. The result is ASCII, so a byte slice is safe.
const maxSSOUsernameLen = 32

// ssoUsername derives a valid Linux/PAM username from the owner's email: the
// local part, lowercased, with every character outside [a-z0-9] folded to '_'.
// Folding (rather than emitting '-') keeps the result clear of the '--' instance
// separator and the 'xn--' prefix validateUsername guards (users.go), and the
// leading-letter guard satisfies useradd. The derivation is the canonical source
// of a valid username here (the SSO path doesn't round-trip through
// validateUsername); it folds to ASCII [a-z0-9_], starts with a letter, and is
// length-capped, so it satisfies every current validateUsername rule by
// construction. Falls back to "owner" when the email yields nothing usable.
func ssoUsername(email string) string {
	local, _, _ := strings.Cut(email, "@")
	local = strings.ToLower(strings.TrimSpace(local))
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "owner_" + name
		name = strings.TrimRight(name, "_")
	}
	if len(name) > maxSSOUsernameLen {
		name = strings.TrimRight(name[:maxSSOUsernameLen], "_")
		if name == "" {
			name = "owner"
		}
	}
	return name
}

// randomPassword returns a 32-hex-char (128-bit) random password for the
// SSO-provisioned owner. The owner never sees or uses it — login is only ever via
// the portal handshake — so it exists solely to give the PAM account a non-empty,
// unguessable shadow entry.
func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
