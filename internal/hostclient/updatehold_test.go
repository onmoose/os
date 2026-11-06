package hostclient

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/onmoose/os/internal/hostagent"
)

// cuttingHolder keeps a hold, cut to cut, the way the loop cuts it to its cap.
type cuttingHolder struct {
	cut     time.Time
	end     time.Time
	cleared bool
}

func (h *cuttingHolder) SetHold(until time.Time) time.Time {
	if until.After(h.cut) {
		until = h.cut
	}
	h.end = until
	return until
}
func (h *cuttingHolder) ClearHold() { h.cleared = true }

func startHoldAgent(t *testing.T, h hostagent.UpdateHolder) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := hostagent.New(nil, hostagent.NewFakePublisher(".local"))
	if h != nil {
		a.UpdateHold = h
	}
	mux := http.NewServeMux()
	a.Mount(mux)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// The hold round-trips over the socket, with host-agent's cut.
func TestUpdateHold(t *testing.T) {
	cut := time.Date(2026, 11, 7, 3, 55, 0, 0, time.UTC)
	h := &cuttingHolder{cut: cut}
	c := New(startHoldAgent(t, h))
	ctx := context.Background()

	end, err := c.SetUpdateHold(ctx, cut.Add(time.Hour))
	if err != nil || !end.Equal(cut) {
		t.Fatalf("SetUpdateHold = %v, %v; want the cut end %v", end, err, cut)
	}
	if err := c.ClearUpdateHold(ctx); err != nil || !h.cleared {
		t.Fatalf("ClearUpdateHold = %v, cleared %v", err, h.cleared)
	}
	// A hold that is not live after the cuts is the zero time.
	h.cut = time.Time{}
	if end, err := c.SetUpdateHold(ctx, time.Now()); err != nil || !end.IsZero() {
		t.Fatalf("not live: %v, %v; want the zero time", end, err)
	}
}

// A host-agent with nothing to hold answers an error, which the brain logs.
func TestUpdateHold_Unsupported(t *testing.T) {
	c := New(startHoldAgent(t, nil))
	if _, err := c.SetUpdateHold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("SetUpdateHold on a host-agent with no holder succeeded")
	}
}
