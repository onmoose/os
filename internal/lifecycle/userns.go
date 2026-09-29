package lifecycle

// The user-namespace tiers (APP_ISOLATION.md # User-namespace tiers). On a
// Docker daemon with a daemon-wide userns-remap, every app container the brain
// launches is in one of three tiers:
//
//   - default: remapped, today's sandbox, a pinned user:. Data is owned by
//     base+uid.
//   - caps: remapped, five capabilities back, no user: pin, for a root_setup
//     app. Data is owned by base.
//   - host: userns_mode: host, today's sandbox, real host owners. For an app
//     with a folders grant, gpu: true or devices.
//
// On a daemon with no remap every container is in the host user namespace
// anyway, so every instance is stored as the host tier and the override is the
// same as before the tiers existed.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// capsTierCaps is the fixed set a caps-tier container gets back. Inside a
// remapped namespace they are powers over the remap range only.
var capsTierCaps = []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER"}

// remapIDCount is the smallest range host-agent accepts for moose-remap
// (usermgr.minRemapCount). An in-container id at or above it has no host id
// in the range, so the brain refuses to give a bind dir that owner.
const remapIDCount = 65536

// ErrRootSetupNeedsRemap refuses a root_setup install on a daemon with no
// remap. There the five capabilities would be real host capabilities.
var ErrRootSetupNeedsRemap = errors.New("this app needs to start as root inside its own user namespace, and Docker on this box does not give apps one. It cannot be installed on this box")

// ErrRemapMismatch refuses every install while Docker and host-agent disagree
// about the remap. The brain cannot know which owner to give a bind dir.
var ErrRemapMismatch = errors.New("Docker and the host disagree about how app containers are isolated, so no app can be installed until they agree. Restart the box, and ask for help if this message stays")

// ErrTierChange refuses an update whose new manifest would move the instance
// to another tier: its data would need a different owner.
var ErrTierChange = errors.New("this version of the app needs a different kind of isolation, so its data would need a new owner. To use it, uninstall the app and install it again")

// hostIdentity reads the well-known host identities and the remap, and checks
// that Docker and host-agent agree about the remap. base is 0 when the box runs
// no remap. The base only ever comes from host-agent, never from an env var.
func (m *Manager) hostIdentity(ctx context.Context) (protocol.WellKnownIdentityResponse, int, error) {
	wk, err := m.host.WellKnownIdentity(ctx)
	if err != nil {
		return wk, 0, fmt.Errorf("read the host identities: %w", err)
	}
	dockerRemap, err := m.docker.UsernsRemap(ctx)
	if err != nil {
		return wk, 0, fmt.Errorf("read the Docker security options: %w", err)
	}
	base := 0
	if wk.RemapBase != nil {
		base = *wk.RemapBase
	}
	if base < 0 || dockerRemap != (base > 0) {
		slog.Warn("docker and host-agent disagree about the userns remap; refusing installs",
			"remap_base", base, "docker_userns", dockerRemap)
		return wk, 0, ErrRemapMismatch
	}
	return wk, base, nil
}

// pickTier is the tier an app gets on this daemon. The brain picks it from the
// manifest's grants, never from a manifest or compose field naming the tier.
// A root_setup app is refused on a daemon with no remap. admission refuses
// root_setup together with a host-tier grant before this runs, and the host
// tier is checked first anyway, so a caps tier never has a host grant.
func pickTier(man *manifest.Manifest, base int) (string, error) {
	if base == 0 {
		if man.RootSetup {
			return "", ErrRootSetupNeedsRemap
		}
		return store.UsernsTierHost, nil
	}
	p := man.Permissions
	if len(p.Folders) > 0 || p.GPU || len(p.Devices) > 0 {
		return store.UsernsTierHost, nil
	}
	if man.RootSetup {
		return store.UsernsTierCaps, nil
	}
	return store.UsernsTierDefault, nil
}

// checkTierKept is the refusal the app update path runs before it writes a new
// override (APP_LIFECYCLE.md # Locked: update + rollback): the tier is fixed
// for the life of an instance, because the owner of its data follows it. The
// update path is not built yet; it must call this with the new manifest.
func (m *Manager) checkTierKept(ctx context.Context, inst store.Instance, man *manifest.Manifest) error {
	_, base, err := m.hostIdentity(ctx)
	if err != nil {
		return err
	}
	tier, err := pickTier(man, base)
	if err != nil {
		return err
	}
	if tier != inst.UsernsTier {
		slog.Warn("update refused: it would change the userns tier",
			"instance_id", inst.ID, "manifest_id", man.ID, "tier", inst.UsernsTier, "new_tier", tier)
		return ErrTierChange
	}
	return nil
}

// bindOwner is the host owner of a private bind dir for the instance's tier.
// uid and gid are the ids the container runs as (the user: pin).
func (it *isolation) bindOwner() (uid, gid int, err error) {
	switch {
	case it.remapBase == 0 || it.tier == store.UsernsTierHost:
		return it.uid, it.gid, nil
	case it.tier == store.UsernsTierCaps:
		// The container's own root. The image's entrypoint gives the dir to
		// its user from there.
		return it.remapBase, it.remapBase, nil
	default:
		if it.uid < 0 || it.gid < 0 || it.uid >= remapIDCount || it.gid >= remapIDCount {
			return 0, 0, fmt.Errorf("container identity %d:%d is outside the remap range", it.uid, it.gid)
		}
		return it.remapBase + it.uid, it.remapBase + it.gid, nil
	}
}

// applyTier edits one service's override entry for the instance's tier. It
// writes nothing on a daemon with no remap, so the override stays what it was
// before the tiers. It never writes cap_add together with userns_mode: host:
// in the host namespace the capabilities would be real root's.
func (it *isolation) applyTier(entry map[string]any) error {
	if it.remapBase == 0 {
		if it.tier == store.UsernsTierCaps {
			return errors.New("caps tier on a daemon with no remap")
		}
		return nil
	}
	switch it.tier {
	case store.UsernsTierHost:
		entry["userns_mode"] = "host"
	case store.UsernsTierCaps:
		entry["cap_add"] = capsTierCaps
		delete(entry, "user")
	case store.UsernsTierDefault:
	default:
		return fmt.Errorf("unknown userns tier %q", it.tier)
	}
	_, host := entry["userns_mode"]
	_, caps := entry["cap_add"]
	if host && caps {
		return errors.New("refusing to write cap_add with userns_mode: host")
	}
	return nil
}
