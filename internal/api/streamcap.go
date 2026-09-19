package api

import (
	"net/http"
	"sync"

	"github.com/onmoose/os/internal/auth"
)

// maxStreamsPerSession caps concurrent SSE streams per session
// (BRAIN_UI_PROTOCOL.md # Stream cap): a backstop against buggy dashboards or
// many open tabs. The 17th concurrent stream on a session is refused with 429.
// Every raw SSE handler (events, systemLive) shares one count keyed on the
// session token, so the cap is per-session, not per-endpoint.
const maxStreamsPerSession = 16

// streamCap tracks the number of live SSE streams per session token and
// enforces maxStreamsPerSession across every raw SSE handler. Safe for
// concurrent use; the map entry is dropped when a session's last stream closes
// so a churn of short-lived sessions doesn't leak keys.
type streamCap struct {
	mu    sync.Mutex
	limit int
	count map[string]int // session token → currently-open stream count
}

func newStreamCap(limit int) *streamCap {
	return &streamCap{limit: limit, count: map[string]int{}}
}

// acquire reserves a stream slot for token. It returns a release func and true
// when a slot was free; nil and false when the session is already at the limit
// (the caller must then reject with 429). The release func must be called
// exactly once — via defer — when the stream ends.
func (c *streamCap) acquire(token string) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count[token] >= c.limit {
		return nil, false
	}
	c.count[token]++
	return func() { c.release(token) }, true
}

func (c *streamCap) release(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count[token] <= 1 {
		delete(c.count, token)
		return
	}
	c.count[token]--
}

// beginStream authenticates a raw SSE request and reserves its slot under the
// per-session cap. It writes 401 when no session identity is present and 429
// when the session is already at the cap; in both cases it returns ok=false and
// the caller must return immediately without writing stream headers. On success
// the caller must `defer release()` so the slot is freed on disconnect.
//
// Auth is normally enforced by authMiddleware before the handler runs; resolving
// the identity here is what keys the cap to the session token, and double-guards
// the raw handlers, which sit outside huma's per-operation handling.
func (s *Server) beginStream(w http.ResponseWriter, r *http.Request) (func(), bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return nil, false
	}
	release, ok := s.streams.acquire(id.Session.Token)
	if !ok {
		writeStreamCapExceeded(w)
		return nil, false
	}
	return release, true
}

// writeStreamCapExceeded writes the locked 429 shape (BRAIN_UI_PROTOCOL.md #
// Rate limiting & abuse, # Errors): {code, message, details?} envelope with
// code "rate-limited", scope "session", and a Retry-After: 0 header (the slot
// frees when a stream closes, not after a fixed delay).
func writeStreamCapExceeded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"code":"rate-limited","message":"Too many live streams open for this session.","details":{"scope":"session"}}`))
}
