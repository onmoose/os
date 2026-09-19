// Package profile defines the moose environment-profile marker and reads it.
//
// moose ships in two environment profiles (ENVIRONMENT.md # The two profiles):
// the bare-metal "appliance" a user installs on their own hardware, and the
// moose-operated "hosted" cloud VM. ~95% of the brain (Layer 1) is identical
// across both profiles; only a handful of narrow seams branch on the profile
// (ENVIRONMENT.md # Two layers, treated differently). This package is the single
// source of that signal: it reads the runtime marker the image stamps and
// resolves it to a typed Profile those seams consult.
//
// The marker is a file (default /etc/moose/profile) whose sole content is the
// profile name. An absent, unreadable, or unrecognized marker all resolve to
// Appliance — the no-op default — so an unmarked box (and `make dev`) behaves
// exactly as today (ENVIRONMENT.md # How the profile is realized, "A runtime
// marker").
//
// No behavioral seam consults this yet; each hosted behavior branches on the
// resolved Profile when its own feature lands.
package profile

import (
	"log/slog"
	"os"
	"strings"
)

// Profile is the environment moose is built and configured for.
type Profile string

const (
	// Appliance is the bring-your-own x86 box on the user's LAN — the default
	// every existing spec describes, and what an unmarked box resolves to.
	Appliance Profile = "appliance"
	// Hosted is the moose-operated cloud VM, one per tenant.
	Hosted Profile = "hosted"
)

// DefaultMarkerPath is where the image stamps the profile marker. The brain
// makes it overridable via MOOSE_PROFILE_PATH for tests and `make dev`.
const DefaultMarkerPath = "/etc/moose/profile"

// Read resolves the profile from the marker at path. It never fails: an absent
// marker (the unmarked appliance box), an unreadable one, or unrecognized
// content all resolve to Appliance, the no-op default. Surrounding whitespace
// and a trailing newline are tolerated.
//
// An absent marker is the silent, expected case for an appliance box, so it is
// not logged. An existing-but-unreadable marker and a present-but-unrecognized
// value are both warn-logged: they signal a mis-stamped or mis-permissioned
// image rather than the default, and degrading to appliance could hide a hosted
// box running in the wrong posture.
func Read(path string) Profile {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("profile marker unreadable; defaulting to appliance", "err", err)
		}
		return Appliance
	}
	switch p := Profile(strings.TrimSpace(string(b))); p {
	case Appliance, Hosted:
		return p
	default:
		slog.Warn("unrecognized profile marker; defaulting to appliance", "profile", string(p))
		return Appliance
	}
}
