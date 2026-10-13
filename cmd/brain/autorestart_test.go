package main

// The automatic restart of an app that stays unresponsive (#598), and the
// brain giving up on it (#620). Driven through appProbeDetector.check with a
// hand-advanced clock, a fake lifecycle and a real store, one tick a minute
// like the production health poll.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/store"
)

// fakeRestarter stands in for lifecycle.Manager.BeginRestart. Like the real
// one it counts the restart in the app's row (the real store) before it
// returns. A restart sets the main container's StartedAt to the clock, as a
// real one does, and then calls onRun, so a test can make the app recover or
// not.
type fakeRestarter struct {
	mu      sync.Mutex
	clk     *stepClock
	st      *store.Store
	reader  *fakeContainerReader
	busy    bool
	runErr  error
	onRun   func()
	runCtx  error // ctx.Err() the last run saw
	begins  []time.Time
	refused int
	// beforeBegin runs once inside the next BeginRestart, before the row is
	// read: a person acting between the tick's list and the restart.
	beforeBegin func()
}

func (f *fakeRestarter) BeginRestart(id string, at time.Time) (func(context.Context) error, store.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		f.refused++
		return nil, store.Instance{}, lifecycle.ErrBusy
	}
	if f.beforeBegin != nil {
		f.beforeBegin()
		f.beforeBegin = nil
	}
	row, err := f.st.Get(id)
	if err != nil {
		return nil, store.Instance{}, err
	}
	if err := f.st.RecordInstanceAutoRestart(id, at); err != nil {
		return nil, store.Instance{}, err
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
	}, row, nil
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
	st     *store.Store
	loader *fakeManifestLoader
	reader *fakeContainerReader
	fr     *fakeRestarter
	es     *fakeEventStore
	r      *autoRestarter
}

// newRestartHarness is one app ("app", probe on /healthz with a 1s start
// period, started an hour ago) answering 503, with the automatic restart on.
// The app's row is in a real store, the one the detector lists and the
// restart count lives in.
func newRestartHarness(t *testing.T) *restartHarness {
	t.Helper()
	clk := &stepClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	_, loader, reader := probeScenario(clk, "/healthz", nil, time.Second, time.Hour)
	st, err := store.Open(filepath.Join(t.TempDir(), "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Create(store.Instance{
		ID: "app", ManifestID: "app", Name: "My App", Slug: "app", MDNSName: "app.local",
		Version: "1", State: "running", OwnerUserID: "u_owner", Scope: store.ScopeHousehold,
		CreatedAt: clk.now(),
	}); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	h := &restartHarness{clk: clk, rt: rt, st: st, loader: loader, reader: reader}
	h.fr = &fakeRestarter{clk: clk, st: st, reader: reader}
	h.brainStarts()
	return h
}

// brainStarts wires a new detector and restarter on the same store, Docker
// and app, as a brain process that starts does: the health issues and the
// restart history in memory are gone, the rows are not.
func (h *restartHarness) brainStarts() {
	d, mgr, es, _ := newTestProbeDetector(h.st, h.loader, h.reader, h.rt, h.clk)
	mgr.SetClock(h.clk.now)
	if h.es != nil {
		d.auditor = audit.New(h.es) // one audit log across brain restarts
	} else {
		h.es = es
	}
	h.r = newAutoRestarter(h.fr, h.st, d.auditor)
	d.SetRestarter(h.r)
	h.d = d
}

func (h *restartHarness) row(t *testing.T) store.Instance {
	t.Helper()
	row, err := h.st.Get("app")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	return row
}

// restartMinutes is when each restart began, in minutes from from.
func (h *restartHarness) restartMinutes(from time.Time) []int {
	h.fr.mu.Lock()
	defer h.fr.mu.Unlock()
	var mins []int
	for _, b := range h.fr.begins {
		mins = append(mins, int(b.Sub(from)/time.Minute))
	}
	return mins
}

func (h *restartHarness) autoRestartRecords() int {
	n := 0
	for _, e := range h.es.events {
		if e.Action == audit.ActionAppAutoRestart {
			n++
		}
	}
	return n
}

// giveUp runs the app broken until the brain made its 3 restarts and gave
// up: restarts at minutes 6, 17 and 38, and the issue raised again after the
// third.
func (h *restartHarness) giveUp(t *testing.T) {
	t.Helper()
	start := h.clk.now()
	for i := 0; i < 45; i++ {
		h.tick()
	}
	if mins := h.restartMinutes(start); len(mins) != 3 || mins[0] != 6 || mins[1] != 17 || mins[2] != 38 {
		t.Fatalf("restart minutes = %v, want 6, 17, 38", mins)
	}
	d, ok := h.issueDetails(t)
	if !ok || !strings.Contains(d, givenUpText) {
		t.Fatalf("details = %q (raised %v), want that moose gave up", d, ok)
	}
	if n := h.row(t).AutoRestarts; n != 3 {
		t.Fatalf("stored restarts = %d, want 3", n)
	}
}

// givenUpText is part of the details once the brain gave up.
const givenUpText = "so moose will not restart it again by itself. To try again, stop the app and start it."

// noRestartFor runs n ticks and fails if any restart begins.
func (h *restartHarness) noRestartFor(t *testing.T, n int) {
	t.Helper()
	before := h.fr.count()
	for i := 0; i < n; i++ {
		h.tick()
	}
	if got := h.fr.count(); got != before {
		t.Fatalf("%d restarts in %d ticks, want none", got-before, n)
	}
}

// fakeCaddyNets says Caddy is off the app's network, so the probe has no
// sample (#613).
type fakeCaddyNets struct{ off bool }

func (f *fakeCaddyNets) CaddyOffNetwork(string) bool { return f.off }

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
	if n := h.row(t).AutoRestarts; n != 0 {
		t.Fatalf("stored restarts = %d after the app answered again, want 0", n)
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
// minutes), 3 times, and then never again (#620): not after the hour, not in
// the next hours. The issue says moose gave up and what the user can do, and
// no audit record is written while it has given up.
func TestAutoRestart_GivesUpAfterThreeFailedRestarts(t *testing.T) {
	h := newRestartHarness(t)
	start := h.clk.now()
	h.giveUp(t)
	if d, _ := h.issueDetails(t); !strings.Contains(d, "moose restarted it 3 times, the last time at 12:38 UTC.") {
		t.Fatalf("details = %q, want the count and the last restart", d)
	}
	h.noRestartFor(t, 5*60)
	if mins := h.restartMinutes(start); len(mins) != 3 {
		t.Fatalf("restart minutes = %v, want only 3", mins)
	}
	if n := h.autoRestartRecords(); n != 3 {
		t.Fatalf("app.restart.auto records = %d, want 3 (none while given up)", n)
	}
	d, ok := h.issueDetails(t)
	if !ok || !strings.Contains(d, givenUpText) {
		t.Fatalf("details after 5 hours = %q (raised %v), want that moose gave up", d, ok)
	}
}

// Given up survives a brain restart (#620): the count is in the row, so a new
// brain process does not give the app a fresh hour.
func TestAutoRestart_GivenUpSurvivesBrainRestart(t *testing.T) {
	h := newRestartHarness(t)
	h.giveUp(t)
	h.brainStarts()
	h.noRestartFor(t, 3*60)
	d, ok := h.issueDetails(t)
	if !ok || !strings.Contains(d, givenUpText) {
		t.Fatalf("details after the brain restart = %q (raised %v), want that moose gave up", d, ok)
	}
}

// A brain restart in the middle keeps the backoff: after 2 restarts the next
// wait is 20 minutes, not 5.
func TestAutoRestart_BrainRestartKeepsTheBackoff(t *testing.T) {
	h := newRestartHarness(t)
	for i := 0; i < 20; i++ {
		h.tick() // restarts at 6 and 17
	}
	if n := h.fr.count(); n != 2 {
		t.Fatalf("restarts before the brain restart = %d, want 2", n)
	}
	h.brainStarts()
	start := h.clk.now()
	for i := 0; i < 30; i++ {
		h.tick() // bad at 0, raised at 1, 20 minutes later
	}
	mins := h.restartMinutes(start)
	if len(mins) != 3 || mins[2] != 21 {
		t.Fatalf("restart minutes after the brain restart = %v, want the 3rd at 21", mins)
	}
}

// A real healthy answer ends giving up: the next time the app stops
// answering, the brain starts again from the 5-minute wait.
func TestAutoRestart_RealRecoveryEndsGivingUp(t *testing.T) {
	h := newRestartHarness(t)
	h.giveUp(t)
	h.noRestartFor(t, 60) // the restarts are more than an hour old after this
	h.rt.status["app.local"] = 200
	h.tick()
	if _, ok := h.issueDetails(t); ok {
		t.Fatal("app-unresponsive still raised after the app answered")
	}
	if n := h.row(t).AutoRestarts; n != 0 {
		t.Fatalf("stored restarts = %d after a healthy answer, want 0", n)
	}
	h.rt.status["app.local"] = 503
	before := h.fr.count()
	for i := 0; i < 7; i++ {
		h.tick() // bad at 0, raised at 1, restarted at 6
	}
	if n := h.fr.count() - before; n != 1 {
		t.Fatalf("restarts after the app broke again = %d, want 1", n)
	}
}

// A tick with no sample clears the issue but does not end giving up: Caddy
// off the app's network, or the app's container not running.
func TestAutoRestart_NoSampleDoesNotEndGivingUp(t *testing.T) {
	h := newRestartHarness(t)
	h.giveUp(t)
	nets := &fakeCaddyNets{off: true}
	h.d.SetCaddyNetworks(nets)
	h.tick()
	if _, ok := h.issueDetails(t); ok {
		t.Fatal("a tick with no sample should clear the issue")
	}
	nets.off = false
	h.reader.containers[0].Running = false
	h.tick()
	h.reader.containers[0].Running = true
	if n := h.row(t).AutoRestarts; n != 3 {
		t.Fatalf("stored restarts = %d after ticks with no sample, want 3", n)
	}
	h.noRestartFor(t, 2*60)
	if d, ok := h.issueDetails(t); !ok || !strings.Contains(d, givenUpText) {
		t.Fatalf("details = %q (raised %v), want that moose gave up", d, ok)
	}
}

// A person's Start ends giving up: Stop then Start, as lifecycle.Manager.Start
// does it to the row (it resets the count, TestStartResetsAutoRestarts).
func TestAutoRestart_ManualStartEndsGivingUp(t *testing.T) {
	h := newRestartHarness(t)
	h.giveUp(t)
	// Stop: the row reads stopped and the container is down.
	if err := h.st.SetState("app", "stopped"); err != nil {
		t.Fatal(err)
	}
	h.reader.containers[0].Running = false
	h.noRestartFor(t, 3)
	if n := h.row(t).AutoRestarts; n != 3 {
		t.Fatalf("stored restarts = %d after Stop, want 3 (Stop alone does not reset)", n)
	}
	// Start: the count is reset, the row reads running, a new container.
	if err := h.st.ResetInstanceAutoRestarts("app", true); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetState("app", "running"); err != nil {
		t.Fatal(err)
	}
	h.reader.containers[0].Running = true
	h.reader.containers[0].StartedAt = h.clk.now()
	before := h.fr.count()
	for i := 0; i < 8; i++ {
		// Still broken: inside the start grace at 0, bad at 1, raised at 2,
		// restarted at 7.
		h.tick()
	}
	if n := h.fr.count() - before; n != 1 {
		t.Fatalf("restarts after the person started the app = %d, want 1", n)
	}
}

// Turning the switch off and on again ends giving up. The real store does the
// reset, the same call the API makes.
func TestAutoRestart_ToggleEndsGivingUp(t *testing.T) {
	h := newRestartHarness(t)
	h.giveUp(t)
	if err := h.st.SetInstanceAutoRestart("app", false); err != nil {
		t.Fatal(err)
	}
	h.noRestartFor(t, 10)
	if err := h.st.SetInstanceAutoRestart("app", true); err != nil {
		t.Fatal(err)
	}
	// The issue stayed raised the whole time, so the 5-minute wait is long
	// over: the restart comes on the next tick.
	before := h.fr.count()
	h.tick()
	if n := h.fr.count() - before; n != 1 {
		t.Fatalf("restarts after the switch went off and on = %d, want 1", n)
	}
	if d, _ := h.issueDetails(t); strings.Contains(d, givenUpText) {
		t.Fatalf("details = %q, still say moose gave up", d)
	}
}

// An app that recovers after each restart and breaks again soon after is
// never given up on, since each real healthy answer resets the stored count,
// but it is still restarted at most 3 times in any hour (#598).
func TestAutoRestart_RecoveringAppCappedPerHour(t *testing.T) {
	h := newRestartHarness(t)
	recoveredAt := -1
	minute := 0
	h.fr.onRun = func() { h.rt.status["app.local"] = 200; recoveredAt = minute + 2 }
	start := h.clk.now()
	for ; minute < 180; minute++ {
		if minute == recoveredAt {
			h.rt.status["app.local"] = 503 // breaks again a minute after it answered
		}
		h.tick()
	}
	mins := h.restartMinutes(start)
	if len(mins) < 4 {
		t.Fatalf("restart minutes = %v, want restarts to go on for an app that recovers", mins)
	}
	for i := range mins {
		n := 0
		for _, o := range mins[:i+1] {
			if mins[i]-o < 60 {
				n++
			}
		}
		if n > 3 {
			t.Fatalf("restart at minute %d is the %dth in an hour: %v", mins[i], n, mins)
		}
	}
	if d, _ := h.issueDetails(t); strings.Contains(d, givenUpText) {
		t.Fatalf("details = %q, say moose gave up on an app that recovers", d)
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
		set  func(*store.Store) error
	}{
		{"stopped", func(s *store.Store) error { return s.SetState("app", "stopped") }},
		{"failed", func(s *store.Store) error { return s.SetState("app", "failed") }},
		{"setting off", func(s *store.Store) error { return s.SetInstanceAutoRestart("app", false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestartHarness(t)
			if err := tc.set(h.st); err != nil {
				t.Fatal(err)
			}
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
	man := h.loader.m["app"]
	man.HealthProbe = nil
	reader := h.reader
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
	man := h.loader.m["app"]
	man.HealthProbe.StartPeriod = 10 * time.Minute
	// Docker says unhealthy too, so the issue stays raised through the grace
	// (the probe itself is skipped inside it).
	h.reader.containers[0].Health = dockerUnhealthy
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

// A person turns the switch off and on after the tick read the row but
// before the restart begins (#620). The restart is counted as the first of a
// fresh hour, and the restarts from before are forgotten, so the brain goes
// on to the second and third.
func TestAutoRestart_ToggleRacingTheTickGivesAFreshHour(t *testing.T) {
	h := newRestartHarness(t)
	for i := 0; i < 20; i++ {
		h.tick() // restarts at 6 and 17
	}
	if n := h.fr.count(); n != 2 {
		t.Fatalf("restarts = %d, want 2", n)
	}
	h.fr.mu.Lock()
	h.fr.beforeBegin = func() {
		if err := h.st.SetInstanceAutoRestart("app", false); err != nil {
			t.Error(err)
		}
		if err := h.st.SetInstanceAutoRestart("app", true); err != nil {
			t.Error(err)
		}
	}
	h.fr.mu.Unlock()
	start := h.clk.now()
	for i := 0; i < 60; i++ {
		h.tick()
	}
	if n := h.row(t).AutoRestarts; n != 3 {
		t.Fatalf("stored restarts = %d, want 3 (one, then two more in the fresh hour)", n)
	}
	// The 3rd restart (20-minute wait, at 38) counts as the 1st of the fresh
	// hour; then waits of 10 and 20 minutes from each restart.
	mins := h.restartMinutes(start)
	if len(mins) != 5 || mins[2] != 18 || mins[3] != 29 || mins[4] != 50 {
		t.Fatalf("restart minutes from 20 = %v, want 18, 29, 50 after the two before", mins)
	}
}
