package main

import (
	"time"

	"github.com/onmoose/moose/internal/hostagent/updatetarget"
	"github.com/onmoose/moose/internal/protocol"
)

// updateTargetReport answers GET /v1/system/update-target: what the loop last
// decided, plus what the box is running right now (UPDATES.md # 8.4).
//
// It lives here, next to where the loop is built, because it is assembled from
// three things only this file has all of: the loop's snapshot, the box's
// control-plane declaration, and the configuration that decided where the
// target comes from. internal/hostagent holds only the consumer-side interface.
type updateTargetReport struct {
	// loop is the running loop, or nil when there is none. Nil is the
	// `disabled` state: host-agent refused an unusable seeded target and never
	// started one, so this box will not update itself.
	loop *updatetarget.Loop
	// disabledErr is why there is no loop. Set exactly when loop is nil.
	disabledErr string
	// running reads the box's own declaration. Read on every request, not
	// carried from the last tick: a tick that ended early never read it, and
	// host-agent outlives an update, so a stored pair would be stale exactly
	// after one.
	running updatetarget.RunningPair
	// from is where this box's update-target URL came from: "seed", "env" or
	// "default" (updateconfig.go). Empty on appliance, which has no such URL —
	// its target comes from the signed release manifest.
	from string
	// window and windowFrom are the box's own setting, reported when no tick
	// has resolved one yet. A tick that read an answer reports what the answer
	// said instead.
	window     updatetarget.Window
	windowFrom string
	// profile and autoApply describe who decides. They come from the same
	// build-tagged source that built the loop.
	profile   string
	autoApply bool
}

// Read assembles the report. It never fails: every way this can go wrong is a
// state on the wire, and a box that cannot answer this question still has to
// say so in a form the dashboard can render.
func (r updateTargetReport) Read() protocol.UpdateTarget {
	out := protocol.UpdateTarget{
		From:       r.from,
		Window:     r.window.String(),
		WindowFrom: r.windowFrom,
		Profile:    r.profile,
		AutoApply:  r.autoApply,
	}
	var runErr error
	if r.running != nil {
		brain, ui, err := r.running.Running()
		out.Running, runErr = protocol.ControlPlanePair{Brain: brain, UI: ui}, err
	}

	// disabled outranks everything, including an unreadable declaration. Both
	// are true on a box with a bad seed and a missing compose, and only one of
	// them is the reason this box will never update — reporting the read
	// failure instead would hide the fault that needs a human behind one that
	// looks transient. The read failure is not dropped: it rides along in
	// detail, because a box with two faults must not report one of them.
	if r.loop == nil {
		out.State, out.Detail = protocol.UpdateTargetDisabled, r.disabledErr
		if runErr != nil {
			out.Detail += "; the box also cannot read what it is running: " + runErr.Error()
		}
		return out
	}
	if runErr != nil {
		// The box cannot read its own declaration, so it cannot say whether a
		// target is a change. That is the one failure this report has that the
		// loop's snapshot cannot describe, so it wins over the tick's outcome.
		out.State, out.Detail = protocol.UpdateTargetUnreachable, runErr.Error()
		return out
	}

	s := r.loop.Snapshot()
	if s.CheckedAt.IsZero() {
		// The loop is up but has not finished its first tick. It ticks
		// immediately at startup, so this lasts seconds.
		out.State = protocol.UpdateTargetUnknown
		return out
	}
	out.CheckedAt = s.CheckedAt.UTC().Format(time.RFC3339)
	out.Detail = s.Err
	out.Window, out.WindowFrom = s.Window.String(), s.WindowFrom

	switch s.Outcome {
	case updatetarget.OutcomeNone:
		out.State = protocol.UpdateTargetNone
	case updatetarget.OutcomeUnreachable:
		out.State = protocol.UpdateTargetUnreachable
	case updatetarget.OutcomeRefused:
		// The refused answer is reported, so an operator can see what the
		// source is serving. Nothing acted on it.
		out.State = protocol.UpdateTargetRefused
		out.Target = offer(s.Target)
	case updatetarget.OutcomeOK:
		out.Target = offer(s.Target)
		if out.Running.Brain == s.Target.BrainImage && out.Running.UI == s.Target.UIImage {
			out.State = protocol.UpdateTargetCurrent
		} else {
			out.State = protocol.UpdateTargetAvailable
		}
	default:
		out.State = protocol.UpdateTargetUnknown
	}
	return out
}

// offer converts a target to the wire shape. PublishedAt is omitted when the
// source did not say, rather than being sent as the zero time — "published at
// year 1" is worse than saying nothing.
func offer(t updatetarget.Target) *protocol.ControlPlaneOffer {
	o := &protocol.ControlPlaneOffer{
		Version:    t.Version,
		BrainImage: t.BrainImage,
		UIImage:    t.UIImage,
	}
	if !t.PublishedAt.IsZero() {
		o.PublishedAt = t.PublishedAt.UTC().Format(time.RFC3339)
	}
	return o
}
