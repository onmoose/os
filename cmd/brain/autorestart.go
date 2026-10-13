package main

// The automatic restart of an app that stays unresponsive (#598, HEALTH.md #
// Automatic restart, APP_LIFECYCLE.md # Automatic restart). The
// app-unresponsive detector (appProbeDetector) calls it on every tick for each
// app whose issue it holds raised.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/store"
)

const (
	// autoRestartAfter is how long app-unresponsive must stay raised before
	// the first automatic restart. Each restart in the last hour doubles it,
	// so the waits are 5, 10 and 20 minutes.
	autoRestartAfter = 5 * time.Minute
	// autoRestartLimit is the most automatic restarts of one app in any
	// autoRestartPeriod. After that the brain stops trying until the oldest
	// one is more than autoRestartPeriod old.
	autoRestartLimit  = 3
	autoRestartPeriod = time.Hour
)

// appRestarter is the brain's consumer-side slice of the lifecycle seam the
// automatic restart needs. *lifecycle.Manager satisfies it.
type appRestarter interface {
	BeginRestart(id string) (func(context.Context) error, error)
}

// autoRestarter decides when an app that stays unresponsive is restarted, and
// starts the restart. Not thread-safe, like the detector that drives it: its
// history is only read and written on the app-runtime poll goroutine. The
// restart itself runs on its own goroutine, which touches none of that state.
type autoRestarter struct {
	life    appRestarter
	auditor *audit.Recorder
	after   time.Duration
	limit   int
	period  time.Duration
	history map[string][]time.Time // instance_id -> automatic restarts within period
	wg      sync.WaitGroup         // running restarts; tests wait on it
}

func newAutoRestarter(life appRestarter, auditor *audit.Recorder) *autoRestarter {
	return &autoRestarter{
		life:    life,
		auditor: auditor,
		after:   autoRestartAfter,
		limit:   autoRestartLimit,
		period:  autoRestartPeriod,
		history: map[string][]time.Time{},
	}
}

// recent returns the instance's automatic restarts within the period, and
// forgets the older ones.
func (r *autoRestarter) recent(id string, now time.Time) []time.Time {
	var kept []time.Time
	for _, t := range r.history[id] {
		if now.Sub(t) < r.period {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(r.history, id)
		return nil
	}
	r.history[id] = kept
	return kept
}

// forget drops the history of instances that are gone (uninstalled), so the
// map cannot grow without bound.
func (r *autoRestarter) forget(present map[string]bool) {
	for id := range r.history {
		if !present[id] {
			delete(r.history, id)
		}
	}
}

// note is what the issue's details add about automatic restarts: when the
// brain last restarted the app, and, once the limit is reached, that it
// stopped trying. Empty when there was no automatic restart in the period.
func (r *autoRestarter) note(id string, now time.Time) string {
	recent := r.recent(id, now)
	if len(recent) == 0 {
		return ""
	}
	last := recent[len(recent)-1].UTC().Format("15:04")
	if len(recent) >= r.limit {
		return fmt.Sprintf(" moose restarted it %d times in the last hour, last at %s UTC, and that did not help, so it stopped trying. Stop and start it by hand, or check its logs.", len(recent), last)
	}
	return fmt.Sprintf(" moose restarted it automatically at %s UTC.", last)
}

// maybeRestart restarts the app when its app-unresponsive issue has been
// raised long enough. since is when the issue started to count: when it was
// raised, or the end of the app's start-period grace if that is later. The
// app must be running and not opted out, and must have restarts left in the
// period. A lifecycle operation in flight skips it (BeginRestart refuses), and
// that does not use up a restart. Returns true when a restart was started.
func (r *autoRestarter) maybeRestart(ctx context.Context, inst store.Instance, since, now time.Time) bool {
	if inst.State != "running" || inst.AutoRestartOff {
		return false
	}
	n := len(r.recent(inst.ID, now))
	if n >= r.limit {
		return false
	}
	if now.Sub(since) < r.after<<n {
		return false
	}
	run, err := r.life.BeginRestart(inst.ID)
	if err != nil {
		if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrNotRunning) || errors.Is(err, lifecycle.ErrAutoRestartOff) {
			slog.Info("auto-restart: app busy, not running, or opted out; skipping", "instance_id", inst.ID, "err", err)
		} else {
			slog.Warn("auto-restart: could not begin", "instance_id", inst.ID, "err", err)
		}
		return false
	}
	r.history[inst.ID] = append(r.history[inst.ID], now)
	attempt := n + 1
	slog.Info("auto-restart: restarting an unresponsive app", "instance_id", inst.ID, "name", inst.Name, "count", attempt)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// Not cancelled when the brain stops: a cancelled compose up would
		// park the app in failed, which nothing brings back. If the brain
		// exits halfway instead, the row is still running and the reconcile
		// pass brings the app back up.
		err := run(context.WithoutCancel(ctx))
		meta := map[string]any{"name": inst.Name, "reason": "app-unresponsive", "attempt": attempt}
		if err != nil {
			slog.Warn("auto-restart: restart failed", "instance_id", inst.ID, "name", inst.Name, "err", err)
		} else {
			slog.Info("auto-restart: app restarted", "instance_id", inst.ID, "name", inst.Name)
		}
		// A fresh context: the audit record must land even when the brain is
		// shutting down and ctx is already cancelled.
		r.auditor.Record(context.Background(), audit.ActionAppAutoRestart,
			audit.Target{Kind: "app", ID: inst.ID}, meta, err == nil)
	}()
	return true
}
