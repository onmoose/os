package updatetarget

import (
	"log/slog"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// The brain's hold on the OS switch (#632, UPDATES.md # 7 Reboots).
//
// Only the brain calls host-agent, never the other way round. So when the
// brain has app updates to run tonight, it tells host-agent to hold the OS
// switch, which reboots the box, until they are done. The hold is a lease:
// the brain renews it about every minute, each time for at most
// protocol.UpdateHoldLease, and clears it when it has no work left. A brain
// that dies stops renewing, so its hold ends on its own and the OS switch
// still goes that night.
//
// host-agent enforces both limits itself, so a brain bug cannot block the OS
// update: a hold never lasts more than the lease from now, and never past the
// window's end minus protocol.UpdateHoldCapBeforeEnd.
//
// Only the switch waits. The download and the install into the other slot
// run while a hold is live: they do not touch the running slot.
//
// The hold lives in memory. A host-agent restart forgets it, and the brain
// sets it again within a minute.

// occurrenceEnd is when the window that opens on night ends, on the wall
// clock. A window that wraps past midnight ends on the day after.
func occurrenceEnd(w Window, night time.Time) time.Time {
	h, m := int(w.End/time.Hour), int(w.End%time.Hour/time.Minute)
	day := night.Day()
	if w.End <= w.Start {
		day++
	}
	return time.Date(night.Year(), night.Month(), day, h, m, 0, 0, night.Location())
}

// occurrenceStart is when the window that opens on night opens, on the wall
// clock.
func occurrenceStart(w Window, night time.Time) time.Time {
	h, m := int(w.Start/time.Hour), int(w.Start%time.Hour/time.Minute)
	return time.Date(night.Year(), night.Month(), night.Day(), h, m, 0, 0, night.Location())
}

// HoldCap is the latest a hold may last when it is set at now: the end of the
// window that is open at now, or of the next one to open, minus
// protocol.UpdateHoldCapBeforeEnd.
func HoldCap(w Window, now time.Time) time.Time {
	return occurrenceEnd(w, nextNight(w, now)).Add(-protocol.UpdateHoldCapBeforeEnd)
}

// holdWindow is the window a hold is cut to: the one the last tick resolved,
// or the configured one before the first tick.
func (l *Loop) holdWindow() Window {
	if w := l.Snapshot().Window; w != (Window{}) {
		return w
	}
	return l.window()
}

// SetHold sets or renews the brain's hold on the OS switch, until until, cut
// to at most protocol.UpdateHoldLease from now and to HoldCap. It returns the
// end it kept, or the zero time when the hold is not live after the cuts (an
// until in the past, or a call after the cap), which also clears a live one.
func (l *Loop) SetHold(until time.Time) time.Time {
	now := l.now()
	end := until
	if lim := now.Add(protocol.UpdateHoldLease); end.After(lim) {
		end = lim
	}
	if limit := HoldCap(l.holdWindow(), now); end.After(limit) {
		end = limit
	}
	if !end.After(now) {
		l.ClearHold()
		return time.Time{}
	}
	l.holdMu.Lock()
	was := l.holdUntil.After(now)
	l.holdUntil = end
	l.holdMu.Unlock()
	if !was {
		slog.Info("os update: the brain holds the switch for app updates", "until", end.Format(time.RFC3339))
	}
	return end
}

// ClearHold ends the brain's hold at once. When one was live, it wakes the
// loop, so the switch does not wait up to 15 minutes for the next tick and
// miss the window.
func (l *Loop) ClearHold() {
	now := l.now()
	l.holdMu.Lock()
	was := l.holdUntil.After(now)
	l.holdUntil = time.Time{}
	l.holdMu.Unlock()
	if was {
		slog.Info("os update: the brain cleared its hold on the switch")
		l.wakeUp()
	}
}

// Hold reports the end of the live hold, or false when there is none.
func (l *Loop) Hold() (time.Time, bool) {
	now := l.now()
	l.holdMu.Lock()
	defer l.holdMu.Unlock()
	if !l.holdUntil.After(now) {
		return time.Time{}, false
	}
	return l.holdUntil, true
}

// held reports whether a hold is live at now.
func (l *Loop) held(now time.Time) bool {
	l.holdMu.Lock()
	defer l.holdMu.Unlock()
	return l.holdUntil.After(now)
}

// holdWake is how long Run may sleep while a hold is live, so the loop ticks
// when the hold ends on its own and when the window opens: the two moments
// stream A or stream B may have work that cannot wait 15 minutes. ok is false
// when there is no live hold.
func (l *Loop) holdWake(now time.Time) (time.Duration, bool) {
	until, ok := l.Hold()
	if !ok {
		return 0, false
	}
	d := until.Sub(now) + time.Second
	w := l.holdWindow()
	if !w.Contains(now) {
		if open := occurrenceStart(w, nextNight(w, now)); open.After(now) && open.Sub(now)+time.Second < d {
			d = open.Sub(now) + time.Second
		}
	}
	return d, true
}

// SwitchWaitsForApps is the OS detail while a hold keeps the switch waiting.
const SwitchWaitsForApps = "the switch waits for app updates to finish"

// ControlPlaneDone reports whether stream B is finished for tonight's window,
// and the night it is finished for (#632, protocol.UpdateTarget
// ControlPlaneDoneNight). It is done when the window is open, the last tick
// ran inside this window, and stream B will do nothing more tonight: the tick
// found nothing to do, or tonight's one attempt is over and its job no longer
// runs. The running pair and the job are read fresh, so a finished update is
// seen at once, not at the next tick.
func (l *Loop) ControlPlaneDone() (time.Time, bool) {
	night, done := l.controlPlaneDone()
	if done {
		// The brain may start app updates now, so stream B is over for this
		// night, even when the done came from a failed read of the source.
		// A good target on a later tick then waits for the next night while
		// the apps hold the window, as any late target does
		// (applyControlPlane). Without this, a 03:00 blip would let the
		// brain start an app update and a 03:15 target restart the brain in
		// the middle of it.
		l.applyMu.Lock()
		if night.After(l.settled) {
			l.settled = night
		}
		l.applyMu.Unlock()
	}
	return night, done
}

func (l *Loop) controlPlaneDone() (time.Time, bool) {
	s := l.Snapshot()
	if s.CheckedAt.IsZero() {
		return time.Time{}, false
	}
	w := s.Window
	if w == (Window{}) {
		w = l.window()
	}
	now := l.now()
	if !w.Contains(now) {
		return time.Time{}, false
	}
	night := occurrenceNight(w, now)
	checked := s.CheckedAt.In(now.Location())
	if !w.Contains(checked) || !occurrenceNight(w, checked).Equal(night) {
		// No tick in this window yet: a target published since the last one
		// would still be applied first.
		return time.Time{}, false
	}
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	triedTonight := !l.attempted.night.IsZero() && l.attempted.night.Equal(night)
	if triedTonight && l.attempted.jobID != "" && l.Applier != nil && l.Applier.JobRunning(l.attempted.jobID) {
		return time.Time{}, false
	}
	if s.Outcome != OutcomeOK || !l.AutoApply || l.Applier == nil {
		// Nothing usable to apply, or the appliance, which never applies its
		// control plane in the window.
		return night, true
	}
	brain, ui, err := l.Current.Running()
	if err != nil || (brain == s.Target.BrainImage && ui == s.Target.UIImage) {
		// The tick cannot apply what it cannot compare, and a current box
		// has nothing to apply.
		return night, true
	}
	if triedTonight && l.attempted.version == s.Target.Version {
		// Tonight's one attempt is over; it put the box back.
		return night, true
	}
	if l.settled.Equal(night) && l.held(now) {
		// A target that showed up after stream B was done waits for the next
		// night while the apps hold the window (applyControlPlane).
		return night, true
	}
	return time.Time{}, false
}

// settle marks tonight as a night whose stream B had nothing to start, when
// the window is open. Called at the end of every tick that started nothing.
// Only a tick whose source answered settles the night: an unreachable source,
// a refused answer or no target says nothing about tonight's control plane, so
// one DNS blip at 03:00 must not make a good target that comes a tick later
// wait a whole night (applyControlPlane). Once ControlPlaneDone has told the
// brain the night is done, though, the night is settled whatever the outcome.
func (l *Loop) settle(w Window, started bool) {
	if started || l.Snapshot().Outcome != OutcomeOK {
		return
	}
	now := l.now()
	if !w.Contains(now) {
		return
	}
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	l.settled = occurrenceNight(w, now)
}

// NightOf is the night of the window open at t, the date it opened on, or
// false when the window is shut at t.
func NightOf(w Window, t time.Time) (time.Time, bool) {
	if !w.Contains(t) {
		return time.Time{}, false
	}
	return occurrenceNight(w, t), true
}
