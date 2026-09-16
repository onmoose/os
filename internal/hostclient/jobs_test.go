package hostclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/onmoose/moose/internal/hostagent"
	"github.com/onmoose/moose/internal/protocol"
)

// blockingUpdater keeps a job running until the test releases it, so the
// "already running" window is deterministic.
type blockingUpdater struct {
	release chan struct{}
	started chan struct{}
}

func (u *blockingUpdater) Update(ctx context.Context, _, _ string) (protocol.SystemUpdateResult, error) {
	close(u.started)
	select {
	case <-u.release:
	case <-ctx.Done():
	}
	return protocol.SystemUpdateResult{BrainChanged: true}, nil
}

// startJobAgent runs the real hostagent handler set over a real UNIX socket.
// Using the real agent rather than a hand-rolled mux is the point: this test
// covers the wire seam brain ↔ socket ↔ /v1/jobs/*, so a status code either
// side would get wrong is a status code this test sees.
func startJobAgent(t *testing.T, u hostagent.SystemUpdater) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := hostagent.New(nil, hostagent.NewFakePublisher(".local"))
	a.Updater = u
	mux := http.NewServeMux()
	a.Mount(mux)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// TestStartSystemUpdateAndPoll: the accepted job comes back with an id, and
// polling it by that id returns the finished record.
func TestStartSystemUpdateAndPoll(t *testing.T) {
	u := &blockingUpdater{release: make(chan struct{}), started: make(chan struct{})}
	c := New(startJobAgent(t, u))
	ctx := context.Background()

	job, err := c.StartSystemUpdate(ctx, "brain:v2", "ui:v2")
	if err != nil {
		t.Fatalf("StartSystemUpdate: %v", err)
	}
	if job.ID == "" || job.Kind != protocol.JobKindSystemUpdate || job.Status != protocol.JobStatusRunning {
		t.Fatalf("accepted job = %+v", job)
	}

	<-u.started
	close(u.release)
	for {
		got, err := c.Job(ctx, job.ID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if got.Status == protocol.JobStatusRunning {
			continue
		}
		if got.Status != protocol.JobStatusCompleted {
			t.Fatalf("finished job = %+v, want completed", got)
		}
		if got.Result == nil || !got.Result.BrainChanged {
			t.Fatalf("result lost over the wire: %+v", got.Result)
		}
		return
	}
}

// TestStartSystemUpdate_Conflict: a second update while one runs must reach the
// brain as a typed sentinel, not an opaque host error — the brain turns it into
// a 409 the admin can act on ("one is already running"), which is the one
// refusal that is not a bug.
func TestStartSystemUpdate_Conflict(t *testing.T) {
	u := &blockingUpdater{release: make(chan struct{}), started: make(chan struct{})}
	c := New(startJobAgent(t, u))
	ctx := context.Background()

	if _, err := c.StartSystemUpdate(ctx, "brain:v2", ""); err != nil {
		t.Fatalf("first StartSystemUpdate: %v", err)
	}
	<-u.started
	defer close(u.release)

	_, err := c.StartSystemUpdate(ctx, "brain:v3", "")
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("second StartSystemUpdate: want ErrUpdateInProgress, got %v", err)
	}
}

// TestJob_Unknown: an id host-agent has never seen — every id is unknown after
// a host-agent restart, since the records are in memory.
func TestJob_Unknown(t *testing.T) {
	c := New(startJobAgent(t, &blockingUpdater{release: make(chan struct{}), started: make(chan struct{})}))
	_, err := c.Job(context.Background(), "j_nope")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Job(unknown): want ErrJobNotFound, got %v", err)
	}
}

// TestStartSystemUpdate_NoUpdater: a host-agent with no updater wired (the fake
// binary) answers 501, and that is a plain error, not one of the two sentinels
// — the brain must not report "already running" for "not supported".
func TestStartSystemUpdate_NoUpdater(t *testing.T) {
	c := New(startJobAgent(t, nil))
	_, err := c.StartSystemUpdate(context.Background(), "brain:v2", "")
	if err == nil {
		t.Fatal("want an error from a host-agent with no updater")
	}
	if errors.Is(err, ErrUpdateInProgress) || errors.Is(err, ErrJobNotFound) {
		t.Fatalf("501 misreported as a sentinel: %v", err)
	}
}
