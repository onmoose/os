package updatetarget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// The brain's hold on the OS switch (#632, hold.go).

func osTarget() Target {
	t := Target{Version: "v0.16.0", BrainImage: brainRef, UIImage: uiRef, BrainDigest: digest("a"), UIDigest: digest("b")}
	t.OS = []OSRelease{rel("0.15.1")}
	return t
}

func local(day, hour, min int) time.Time {
	return time.Date(2026, 10, day, hour, min, 0, 0, time.Local)
}

// A live hold keeps the switch from happening in the window, and the install
// still runs: Apply is called with the window shut, which is the path that
// downloads and installs into the other slot.
func TestHold_KeepsTheSwitchButNotTheInstall(t *testing.T) {
	now := local(3, 3, 10)
	l, f, _ := osLoop(osTarget(), "0.15.0", DefaultWindow, now)
	l.Now = func() time.Time { return now }
	if end := l.SetHold(now.Add(protocol.UpdateHoldLease)); end.IsZero() {
		t.Fatal("the hold was not kept")
	}
	l.Tick(context.Background())
	if len(f.calls) != 1 || f.calls[0] {
		t.Fatalf("with a live hold: Apply calls %v, want one with the window shut", f.calls)
	}
	if s := l.Snapshot().OS; s.State != protocol.OSUpdateInstalled || s.Detail != SwitchWaitsForApps {
		t.Fatalf("OS snapshot = %+v, want installed and waiting for the apps", s)
	}
}

// A hold that ends on its own lets the next tick switch.
func TestHold_ExpiredLetsItSwitch(t *testing.T) {
	now := local(3, 3, 5)
	l, f, _ := osLoop(osTarget(), "0.15.0", DefaultWindow, now)
	l.Now = func() time.Time { return now }
	l.SetHold(now.Add(5 * time.Minute))
	l.Tick(context.Background())
	now = now.Add(6 * time.Minute)
	if _, ok := l.Hold(); ok {
		t.Fatal("the hold is still live after its end")
	}
	l.Tick(context.Background())
	if len(f.calls) != 2 || f.calls[0] || !f.calls[1] {
		t.Fatalf("Apply calls %v, want shut and then open", f.calls)
	}
}

// signalOS tells a test when the window opened to the switch. Run calls it
// from its own goroutine.
type signalOS struct {
	mu     sync.Mutex
	f      fakeOS
	opened chan struct{}
}

func (s *signalOS) Running() (string, string) { return s.f.Running() }
func (s *signalOS) Floor() string             { return "" }
func (s *signalOS) Peek(r OSRelease) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Peek(r)
}
func (s *signalOS) SwitchNow(r OSRelease, n time.Time) OSDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.SwitchNow(r, n)
}
func (s *signalOS) Apply(r OSRelease, open bool, n time.Time) OSDecision {
	s.mu.Lock()
	d := s.f.Apply(r, open, n)
	s.mu.Unlock()
	if open {
		select {
		case s.opened <- struct{}{}:
		default:
		}
	}
	return d
}

// Clearing the hold wakes the loop, so the switch happens at once and not at
// the next 15-minute tick, which could miss the window.
func TestHold_ClearWakesTheLoop(t *testing.T) {
	now := local(3, 3, 10)
	tgt := osTarget()
	sig := &signalOS{f: fakeOS{running: "0.15.0"}, opened: make(chan struct{}, 1)}
	never := make(chan time.Time)
	l := &Loop{
		Source: &fakeSource{target: tgt}, Current: fakeRunning{brain: tgt.BrainImage, ui: tgt.UIImage},
		Applier: &fakeApplier{}, AutoApply: true, OS: sig,
		Now:   func() time.Time { return now },
		After: func(time.Duration) <-chan time.Time { return never },
	}
	l.SetHold(now.Add(protocol.UpdateHoldLease))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(2 * time.Second)
	for l.Snapshot().OS.State == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if st := l.Snapshot().OS.State; st != protocol.OSUpdateInstalled {
		t.Fatalf("first tick: OS state %q, want installed and held", st)
	}
	l.ClearHold()
	select {
	case <-sig.opened:
	case <-time.After(2 * time.Second):
		t.Fatal("clearing the hold did not wake the loop to switch")
	}
}

// A hold is cut to the lease and to the window's end minus the cap.
func TestHold_IsCutToTheLeaseAndTheCap(t *testing.T) {
	now := local(3, 2, 45)
	l, _, _ := osLoop(osTarget(), "0.15.0", DefaultWindow, now)
	l.Now = func() time.Time { return now }

	if got := l.SetHold(now.Add(time.Hour)); !got.Equal(local(3, 2, 55)) {
		t.Fatalf("at 02:45, a hold for an hour ends %v, want 02:55 (the lease)", got)
	}
	now = local(3, 3, 50)
	if got := l.SetHold(local(3, 4, 30)); !got.Equal(local(3, 3, 55)) {
		t.Fatalf("at 03:50, a hold to 04:30 ends %v, want 03:55 (the cap)", got)
	}
	now = local(3, 3, 57)
	if got := l.SetHold(now.Add(5 * time.Minute)); !got.IsZero() {
		t.Fatalf("at 03:57, past the cap, the hold ends %v, want none", got)
	}
	if _, ok := l.Hold(); ok {
		t.Fatal("a hold set past the cap is live")
	}
	if got := l.SetHold(now.Add(-time.Minute)); !got.IsZero() {
		t.Fatalf("a hold in the past ends %v, want none", got)
	}
}

// The cap follows a window from the answer and a window that wraps past
// midnight.
func TestHoldCap(t *testing.T) {
	wrap := Window{Start: 23 * time.Hour, End: 30 * time.Minute}
	if got := HoldCap(wrap, local(3, 23, 30)); !got.Equal(local(4, 0, 25)) {
		t.Fatalf("wrapping window at 23:30: cap %v, want 00:25 the next day", got)
	}
	if got := HoldCap(DefaultWindow, local(3, 14, 0)); !got.Equal(local(4, 3, 55)) {
		t.Fatalf("at 14:00: cap %v, want tomorrow's 03:55", got)
	}
}

// Update now with a live hold does not refuse: the switch waits, the way it
// waits for stream B, and goes when the hold is cleared or ends.
func TestApplyNow_WaitsForTheHold(t *testing.T) {
	for _, how := range []string{"cleared", "expired"} {
		t.Run(how, func(t *testing.T) {
			l, f, _ := osNow(t, true)
			now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
			l.Now = func() time.Time { return now }
			l.SetHold(now.Add(protocol.UpdateHoldLease))

			id, err := l.ApplyNow()
			if err != nil || id != "" {
				t.Fatalf("ApplyNow = %q, %v; want accepted with no job yet", id, err)
			}
			if !l.manualPending() || len(f.switches) != 0 {
				t.Fatalf("pending %v, switches %d; want the switch waiting", l.manualPending(), len(f.switches))
			}
			if s := l.Snapshot().OS; s.State != protocol.OSUpdateWaiting || s.Detail != SwitchWaitsForApps {
				t.Fatalf("OS snapshot = %+v, want waiting for the apps", s)
			}
			l.manualStep()
			if len(f.switches) != 0 {
				t.Fatal("the switch went while the hold was live")
			}
			if how == "cleared" {
				l.ClearHold()
			} else {
				now = now.Add(protocol.UpdateHoldLease + time.Second)
			}
			l.manualStep()
			if l.manualPending() || len(f.switches) != 1 {
				t.Fatalf("after the hold %s: pending %v, switches %d; want one switch", how, l.manualPending(), len(f.switches))
			}
		})
	}
}

// TestControlPlaneDone covers each answer the read gives about stream B
// tonight.
func TestControlPlaneDone(t *testing.T) {
	tonight := time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local)
	run := func(src Source, running RunningPair, auto bool, tick time.Time) (*Loop, *fakeApplier, *time.Time) {
		ap := &fakeApplier{running: map[string]bool{}}
		now := tick
		l := newLoop(src, running, ap, &now)
		l.AutoApply = auto
		l.Tick(context.Background())
		return l, ap, &now
	}
	isDone := func(t *testing.T, l *Loop, want bool) {
		t.Helper()
		night, ok := l.ControlPlaneDone()
		if ok != want || (ok && !night.Equal(tonight)) {
			t.Fatalf("ControlPlaneDone = %v, %v; want %v for %v", night, ok, want, tonight)
		}
	}
	old := fakeRunning{brain: oldBrain, ui: oldUI}
	current := fakeRunning{brain: brainRef, ui: uiRef}

	t.Run("current", func(t *testing.T) {
		l, _, _ := run(&fakeSource{target: goodTarget()}, current, true, at(12, 3, 15))
		isDone(t, l, true)
	})
	t.Run("no target", func(t *testing.T) {
		l, _, _ := run(&fakeSource{err: ErrNoTarget}, old, true, at(12, 3, 15))
		isDone(t, l, true)
	})
	t.Run("unreachable", func(t *testing.T) {
		l, _, _ := run(&fakeSource{err: errors.New("down")}, old, true, at(12, 3, 15))
		isDone(t, l, true)
	})
	t.Run("appliance", func(t *testing.T) {
		l, ap, _ := run(&fakeSource{target: goodTarget()}, old, false, at(12, 3, 15))
		if len(ap.calls) != 0 {
			t.Fatal("the appliance applied in the window")
		}
		isDone(t, l, true)
	})
	t.Run("an update running, then failed", func(t *testing.T) {
		ap := &fakeApplier{running: map[string]bool{"j_1": true}}
		now := at(12, 3, 15)
		l := newLoop(&fakeSource{target: goodTarget()}, old, ap, &now)
		l.Tick(context.Background())
		if len(ap.calls) != 1 {
			t.Fatalf("calls = %d, want the update started", len(ap.calls))
		}
		isDone(t, l, false)
		// The job ended and put the box back: tonight's one attempt is over.
		ap.running["j_1"] = false
		isDone(t, l, true)
	})
	t.Run("not yet checked in this window", func(t *testing.T) {
		l, _, now := run(&fakeSource{target: goodTarget()}, current, true, at(12, 2, 50))
		*now = at(12, 3, 5)
		isDone(t, l, false)
	})
	t.Run("outside the window", func(t *testing.T) {
		l, _, now := run(&fakeSource{target: goodTarget()}, current, true, at(12, 3, 15))
		*now = at(12, 4, 5)
		isDone(t, l, false)
	})
	t.Run("checked last night", func(t *testing.T) {
		l, _, now := run(&fakeSource{target: goodTarget()}, current, true, at(11, 3, 15))
		*now = at(12, 3, 5)
		isDone(t, l, false)
	})
}

// After stream B was done tonight, a target that shows up while the brain
// holds the window for app updates waits for the next night: the
// control-plane update would restart the brain in the middle of an app
// update. Without a hold it applies as before.
func TestHold_LateTargetWaitsWhileAppsUpdate(t *testing.T) {
	for _, hold := range []bool{true, false} {
		ap := &fakeApplier{running: map[string]bool{}}
		now := at(12, 3, 2)
		src := &fakeSource{target: goodTarget()}
		l := newLoop(src, fakeRunning{brain: brainRef, ui: uiRef}, ap, &now)
		l.Tick(context.Background()) // current: stream B is done
		if hold {
			l.SetHold(now.Add(protocol.UpdateHoldLease))
		}
		later := goodTarget()
		later.Version, later.BrainImage, later.BrainDigest = "v0.7.1", "ghcr.io/onmoose/brain@"+digest("e"), digest("e")
		src.target = later
		now = at(12, 3, 9)
		l.Tick(context.Background())
		if hold {
			if len(ap.calls) != 0 {
				t.Fatalf("with a hold: the late target was applied")
			}
			if _, ok := l.ControlPlaneDone(); !ok {
				t.Fatal("with a hold: stream B is not done, so the apps would stall")
			}
		} else if len(ap.calls) != 1 {
			t.Fatalf("without a hold: calls = %d, want the late target applied", len(ap.calls))
		}
	}
}

// While a hold is live, Run sleeps no longer than until the hold ends, or
// until the window opens when that is sooner.
func TestHoldWake(t *testing.T) {
	now := local(3, 2, 50)
	l, _, _ := osLoop(osTarget(), "0.15.0", DefaultWindow, now)
	l.Now = func() time.Time { return now }
	if _, ok := l.holdWake(now); ok {
		t.Fatal("no hold, but a wake")
	}
	l.SetHold(now.Add(protocol.UpdateHoldLease))
	if d, ok := l.holdWake(now); !ok || d != 10*time.Minute+time.Second {
		t.Fatalf("at 02:50: wake in %v, want at the window's start, 03:00", d)
	}
	now = local(3, 3, 10)
	l.SetHold(now.Add(4 * time.Minute))
	if d, ok := l.holdWake(now); !ok || d != 4*time.Minute+time.Second {
		t.Fatalf("at 03:10: wake in %v, want at the hold's end", d)
	}
}
