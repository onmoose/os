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
	// the first automatic restart. Each restart doubles it, so the waits are
	// 5, 10 and 20 minutes, and the three restarts fit in about an hour.
	autoRestartAfter = 5 * time.Minute
	// autoRestartLimit is the most automatic restarts of one app before the
	// brain gives up on it (#620). The count is in the app's row, so a brain
	// restart keeps it. It goes back to 0 only when the app answers a real
	// health check, or a person starts the app or flips its setting.
	// autoRestartPeriod also caps an app that recovers and breaks again (#598):
	// at most autoRestartLimit restarts in any autoRestartPeriod, counted in
	// memory, and forgotten when a person acts on the app.
	autoRestartLimit  = 3
	autoRestartPeriod = time.Hour
)

// appRestarter is the brain's consumer-side slice of the lifecycle seam the
// automatic restart needs. *lifecycle.Manager satisfies it. BeginRestart
// counts the restart in the app's row, and returns the row as it read it
// under the app's lock, before the count.
type appRestarter interface {
	BeginRestart(id string, at time.Time) (func(context.Context) error, store.Instance, error)
}

// autoRestartResetter sets an app's stored automatic restart count back to 0
// when the app answers a real health check (#620). *store.Store satisfies it.
type autoRestartResetter interface {
	ResetInstanceAutoRestarts(id string, byPerson bool) error
}

// autoRestarter decides when an app that stays unresponsive is restarted, and
// starts the restart. Not thread-safe, like the detector that drives it: its
// history is only read and written on the app-runtime poll goroutine. The
// restart itself runs on its own goroutine, which touches none of that state.
//
// Two counts bound it. The stored one (store.Instance.AutoRestarts, written
// by BeginRestart) makes the brain give up for good after autoRestartLimit
// restarts that did not bring the app back, across brain restarts (#620). The
// history here, in memory, caps an app that recovers between restarts at
// autoRestartLimit in any hour (#598).
type autoRestarter struct {
	life    appRestarter
	counts  autoRestartResetter
	auditor *audit.Recorder
	after   time.Duration
	limit   int
	period  time.Duration
	history map[string][]time.Time // instance_id -> automatic restarts within period
	wg      sync.WaitGroup         // running restarts; tests wait on it
}

func newAutoRestarter(life appRestarter, counts autoRestartResetter, auditor *audit.Recorder) *autoRestarter {
	return &autoRestarter{
		life:    life,
		counts:  counts,
		auditor: auditor,
		after:   autoRestartAfter,
		limit:   autoRestartLimit,
		period:  autoRestartPeriod,
		history: map[string][]time.Time{},
	}
}

// recent returns the instance's automatic restarts within the period, and
// forgets the older ones. A row with no last restart time means a person
// acted on the app since (a Start, or a change of its setting), so the app
// gets a fresh hour and the history goes too.
func (r *autoRestarter) recent(inst store.Instance, now time.Time) []time.Time {
	if inst.LastAutoRestart.IsZero() {
		delete(r.history, inst.ID)
		return nil
	}
	var kept []time.Time
	for _, t := range r.history[inst.ID] {
		if now.Sub(t) < r.period {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(r.history, inst.ID)
		return nil
	}
	r.history[inst.ID] = kept
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

// givenUp reports whether the brain has given up on the app (#620): it made
// autoRestartLimit restarts and the app has not answered a real health check
// since, nor has a person started it or flipped its setting.
func (r *autoRestarter) givenUp(inst store.Instance) bool {
	return inst.AutoRestarts >= r.limit
}

// recovered is called when the app answered a real health check. It ends the
// brain's giving up, or a run of restarts that is still going. The history in
// memory stays, so an app that keeps breaking right after it recovers is
// still restarted at most autoRestartLimit times in an hour.
func (r *autoRestarter) recovered(inst store.Instance) {
	if inst.AutoRestarts == 0 {
		return
	}
	if err := r.counts.ResetInstanceAutoRestarts(inst.ID, false); err != nil {
		slog.Warn("auto-restart: could not reset the restart count", "instance_id", inst.ID, "err", err)
		return
	}
	slog.Info("auto-restart: app answered its health check; restart count reset", "instance_id", inst.ID, "name", inst.Name, "count", inst.AutoRestarts)
}

// note is what the issue's details add about automatic restarts: that the
// brain gave up and what the user can do, or else when it last restarted the
// app. Empty when there was no automatic restart in the last hour and the
// brain has not given up.
func (r *autoRestarter) note(inst store.Instance, now time.Time) string {
	last := inst.LastAutoRestart
	at := last.UTC().Format("15:04")
	if r.givenUp(inst) {
		return fmt.Sprintf(" moose restarted it %d times, the last time at %s UTC. That did not help, so moose will not restart it again by itself. To try again, stop the app and start it. Its logs may show what is wrong.", inst.AutoRestarts, at)
	}
	if last.IsZero() || now.Sub(last) >= r.period {
		return ""
	}
	return fmt.Sprintf(" moose restarted it automatically at %s UTC.", at)
}

// maybeRestart restarts the app when its app-unresponsive issue has been
// raised long enough. since is when the issue started to count: when it was
// raised, or the end of the app's start-period grace if that is later. The
// app must be running and not opted out, and the brain must not have given up
// on it. A lifecycle operation in
// flight skips it (BeginRestart refuses), and that does not use up a restart.
// Returns true when a restart was started.
func (r *autoRestarter) maybeRestart(ctx context.Context, inst store.Instance, since, now time.Time) bool {
	if inst.State != "running" || inst.AutoRestartOff || r.givenUp(inst) {
		return false
	}
	// The wait doubles with each restart: those since the app last worked,
	// from the row, so a brain restart does not shorten it, or those in the
	// last hour, for an app that recovers and breaks again.
	n := max(len(r.recent(inst, now)), inst.AutoRestarts)
	if n >= r.limit {
		return false
	}
	if now.Sub(since) < r.after<<n {
		return false
	}
	run, fresh, err := r.life.BeginRestart(inst.ID, now)
	if err != nil {
		if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrNotRunning) || errors.Is(err, lifecycle.ErrAutoRestartOff) {
			slog.Info("auto-restart: app busy, not running, or opted out; skipping", "instance_id", inst.ID, "err", err)
		} else {
			slog.Warn("auto-restart: could not begin", "instance_id", inst.ID, "err", err)
		}
		return false
	}
	// A person may have acted since this tick read the row (a Start, or the
	// switch off and on): the fresh row under the lock then has no last
	// restart time. Forget the history here, or this restart's new time
	// would hide that reset from every later tick.
	if fresh.LastAutoRestart.IsZero() {
		delete(r.history, inst.ID)
	}
	r.history[inst.ID] = append(r.history[inst.ID], now)
	attempt := fresh.AutoRestarts + 1
	slog.Info("auto-restart: restarting an unresponsive app", "instance_id", inst.ID, "name", inst.Name, "count", attempt)
	if attempt >= r.limit {
		slog.Info("auto-restart: last automatic restart; the brain gives up if it does not help", "instance_id", inst.ID, "name", inst.Name)
	}
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
