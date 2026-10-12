package updatetarget

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Update now applies the target the last tick checked, outside the window,
// and hands over the target's version for the outcome record (#488).
func TestApplyNow_AppliesTheCheckedTargetOutsideTheWindow(t *testing.T) {
	ap := &fakeApplier{}
	now := at(12, 14, 0)
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("the tick at 14:00 applied %v, want it held for the window", ap.calls)
	}

	id, err := l.ApplyNow()
	if err != nil {
		t.Fatalf("ApplyNow: %v", err)
	}
	if id != "j_1" || len(ap.calls) != 1 || ap.calls[0] != [2]string{brainRef, uiRef} || ap.versions[0] != "v0.7.0" {
		t.Fatalf("ApplyNow started %v %v (id %q), want the checked target with its version", ap.calls, ap.versions, id)
	}
}

// A manual apply counts as the attempt for the coming window: a failed update
// at noon is not tried again by the window that night. The night after is.
func TestApplyNow_CountsAsTonightsAttempt(t *testing.T) {
	ap := &fakeApplier{}
	now := at(12, 14, 0)
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())
	if _, err := l.ApplyNow(); err != nil {
		t.Fatalf("ApplyNow: %v", err)
	}
	// The update failed and put the box back: the running pair is still old.
	now = at(13, 3, 15)
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("the window after a manual apply started %d updates, want 0 more", len(ap.calls)-1)
	}
	now = at(14, 3, 15)
	l.Tick(context.Background())
	if len(ap.calls) != 2 {
		t.Fatalf("the next night started %d updates in all, want a retry", len(ap.calls))
	}
}

// A manual apply ignores the one-attempt-per-night guard: an admin who presses
// Update now after a failed night is choosing to retry.
func TestApplyNow_BypassesTheNightGuard(t *testing.T) {
	ap := &fakeApplier{}
	now := at(12, 3, 15)
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background()) // the window's attempt, which "failed"
	now = at(12, 3, 30)
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("the window tried %d times, want once", len(ap.calls))
	}
	if _, err := l.ApplyNow(); err != nil {
		t.Fatalf("ApplyNow after a failed night: %v", err)
	}
	if len(ap.calls) != 2 {
		t.Fatalf("ApplyNow did not retry; calls %v", ap.calls)
	}
}

func TestApplyNow_RefusesWhenThereIsNothingToApply(t *testing.T) {
	now := at(12, 14, 0)
	cases := map[string]struct {
		src     Source
		running fakeRunning
		tick    bool
	}{
		"no tick yet":     {src: &fakeSource{target: goodTarget()}, running: fakeRunning{brain: oldBrain, ui: oldUI}},
		"already current": {src: &fakeSource{target: goodTarget()}, running: fakeRunning{brain: brainRef, ui: uiRef}, tick: true},
		"no target":       {src: &fakeSource{err: ErrNoTarget}, running: fakeRunning{brain: oldBrain, ui: oldUI}, tick: true},
		"unreachable":     {src: &fakeSource{err: errors.New("down")}, running: fakeRunning{brain: oldBrain, ui: oldUI}, tick: true},
		"refused":         {src: &fakeSource{target: Target{Version: "v1", BrainImage: "x:latest", UIImage: "y:latest"}}, running: fakeRunning{brain: oldBrain, ui: oldUI}, tick: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ap := &fakeApplier{}
			l := newLoop(c.src, c.running, ap, &now)
			if c.tick {
				l.Tick(context.Background())
			}
			if _, err := l.ApplyNow(); !errors.Is(err, ErrNothingToApply) {
				t.Fatalf("ApplyNow = %v, want ErrNothingToApply", err)
			}
			if len(ap.calls) != 0 {
				t.Fatalf("started %v, want nothing", ap.calls)
			}
		})
	}
}

// The appliance does not apply its own target (# 8.2, parked in #400), so
// there is no Update now on it.
func TestApplyNow_RefusedOnTheAppliance(t *testing.T) {
	ap := &fakeApplier{}
	now := at(12, 14, 0)
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.AutoApply = false
	l.Tick(context.Background())
	if _, err := l.ApplyNow(); !errors.Is(err, ErrManualApplyUnsupported) {
		t.Fatalf("ApplyNow on the appliance = %v, want ErrManualApplyUnsupported", err)
	}
}

// A refusal from the job lock reaches the caller and marks nothing, so the
// window still tries tonight.
func TestApplyNow_JobLockRefusalMarksNothing(t *testing.T) {
	ap := &fakeApplier{err: errors.New("a job is already running")}
	now := at(12, 14, 0)
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())
	if _, err := l.ApplyNow(); err == nil {
		t.Fatal("ApplyNow succeeded, want the lock's refusal")
	}
	ap.err = nil
	now = at(13, 3, 15)
	l.Tick(context.Background())
	if len(ap.calls) != 2 {
		t.Fatalf("calls = %d, want the window to try after a refused manual apply", len(ap.calls))
	}
}

// The tick and ApplyNow share the guard. Run under -race, this is what
// proves the lock.
func TestApplyNow_RacesTheTickSafely(t *testing.T) {
	ap := &lockedApplier{}
	now := at(12, 3, 15)
	l := &Loop{
		Source: &fakeSource{target: goodTarget()}, Current: fakeRunning{brain: oldBrain, ui: oldUI},
		Applier: ap, AutoApply: true, Now: func() time.Time { return now },
	}
	l.Tick(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = l.ApplyNow() }()
		go func() { defer wg.Done(); l.applyControlPlane(goodTarget(), DefaultWindow, oldBrain, oldUI) }()
	}
	wg.Wait()
}

type lockedApplier struct {
	mu sync.Mutex
	n  int
}

func (a *lockedApplier) StartUpdate(string, string, string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return "j", nil
}

func TestNextNight(t *testing.T) {
	w := DefaultWindow
	day := func(d int) time.Time { return time.Date(2026, 8, d, 0, 0, 0, 0, time.Local) }
	for _, c := range []struct {
		t    time.Time
		want time.Time
	}{
		{at(12, 2, 0), day(12)},  // before tonight's window
		{at(12, 3, 30), day(12)}, // inside it
		{at(12, 14, 0), day(13)}, // after it: the next one
	} {
		if got := nextNight(w, c.t); !got.Equal(c.want) {
			t.Errorf("nextNight(%v) = %v, want %v", c.t, got, c.want)
		}
	}
	wrap := Window{Start: 23*time.Hour + 30*time.Minute, End: 30 * time.Minute}
	if got := nextNight(wrap, at(13, 0, 10)); !got.Equal(day(12)) {
		t.Errorf("wrapping window at 00:10: nextNight = %v, want the night it opened", got)
	}
	if got := nextNight(wrap, at(12, 14, 0)); !got.Equal(day(12)) {
		t.Errorf("wrapping window at 14:00: nextNight = %v, want tonight", got)
	}
}

// The zone comes from the /etc/localtime link, so a zone set after host-agent
// started is followed, and it can be named.
func TestZoneFromLink(t *testing.T) {
	t.Setenv("TZ", "")
	dir := t.TempDir()
	link := filepath.Join(dir, "localtime")
	if err := os.Symlink("../usr/share/zoneinfo/Europe/Stockholm", link); err != nil {
		t.Fatal(err)
	}
	loc, name := zoneFromLink(link)
	if name != "Europe/Stockholm" || loc.String() != "Europe/Stockholm" {
		t.Fatalf("zone = %q (%v), want Europe/Stockholm", name, loc)
	}
	if _, name := zoneFromLink(filepath.Join(dir, "missing")); name != "" {
		t.Fatalf("a missing link named %q, want no name", name)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/share/zoneinfo/Not/AZone", link); err != nil {
		t.Fatal(err)
	}
	if loc, name := zoneFromLink(link); name != "" || loc != time.Local {
		t.Fatalf("an unknown zone = %q (%v), want time.Local with no name", name, loc)
	}
	t.Setenv("TZ", "America/New_York")
	if _, name := zoneFromLink(link); name != "America/New_York" {
		t.Fatalf("with TZ set: %q, want America/New_York", name)
	}
}

// The loop reads the clock, and so the window, in the zone Zone names.
func TestNow_IsInTheBoxZone(t *testing.T) {
	ap := &fakeApplier{}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	l := &Loop{
		Source: &fakeSource{target: goodTarget()}, Current: fakeRunning{brain: oldBrain, ui: oldUI},
		Applier: ap, AutoApply: true,
		Zone: func() (*time.Location, string) { return tokyo, "Asia/Tokyo" },
	}
	if got := l.now().Location(); got != tokyo {
		t.Fatalf("now() is in %v, want Asia/Tokyo", got)
	}
}

// On the night the clocks go back, an apply at the second 02:30 counts for
// that night's 03:00 window, not the next one (Greptile on #605).
func TestNextNight_AcrossTheClockChange(t *testing.T) {
	sto, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	// 2026-10-25 01:30 UTC is 02:30 CET, after 03:00 CEST became 02:00 CET.
	second230 := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).In(sto)
	if h, m, _ := second230.Clock(); h != 2 || m != 30 {
		t.Fatalf("setup: %v is not 02:30", second230)
	}
	want := time.Date(2026, 10, 25, 0, 0, 0, 0, sto)
	if got := nextNight(DefaultWindow, second230); !got.Equal(want) {
		t.Fatalf("nextNight = %v, want %v", got, want)
	}
}
