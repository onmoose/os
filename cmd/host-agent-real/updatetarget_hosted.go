//go:build hosted

package main

import (
	"log/slog"

	"github.com/onmoose/moose/internal/hostagent/relmanifest"
	"github.com/onmoose/moose/internal/hostagent/updatetarget"
	"github.com/onmoose/moose/internal/profile"
)

// buildProfile is the environment profile this binary was built for. It is a
// build-time fact, not a source-time one, so the disabled path can still report
// it after updateTargetSource has refused to produce a source at all.
const buildProfile = string(profile.Hosted)

// updateTargetSource is the **hosted** half of the update-target seam: the box
// reads its target from the cloud over its existing outbound path (UPDATES.md
// # 8.1 — the box asks, nothing connects in), and applies it without a prompt
// (# 8.2 — we operate the box, so an unpatched one is our liability).
//
// The URL is configuration, not a constant, and it is settable **per box at
// provision time** through the seed's update_target_url field (updateconfig.go,
// UPDATES.md # 8.4). That is how one box proves a release before the fleet gets
// it: a hosted box has no SSH, and the seed is the only channel a real box has
// for a per-box fact. MOOSE_UPDATE_TARGET_URL stays underneath it as the local
// hand-edit the boot proof uses. Empty means the control plane's public endpoint.
//
// **The box says who it is.** The seed's box_id goes out as a query parameter,
// which is what lets the control plane answer for this box and not for the whole
// fleet (UPDATES.md # 8.1). A box with no box id asks anonymously and gets
// whatever the endpoint serves everyone, exactly as every box did before. The
// identity is a bare id on an unauthenticated endpoint and is deliberately weak;
// the trade-off is written down in UPDATES.md # 8.1 and the real credential is
// parked in NEXT.md.
//
// An unusable seeded target, and a seed that will not parse, are returned as an
// error rather than resolved away, so the caller can refuse instead of quietly
// reading the fleet default.
//
// The release-manifest poller is unused here and is always nil on this build:
// a hosted box has one opinion about its target, and a signed public broadcast
// would be a second.
func updateTargetSource(*relmanifest.Poller) (targetSource, error) {
	target, from, boxID, err := updateTarget()
	if err != nil {
		return targetSource{}, err
	}
	shown := target
	if shown == "" {
		shown = updatetarget.DefaultURL
	}
	// Redacted: an operator-set URL may carry credentials, and this line is the
	// one place the whole URL is written out.
	shown = updatetarget.RedactURL(shown)
	if from == fromDefault {
		slog.Info("update target resolved", "url", shown, "from", from, "box_id", boxID)
	} else {
		// Loud on purpose. "This box is not following the fleet" is the first
		// thing anyone needs to know when one box behaves unlike the rest, and
		// it has to be visible without knowing to go looking for it.
		slog.Warn("this box is not following the fleet update target", "url", shown, "from", from, "box_id", boxID)
	}
	return targetSource{
		Source:    updatetarget.HTTPSource{URL: target, BoxID: boxID},
		AutoApply: true,
		Profile:   buildProfile,
		From:      from,
	}, nil
}
