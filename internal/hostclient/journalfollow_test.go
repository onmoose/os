package hostclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// startLogAgent serves /v1/journal/follow on a UNIX socket. container "two"
// streams two frames then closes; "block" streams one frame then holds the
// connection open until the request is cancelled; "none" returns 501.
func startLogAgent(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/journal/follow", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("container") {
		case "none":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "logs-unsupported", Message: "no log source"})
			return
		case "block":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "id: 1\ndata: %s\n\n", `{"stream":"stdout","line":"first"}`)
			fl.Flush()
			<-r.Context().Done() // hold open until the client cancels
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "id: 1\ndata: %s\n\n", `{"stream":"stdout","line":"a"}`)
			fl.Flush()
			fmt.Fprintf(w, "id: 2\ndata: %s\n\n", `{"stream":"stderr","line":"b"}`)
			fl.Flush()
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return sock
}

func recvJL(t *testing.T, ch <-chan protocol.JournalLine) (protocol.JournalLine, bool) {
	t.Helper()
	select {
	case l, ok := <-ch:
		return l, ok
	case <-time.After(time.Second):
		t.Fatal("timed out waiting on the log channel")
		return protocol.JournalLine{}, false
	}
}

func TestJournalFollow_ParsesFramesThenCloses(t *testing.T) {
	c := New(startLogAgent(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := c.JournalFollow(ctx, "two")
	if err != nil {
		t.Fatalf("JournalFollow: %v", err)
	}
	l1, ok := recvJL(t, ch)
	if !ok || l1.Line != "a" || l1.Stream != "stdout" {
		t.Fatalf("frame 1: got %+v ok=%v", l1, ok)
	}
	l2, ok := recvJL(t, ch)
	if !ok || l2.Line != "b" || l2.Stream != "stderr" {
		t.Fatalf("frame 2: got %+v ok=%v", l2, ok)
	}
	if _, ok := recvJL(t, ch); ok {
		t.Fatal("channel must close after the server ends the stream")
	}
}

func TestJournalFollow_NonOKReturnsError(t *testing.T) {
	c := New(startLogAgent(t))
	if _, err := c.JournalFollow(context.Background(), "none"); err == nil {
		t.Fatal("a 501 from host-agent must surface as an error, not a stream")
	}
}

// Cancelling the caller's context tears the long-lived follow down: the channel
// closes and the reader goroutine exits.
func TestJournalFollow_ContextCancelClosesChannel(t *testing.T) {
	c := New(startLogAgent(t))
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := c.JournalFollow(ctx, "block")
	if err != nil {
		t.Fatalf("JournalFollow: %v", err)
	}
	if l, ok := recvJL(t, ch); !ok || l.Line != "first" {
		t.Fatalf("first frame: got %+v ok=%v", l, ok)
	}
	cancel()
	if _, ok := recvJL(t, ch); ok {
		t.Fatal("channel must close once the context is cancelled")
	}
}
