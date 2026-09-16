package updatetarget

import (
	"context"
	"fmt"

	"github.com/onmoose/moose/internal/hostagent/relmanifest"
)

// ManifestSource is the **appliance** implementation of Source: the target comes
// from the signed release manifest (RELEASE_MANIFEST.md), which host-agent
// already fetches, verifies and caches on an hourly poll
// (internal/hostagent/relmanifest). Nothing about that fetch or that
// verification changes here — this only reads what the poller last accepted.
//
// # Why it cannot produce a target today
//
// The manifest names **versions**, not images: `"brain": "1.4.2"`, with the
// registry path left implicit so the registry can move without re-cutting every
// signed file (RELEASE_MANIFEST.md # What the manifest is). Turning that into
// something pullable means composing `…/brain:v1.4.2` — a **tag** — and the box
// resolving a tag is the one thing this package refuses to do (see the package
// comment in target.go).
//
// So a verified, current manifest is answered with ErrNotPinned: a refusal,
// logged, leaving the box exactly where it is. That is not a workaround, it is
// the honest state of the appliance publisher — and today it is also academic,
// because no build has a signing key baked in, so the poller refuses every
// manifest and this source never sees one at all.
//
// Closing it is a manifest-schema change (optional pinned reference fields
// alongside the versions), which belongs to the appliance release pipeline and
// is tracked in NEXT.md rather than guessed at here.
type ManifestSource struct {
	// Poller is the release-manifest poller whose last verified manifest this
	// source reads. Nil means the box is not polling, which is ErrNoTarget.
	Poller ManifestPoller
}

// ManifestPoller is the slice of the release-manifest poller this source needs:
// the last manifest that verified. Consumer-side (CLAUDE.md # Go code
// discipline); the provider is *relmanifest.Poller.
type ManifestPoller interface {
	State() relmanifest.State
}

// Target reports the appliance target.
//
// No manifest — the box has never verified one, or has no signing key and so
// refuses them all — is ErrNoTarget: the source has nothing to offer, and the
// box keeps running what it runs. A manifest that IS present is refused as
// unpinned, for the reason in the type comment.
func (s ManifestSource) Target(context.Context) (Target, error) {
	if s.Poller == nil {
		return Target{}, ErrNoTarget
	}
	st := s.Poller.State()
	if !st.HasManifest {
		return Target{}, ErrNoTarget
	}
	return Target{}, fmt.Errorf("%w: the release manifest names versions (brain %s, ui %s), not pinned references",
		ErrNotPinned, st.Manifest.Brain, st.Manifest.UI)
}
