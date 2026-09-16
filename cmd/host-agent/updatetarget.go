package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/onmoose/moose/internal/protocol"
)

// The pair the fake box "runs", and the pair a fake target offers. Both are
// pinned references in the production shape, because the one thing the box
// refuses in production is an answer that is not (UPDATES.md # 8.4). A dev
// payload that carried tags would let a dashboard be built against a shape no
// real box will ever send.
const (
	fakeRunningBrain = "ghcr.io/onmoose/brain@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fakeRunningUI    = "ghcr.io/onmoose/ui@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	fakeTargetBrain  = "ghcr.io/onmoose/brain@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	fakeTargetUI     = "ghcr.io/onmoose/ui@sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// fakeUpdateTarget builds the canned GET /v1/system/update-target report.
//
// The default is "the source has nothing to offer", which is the honest answer
// for a dev box: there is no control plane behind the inner loop to move to.
// MOOSE_FAKE_UPDATE_TARGET picks another state, because the point of this read
// is a dashboard surface and that surface has to be built against every state —
// including the three nobody would think to design for (refused, unreachable,
// disabled).
func fakeUpdateTarget() protocol.UpdateTarget {
	out := protocol.UpdateTarget{
		State:      protocol.UpdateTargetNone,
		Running:    protocol.ControlPlanePair{Brain: fakeRunningBrain, UI: fakeRunningUI},
		CheckedAt:  time.Now().UTC().Format(time.RFC3339),
		From:       "default",
		Window:     "03:00-04:00",
		WindowFrom: "default",
		Profile:    "appliance",
	}
	offer := &protocol.ControlPlaneOffer{
		Version:     "v0.8.0",
		BrainImage:  fakeTargetBrain,
		UIImage:     fakeTargetUI,
		PublishedAt: "2026-09-01T10:00:00Z",
	}

	want := os.Getenv("MOOSE_FAKE_UPDATE_TARGET")
	switch want {
	case "", protocol.UpdateTargetNone:
		return out
	case protocol.UpdateTargetAvailable:
		out.State, out.Target = protocol.UpdateTargetAvailable, offer
	case protocol.UpdateTargetCurrent:
		out.State, out.Target = protocol.UpdateTargetCurrent, offer
		out.Running = protocol.ControlPlanePair{Brain: fakeTargetBrain, UI: fakeTargetUI}
	case protocol.UpdateTargetRefused:
		out.State, out.Target = protocol.UpdateTargetRefused, offer
		out.Target.BrainImage, out.Target.UIImage = "ghcr.io/onmoose/brain:v0.8.0", "ghcr.io/onmoose/ui:v0.8.0"
		out.Detail = "updatetarget: image reference is not pinned to a digest: brain image \"ghcr.io/onmoose/brain:v0.8.0\""
	case protocol.UpdateTargetUnreachable:
		out.State = protocol.UpdateTargetUnreachable
		out.Detail = "updatetarget: fetch https://onmoose.network/api/updates/target: dial tcp: connection refused"
	case protocol.UpdateTargetDisabled:
		// No loop ever started, so nothing was checked and no URL was
		// resolved — both fields stay empty, as they do on a real box.
		out.State, out.CheckedAt, out.From = protocol.UpdateTargetDisabled, "", ""
		out.Detail = "seed update_target_url must be an http or https URL, got \"moose.example\""
	default:
		slog.Warn("host-agent (fake): unknown MOOSE_FAKE_UPDATE_TARGET; reporting none", "state", want)
	}
	return out
}
