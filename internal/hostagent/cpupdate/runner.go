package cpupdate

import (
	"context"
	"errors"

	"github.com/onmoose/moose/internal/protocol"
)

// Runner adapts Apply to the shape host-agent's job handler wants: two refs in,
// one wire result out. It is the concrete provider behind hostagent's
// SystemUpdater seam (CLAUDE.md # Go code discipline — the interface lives with
// the consumer, this package exports a concrete type).
//
// Base carries everything about the box that does not change per update: where
// the declaration lives, how to launch the brain, where snapshots go. Only the
// target pair comes from the request.
type Runner struct {
	Docker Docker
	Prober Prober
	Base   Options
}

// Update runs one transaction and maps its outcome onto the wire.
//
// ErrNothingToDo is reported as **success with nothing changed**, not as an
// error. Asking for the pair the box is already running is not a failure, and
// turning it into one would make a retry after a partial network problem look
// broken. The caller can tell the two apart: brain_changed and ui_changed are
// both false.
func (r Runner) Update(ctx context.Context, brainRef, uiRef string) (protocol.SystemUpdateResult, error) {
	o := r.Base
	o.BrainRef = brainRef
	o.UIRef = uiRef

	res, err := Apply(ctx, r.Docker, r.Prober, o)
	out := protocol.SystemUpdateResult{
		BrainChanged: res.BrainChanged,
		UIChanged:    res.UIChanged,
		Reverted:     res.Reverted,
		FailureMode:  res.FailureMode,
	}
	if res.RevertErr != nil {
		out.RevertError = res.RevertErr.Error()
	}
	if err != nil && !errors.Is(err, ErrNothingToDo) {
		return out, err
	}
	return out, nil
}
