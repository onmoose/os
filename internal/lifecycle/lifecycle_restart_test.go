package lifecycle

// BeginRestart (#598, APP_LIFECYCLE.md # Automatic restart): the automatic
// restart of an app that stays unresponsive. It is a Stop then a Start under
// one lock, and it never runs into an operation already holding the app.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBeginRestartStopsThenStarts(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	before := len(e.docker.methods())
	pubBefore := e.host.publishCount(inst.Slug)

	run, err := e.m.BeginRestart(inst.ID, time.Now())
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
	if _, err := e.m.BeginRestart(inst.ID, time.Now()); !errors.Is(err, ErrNotRunning) {
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
	if _, err := e.m.BeginRestart(inst.ID, time.Now()); !errors.Is(err, ErrBusy) {
		unlock()
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	unlock()
	// A restart that began holds the app: a second one is refused until it runs.
	run, err := e.m.BeginRestart(inst.ID, time.Now())
	if err != nil {
		t.Fatalf("begin restart: %v", err)
	}
	if _, err := e.m.BeginRestart(inst.ID, time.Now()); !errors.Is(err, ErrBusy) {
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
	run, err := e.m.BeginRestart(inst.ID, time.Now())
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

// The setting is read from the fresh row under the lock: an app turned off
// after the detector read it is not restarted.
func TestBeginRestartRefusesOptedOutApp(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	if err := e.store.SetInstanceAutoRestart(inst.ID, false); err != nil {
		t.Fatalf("turn off: %v", err)
	}
	if _, err := e.m.BeginRestart(inst.ID, time.Now()); !errors.Is(err, ErrAutoRestartOff) {
		t.Fatalf("err = %v, want ErrAutoRestartOff", err)
	}
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop after refusal (lock released?): %v", err)
	}
}

// A restart that cannot read the manifest fails before it stops anything,
// so the app is never left stopped with nothing to start it.
func TestBeginRestartFailsBeforeStop(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	run, err := e.m.BeginRestart(inst.ID, time.Now())
	if err != nil {
		t.Fatalf("begin restart: %v", err)
	}
	if err := os.Remove(filepath.Join(e.m.instanceDir(inst.ID), "manifest.yml")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	before := len(e.docker.methods())
	if err := run(context.Background()); err == nil {
		t.Fatal("restart: want a manifest error")
	}
	for _, m := range e.docker.methods()[before:] {
		if m == "ComposeStop" {
			t.Fatal("the app was stopped although the restart could not start it")
		}
	}
}

// A begun restart is counted in the row before it runs, with its time, and
// a refused one is not (#620). The count survives in the store, so a brain
// restart does not reset it.
func TestBeginRestartCountsInTheRow(t *testing.T) {
	e := newTestEnv(t)
	inst := installRunning(t, e)
	at := time.Date(2026, 10, 13, 12, 6, 0, 0, time.UTC)
	run, err := e.m.BeginRestart(inst.ID, at)
	if err != nil {
		t.Fatalf("begin restart: %v", err)
	}
	row, _ := e.store.Get(inst.ID)
	if row.AutoRestarts != 1 || !row.LastAutoRestart.Equal(at) {
		t.Fatalf("row = %d restarts, last %v; want 1 at %v, counted before the run", row.AutoRestarts, row.LastAutoRestart, at)
	}
	if _, err := e.m.BeginRestart(inst.ID, at.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if err := run(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.AutoRestarts != 1 {
		t.Fatalf("restarts = %d after a refused begin, want 1", row.AutoRestarts)
	}
	if err := e.store.SetInstanceAutoRestart(inst.ID, false); err != nil {
		t.Fatalf("turn off: %v", err)
	}
	if _, err := e.m.BeginRestart(inst.ID, at.Add(2*time.Minute)); !errors.Is(err, ErrAutoRestartOff) {
		t.Fatalf("err = %v, want ErrAutoRestartOff", err)
	}
	if row, _ := e.store.Get(inst.ID); row.AutoRestarts != 0 {
		t.Fatalf("restarts = %d; turning the setting off resets the count", row.AutoRestarts)
	}
}

// A person's Start gives the app a fresh hour: the count the brain gave up
// at goes back to 0 and the last time is cleared, for a stopped app and for a
// failed one (#620).
func TestStartResetsAutoRestarts(t *testing.T) {
	for _, prev := range []string{"stopped", "failed"} {
		t.Run(prev, func(t *testing.T) {
			e := newTestEnv(t)
			inst := installRunning(t, e)
			at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			for i := 0; i < 3; i++ {
				run, err := e.m.BeginRestart(inst.ID, at)
				if err != nil {
					t.Fatalf("begin restart: %v", err)
				}
				if err := run(context.Background()); err != nil {
					t.Fatalf("restart: %v", err)
				}
			}
			if prev == "stopped" {
				if err := e.m.Stop(context.Background(), inst.ID); err != nil {
					t.Fatalf("stop: %v", err)
				}
			} else if err := e.store.SetState(inst.ID, "failed"); err != nil {
				t.Fatalf("set failed: %v", err)
			}
			if row, _ := e.store.Get(inst.ID); row.AutoRestarts != 3 {
				t.Fatalf("restarts before Start = %d, want 3 (Stop alone does not reset)", row.AutoRestarts)
			}
			if err := e.m.Start(context.Background(), inst.ID); err != nil {
				t.Fatalf("start: %v", err)
			}
			// The time goes too: it tells the brain to forget the restarts it
			// holds in memory, so the app gets a fresh hour.
			row, _ := e.store.Get(inst.ID)
			if row.AutoRestarts != 0 || !row.LastAutoRestart.IsZero() {
				t.Fatalf("after Start: %d restarts, last %v; want 0 and no time", row.AutoRestarts, row.LastAutoRestart)
			}
		})
	}
}
