package main

// The automatic restart of an app that stays unresponsive (#598). Driven
// through appProbeDetector.check with a hand-advanced clock and a fake
// lifecycle, one tick a minute like the production health poll.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/store"
)

// fakeRestarter stands in for lifecycle.Manager.BeginRestart. A restart sets
// the main container's StartedAt to the clock, as a real one does, and then
// calls onRun, so a test can make the app recover or not.
type fakeRestarter struct {
	mu      sync.Mutex
	clk     *stepClock
	reader  *fakeContainerReader
	busy    bool
	runErr  error
	onRun   func()
	runCtx  error // ctx.Err() the last run saw
	begins  []time.Time
	refused int
}

func (f *fakeRestarter) BeginRestart(id string) (func(context.Context) error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		f.refused++
		return nil, lifecycle.ErrBusy
	}
	f.begins = append(f.begins, f.clk.now())
	return func(ctx context.Context) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.runCtx = ctx.Err()
		for i := range f.reader.containers {
			if f.reader.containers[i].InstanceID == id {
				f.reader.containers[i].StartedAt = f.clk.now()
			}
		}
		if f.onRun != nil {
			f.onRun()
		}
		return f.runErr
	}, nil
}

func (f *fakeRestarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.begins)
}

type restartHarness struct {
	clk    *stepClock
	d      *appProbeDetector
	rt     *stubRoundTripper
	lister *fakeInstanceLister
	fr     *fakeRestarter
	es     *fakeEventStore
	r      *autoRestarter
}

// newRestartHarness is one app ("app", probe on /healthz with a 1s start
// period, started an hour ago) answering 503, with the automatic restart on.
func newRestartHarness(t *testing.T) *restartHarness {
	t.Helper()
	clk := &stepClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, time.Second, time.Hour)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, es, _ := newTestProbeDetector(lister, loader, reader, rt, clk)
	mgr.SetClock(clk.now)
	fr := &fakeRestarter{clk: clk, reader: reader}
	r := newAutoRestarter(fr, d.auditor)
	d.SetRestarter(r)
	return &restartHarness{clk: clk, d: d, rt: rt, lister: lister, fr: fr, es: es, r: r}
}

// tick runs one health tick, waits for any restart it started, then moves
// the clock a minute on.
func (h *restartHarness) tick() {
	h.d.check(context.Background())
	h.r.wg.Wait()
	h.clk.add(time.Minute)
}

func (h *restartHarness) issueDetails(t *testing.T) (string, bool) {
	t.Helper()
	iss, ok := h.d.healthMgr.Get("app-unresponsive", "app")
	return iss.Details, ok
}

// An app that stays unresponsive for 5 minutes is restarted once, recovers,
// and the restart is in the audit log naming the app.
func TestAutoRestart_RestartsAfterWindowAndRecovers(t *testing.T) {
	h := newRestartHarness(t)
	h.lister.instances[0].Name = "My App"
	h.fr.onRun = func() { h.rt.status["app.local"] = 200 }

	// t=0 first bad, t=1 raised. The window runs from the raise.
	for i := 0; i < 6; i++ {
		h.tick() // t=0..5
	}
	if n := h.fr.count(); n != 0 {
		t.Fatalf("restarted %d times before the 5-minute window", n)
	}
	h.tick() // t=6: raised 5 minutes ago
	if n := h.fr.count(); n != 1 {
		t.Fatalf("restarts after the window = %d, want 1", n)
	}
	h.tick() // t=7: the app answers again
	if _, ok := h.issueDetails(t); ok {
		t.Fatal("app-unresponsive still raised after the app recovered")
	}
	for i := 0; i < 30; i++ {
		h.tick()
	}
	if n := h.fr.count(); n != 1 {
		t.Fatalf("restarts = %d, want exactly 1", n)
	}
	var rec *store.AuditEvent
	for i := range h.es.events {
		if h.es.events[i].Action == audit.ActionAppAutoRestart {
			if rec != nil {
				t.Fatal("more than one auto-restart audit record")
			}
			rec = &h.es.events[i]
		}
	}
	if rec == nil {
		t.Fatal("no app.restart.auto audit record")
	}
	if !rec.Success || rec.TargetKind != "app" || rec.TargetID != "app" || rec.ActorRole != "system" {
		t.Fatalf("audit record = %+v, want success, target app/app, system actor", *rec)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(rec.Metadata), &meta); err != nil {
		t.Fatalf("metadata %q: %v", rec.Metadata, err)
	}
	if meta["name"] != "My App" || meta["reason"] != "app-unresponsive" {
		t.Fatalf("metadata = %v, want the app's name and the reason", meta)
	}
}

// An app that stays broken is restarted with growing waits (5, 10, 20
// minutes), at most 3 times in any hour, and the issue says it stopped
// trying.
func TestAutoRestart_BoundedWithBackoff(t *testing.T) {
	h := newRestartHarness(t)
	start := h.clk.now()
	sawGaveUp := false
	for i := 0; i < 180; i++ {
		h.tick()
		if d, ok := h.issueDetails(t); ok && strings.Contains(d, "stopped trying") {
			sawGaveUp = true
		}
	}
	var mins []int
	for _, b := range h.fr.begins {
		mins = append(mins, int(b.Sub(start)/time.Minute))
	}
	if len(mins) < 4 || mins[0] != 6 || mins[1] != 17 || mins[2] != 38 {
		t.Fatalf("restart minutes = %v, want the first three at 6, 17, 38", mins)
	}
	// The limit is per rolling hour: the 4th restart waits until the 1st is
	// an hour old (minute 66), and its wait is the one for 2 restarts in the
	// hour (20 minutes since the restart at 38, long over).
	if mins[3] != 66 {
		t.Fatalf("restart minutes = %v, want the 4th at 66", mins)
	}
	for i, b := range h.fr.begins {
		n := 0
		for _, o := range h.fr.begins[:i+1] {
			if b.Sub(o) < time.Hour {
				n++
			}
		}
		if n > 3 {
			t.Fatalf("restart at minute %d is the %dth in an hour: %v", mins[i], n, mins)
		}
	}
	if !sawGaveUp {
		t.Fatal("the issue never said automatic restarts stopped")
	}
	if _, ok := h.issueDetails(t); !ok {
		t.Fatal("the issue is not raised for an app that stays broken")
	}
}

// The details name the last automatic restart.
func TestAutoRestart_DetailsNameLastRestart(t *testing.T) {
	h := newRestartHarness(t)
	for i := 0; i < 9; i++ {
		h.tick() // restart at t=6 (12:06 UTC), raised again by t=8
	}
	d, ok := h.issueDetails(t)
	if !ok || !strings.Contains(d, "restarted it automatically at 12:06 UTC") {
		t.Fatalf("details = %q (raised %v), want the 12:06 UTC restart", d, ok)
	}
}

// A stopped or failed app, and an app with the setting off, are never
// restarted.
func TestAutoRestart_NeverForStoppedFailedOrOptedOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*store.Instance)
	}{
		{"stopped", func(i *store.Instance) { i.State = "stopped" }},
		{"failed", func(i *store.Instance) { i.State = "failed" }},
		{"setting off", func(i *store.Instance) { i.AutoRestartOff = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestartHarness(t)
			tc.set(&h.lister.instances[0])
			for i := 0; i < 90; i++ {
				h.tick()
			}
			if n := h.fr.count(); n != 0 {
				t.Fatalf("restarted %d times", n)
			}
			if _, ok := h.issueDetails(t); !ok {
				t.Fatal("the issue should stay raised")
			}
		})
	}
}

// An app with a lifecycle operation in flight is skipped, and the skip does
// not use up a restart: once the operation ends, the restart happens.
func TestAutoRestart_SkipsBusyApp(t *testing.T) {
	h := newRestartHarness(t)
	h.fr.busy = true
	for i := 0; i < 10; i++ {
		h.tick()
	}
	if h.fr.count() != 0 || h.fr.refused == 0 {
		t.Fatalf("begins = %d, refused = %d; want 0 begins and some refusals", h.fr.count(), h.fr.refused)
	}
	h.fr.busy = false
	h.tick()
	if n := h.fr.count(); n != 1 {
		t.Fatalf("restarts after the operation ended = %d, want 1", n)
	}
}

// A tick that clears the issue starts the 5-minute wait again: only an issue
// that stayed raised the whole time leads to a restart.
func TestAutoRestart_ClearStartsTheWaitAgain(t *testing.T) {
	h := newRestartHarness(t)
	for i := 0; i < 4; i++ {
		h.tick() // t=0..3, raised at t=1
	}
	h.rt.status["app.local"] = 200
	h.tick() // t=4 clears
	h.rt.status["app.local"] = 503
	for i := 0; i < 6; i++ {
		h.tick() // t=5..10: bad at 5, raised at 6
	}
	if n := h.fr.count(); n != 0 {
		t.Fatalf("restarted %d times; the clear at t=4 should restart the wait", n)
	}
	h.tick() // t=11: raised 5 minutes ago
	if n := h.fr.count(); n != 1 {
		t.Fatalf("restarts = %d, want 1 at t=11", n)
	}
}

// A restart that fails is audited as a failure.
func TestAutoRestart_FailureIsAudited(t *testing.T) {
	h := newRestartHarness(t)
	h.fr.runErr = errors.New("compose up failed")
	for i := 0; i < 7; i++ {
		h.tick()
	}
	found := false
	for _, e := range h.es.events {
		if e.Action == audit.ActionAppAutoRestart {
			found = true
			if e.Success {
				t.Fatalf("failed restart audited as success: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("no app.restart.auto audit record for the failed restart")
	}
}

// A brain that is stopping does not cancel a restart in flight: a cancelled
// compose up would park the app in failed, which nothing brings back.
func TestAutoRestart_ShutdownDoesNotCancelRestart(t *testing.T) {
	h := newRestartHarness(t)
	for i := 0; i < 6; i++ {
		h.tick()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.d.check(ctx)
	h.r.wg.Wait()
	if n := h.fr.count(); n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}
	if h.fr.runCtx != nil {
		t.Fatalf("the restart ran on a cancelled context: %v", h.fr.runCtx)
	}
}

// An app with no health_probe, flagged by Docker's own "unhealthy", is
// restarted on the same rule; its wait counts from its container's start,
// so it starts again after each restart.
func TestAutoRestart_DockerOnlySignal(t *testing.T) {
	h := newRestartHarness(t)
	man := h.d.manifests.(*fakeManifestLoader).m["app"]
	man.HealthProbe = nil
	reader := h.fr.reader
	reader.containers[0].Health = dockerUnhealthy
	start := h.clk.now()
	for i := 0; i < 30; i++ {
		h.tick()
	}
	var mins []int
	for _, b := range h.fr.begins {
		mins = append(mins, int(b.Sub(start)/time.Minute))
	}
	// Raised at t=1: first at 6, then 10 minutes after that container's start.
	if len(mins) < 2 || mins[0] != 6 || mins[1] != 16 {
		t.Fatalf("restart minutes = %v, want 6 then 16", mins)
	}
}

// A long start-period grace holds the wait back after a restart: the clock
// starts only once the new container is past its grace.
func TestAutoRestart_LongStartGraceDelaysNextRestart(t *testing.T) {
	h := newRestartHarness(t)
	man := h.d.manifests.(*fakeManifestLoader).m["app"]
	man.HealthProbe.StartPeriod = 10 * time.Minute
	// Docker says unhealthy too, so the issue stays raised through the grace
	// (the probe itself is skipped inside it).
	h.fr.reader.containers[0].Health = dockerUnhealthy
	start := h.clk.now()
	for i := 0; i < 40; i++ {
		h.tick()
	}
	var mins []int
	for _, b := range h.fr.begins {
		mins = append(mins, int(b.Sub(start)/time.Minute))
	}
	// First at 6 (the old container is long past its grace). The second waits
	// for the 10-minute grace of the new container, then the 10-minute wait.
	if len(mins) < 2 || mins[0] != 6 || mins[1] != 26 {
		t.Fatalf("restart minutes = %v, want 6 then 26", mins)
	}
}
