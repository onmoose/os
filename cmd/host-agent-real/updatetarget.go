package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/onmoose/os/internal/hostagent"
	"github.com/onmoose/os/internal/hostagent/brainlaunch"
	"github.com/onmoose/os/internal/hostagent/relmanifest"
	"github.com/onmoose/os/internal/hostagent/updatetarget"
)

// startUpdateTarget starts the loop that learns this box's control-plane target
// and applies it, and returns a stop function.
//
// **This file is the one place the update-target seam is consumed.** The profile
// decides where the answer comes from (updateTargetSource, build-tagged) and
// whether the box may act on it alone; everything after that — the compare, the
// validation, the window, the apply, the failure handling — is one loop shared by
// both profiles. A second copy of any of it, per profile, is what UPDATES.md # 8
// means by "we only build it once".
func startUpdateTarget(brainCfg brainlaunch.Config, a *hostagent.Agent, poller *relmanifest.Poller) func() {
	window, windowFrom := updateWindow()
	// What the box is running, read fresh on every socket read. Built here
	// whatever happens below: a box that will not update itself still has to be
	// able to say what it is running.
	running := updatetarget.LedgerPair{
		Dir: brainCfg.ControlPlaneDir,
		// What this box shipped with, used until an update writes a ledger. The
		// same value brainlaunch used to launch the brain, so the loop compares
		// against the ref actually running.
		BrainDefault: brainCfg.Image,
	}

	src, err := updateTargetSource(poller)
	if err != nil {
		// Refuse; do not fall back. The box was pointed somewhere deliberately
		// and we cannot honour it, so running the loop against the fleet default
		// would quietly move a pinned box onto stable (updateconfig.go). No loop
		// means the box keeps serving whatever it is already running.
		//
		// It is also reported on the socket as `disabled`, not left to the
		// journal. "This box will never update" and "this box has not checked
		// yet" look identical otherwise, and only the first one needs a human.
		slog.Error("update target is not usable; this box will not update itself", "err", err)
		a.UpdateTarget = updateTargetReport{
			disabledErr: err.Error(),
			running:     running,
			window:      window,
			windowFrom:  windowFrom,
			// The profile is a build-time fact, so it survives a source this
			// box could not build. autoApply is left false on purpose: nothing
			// on this box will apply anything, whatever profile it is.
			profile: buildProfile,
		}
		return func() {}
	}

	loop := &updatetarget.Loop{
		Source:  src.Source,
		Current: running,
		Applier: agentApplier{a},
		Repos:   repositories(),
		// The box's own window. An answer that names one wins over it, so this
		// is the fallback the loop uses until a source has an opinion.
		Window:     window,
		WindowFrom: windowFrom,
		AutoApply:  src.AutoApply,
		Profile:    src.Profile,
	}
	// The socket read (GET /v1/system/update-target) is served from this loop's
	// snapshot. Set before Run, so a read that arrives during the first tick
	// finds a reporter rather than a nil one.
	a.UpdateTarget = updateTargetReport{
		loop:       loop,
		running:    running,
		from:       src.From,
		window:     window,
		windowFrom: windowFrom,
		profile:    src.Profile,
		autoApply:  src.AutoApply,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go loop.Run(ctx)
	// The window here is the local setting. A source that names its own window
	// replaces it, and the loop logs that with from=answer when it happens.
	slog.Info("update target loop started", "profile", src.Profile, "window", window.String(), "from", windowFrom)
	return cancel
}

// targetSource is what the build-tagged updateTargetSource hands back: the seam
// implementation for this profile, plus the facts about it worth reporting.
//
// A struct rather than four return values because two of them — Profile and
// From — exist only to be shown to a person, and a positional list of four
// strings and bools is where they get swapped by accident.
type targetSource struct {
	// Source answers what this box should be running.
	Source updatetarget.Source
	// AutoApply is the UPDATES.md # 8.2 split: hosted applies on its own,
	// appliance waits for an admin.
	AutoApply bool
	// Profile is the environment profile that produced this source.
	Profile string
	// From is where the update-target URL came from ("seed", "env",
	// "default"). Empty on appliance, which has no such URL.
	From string
}

// repositories is where this box expects its control-plane images to come from.
// A pinned digest is only as trustworthy as the repository it is pulled from, so
// a source that named the right digest shape in the wrong place is refused
// (updatetarget.Validate).
//
// Overridable because the CI boot proof serves both images from a registry
// inside the guest, and because a box under test may be pointed elsewhere.
func repositories() updatetarget.Repositories {
	r := updatetarget.DefaultRepositories
	if v := os.Getenv("MOOSE_UPDATE_BRAIN_REPO"); v != "" {
		r.Brain = v
	}
	if v := os.Getenv("MOOSE_UPDATE_UI_REPO"); v != "" {
		r.UI = v
	}
	return r
}

// agentApplier adapts the agent's job surface to the loop's Applier seam. The
// loop wants a job id and an error; the agent hands back the whole record.
//
// Going through the agent rather than straight to cpupdate is deliberate: it is
// what puts a target-driven update under the same single job lock as an
// admin-triggered one (hostagent.Agent.StartUpdate).
type agentApplier struct{ a *hostagent.Agent }

func (ap agentApplier) StartUpdate(brainRef, uiRef string) (string, error) {
	job, err := ap.a.StartUpdate(brainRef, uiRef)
	if err != nil {
		return "", err
	}
	return job.ID, nil
}
