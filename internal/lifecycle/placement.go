package lifecycle

import (
	"errors"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// Three manifest flags limit where and how an app may run (APP_MANIFEST.md #
// B, # E2, TERMINAL_APPS.md # Security and access). Each is enforced here, in
// the transaction owner, so a direct API call cannot get past a hidden
// control.

// ErrPersonalOnly refuses a household instance of an app whose manifest sets
// personal_only.
var ErrPersonalOnly = errors.New("this app can only be installed just for you, not for the whole household")

// ErrHostedOnly refuses an app whose manifest sets hosted_only on a box that
// does not run the hosted profile.
var ErrHostedOnly = errors.New("this app runs only on a moose box in the cloud, so it cannot be installed on this box")

// ErrExposureLocked refuses to make public an app whose manifest sets
// access.owner_only.
var ErrExposureLocked = errors.New("this app is always private to you, so it cannot be made public")

// placementAllowed reports whether man may run as an instance of scope on a
// box with this profile. Install calls it, and the update check calls it with
// the new version's manifest.
func placementAllowed(man *manifest.Manifest, scope string, prof profile.Profile) error {
	if man.HostedOnly && prof != profile.Hosted {
		return ErrHostedOnly
	}
	if man.PersonalOnly && scope == store.ScopeHousehold {
		return ErrPersonalOnly
	}
	return nil
}

// ownerOnly reports whether the app's access mode is locked to restricted.
// The appliance has no gate, so the lock means nothing there.
func (m *Manager) ownerOnly(man *manifest.Manifest) bool {
	return man != nil && man.Access.OwnerOnly && m.hosted()
}
