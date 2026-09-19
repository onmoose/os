package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// forwardAuthVerifyPath is the internal endpoint the box Caddy's per-app
// forward_auth handler calls (issue #305, wired by #306). Sourced from
// profile.ForwardAuthVerifyPath — the one canonical string shared with the
// lifecycle route builder — so the public-allowlist entry (auth.go), the route
// registration (api.go), and the Caddy route (internal/lifecycle) can't drift.
const forwardAuthVerifyPath = profile.ForwardAuthVerifyPath

// forwardAuthVerify is the box side of the hosted per-app forward_auth
// (issue #305). The box Caddy calls it on every request to a restricted app,
// forwarding the request's cookies; this handler reads the Domain-scoped
// forward-auth cookie — never the host-only dashboard session, which app
// subdomains never receive — and answers:
//
//   - 200 + identity headers when the cookie resolves to a live session owned by
//     the box owner; Caddy then proxies to the app (and, per #306, strips the
//     cookie so the app upstream never sees it);
//   - 401 otherwise; Caddy turns that into a redirect to the box login.
//
// Owner-only in v1 (ENVIRONMENT.md # Public-by-default, auth-gated): only the box
// owner's session validates — box users the owner may later create are a
// follow-up. Hosted-only: on the appliance there is no portal, no app
// subdomains, and no forward-auth cookie, so the route 404s exactly like
// /_moose/sso. This is a pure session probe, so it never audits (CLAUDE.md #
// Elevation-class — pure reads don't audit).
func (s *Server) forwardAuthVerify(w http.ResponseWriter, r *http.Request) {
	if s.profile != profile.Hosted {
		http.NotFound(w, r)
		return
	}

	id, err := s.auth.ValidateForwardAuth(auth.ForwardAuthTokenFromRequest(r))
	if err != nil {
		forwardAuthUnauthorized(w)
		return
	}

	// Owner-only (v1). The token already proved a live session; the box-identity
	// policy is enforced here rather than in auth, so the auth layer stays free of
	// box concepts. Fail closed on any owner-lookup trouble — an unprovisioned or
	// half-written owner must never fall through to "allow".
	ownerID, err := s.store.GetBoxMeta(store.BoxMetaOwnerUserID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("forward-auth: owner lookup failed", "err", err)
		}
		forwardAuthUnauthorized(w)
		return
	}
	if id.User.ID != ownerID {
		forwardAuthUnauthorized(w)
		return
	}

	// Identity for the app upstream. #306 decides which of these it forwards (and
	// strips the forward-auth cookie before proxying); #305 only makes the identity
	// available on the allow response.
	w.Header().Set("X-Moose-User", id.User.Username)
	w.Header().Set("X-Moose-User-Id", id.User.ID)
	w.WriteHeader(http.StatusOK)
}

// forwardAuthUnauthorized writes the 401 the box Caddy turns into a login
// redirect. Plain text with no body contract — Caddy reads only the status.
func forwardAuthUnauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
