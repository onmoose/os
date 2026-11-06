package lifecycle

// The nightly app updates (#632, nightly.go) against a fake clock and a fake
// host-agent. Most tests stub the update itself and drive rows in a real
// store; the last ones run the real update transaction against the fakes.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// fakeNightHost is host-agent's side: the update-target read, and the hold.
type fakeNightHost struct {
	mu      sync.Mutex
	target  protocol.UpdateTarget
	readErr error
	holdErr error
	holds   []time.Time
	clears  int
	// held, when set, hears of every hold.
	held chan time.Time
}

func (h *fakeNightHost) SystemUpdateTarget(context.Context) (protocol.UpdateTarget, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.target, h.readErr
}

func (h *fakeNightHost) SetUpdateHold(_ context.Context, until time.Time) (time.Time, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holdErr != nil {
		return time.Time{}, h.holdErr
	}
	h.holds = append(h.holds, until)
	if h.held != nil {
		h.held <- until
	}
	return until, nil
}

func (h *fakeNightHost) ClearUpdateHold(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clears++
	return nil
}

func (h *fakeNightHost) done(night string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.target.ControlPlaneDoneNight = &night
}

func (h *fakeNightHost) counts() (holds, clears int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.holds), h.clears
}

func (h *fakeNightHost) lastHold() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.holds) == 0 {
		return time.Time{}
	}
	return h.holds[len(h.holds)-1]
}

func stockholm(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	return loc
}

// nightRig is a nightly loop on a fake clock and a fake host-agent, with the
// update stubbed: it records the ids in order and answers fail[id].
type nightRig struct {
	e     *testEnv
	n     *Nightly
	host  *fakeNightHost
	now   time.Time
	calls []string
	fail  map[string]bool
}

func newNightRig(t *testing.T, zone string) *nightRig {
	t.Helper()
	r := &nightRig{e: newTestEnv(t), fail: map[string]bool{}}
	empty := ""
	r.host = &fakeNightHost{target: protocol.UpdateTarget{
		State: protocol.UpdateTargetCurrent, Window: "03:00-04:00", Zone: zone, ControlPlaneDoneNight: &empty,
	}}
	r.n = r.newLoop()
	return r
}

// newLoop is a fresh loop over the same store and host, as after a brain
// restart.
func (r *nightRig) newLoop() *Nightly {
	n := r.e.m.NewNightly(r.host)
	n.Now = func() time.Time { return r.now }
	n.After = func(time.Duration) <-chan time.Time { return nil } // never fires
	n.check = func(context.Context) {}
	n.update = func(_ context.Context, id string) error {
		r.calls = append(r.calls, id)
		if r.fail[id] {
			row, _ := r.e.store.Get(id)
			if err := r.e.store.RecordInstanceUpdateFailure(id, store.UpdateFailure{Version: row.Update.Version, Kind: store.UpdateFailedHealth, Message: "x"}); err != nil {
				return err
			}
			return errors.New("the new version did not become healthy")
		}
		return r.e.store.FinishInstanceUpdate(id, "2.0", "App "+id)
	}
	return n
}

// app adds a running app with an available update to 2.0, installed at
// minute min of a fixed day, so the install order is known.
func (r *nightRig) app(t *testing.T, id string, min int, edit func(*store.Instance)) {
	t.Helper()
	inst := store.Instance{
		ID: id, ManifestID: "app-" + id, Name: "App " + id, Slug: id, Version: "1.0", State: "running",
		Scope: store.ScopeHousehold, CreatedAt: time.Date(2026, 1, 1, 0, min, 0, 0, time.UTC),
	}
	if edit != nil {
		edit(&inst)
	}
	if err := r.e.store.Create(inst); err != nil {
		t.Fatal(err)
	}
	if err := r.e.store.SetInstanceUpdateCheck(id, store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable}); err != nil {
		t.Fatal(err)
	}
	if inst.AutoUpdateOff {
		if err := r.e.store.SetInstanceAutoUpdate(id, false); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *nightRig) step(at time.Time) bool {
	r.now = at
	return r.n.Step(context.Background())
}

func TestNightly_NothingOutsideTheWindow(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "a", 1, nil)
	r.host.done("2026-11-07")
	for _, at := range []time.Time{
		time.Date(2026, 11, 7, 14, 0, 0, 0, sto),
		time.Date(2026, 11, 7, 2, 44, 0, 0, sto),
		time.Date(2026, 11, 7, 4, 0, 0, 0, sto),
	} {
		if r.step(at) {
			t.Fatalf("at %v: an update ran", at)
		}
	}
	holds, clears := r.host.counts()
	if len(r.calls) != 0 || holds != 0 {
		t.Fatalf("outside the window: %d updates, %d holds; want none", len(r.calls), holds)
	}
	// A brain that starts does not know whether it held the switch, so it
	// clears once, and only once.
	if clears != 1 {
		t.Fatalf("clears = %d, want 1", clears)
	}
}

// The first hold goes at 02:45, and only when an app has work tonight.
func TestNightly_HoldsFromAQuarterBeforeOnlyWithWork(t *testing.T) {
	sto := stockholm(t)
	t.Run("work", func(t *testing.T) {
		r := newNightRig(t, "Europe/Stockholm")
		r.app(t, "a", 1, nil)
		r.step(time.Date(2026, 11, 7, 2, 44, 0, 0, sto))
		if holds, _ := r.host.counts(); holds != 0 {
			t.Fatal("a hold went before 02:45")
		}
		r.step(time.Date(2026, 11, 7, 2, 45, 0, 0, sto))
		if got := r.host.lastHold(); !got.Equal(time.Date(2026, 11, 7, 2, 55, 0, 0, sto)) {
			t.Fatalf("hold until %v, want 02:55 (the lease)", got)
		}
		if len(r.calls) != 0 {
			t.Fatal("an update ran before the window opened")
		}
		r.step(time.Date(2026, 11, 7, 2, 46, 0, 0, sto))
		if holds, _ := r.host.counts(); holds != 2 {
			t.Fatalf("holds = %d, want the hold renewed every wake", holds)
		}
	})
	t.Run("no work", func(t *testing.T) {
		r := newNightRig(t, "Europe/Stockholm")
		r.app(t, "a", 1, func(i *store.Instance) { i.AutoUpdateOff = true })
		r.step(time.Date(2026, 11, 7, 2, 50, 0, 0, sto))
		if holds, _ := r.host.counts(); holds != 0 {
			t.Fatal("a hold went with no work")
		}
	})
}

// No app update starts before host-agent says the control plane is done
// for tonight.
func TestNightly_WaitsForTheControlPlane(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "a", 1, nil)
	r.host.done("2026-11-06") // last night's, not tonight's
	r.step(time.Date(2026, 11, 7, 3, 5, 0, 0, sto))
	if len(r.calls) != 0 {
		t.Fatal("an update ran before the control plane was done")
	}
	if holds, _ := r.host.counts(); holds != 1 {
		t.Fatalf("holds = %d, want the switch held while waiting", holds)
	}
	r.host.done("2026-11-07")
	if !r.step(time.Date(2026, 11, 7, 3, 6, 0, 0, sto)) || !slices.Equal(r.calls, []string{"a"}) {
		t.Fatalf("calls = %v, want a once the control plane is done", r.calls)
	}
}

// One app at a time, oldest install first; then the hold is cleared at once.
func TestNightly_OneAtATimeInOrder(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "c", 3, nil)
	r.app(t, "a", 1, nil)
	r.app(t, "b", 2, nil)
	r.host.done("2026-11-07")
	at := time.Date(2026, 11, 7, 3, 10, 0, 0, sto)
	for i := 1; i <= 3; i++ {
		if !r.step(at) || len(r.calls) != i {
			t.Fatalf("step %d: calls %v, want one more update", i, r.calls)
		}
	}
	if !slices.Equal(r.calls, []string{"a", "b", "c"}) {
		t.Fatalf("order = %v, want oldest install first", r.calls)
	}
	_, before := r.host.counts()
	if r.step(at) {
		t.Fatal("an update ran with no work left")
	}
	if _, clears := r.host.counts(); clears != before+1 {
		t.Fatalf("clears = %d, want the hold cleared once the work is done", clears-before)
	}
}

// No new update starts at or after the cut-off, 20 minutes before the end.
func TestNightly_StopsAtTheCutoff(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "a", 1, nil)
	r.app(t, "b", 2, nil)
	r.host.done("2026-11-07")
	if !r.step(time.Date(2026, 11, 7, 3, 39, 0, 0, sto)) {
		t.Fatal("no update at 03:39")
	}
	if r.step(time.Date(2026, 11, 7, 3, 40, 0, 0, sto)) || len(r.calls) != 1 {
		t.Fatalf("calls = %v, want none after the cut-off", r.calls)
	}
	if _, clears := r.host.counts(); clears == 0 {
		t.Fatal("the hold was not cleared at the cut-off")
	}
	// What was left goes the next night.
	r.host.done("2026-11-08")
	if !r.step(time.Date(2026, 11, 8, 3, 10, 0, 0, sto)) || !slices.Equal(r.calls, []string{"a", "b"}) {
		t.Fatalf("calls = %v, want b the next night", r.calls)
	}
}

// The window skips the toggle off, every class but available, a version
// already tried tonight, a paused app, a stopped app and a Door-2 app.
func TestNightly_Skips(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "off", 1, func(i *store.Instance) { i.AutoUpdateOff = true })
	r.app(t, "stopped", 2, func(i *store.Instance) { i.State = "stopped" })
	r.app(t, "custom", 3, func(i *store.Instance) { i.Version = manifest.CustomVersion })
	r.app(t, "approval", 4, nil)
	if err := r.e.store.SetInstanceUpdateCheck("approval", store.UpdateCheck{Version: "2.0", Status: store.UpdateNeedsApproval, Reason: "more"}); err != nil {
		t.Fatal(err)
	}
	r.app(t, "major", 5, nil)
	if err := r.e.store.SetInstanceUpdateCheck("major", store.UpdateCheck{Version: "2.0", Status: store.UpdateServiceMajor, Reason: "pg"}); err != nil {
		t.Fatal(err)
	}
	r.app(t, "tried", 6, nil)
	if err := r.e.store.RecordInstanceUpdateTry("tried", "2.0", "2026-11-07"); err != nil {
		t.Fatal(err)
	}
	r.app(t, "paused", 7, nil)
	for range store.UpdatePauseAfter {
		if err := r.e.store.RecordInstanceUpdateFailure("paused", store.UpdateFailure{Version: "2.0", Kind: store.UpdateFailedHealth}); err != nil {
			t.Fatal(err)
		}
	}
	r.host.done("2026-11-07")
	if r.step(time.Date(2026, 11, 7, 3, 10, 0, 0, sto)) || len(r.calls) != 0 {
		t.Fatalf("calls = %v, want every app skipped", r.calls)
	}
	if holds, _ := r.host.counts(); holds != 0 {
		t.Fatal("a hold went with nothing to do")
	}
}

// One try per version per night, also across a brain restart: the try is in
// SQLite, written before the update runs. The next night tries again.
func TestNightly_OneTryPerNightAcrossARestart(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "a", 1, nil)
	r.fail["a"] = true
	r.host.done("2026-11-07")
	if !r.step(time.Date(2026, 11, 7, 3, 10, 0, 0, sto)) {
		t.Fatal("no update")
	}
	if r.step(time.Date(2026, 11, 7, 3, 11, 0, 0, sto)) {
		t.Fatal("the same version was tried twice in one night")
	}
	// The brain restarts (a control-plane update) in the middle of the night.
	r.n = r.newLoop()
	if r.step(time.Date(2026, 11, 7, 3, 12, 0, 0, sto)) || len(r.calls) != 1 {
		t.Fatalf("after a restart: calls %v, want no second try tonight", r.calls)
	}
	// A brain that stops in the middle of an update has written its try.
	r.app(t, "b", 2, nil)
	r.n.update = func(context.Context, string) error { return context.Canceled }
	r.step(time.Date(2026, 11, 7, 3, 13, 0, 0, sto))
	if row := r.e.row(t, "b"); row.UpdateTry != (store.UpdateTry{Version: "2.0", Night: "2026-11-07"}) {
		t.Fatalf("try = %+v, want it written before the update", row.UpdateTry)
	}
	r.n = r.newLoop()
	r.host.done("2026-11-08")
	if !r.step(time.Date(2026, 11, 8, 3, 10, 0, 0, sto)) || r.calls[len(r.calls)-1] != "a" {
		t.Fatalf("calls = %v, want a tried again the next night", r.calls)
	}
}

// CheckUpdates runs once a night, before the first update.
func TestNightly_ChecksTheCatalogOncePerNight(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	checks := 0
	r.n.check = func(context.Context) { checks++ }
	r.step(time.Date(2026, 11, 7, 2, 45, 0, 0, sto))
	r.step(time.Date(2026, 11, 7, 3, 10, 0, 0, sto))
	r.step(time.Date(2026, 11, 8, 2, 50, 0, 0, sto))
	if checks != 2 {
		t.Fatalf("checks = %d, want one per night", checks)
	}
}

// The zone and the window come from the read. Without them the brain reads
// the default window in UTC.
func TestNightly_ZoneAndWindowFromTheRead(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	r := newNightRig(t, "Asia/Tokyo")
	r.host.target.Window = "01:00-02:00"
	r.app(t, "a", 1, nil)
	r.host.done("2026-11-07")
	// 01:10 in Tokyo is 16:10 UTC the day before.
	at := time.Date(2026, 11, 7, 1, 10, 0, 0, tokyo).UTC()
	if !r.step(at) {
		t.Fatal("no update at 01:10 in the box's zone")
	}

	r = newNightRig(t, "")
	r.host.target.Window = ""
	r.app(t, "a", 1, nil)
	r.host.done("2026-11-07")
	if r.step(at) {
		t.Fatal("with no zone and no window, 16:10 UTC is not in the default window")
	}
	if !r.step(time.Date(2026, 11, 7, 3, 10, 0, 0, time.UTC)) {
		t.Fatal("with no zone and no window, 03:10 UTC is in the default window")
	}
	// An unreadable window and an unknown zone fall back the same way.
	r = newNightRig(t, "Not/AZone")
	r.host.target.Window = "late"
	r.app(t, "a", 1, nil)
	r.host.done("2026-11-07")
	if !r.step(time.Date(2026, 11, 7, 3, 10, 0, 0, time.UTC)) {
		t.Fatal("an unreadable window and zone did not fall back to 03:00-04:00 UTC")
	}
}

// The nights the clocks change keep their wall-clock hours.
func TestNightly_DSTNights(t *testing.T) {
	t.Run("the clocks go back before the window", func(t *testing.T) {
		stockholm(t)
		// 2026-10-25: at 03:00 CEST the clocks go back to 02:00 CET, so 02:50
		// happens twice. The hold goes 15 minutes before the 03:00 that opens
		// the window, which is the second one.
		r := newNightRig(t, "Europe/Stockholm")
		r.app(t, "a", 1, nil)
		r.step(time.Date(2026, 10, 25, 0, 50, 0, 0, time.UTC)) // 02:50 CEST
		if holds, _ := r.host.counts(); holds != 0 {
			t.Fatal("a hold went at the first 02:50, an hour and ten minutes before the window")
		}
		r.step(time.Date(2026, 10, 25, 1, 50, 0, 0, time.UTC)) // 02:50 CET
		if holds, _ := r.host.counts(); holds != 1 {
			t.Fatal("no hold at the second 02:50")
		}
		r.host.done("2026-10-25")
		if !r.step(time.Date(2026, 10, 25, 2, 10, 0, 0, time.UTC)) { // 03:10 CET
			t.Fatal("no update at 03:10 on the night the clocks went back")
		}
	})
	t.Run("the clocks go back inside the window", func(t *testing.T) {
		if _, err := time.LoadLocation("America/New_York"); err != nil {
			t.Skip("no tz database:", err)
		}
		// 2026-11-01: at 02:00 EDT the clocks go back to 01:00 EST. The window
		// 01:00-03:00 is three hours long that night, and the cut-off is
		// 02:40 EST on the wall clock, not two hours less twenty minutes
		// after it opened.
		r := newNightRig(t, "America/New_York")
		r.host.target.Window = "01:00-03:00"
		r.app(t, "a", 1, nil)
		r.app(t, "b", 2, nil)
		r.host.done("2026-11-01")
		if !r.step(time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC)) { // 02:30 EST
			t.Fatal("no update at 02:30 EST, before the wall-clock cut-off")
		}
		if r.step(time.Date(2026, 11, 1, 7, 45, 0, 0, time.UTC)) { // 02:45 EST
			t.Fatal("an update started at 02:45 EST, after the cut-off")
		}
	})
}

// An older host-agent sends no control_plane_done_night. The brain goes when
// the read was checked inside tonight's window and no control-plane update
// waits to apply.
func TestNightly_OldHostAgent(t *testing.T) {
	sto := stockholm(t)
	in := time.Date(2026, 11, 7, 3, 2, 0, 0, sto).UTC().Format(time.RFC3339)
	before := time.Date(2026, 11, 7, 2, 58, 0, 0, sto).UTC().Format(time.RFC3339)
	for name, c := range map[string]struct {
		state   string
		auto    bool
		checked string
		want    bool
	}{
		"current, checked in the window": {state: protocol.UpdateTargetCurrent, auto: true, checked: in, want: true},
		"available on the appliance":     {state: protocol.UpdateTargetAvailable, auto: false, checked: in, want: true},
		"available on hosted":            {state: protocol.UpdateTargetAvailable, auto: true, checked: in, want: false},
		"checked before the window":      {state: protocol.UpdateTargetCurrent, auto: true, checked: before, want: false},
		"none, checked in the window":    {state: protocol.UpdateTargetNone, auto: true, checked: in, want: true},
		"never checked":                  {state: protocol.UpdateTargetUnknown, auto: true, want: false},
		"unreachable, checked in window": {state: protocol.UpdateTargetUnreachable, auto: true, checked: in, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newNightRig(t, "Europe/Stockholm")
			r.app(t, "a", 1, nil)
			r.host.target = protocol.UpdateTarget{State: c.state, AutoApply: c.auto, CheckedAt: c.checked, Window: "03:00-04:00", Zone: "Europe/Stockholm"}
			if got := r.step(time.Date(2026, 11, 7, 3, 5, 0, 0, sto)); got != c.want {
				t.Fatalf("update ran = %v, want %v", got, c.want)
			}
		})
	}
}

// A host-agent that knows the hold but did not take it makes the app
// updates wait, since the OS switch could reboot the box in the middle of one.
// An older host-agent (no control_plane_done_night on its read, and no hold
// route) does not stop them. A read that fails stops them until it works
// again.
func TestNightly_HostAgentProblems(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "a", 1, nil)
	r.app(t, "b", 2, nil)
	r.host.done("2026-11-07")
	r.host.holdErr = errors.New("host-agent: connection reset")
	if r.step(time.Date(2026, 11, 7, 3, 10, 0, 0, sto)) {
		t.Fatal("an update ran although host-agent did not take the hold")
	}
	r.host.holdErr = nil
	if !r.step(time.Date(2026, 11, 7, 3, 11, 0, 0, sto)) {
		t.Fatal("no update once the hold was taken")
	}

	// An older host-agent: no done field, no hold route. One with no update
	// loop answers 501 hold-unsupported the same way.
	in := time.Date(2026, 11, 7, 3, 11, 30, 0, sto).UTC().Format(time.RFC3339)
	r.host.target = protocol.UpdateTarget{State: protocol.UpdateTargetCurrent, AutoApply: true, CheckedAt: in, Window: "03:00-04:00", Zone: "Europe/Stockholm"}
	r.host.holdErr = fmt.Errorf("%w: host-agent /v1/system/update-hold: 404 Not Found (host-agent-error)", hostclient.ErrHoldUnsupported)
	if !r.step(time.Date(2026, 11, 7, 3, 12, 0, 0, sto)) {
		t.Fatal("no update with an older host-agent that cannot hold")
	}

	r.host.readErr = errors.New("host-agent unreachable")
	if r.step(time.Date(2026, 11, 7, 3, 13, 0, 0, sto)) {
		t.Fatal("an update ran with no update-target read")
	}
}

// An update can run longer than the lease, so the hold is renewed while it
// runs.
func TestNightly_RenewsTheHoldDuringAnUpdate(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	r.app(t, "a", 1, nil)
	r.host.done("2026-11-07")
	tick := make(chan time.Time, 1)
	r.n.After = func(time.Duration) <-chan time.Time { return tick }
	r.host.held = make(chan time.Time, 4)
	r.n.update = func(context.Context, string) error {
		<-r.host.held // the hold of this step, before the update
		r.now = r.now.Add(time.Minute)
		tick <- r.now
		select {
		case until := <-r.host.held:
			if want := time.Date(2026, 11, 7, 3, 21, 0, 0, sto); !until.Equal(want) {
				t.Errorf("renewed until %v, want %v", until, want)
			}
		case <-time.After(2 * time.Second):
			t.Error("the hold was not renewed during the update")
		}
		return nil
	}
	if !r.step(time.Date(2026, 11, 7, 3, 10, 0, 0, sto)) {
		t.Fatal("no update")
	}
}

// The hold never asks past the window's end minus the cap.
func TestNightly_HoldIsCappedBeforeTheEnd(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	nt := defaultNightWindow.night(time.Date(2026, 11, 7, 3, 50, 0, 0, sto))
	r.n.hold(context.Background(), time.Date(2026, 11, 7, 3, 50, 0, 0, sto), nt)
	if got := r.host.lastHold(); !got.Equal(time.Date(2026, 11, 7, 3, 55, 0, 0, sto)) {
		t.Fatalf("hold until %v, want 03:55", got)
	}
}

func TestNightWindow(t *testing.T) {
	sto := stockholm(t)
	wrap, err := parseNightWindow("23:30-00:30")
	if err != nil {
		t.Fatal(err)
	}
	nt := wrap.night(time.Date(2026, 11, 8, 0, 10, 0, 0, sto))
	if nt.key != "2026-11-07" || !nt.ends.Equal(time.Date(2026, 11, 8, 0, 30, 0, 0, sto)) {
		t.Fatalf("a wrapping window at 00:10: %+v, want the night it opened, ending 00:30", nt)
	}
	nt = defaultNightWindow.night(time.Date(2026, 11, 7, 14, 0, 0, 0, sto))
	if nt.key != "2026-11-08" {
		t.Fatalf("at 14:00 the next night is %s, want tomorrow", nt.key)
	}
	for _, bad := range []string{"", "3-4", "03:00", "03:00-03:00", "25:00-26:00"} {
		if _, err := parseNightWindow(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// --- with the real update transaction ---------------------------------------

// realNight is a nightly loop over the real update and the real catalog
// check, in UTC, with host-agent saying the control plane is done.
func realNight(t *testing.T, e *testEnv) (*Nightly, *fakeNightHost, *time.Time) {
	t.Helper()
	host := &fakeNightHost{target: protocol.UpdateTarget{State: protocol.UpdateTargetCurrent, Window: "03:00-04:00", Zone: "UTC"}}
	now := new(time.Time)
	n := e.m.NewNightly(host)
	n.Now = func() time.Time { return *now }
	n.After = func(time.Duration) <-chan time.Time { return nil }
	return n, host, now
}

func onNight(n *Nightly, host *fakeNightHost, now *time.Time, day int) bool {
	*now = time.Date(2026, 11, day, 3, 10, 0, 0, time.UTC)
	host.done(now.Format(time.DateOnly))
	return n.Step(context.Background())
}

// An update waiting in the window is applied, and the app runs the new
// version on its old data.
func TestNightly_AppliesAnUpdate(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.newVersionMigrates(t, true)
	e.publish(t, "2.0", upImage2, upDigest2, "")
	n, host, now := realNight(t, e)
	if !onNight(n, host, now, 7) {
		t.Fatal("no update in the window")
	}
	row := e.row(t, inst.ID)
	if row.Version != "2.0" || row.State != "running" || row.UpdateFailure.Count != 0 {
		t.Fatalf("after the night: version %q state %q count %d; want 2.0, running, 0", row.Version, row.State, row.UpdateFailure.Count)
	}
}

// A failing update is tried once a night, paused after 3 failures in a row,
// tried again for a new catalog version, and a person's Update resets the
// count.
func TestNightly_PausesAfterThreeFailures(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.newVersionMigrates(t, false) // 2.0 never becomes healthy
	e.publish(t, "2.0", upImage2, upDigest2, "")
	n, host, now := realNight(t, e)

	for day := 7; day <= 9; day++ {
		if !onNight(n, host, now, day) {
			t.Fatalf("night %d: no try", day)
		}
		if n.Step(context.Background()) {
			t.Fatalf("night %d: a second try", day)
		}
		if c := e.row(t, inst.ID).UpdateFailure.Count; c != day-6 {
			t.Fatalf("night %d: count %d, want %d", day, c, day-6)
		}
	}
	row := e.row(t, inst.ID)
	if !row.UpdatePaused() || row.Version != "1.0" || row.State != "running" {
		t.Fatalf("after three failures: paused %v version %q state %q; want paused on 1.0", row.UpdatePaused(), row.Version, row.State)
	}
	if onNight(n, host, now, 10) {
		t.Fatal("a paused update was tried")
	}

	// A person's Update still works and starts the count again.
	if err := e.m.UpdateByPerson(context.Background(), inst.ID, nil); err == nil {
		t.Fatal("2.0 updated, want it to fail again")
	}
	if row := e.row(t, inst.ID); row.UpdateFailure.Count != 1 || row.UpdatePaused() {
		t.Fatalf("after a person's Update: count %d paused %v; want 1 and not paused", row.UpdateFailure.Count, row.UpdatePaused())
	}
	if !onNight(n, host, now, 11) {
		t.Fatal("the window did not try again after the reset")
	}

	// A new catalog version starts the count again. The check before the
	// night's first update sees it.
	for range 2 {
		if err := e.store.RecordInstanceUpdateFailure(inst.ID, store.UpdateFailure{Version: "2.0", Kind: store.UpdateFailedHealth}); err != nil {
			t.Fatal(err)
		}
	}
	if !e.row(t, inst.ID).UpdatePaused() {
		t.Fatal("setup: not paused")
	}
	e.publish(t, "2.1", upImage2, upDigest2, "")
	if !onNight(n, host, now, 12) {
		t.Fatal("the new version was not tried")
	}
	if row := e.row(t, inst.ID); row.UpdateFailure.Version != "2.1" || row.UpdateFailure.Count != 1 {
		t.Fatalf("after 2.1 failed: %+v, want a count of 1 on 2.1", row.UpdateFailure)
	}
}
