package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeRestoreCaddy is the slice of Caddy the restorer drives, with the mark as
// a plain flag. calls is shared with fakeRouteRestorer, so a test sees the
// order of every step.
type fakeRestoreCaddy struct {
	calls      *[]string
	marked     bool
	readErr    error
	restoreErr error
	markErr    error
}

func (f *fakeRestoreCaddy) Configured(context.Context) (bool, error) {
	*f.calls = append(*f.calls, "Configured")
	return f.marked, f.readErr
}

func (f *fakeRestoreCaddy) RestoreServer(context.Context) error {
	*f.calls = append(*f.calls, "RestoreServer")
	return f.restoreErr
}

func (f *fakeRestoreCaddy) MarkConfigured(context.Context) error {
	*f.calls = append(*f.calls, "MarkConfigured")
	if f.markErr != nil {
		return f.markErr
	}
	f.marked = true
	return nil
}

type fakeRouteRestorer struct {
	calls *[]string
	// asked is the only set of each RestoreRoutes call; nil is every app.
	asked []map[string]bool
	// owed is what the next call returns.
	owed map[string]bool
	err  error
}

func (f *fakeRouteRestorer) RestoreRoutes(_ context.Context, only map[string]bool) (map[string]bool, error) {
	*f.calls = append(*f.calls, "RestoreRoutes")
	f.asked = append(f.asked, only)
	return f.owed, f.err
}

type restoreHarness struct {
	r       *caddyRestorer
	caddy   *fakeRestoreCaddy
	routes  *fakeRouteRestorer
	calls   []string
	edgeErr error
}

func newRestoreHarness() *restoreHarness {
	h := &restoreHarness{}
	h.caddy = &fakeRestoreCaddy{calls: &h.calls, marked: true}
	h.routes = &fakeRouteRestorer{calls: &h.calls, owed: map[string]bool{}}
	h.r = &caddyRestorer{caddy: h.caddy, life: h.routes, edge: func(context.Context) error {
		h.calls = append(h.calls, "edge")
		return h.edgeErr
	}}
	h.r.armed.Store(true)
	return h
}

// tick runs one health tick and returns the steps it took.
func (h *restoreHarness) tick() []string {
	h.calls = nil
	h.r.check(context.Background())
	return h.calls
}

func wantSteps(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(want) == 0 {
		want = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}

// Until the startup writes are done, the tick does nothing: the startup is
// writing the config and the mark is not there yet.
func TestCaddyRestoreWaitsForStartup(t *testing.T) {
	h := newRestoreHarness()
	h.r.armed.Store(false)
	h.caddy.marked = false
	wantSteps(t, h.tick())
}

// A Caddy that holds the mark costs one read per tick and nothing else.
func TestCaddyRestoreNothingLost(t *testing.T) {
	h := newRestoreHarness()
	wantSteps(t, h.tick(), "Configured")
	wantSteps(t, h.tick(), "Configured")
}

// A Caddy that lost the mark gets its config back in the startup's order,
// with the mark last, and the next tick sees nothing to do.
func TestCaddyRestoreWritesTheConfigAgain(t *testing.T) {
	h := newRestoreHarness()
	h.caddy.marked = false
	wantSteps(t, h.tick(), "Configured", "RestoreServer", "edge", "RestoreRoutes", "MarkConfigured")
	if h.routes.asked[0] != nil {
		t.Errorf("the first restore asked for %v, want every app (nil)", h.routes.asked[0])
	}
	wantSteps(t, h.tick(), "Configured")
}

// The apps RestoreRoutes could not write are asked for again on each tick,
// alone, until none is left. The mark goes on at once, so the owed apps do
// not hold up the rest.
func TestCaddyRestoreRetriesOwedApps(t *testing.T) {
	h := newRestoreHarness()
	h.caddy.marked = false
	h.routes.owed = map[string]bool{"busy": true}
	wantSteps(t, h.tick(), "Configured", "RestoreServer", "edge", "RestoreRoutes", "MarkConfigured")
	if !h.caddy.marked {
		t.Fatal("no mark while an app is owed; a second recreate would not be seen")
	}
	wantSteps(t, h.tick(), "Configured", "RestoreRoutes")
	if got := h.routes.asked[1]; !reflect.DeepEqual(got, map[string]bool{"busy": true}) {
		t.Fatalf("the retry asked for %v, want only the owed app", got)
	}
	h.routes.owed = map[string]bool{}
	wantSteps(t, h.tick(), "Configured", "RestoreRoutes")
	wantSteps(t, h.tick(), "Configured")
}

// A second loss while apps are owed starts over with every app.
func TestCaddyRestoreSecondLossStartsOver(t *testing.T) {
	h := newRestoreHarness()
	h.caddy.marked = false
	h.routes.owed = map[string]bool{"busy": true}
	h.tick()
	h.caddy.marked = false
	wantSteps(t, h.tick(), "Configured", "RestoreServer", "edge", "RestoreRoutes", "MarkConfigured")
	if got := h.routes.asked[len(h.routes.asked)-1]; got != nil {
		t.Fatalf("the second restore asked for %v, want every app (nil)", got)
	}
}

// An unclear answer from Caddy changes nothing.
func TestCaddyRestoreUnclearAnswer(t *testing.T) {
	h := newRestoreHarness()
	h.caddy.marked = false
	h.caddy.readErr = errors.New("caddy admin unreachable")
	wantSteps(t, h.tick(), "Configured")
}

// A failed step stops the restore with no mark, so the next tick does it all
// again; the routes are not written ahead of the server parts they need.
func TestCaddyRestoreFailedStepTriesAgain(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		set  func(h *restoreHarness)
		want []string
	}{
		{"server", func(h *restoreHarness) { h.caddy.restoreErr = boom }, []string{"Configured", "RestoreServer"}},
		{"edge", func(h *restoreHarness) { h.edgeErr = boom }, []string{"Configured", "RestoreServer", "edge"}},
		{"routes", func(h *restoreHarness) { h.routes.err = boom }, []string{"Configured", "RestoreServer", "edge", "RestoreRoutes"}},
		{"mark", func(h *restoreHarness) { h.caddy.markErr = boom }, []string{"Configured", "RestoreServer", "edge", "RestoreRoutes", "MarkConfigured"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRestoreHarness()
			h.caddy.marked = false
			tc.set(h)
			wantSteps(t, h.tick(), tc.want...)
			if h.caddy.marked {
				t.Fatal("marked after a failed step")
			}
			h.caddy.restoreErr, h.edgeErr, h.routes.err, h.caddy.markErr = nil, nil, nil, nil
			wantSteps(t, h.tick(), "Configured", "RestoreServer", "edge", "RestoreRoutes", "MarkConfigured")
		})
	}
}
