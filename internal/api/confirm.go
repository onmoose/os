package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/onmoose/moose/internal/audit"
	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/profile"
	"github.com/onmoose/moose/internal/store"
)

// The hosted confirm step (issue #469). Destructive admin actions ask the user
// to prove it is really them before the brain opens the five-minute elevation
// window (AUTH.md # Re-authentication for destructive actions). On the appliance
// that proof is the account password. A hosted owner has no password to give:
// the portal signs them in and the box gave their PAM account a random one that
// is generated and thrown away (sso.go # createSSOOwner), so every
// elevation-class action was unreachable on a hosted box.
//
// The hosted proof is a portal round-trip instead: the dashboard mints a
// challenge here, sends the browser to the portal's open-box route with a return
// path that carries the challenge, and the portal redirects back to the box's SSO
// landing with a fresh ownership assertion. The landing verifies the assertion —
// a stronger proof of the owner than a password, since it is signed by the portal
// for this box and this account — and elevates the session it mints.
//
// Why the challenge exists at all. The portal's open-box route is a plain GET
// with a SameSite=Lax cookie, so a cross-site page can drive that navigation in
// the victim's browser. While the route only signed the owner in, that was
// harmless. Once the same round-trip also opens a privileged window, a cross-site
// page could arm it silently. The challenge is the thing such a page cannot
// supply -- and the reason is that it cannot READ the mint, not that it cannot
// send it. This handler takes no body and requires no JSON content type, so the
// POST is a simple request in CORS terms: a same-site app page can send it with
// the owner's cookie and a challenge will be minted. What that page never gets
// is the response, because the brain serves no CORS headers at all (api.go #
// Handler), and a challenge nobody can read is inert -- single-use, bound to its
// user, and expiring unspent. A landing that carries no valid challenge signs
// the owner in exactly as before and grants no elevation.

// elevationChallengeTTL bounds a confirm challenge. It has to outlast the portal
// round-trip — which may include a portal login — but nothing more, so it is
// short enough that an unspent challenge is worthless within minutes. It matches
// the elevation window it buys, which keeps one number in the user's head.
const elevationChallengeTTL = 5 * time.Minute

// confirmParam is the query key the dashboard puts in its return path to carry
// the challenge through the portal and back to the SSO landing. The landing
// strips it before redirecting, so the spent nonce never lingers in the address
// bar or in browser history.
const confirmParam = "confirm"

// maxReturnPathLen caps the return path the SSO landing accepts. A dashboard
// route is short; anything longer is not one of ours.
const maxReturnPathLen = 512

// elevateChallenge mints a one-time confirm challenge for the calling session
// (issue #469). Hosted-only: on the appliance the confirm step is the password
// prompt and this route does not exist, mirroring how ssoLanding and authUsers
// hide themselves on the other profile.
//
// Owner-only on top of that, and 404 for the same reason: the owner is the only
// account the portal round-trip can return as (the handshake is owner-only, v1),
// so for anyone else the route may as well not exist. A challenge they minted
// could never be redeemed anyway — the landing refuses one belonging to another
// user — so this closes a write path rather than a privilege gap. It fails
// closed: a box with no owner recorded, or an owner lookup that errors, mints
// nothing.
//
// It is a pure mint with no privilege of its own — holding a challenge elevates
// nothing until a verified portal assertion redeems it — so it does not audit.
// The elevation it may later buy is audited at the landing.
func (s *Server) elevateChallenge(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Challenge string `json:"challenge"`
		ExpiresAt int64  `json:"expires_at"`
	}
}, error) {
	if s.profile != profile.Hosted {
		return nil, huma.Error404NotFound("not found")
	}
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	ownerID, err := s.store.GetBoxMeta(store.BoxMetaOwnerUserID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("confirm: owner lookup failed", "err", err)
			return nil, huma.Error500InternalServerError("store read failed", err)
		}
		slog.Warn("confirm: challenge requested on a box with no recorded owner", "user_id", id.User.ID)
		return nil, huma.Error404NotFound("not found")
	}
	if id.User.ID != ownerID {
		return nil, huma.Error404NotFound("not found")
	}

	challenge, err := newChallengeID()
	if err != nil {
		return nil, huma.Error500InternalServerError("generate challenge", err)
	}
	now := time.Now()
	expires := now.Add(elevationChallengeTTL)
	if err := s.store.CreateElevationChallenge(challenge, id.User.ID, expires, now); err != nil {
		return nil, huma.Error500InternalServerError("store challenge", err)
	}

	out := &struct {
		Body struct {
			Challenge string `json:"challenge"`
			ExpiresAt int64  `json:"expires_at"`
		}
	}{}
	out.Body.Challenge = challenge
	out.Body.ExpiresAt = expires.Unix()
	return out, nil
}

// newChallengeID returns a 256-bit random challenge id, hex-encoded so it rides
// a URL query through the portal untouched.
func newChallengeID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// returnTarget splits the SSO landing's `return` query param into the path to
// redirect to and the confirm challenge it carries. Anything that is not a plain
// relative path on this box falls back to "/" with no challenge, so a bad value
// still lands the owner on their own dashboard.
//
// This is the risky half of the change: the landing hands out a valid session, so
// an open redirect here would hand it to somebody else's page. The rules are
// deliberately narrow — one leading slash, no scheme, no host, no userinfo, no
// backslash (browsers read "\" as "/" in a URL, so "/\evil.com" is an off-box
// target), no control characters, and a length cap.
func returnTarget(raw string) (target, challenge string) {
	if raw == "" || len(raw) > maxReturnPathLen {
		return "/", ""
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/", ""
	}
	if strings.ContainsRune(raw, '\\') {
		return "/", ""
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "/", ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return "/", ""
	}
	// url.Parse keeps a leading "//" only in Host, which is already refused, but
	// re-check the parsed path: "/..//evil" style inputs must stay on-box.
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/", ""
	}
	q := u.Query()
	challenge = q.Get(confirmParam)
	q.Del(confirmParam)
	u.RawQuery = q.Encode()
	return u.String(), challenge
}

// elevateFromConfirm spends a confirm challenge and, if it is good, marks the
// freshly-minted SSO session elevated for the normal window — the hosted
// equivalent of a successful password confirm.
//
// Failure never fails the sign-in: the owner is already authenticated by the
// assertion, so a spent, expired, or unknown challenge only means no elevation.
// The dashboard sees the next elevation-class write rejected and starts the
// confirm round-trip again. Both outcomes audit under the same actions the
// password path uses, so the Activity view sees one elevation story.
func (s *Server) elevateFromConfirm(ctx context.Context, challenge string, user store.User, sess store.Session) {
	target := audit.Target{Kind: "user", ID: user.ID}

	userID, err := s.store.UseElevationChallenge(challenge, time.Now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("sso: confirm challenge lookup failed", "err", err, "user_id", user.ID)
		} else {
			slog.Warn("sso: confirm challenge is unknown, expired, or already spent", "user_id", user.ID)
		}
		s.auditor.Record(ctx, audit.ActionElevateFailure, target, nil, false)
		return
	}
	// The challenge belongs to a session, and so to a user. A challenge minted for
	// somebody else must not elevate this one, even though v1 is owner-only.
	if userID != user.ID {
		slog.Warn("sso: confirm challenge belongs to another user", "user_id", user.ID)
		s.auditor.Record(ctx, audit.ActionElevateFailure, target, nil, false)
		return
	}
	if err := s.auth.Elevate(sess.Token); err != nil {
		slog.Error("sso: elevate session failed", "err", err, "user_id", user.ID)
		s.auditor.Record(ctx, audit.ActionElevateFailure, target, nil, false)
		return
	}
	slog.Info("sso: session elevated by portal confirm", "user_id", user.ID)
	s.auditor.Record(ctx, audit.ActionElevateSuccess, target, nil, true)
}
