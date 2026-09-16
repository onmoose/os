package api

import "net/http"

// healthzPath is the brain's liveness probe. Deliberately **outside**
// /api/v1: it is an operational probe, not a product surface, so it carries no
// versioning promise, is not described in the OpenAPI document, and never
// reaches the generated TS client (BRAIN_UI_PROTOCOL.md # Versioning covers
// /api/v1 only).
//
// The caller is the control-plane updater. UPDATES.md # 3 step 3d has
// host-agent waiting up to 60s for /healthz on a freshly recreated brain before
// it commits the update, and reverting both images if the wait times out. That
// makes this endpoint the single fact the update transaction turns on.
//
// It is **public** (publicPaths, auth.go) because host-agent has no session and
// cannot get one — it drives the box, it is not a dashboard client. Nothing is
// disclosed by the answer: it says "this process is serving HTTP", which anyone
// who can open the connection already learned from the TCP accept. Reach is
// narrow anyway — Caddy proxies only /api/* and /_moose/* to the brain
// (internal/caddy), so this path is not routable from the LAN; the callers who
// can reach it are on the Docker network or on the host.
const healthzPath = "/healthz"

// healthz answers the liveness probe: 200 as soon as the HTTP server is
// answering, with no dependency check at all.
//
// **Liveness, not readiness, and that distinction is load-bearing here.** The
// probe's only consumer reverts a control-plane update when it fails, so
// checking SQLite, Docker, Caddy, or host-agent would make an unrelated sick
// dependency roll back a perfectly good new brain — and roll it back to an old
// brain facing the very same sick dependency. Dependency health is a different
// question with a different answer surface: GET /api/v1/health (HEALTH.md),
// which is admin-authenticated and returns structured issues.
//
// Kept allocation-light and side-effect free: it is polled in a loop during an
// update and is a candidate Docker healthcheck later.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// A cached 200 would let a probe read a dead brain as live.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
