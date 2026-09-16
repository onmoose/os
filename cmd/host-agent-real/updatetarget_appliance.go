//go:build !hosted

package main

import (
	"github.com/onmoose/moose/internal/hostagent/relmanifest"
	"github.com/onmoose/moose/internal/hostagent/updatetarget"
	"github.com/onmoose/moose/internal/profile"
)

// buildProfile is the environment profile this binary was built for. See the
// hosted half for why it is separate from the source.
const buildProfile = string(profile.Appliance)

// updateTargetSource is the **appliance** half of the update-target seam: the
// target comes from the signed release manifest this box already polls
// (RELEASE_MANIFEST.md), read straight off that poller.
//
// autoApply is false, and that is the locked # 8.2 difference between the
// profiles rather than an oversight: on an appliance the control plane is
// admin-prompted (UPDATES.md # 3), so the box learns what it could run and a
// human decides. Only a box we operate patches itself.
//
// The manifest names versions, not pinned references, so this source cannot
// produce an applicable target today — see updatetarget.ManifestSource.
//
// The error is always nil here: the appliance source is the poller this box
// already runs, so there is no per-box configuration to get wrong. It exists so
// the hosted half can refuse an unusable seeded update target.
//
// From is left empty: an appliance has no update-target URL to have a source
// for. Its target comes from the signed manifest this box already polls.
func updateTargetSource(p *relmanifest.Poller) (targetSource, error) {
	return targetSource{
		Source:  updatetarget.ManifestSource{Poller: p},
		Profile: buildProfile,
	}, nil
}
