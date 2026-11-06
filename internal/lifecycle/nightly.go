package lifecycle

// The nightly app updates (UPDATES.md # 4 Update window, # 7 Update ordering
// and Reboots, #632). Every minute the loop asks host-agent for the update
// window and the box's time zone, and inside tonight's window it updates the
// apps whose update the box may apply on its own, one at a time, through the
// same transaction a person's Update runs (Manager.Update).
//
// The order of a night is stream B before stream A (UPDATES.md # 7): the
// control plane first, then the apps, then the OS switch, which reboots the
// box. So the loop:
//
//  1. From 15 minutes before the window opens, when any app has work
//     tonight, holds the OS switch with a lease on host-agent
//     (PUT /v1/system/update-hold) and renews it every minute while the work
//     lasts. Each renewal asks for at most protocol.UpdateHoldLease and never
//     past the window's end minus protocol.UpdateHoldCapBeforeEnd, and
//     host-agent cuts it the same way. A brain that dies stops renewing, so
//     the OS switch still goes that night.
//  2. Waits until host-agent says the control plane is done for tonight
//     (control_plane_done_night). The control-plane update restarts the
//     brain, so every fact the loop needs across a night is in SQLite.
//  3. Updates one app at a time, oldest install first, and starts none after
//     the cut-off, 20 minutes before the window ends.
//  4. Clears the hold as soon as no work is left, so the OS switch goes at
//     once.
//
// One try per version per night: the try is stored before the update runs.
// After store.UpdatePauseAfter failures in a row on one version the app is
// paused, and only a new catalog version or a person's Update starts it
// again.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

const (
	// nightlyTick is how often the loop wakes. It is also how often the hold
	// is renewed, well inside its lease.
	nightlyTick = time.Minute
	// nightlyHoldLead is how long before the window opens the first hold is
	// sent: before host-agent's first tick inside the window could switch.
	nightlyHoldLead = 15 * time.Minute
	// nightlyCutoff is how long before the window ends the last app update
	// may start. What is left waits for the next night.
	nightlyCutoff = 20 * time.Minute
)

// defaultNightWindow is the update window when host-agent names none: 03:00
// to 04:00 (UPDATES.md # 4).
var defaultNightWindow = nightWindow{start: 3 * time.Hour, end: 4 * time.Hour}

// NightlyHost is what the nightly loop needs from host-agent. Consumer-side;
// the provider is *hostclient.Client.
type NightlyHost interface {
	SystemUpdateTarget(ctx context.Context) (protocol.UpdateTarget, error)
	SetUpdateHold(ctx context.Context, until time.Time) (time.Time, error)
	ClearUpdateHold(ctx context.Context) error
}

// Nightly runs the night's app updates. Build it with Manager.NewNightly and
// start Run on its own goroutine.
type Nightly struct {
	m    *Manager
	host NightlyHost

	// Now and After are the clock, so a test drives a night without
	// sleeping. Both default to the real clock.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	// update and check are Manager.Update and Manager.CheckUpdates; a test
	// may set them.
	update func(ctx context.Context, id string) error
	check  func(ctx context.Context)

	// holdState is what the loop last told host-agent: unknown at start,
	// then held or cleared. A brain that restarts does not know, so it
	// clears once when it has no work.
	holdState int
	// checkedNight is the night CheckUpdates last ran for.
	checkedNight string
	// lastWindow and lastLoc are the window and zone of the last good read,
	// nil before the first one. A failed read uses them, so a blip does not
	// read 03:30 Stockholm as 02:30 UTC and clear the hold.
	lastWindow nightWindow
	lastLoc    *time.Location
	// said dedupes log lines that would repeat every minute.
	said map[string]bool
}

const (
	holdUnknown = iota
	holdHeld
	holdCleared
)

// NewNightly returns the nightly loop for this manager, talking to host.
func (m *Manager) NewNightly(host NightlyHost) *Nightly {
	return &Nightly{
		m:    m,
		host: host,
		update: func(ctx context.Context, id string) error {
			return m.Update(ctx, id, nil)
		},
		check: m.CheckUpdates,
		said:  map[string]bool{},
	}
}

func (n *Nightly) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Nightly) after(d time.Duration) <-chan time.Time {
	if n.After != nil {
		return n.After(d)
	}
	return time.After(d)
}

// once logs a line the first time key is seen.
func (n *Nightly) once(key string, level slog.Level, msg string, args ...any) {
	if n.said[key] {
		return
	}
	n.said[key] = true
	slog.Log(context.Background(), level, msg, args...)
}

// Run wakes every minute until ctx is done. After an app update it goes on at
// once: the next app should not wait a minute of the window.
func (n *Nightly) Run(ctx context.Context) {
	for {
		ran := n.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-n.after(nightlyTick):
		}
	}
}

// night is one occurrence of the update window, in the box's zone.
type night struct {
	key      string    // the date the window opens on, YYYY-MM-DD
	opens    time.Time // on the wall clock
	ends     time.Time
	holdFrom time.Time // opens minus nightlyHoldLead
	cutoff   time.Time // ends minus nightlyCutoff
	holdCap  time.Time // ends minus protocol.UpdateHoldCapBeforeEnd
}

// Step runs one wake of the loop. It reports whether it ran an app update.
func (n *Nightly) Step(ctx context.Context) bool {
	t, readErr := n.host.SystemUpdateTarget(ctx)
	if readErr != nil {
		n.once("read-err", slog.LevelWarn, "nightly updates: cannot read host-agent's update target; app updates wait", "err", readErr)
	} else {
		delete(n.said, "read-err")
	}
	if readErr != nil {
		// The read says nothing about tonight. Keep a hold the loop already
		// has, read against the last good window and zone, and never clear
		// it here: a hold not renewed ends on its own within its lease.
		if n.lastLoc != nil && n.holdState == holdHeld {
			now := n.now().In(n.lastLoc)
			if nt := n.lastWindow.night(now); !now.Before(nt.holdFrom) && now.Before(nt.ends) {
				n.hold(ctx, now, nt)
			}
		}
		return false
	}
	w, loc := n.windowOf(t)
	n.lastWindow, n.lastLoc = w, loc
	now := n.now().In(loc)
	nt := w.night(now)

	if now.Before(nt.holdFrom) || !now.Before(nt.ends) {
		n.release(ctx)
		return false
	}
	if n.checkedNight != nt.key {
		// Act on the latest catalog: the check is cheap when nothing moved.
		n.checkedNight = nt.key
		n.check(ctx)
	}
	work, err := n.work(nt.key)
	if err != nil {
		slog.Warn("nightly updates: list the apps", "err", err)
		return false
	}
	if len(work) == 0 {
		n.release(ctx)
		return false
	}
	if !now.Before(nt.cutoff) {
		n.once("cutoff:"+nt.key, slog.LevelInfo, "nightly updates: the cut-off passed; the apps left wait for the next night",
			"night", nt.key, "count", len(work))
		n.release(ctx)
		return false
	}
	held := n.hold(ctx, now, nt)
	if now.Before(nt.opens) {
		return false
	}
	if done, why := controlPlaneDone(t, nt); !done {
		n.once("wait:"+nt.key+":"+why, slog.LevelInfo, "nightly updates: waiting for the control plane before the apps",
			"night", nt.key, "step", why, "count", len(work))
		return false
	}
	if !held {
		// host-agent can hold the switch but did not take the hold this
		// time, so the OS switch could reboot the box in the middle of the
		// update. Wait for the next wake, a minute away (UPDATES.md # 7
		// Reboots).
		return false
	}

	inst := work[0]
	if err := n.m.store.RecordInstanceUpdateTry(inst.ID, inst.Update.Version, nt.key); err != nil {
		// Without the record a brain that stops in the middle would try the
		// same version again tonight, so the update does not run.
		slog.Warn("nightly update: could not record the try; skipping the app tonight", "instance_id", inst.ID, "err", err)
		return false
	}
	slog.Info("nightly update: updating an app", "instance_id", inst.ID, "name", inst.Name,
		"version", inst.Version, "new_version", inst.Update.Version, "night", nt.key)
	stop := n.keepHold(ctx, nt)
	err = n.update(ctx, inst.ID)
	stop()
	n.report(inst, nt.key, err)
	return true
}

// report logs an update's outcome with what the store now says.
func (n *Nightly) report(inst store.Instance, nightKey string, err error) {
	if err == nil {
		slog.Info("nightly update: app updated", "instance_id", inst.ID, "name", inst.Name,
			"version", inst.Version, "new_version", inst.Update.Version, "night", nightKey)
		return
	}
	row, gerr := n.m.store.Get(inst.ID)
	if gerr != nil {
		slog.Warn("nightly update: app update failed", "instance_id", inst.ID, "name", inst.Name,
			"version", inst.Version, "new_version", inst.Update.Version, "night", nightKey, "err", err)
		return
	}
	slog.Warn("nightly update: app update failed", "instance_id", inst.ID, "name", inst.Name,
		"version", inst.Version, "new_version", inst.Update.Version, "night", nightKey,
		"count", row.UpdateFailure.Count, "err", err)
	if row.UpdatePaused() {
		slog.Warn("nightly update: paused; the update failed too many times in a row", "instance_id", inst.ID, "name", inst.Name,
			"new_version", inst.Update.Version, "count", row.UpdateFailure.Count)
	}
}

// work is the apps the window may update tonight, oldest install first: a
// running catalog app with its automatic update on, an update of class
// available, not tried tonight in that version, and not paused.
func (n *Nightly) work(nightKey string) ([]store.Instance, error) {
	insts, err := n.m.store.List()
	if err != nil {
		return nil, err
	}
	var out []store.Instance
	for _, i := range insts {
		switch {
		case i.State != "running", i.Version == manifest.CustomVersion, i.AutoUpdateOff:
		case i.Update.Status != store.UpdateAvailable || i.Update.Version == "":
		case i.UpdateTry.Version == i.Update.Version && i.UpdateTry.Night == nightKey:
		case i.UpdatePaused():
		default:
			out = append(out, i)
		}
	}
	return out, nil
}

// hold sets or renews the hold on the OS switch. It reports whether the app
// updates may go: host-agent took the hold, or it cannot hold the switch at
// all (hostclient.ErrHoldUnsupported: an older host-agent, or one with no
// update loop). A hold that failed this time says no.
func (n *Nightly) hold(ctx context.Context, now time.Time, nt night) bool {
	until := now.Add(protocol.UpdateHoldLease)
	if until.After(nt.holdCap) {
		until = nt.holdCap
	}
	if !until.After(now) {
		return false
	}
	end, err := n.host.SetUpdateHold(ctx, until)
	if errors.Is(err, hostclient.ErrHoldUnsupported) {
		n.once("hold-unsupported", slog.LevelWarn, "nightly updates: this host-agent cannot hold the OS switch; app updates go without a hold", "err", err)
		return true
	}
	if err != nil {
		n.once("hold-err", slog.LevelWarn, "nightly updates: host-agent did not take the hold on the OS switch; the app updates wait", "err", err)
		return false
	}
	delete(n.said, "hold-err")
	if n.holdState != holdHeld {
		slog.Info("nightly updates: holding the OS switch for app updates", "night", nt.key, "until", end.Format(time.RFC3339))
	}
	n.holdState = holdHeld
	return true
}

// keepHold renews the hold every tick while an update runs, which can take
// longer than the lease. The returned stop waits for the renewer to end.
func (n *Nightly) keepHold(ctx context.Context, nt night) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-n.after(nightlyTick):
				n.hold(ctx, n.now(), nt)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// release clears the hold, at once, when the loop may hold it: it held it,
// or it does not know (a brain that restarted).
func (n *Nightly) release(ctx context.Context) {
	if n.holdState == holdCleared {
		return
	}
	if err := n.host.ClearUpdateHold(ctx); err != nil {
		// A hold that is not cleared ends on its own within its lease.
		n.once("clear-err", slog.LevelWarn, "nightly updates: could not clear the hold on the OS switch; it ends on its own", "err", err)
	} else if n.holdState == holdHeld {
		slog.Info("nightly updates: no app work left; the OS switch may go")
	}
	n.holdState = holdCleared
}

// windowOf reads the window and the zone from host-agent's read. A field
// that is missing or unreadable falls back, the window to 03:00-04:00 and the
// zone to UTC, with one warning: the brain runs in a container whose local
// zone is not the box's.
func (n *Nightly) windowOf(t protocol.UpdateTarget) (nightWindow, *time.Location) {
	w, loc := defaultNightWindow, time.UTC
	if t.Window == "" {
		n.once("no-window", slog.LevelWarn, "nightly updates: host-agent names no update window; using the default", "window", w.String())
	} else if pw, err := parseNightWindow(t.Window); err != nil {
		n.once("bad-window:"+t.Window, slog.LevelWarn, "nightly updates: host-agent's update window is not readable; using the default",
			"err", err, "window", w.String())
	} else {
		w = pw
	}
	if t.Zone == "" {
		n.once("no-zone", slog.LevelWarn, "nightly updates: host-agent names no time zone; reading the window in UTC")
	} else if l, err := time.LoadLocation(t.Zone); err != nil {
		n.once("bad-zone:"+t.Zone, slog.LevelWarn, "nightly updates: the box's time zone is not known; reading the window in UTC",
			"zone", t.Zone, "err", err)
	} else {
		loc = l
	}
	return w, loc
}

// controlPlaneDone reports whether stream B is done for tonight, and a short
// word for why not. A host-agent that knows control_plane_done_night says
// it. An older one does not, and then the brain goes when the read was
// checked inside tonight's window and no control-plane update waits to
// apply; otherwise it waits, until the cut-off.
func controlPlaneDone(t protocol.UpdateTarget, nt night) (bool, string) {
	if t.ControlPlaneDoneNight != nil {
		if *t.ControlPlaneDoneNight == nt.key {
			return true, ""
		}
		return false, "control_plane"
	}
	checked, err := time.Parse(time.RFC3339, t.CheckedAt)
	if err != nil || checked.Before(nt.opens) || !checked.Before(nt.ends) {
		return false, "not_checked"
	}
	if t.State == protocol.UpdateTargetAvailable && t.AutoApply {
		return false, "control_plane"
	}
	return true, ""
}

// nightWindow is the update window as host-agent names it, "HH:MM-HH:MM" in
// the box's zone (BRAIN_HOST_PROTOCOL.md # Update-target read). start and end
// are offsets from local midnight; an end at or before the start wraps past
// midnight.
type nightWindow struct {
	start, end time.Duration
}

func parseNightWindow(s string) (nightWindow, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return nightWindow{}, fmt.Errorf("window %q is not HH:MM-HH:MM", s)
	}
	start, err := parseClock(a)
	if err != nil {
		return nightWindow{}, err
	}
	end, err := parseClock(b)
	if err != nil {
		return nightWindow{}, err
	}
	if start == end {
		return nightWindow{}, fmt.Errorf("window %q is empty", s)
	}
	return nightWindow{start: start, end: end}, nil
}

func parseClock(s string) (time.Duration, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q is not a time of day", s)
	}
	return time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute, nil
}

func (w nightWindow) String() string {
	c := func(d time.Duration) string {
		return fmt.Sprintf("%02d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return c(w.start) + "-" + c(w.end)
}

// wallClock is the wall-clock time d after midnight on day's date, plus days.
func wallClock(day time.Time, d time.Duration, days int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day()+days, int(d/time.Hour), int(d%time.Hour/time.Minute), 0, 0, day.Location())
}

// night is the window open at t, or the next one to open after t. Every time
// in it is built on the wall clock, never as midnight plus a duration, so a
// night the clocks change on keeps its hours (host-agent's nextNight does the
// same).
func (w nightWindow) night(t time.Time) night {
	wrap := w.end <= w.start
	tod := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second
	day := t
	switch {
	case wrap && tod < w.end:
		// A window that wraps past midnight and opened yesterday.
		day = t.AddDate(0, 0, -1)
	case !wrap && tod >= w.end:
		// Tonight's window is over: the next one.
		day = t.AddDate(0, 0, 1)
	}
	endDays := 0
	if wrap {
		endDays = 1
	}
	opens, ends := wallClock(day, w.start, 0), wallClock(day, w.end, endDays)
	return night{
		key:      time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location()).Format(time.DateOnly),
		opens:    opens,
		ends:     ends,
		holdFrom: opens.Add(-nightlyHoldLead),
		cutoff:   ends.Add(-nightlyCutoff),
		holdCap:  ends.Add(-protocol.UpdateHoldCapBeforeEnd),
	}
}

// UpdateByPerson is a person's Update (POST /api/v1/apps/{id}/update): the
// count of failures in a row goes back to 0 before it runs, so a paused app
// can be tried, and a failure then counts 1 (#632).
func (m *Manager) UpdateByPerson(ctx context.Context, id string, progress func(step string)) error {
	if err := m.store.ResetInstanceUpdateFailCount(id); err != nil {
		return err
	}
	m.publishUpdateChanged(id)
	return m.Update(ctx, id, progress)
}
