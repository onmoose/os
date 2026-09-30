package lifecycle

// The user-namespace tiers (APP_ISOLATION.md # User-namespace tiers). On a
// Docker daemon with a daemon-wide userns-remap, every app container the brain
// launches is in one of four tiers:
//
//   - default: remapped, today's sandbox, a pinned user:. Data is owned by
//     base+uid.
//   - caps: remapped, five capabilities back, no user: pin, for a root_setup
//     app. Data is owned by base.
//   - image: remapped, no capability back, no user: pin, for an image_user
//     app, so the image's own USER applies. Each service's data is owned by
//     base plus the ids its image runs as (imageuser.go).
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
	"sort"

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

// ErrImageUserNeedsRemap refuses an image_user install on a daemon with no
// remap. There the uid the image names is a real host uid, and it could be a
// real host account's.
var ErrImageUserNeedsRemap = errors.New("this app must run as the user its image names, and that is safe only when Docker on this box gives apps their own user namespace. Docker on this box does not, so this app cannot be installed on this box")

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
// A root_setup or image_user app is refused on a daemon with no remap.
// admission refuses both together with a host-tier grant, and with each
// other, before this runs, and the host tier is checked first anyway, so a
// caps or image tier never has a host grant.
func pickTier(man *manifest.Manifest, base int) (string, error) {
	if base == 0 {
		if man.RootSetup {
			return "", ErrRootSetupNeedsRemap
		}
		if man.ImageUser {
			return "", ErrImageUserNeedsRemap
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
	if man.ImageUser {
		return store.UsernsTierImage, nil
	}
	return store.UsernsTierDefault, nil
}

// checkTierKept is the refusal the app update path runs before it writes a new
// override (APP_LIFECYCLE.md # Locked: update + rollback): the tier is fixed
// for the life of an instance, because the owner of its data follows it. The
// update path is not built yet; it must call this with the new manifest. For
// the image tier the tier alone is not enough: the owner of each bind dir
// follows the new image's user too, so the update path must also resolve the
// new images' users (resolveImageUsers) and refuse a change.
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
// uid and gid are the ids the container runs as (the user: pin). The image
// tier has no single owner, since each service runs as its own image's user:
// use bindDirOwners.
func (it *isolation) bindOwner() (uid, gid int, err error) {
	switch {
	case it.remapBase == 0 || it.tier == store.UsernsTierHost:
		return it.uid, it.gid, nil
	case it.tier == store.UsernsTierCaps:
		// The container's own root. The image's entrypoint gives the dir to
		// its user from there.
		return it.remapBase, it.remapBase, nil
	case it.tier == store.UsernsTierImage:
		return 0, 0, errors.New("the image tier has an owner per service")
	default:
		return remappedOwner(it.remapBase, it.uid, it.gid)
	}
}

// remappedOwner is the host owner of in-container uid:gid on a remapped
// daemon: base+uid:base+gid, refused for an id the range cannot hold.
func remappedOwner(base, uid, gid int) (int, int, error) {
	if uid < 0 || gid < 0 || uid >= remapIDCount || gid >= remapIDCount {
		return 0, 0, fmt.Errorf("container identity %d:%d is outside the remap range", uid, gid)
	}
	return base + uid, base + gid, nil
}

// hostOwner is one bind dir's host owner.
type hostOwner struct{ uid, gid int }

// bindDirOwners gives every private bind dir its host owner. dirsBySvc is
// each service's relative bind dirs (bindDirsByService). In every tier but
// the image tier all dirs get bindOwner. In the image tier a dir goes to base
// plus the ids of the image user of the service that binds it, and a dir that
// two services with different image users bind is refused: it can have only
// one owner.
func (it *isolation) bindDirOwners(dirsBySvc map[string][]string) (map[string]hostOwner, error) {
	owners := map[string]hostOwner{}
	if it.remapBase == 0 || it.tier != store.UsernsTierImage {
		uid, gid, err := it.bindOwner()
		if err != nil {
			return nil, err
		}
		for _, dirs := range dirsBySvc {
			for _, d := range dirs {
				owners[d] = hostOwner{uid, gid}
			}
		}
		return owners, nil
	}
	svcs := make([]string, 0, len(dirsBySvc))
	for svc := range dirsBySvc {
		svcs = append(svcs, svc)
	}
	sort.Strings(svcs)
	firstSvc := map[string]string{} // dir → the service that set its owner
	for _, svc := range svcs {
		ids, ok := it.imageIDs[svc]
		if !ok {
			return nil, fmt.Errorf("no image user resolved for service %q", svc)
		}
		uid, gid, err := remappedOwner(it.remapBase, ids.uid, ids.gid)
		if err != nil {
			return nil, err
		}
		for _, d := range dirsBySvc[svc] {
			own := hostOwner{uid, gid}
			if prev, ok := owners[d]; ok && prev != own {
				return nil, fmt.Errorf("the services %q and %q both use the folder ./%s, but their images run as different users, so the folder cannot belong to both. This app cannot be installed", firstSvc[d], svc, d)
			}
			owners[d] = own
			if _, ok := firstSvc[d]; !ok {
				firstSvc[d] = svc
			}
		}
	}
	return owners, nil
}

// applyTier edits one service's override entry for the instance's tier. It
// writes nothing on a daemon with no remap, so the override stays what it was
// before the tiers. It never writes cap_add together with userns_mode: host:
// in the host namespace the capabilities would be real root's. It never
// drops the user: pin with userns_mode: host either: in the host namespace
// the image's own uid would be a real host uid.
func (it *isolation) applyTier(entry map[string]any) error {
	if it.remapBase == 0 {
		if it.tier == store.UsernsTierCaps || it.tier == store.UsernsTierImage {
			return fmt.Errorf("%s tier on a daemon with no remap", it.tier)
		}
		return nil
	}
	switch it.tier {
	case store.UsernsTierHost:
		entry["userns_mode"] = "host"
	case store.UsernsTierCaps:
		entry["cap_add"] = capsTierCaps
		delete(entry, "user")
	case store.UsernsTierImage:
		// No capability back and no user: pin, so the image's own USER
		// applies, remapped.
		delete(entry, "user")
	case store.UsernsTierDefault:
	default:
		return fmt.Errorf("unknown userns tier %q", it.tier)
	}
	_, host := entry["userns_mode"]
	_, caps := entry["cap_add"]
	_, user := entry["user"]
	if host && caps {
		return errors.New("refusing to write cap_add with userns_mode: host")
	}
	if host && !user {
		return errors.New("refusing to write userns_mode: host with no user: pin")
	}
	return nil
}
