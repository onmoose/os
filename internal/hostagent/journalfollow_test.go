package hostagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/onmoose/moose/internal/protocol"
)

// stubLogSource emits a fixed set of lines then closes the channel, so the
// journalFollow handler reads them all and returns deterministically — no
// ticker, no timing.
type stubLogSource struct {
	lines []protocol.JournalLine
	err   error
}

func (s *stubLogSource) Follow(_ context.Context, _ string) (<-chan protocol.JournalLine, error) {
	if s.err != nil {
		return nil, s.err
	}
	ch := make(chan protocol.JournalLine, len(s.lines))
	for _, l := range s.lines {
		ch <- l
	}
	close(ch)
	return ch, nil
}

func agentWithLogs(src LogSource) *http.ServeMux {
	a := New(nil, NewFakePublisher(".local"))
	a.Logs = src
	mux := http.NewServeMux()
	a.Mount(mux)
	return mux
}

type frame struct {
	id   uint64
	data protocol.JournalLine
}

func parseFrames(t *testing.T, body string) []frame {
	t.Helper()
	var out []frame
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var f frame
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "id: "); ok {
				id, err := strconv.ParseUint(rest, 10, 64)
				if err != nil {
					t.Fatalf("bad id line %q: %v", line, err)
				}
				f.id = id
			} else if rest, ok := strings.CutPrefix(line, "data: "); ok {
				if err := json.Unmarshal([]byte(rest), &f.data); err != nil {
					t.Fatalf("bad data line %q: %v", line, err)
				}
			}
		}
		out = append(out, f)
	}
	return out
}

func TestJournalFollow_NilSourceReturns501(t *testing.T) {
	mux := agentWithLogs(nil)
	w := get(t, mux, "/v1/journal/follow?container=moose-x-web")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("nil log source: want 501, got %d", w.Code)
	}
}

func TestJournalFollow_MissingContainerReturns400(t *testing.T) {
	mux := agentWithLogs(&stubLogSource{})
	w := get(t, mux, "/v1/journal/follow")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing container: want 400, got %d", w.Code)
	}
}

func TestJournalFollow_StreamsLinesWithMonotonicIDs(t *testing.T) {
	mux := agentWithLogs(&stubLogSource{lines: []protocol.JournalLine{
		{Ts: "t1", Stream: "stdout", Line: "first"},
		{Ts: "t2", Stream: "stderr", Line: "second"},
	}})
	w := get(t, mux, "/v1/journal/follow?container=moose-x-web")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type: want text/event-stream, got %q", ct)
	}
	frames := parseFrames(t, w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("want 2 frames, got %d (%q)", len(frames), w.Body.String())
	}
	if frames[0].id != 1 || frames[1].id != 2 {
		t.Errorf("ids must be monotonic 1,2; got %d,%d", frames[0].id, frames[1].id)
	}
	if frames[0].data.Line != "first" || frames[1].data.Line != "second" {
		t.Errorf("line payloads: got %q,%q", frames[0].data.Line, frames[1].data.Line)
	}
	if frames[1].data.Stream != "stderr" {
		t.Errorf("stream tag must survive: want stderr, got %q", frames[1].data.Stream)
	}
}

// A reconnect carrying Last-Event-ID can't be replayed by a fresh per-connection
// follower, so the handler leads with one {"lost":true} frame, then resumes live.
func TestJournalFollow_LastEventIDEmitsLostThenResumes(t *testing.T) {
	mux := agentWithLogs(&stubLogSource{lines: []protocol.JournalLine{
		{Line: "after-reconnect"},
	}})
	req := httptest.NewRequest(http.MethodGet, "/v1/journal/follow?container=moose-x-web", nil)
	req.Header.Set("Last-Event-ID", "42")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	frames := parseFrames(t, w.Body.String())
	if len(frames) != 2 {
		t.Fatalf("want 2 frames (lost + line), got %d (%q)", len(frames), w.Body.String())
	}
	if !frames[0].data.Lost {
		t.Errorf("first frame after a Last-Event-ID must be the lost marker, got %+v", frames[0].data)
	}
	if frames[1].data.Line != "after-reconnect" {
		t.Errorf("second frame: want the live line, got %q", frames[1].data.Line)
	}
}

func TestJournalFollow_SourceErrorReturns500(t *testing.T) {
	mux := agentWithLogs(&stubLogSource{err: errors.New("journalctl exploded")})
	w := get(t, mux, "/v1/journal/follow?container=moose-x-web")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("source error: want 500, got %d", w.Code)
	}
}
