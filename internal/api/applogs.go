package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/onmoose/moose/internal/applog"
	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/store"
)

// appLog is the per-app log tail (BRAIN_UI_PROTOCOL.md Pattern C; LOGGING.md
// # Per-app logs). It is registered raw, like events/systemLive, because the
// reconnect-replay wire format (id: + data:, Last-Event-ID) is the point and
// huma streaming would only obscure it.
//
// Order matters: authenticate, then existence + visibility (so a denial is a
// real HTTP status for curl / defense-in-depth, even though EventSource can't
// read it), then reserve the per-session stream slot, and only then write 200
// and start streaming. The brain's applog Hub owns the ring buffer + replay; we
// resolve the instance's main container, subscribe with the reader's
// Last-Event-ID, write the replay backlog, then fan out live lines.
func (s *Server) appLog(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauthenticated(w)
		return
	}
	instanceID := r.PathValue("id")
	inst, err := s.store.Get(instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeStreamError(w, http.StatusNotFound, "Not found")
		return
	}
	if err != nil {
		writeStreamError(w, http.StatusInternalServerError, "Lookup failed")
		return
	}
	if status := logVisibility(id, inst); status != http.StatusOK {
		writeStreamError(w, status, http.StatusText(status))
		return
	}

	if s.applogs == nil {
		writeStreamError(w, http.StatusServiceUnavailable, "Log streaming not configured")
		return
	}

	// Resolve the container before reserving the stream slot or writing headers,
	// so a manifest-read failure is a clean 500 rather than a half-open stream.
	container, err := s.life.MainContainerName(instanceID)
	if err != nil {
		writeStreamError(w, http.StatusInternalServerError, "Could not resolve container")
		return
	}

	release, ok := s.beginStream(w, r)
	if !ok {
		return // beginStream wrote 401 (no session) or 429 (over the per-session cap)
	}
	defer release()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	replay, live, unsub := s.applogs.Subscribe(instanceID, container, parseLastEventID(r))
	defer unsub()

	// Initial comment so proxies flush headers immediately.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Replay backlog (ring tail from Last-Event-ID, possibly led by a {lost}
	// marker) before going live, so reconnects resume without a gap.
	for _, ln := range replay {
		writeLogFrame(w, flusher, ln)
	}

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case ln, ok := <-live:
			if !ok {
				return
			}
			writeLogFrame(w, flusher, ln)
		}
	}
}

// writeLogFrame writes one Pattern-C frame: `id: <n>` + `data: <json>`, no
// `event:` field, so the dashboard reads it via EventSource.onmessage. The id is
// the brain's monotonic event id, which the browser echoes back as
// Last-Event-ID on reconnect.
func writeLogFrame(w http.ResponseWriter, f http.Flusher, ln applog.Line) {
	data, _ := json.Marshal(ln.Data)
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ln.ID, data)
	f.Flush()
}

// logVisibility decides who may stream an instance's logs. It is STRICTER than
// canSee: a member may *see* a household app in the launcher, but its logs are
// admins-only (logs can leak another household member's activity). The returned
// status is the one to write on denial; http.StatusOK means allowed.
//
//   - admin: always allowed.
//   - owner of a personal instance: allowed (their own logs).
//   - non-admin + someone else's personal instance: 404 — mirrors getApp's leak
//     guard so the app's existence isn't disclosed.
//   - non-admin + household instance: 403 — the app is visible, its logs are not.
func logVisibility(id auth.Identity, i store.Instance) int {
	if id.IsAdmin() {
		return http.StatusOK
	}
	if i.Scope == store.ScopePersonal {
		if i.OwnerUserID == id.User.ID {
			return http.StatusOK
		}
		return http.StatusNotFound
	}
	return http.StatusForbidden
}

// parseLastEventID reads the reader's reconnect position from the Last-Event-ID
// header (set automatically by EventSource). Absent or unparseable → 0, i.e. a
// fresh reader that receives the whole current backlog.
func parseLastEventID(r *http.Request) uint64 {
	n, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// writeStreamError writes a raw {status,title} error for the log SSE endpoint,
// mirroring writeUnauthenticated's shape (these handlers sit outside huma's
// error envelope).
func writeStreamError(w http.ResponseWriter, status int, title string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, _ := json.Marshal(struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
	}{status, title})
	_, _ = w.Write(data)
}
