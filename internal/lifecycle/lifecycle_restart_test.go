package lifecycle

// BeginRestart (#598, APP_LIFECYCLE.md # Automatic restart): the automatic
// restart of an app that stays unresponsive. It is a Stop then a Start under
// one lock, and it never runs into an operation already holding the app.

import (
	"context"
	"errors"
	"testing"
)

func TestBeginRestartStopsThenStarts(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	before := len(e.docker.methods())
	pubBefore := e.host.publishCount(inst.Slug)

	run, err := e.m.BeginRestart(inst.ID)
	if err != nil {
		t.Fatalf("begin restart: %v", err)
	}
	if err := run(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}

	// compose stop, then the Start path's compose up, in that order.
	var stopAt, upAt = -1, -1
	for i, m := range e.docker.methods()[before:] {
		switch m {
		case "ComposeStop":
			if stopAt < 0 {
				stopAt = i
			}
		case "ComposeUp":
			if upAt < 0 {
				upAt = i
			}
		}
	}
	if stopAt < 0 || upAt < 0 || stopAt > upAt {
		t.Fatalf("want ComposeStop then ComposeUp, got %v", e.docker.methods()[before:])
	}
	if row, _ := e.store.Get(inst.ID); row.State != "running" {
		t.Fatalf("state after restart = %q, want running", row.State)
	}
	if got := e.caddy.route(inst.ID); len(got) < 9 || got[:9] != "upstream:" {
		t.Fatalf("route after restart = %q, want upstream:…", got)
	}
	// The Start path re-announces the name, the same as a manual start.
	if got := e.host.publishCount(inst.Slug); got != pubBefore+1 {
		t.Fatalf("publish count = %d, want %d", got, pubBefore+1)
	}
	// The lock is released: a Stop goes through at once.
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop after restart: %v", err)
	}
}

// A stopped app is never restarted, and nothing is called on Docker.
func TestBeginRestartRefusesStoppedApp(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	before := len(e.docker.methods())
	if _, err := e.m.BeginRestart(inst.ID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
	if got := e.docker.methods()[before:]; len(got) != 0 {
		t.Fatalf("docker called for a stopped app: %v", got)
	}
	// The refusal released the lock.
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start after refusal: %v", err)
	}
}

// An app another lifecycle operation holds is skipped, never waited for.
func TestBeginRestartSkipsBusyApp(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	unlock := e.m.lockInstance(inst.ID)
	if _, err := e.m.BeginRestart(inst.ID); !errors.Is(err, ErrBusy) {
		unlock()
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	unlock()
	// A restart that began holds the app: a second one is refused until it runs.
	run, err := e.m.BeginRestart(inst.ID)
	if err != nil {
		t.Fatalf("begin restart: %v", err)
	}
	if _, err := e.m.BeginRestart(inst.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("second begin err = %v, want ErrBusy", err)
	}
	if err := run(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

// A restart that does not come healthy lands in failed, like a Start.
func TestBeginRestartHealthFailureMarksFailed(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	e.docker.inspect = func(_, _ string) (bool, string, error) {
		return true, "unhealthy", nil
	}
	run, err := e.m.BeginRestart(inst.ID)
	if err != nil {
		t.Fatalf("begin restart: %v", err)
	}
	if err := run(context.Background()); err == nil {
		t.Fatal("restart: want a health error")
	}
	if row, _ := e.store.Get(inst.ID); row.State != "failed" {
		t.Fatalf("state = %q, want failed", row.State)
	}
	if got := e.caddy.route(inst.ID); got != "splash:failed" {
		t.Fatalf("route = %q, want splash:failed", got)
	}
}
