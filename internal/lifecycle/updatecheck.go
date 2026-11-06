package lifecycle

// The app update check (UPDATES.md # 4, #631). The box learns of a new
// version of an installed app from the catalog, by version only: the browse
// payload carries no image digests, and a stored pin may hold an older index
// digest that must not read as a new version (APP_STORE.md). For each new
// version the check loads the new manifest once, sorts the update into a
// class, and stores the class on the instance, so a sync that changes nothing
// for an app does not fetch it again.
//
// The catalog cannot call into lifecycle (lifecycle reads the catalog), so
// cmd/brain asks the catalog to call CheckUpdates after each sync that swapped
// in a new payload (catalog.OnChange), and once at startup.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// CheckUpdates compares every installed instance's version with the
// catalog's and stores what it finds. One check runs at a time; a call while
// one runs makes it run once more when it ends, so no catalog change is
// missed. Errors are logged, and the next sync tries again.
func (m *Manager) CheckUpdates(ctx context.Context) {
	m.checkMu.Lock()
	if m.checkRunning {
		m.checkAgain = true
		m.checkMu.Unlock()
		return
	}
	m.checkRunning = true
	m.checkMu.Unlock()
	for {
		m.checkUpdatesOnce(ctx)
		m.checkMu.Lock()
		if !m.checkAgain {
			m.checkRunning = false
			m.checkMu.Unlock()
			return
		}
		m.checkAgain = false
		m.checkMu.Unlock()
	}
}

func (m *Manager) checkUpdatesOnce(ctx context.Context) {
	if m.catalogEntry == nil || m.catalogLoad == nil {
		return
	}
	insts, err := m.store.List()
	if err != nil {
		slog.Warn("update check: list instances", "err", err)
		return
	}
	if m.parkedSeen == nil {
		m.parkedSeen = map[string]string{}
	}
	base, baseRead := 0, false
	found := 0
	for _, inst := range insts {
		// A Door-2 app has no catalog entry and never updates. An app being
		// installed, updated or removed is left for the next check.
		if inst.Version == manifest.CustomVersion {
			continue
		}
		switch inst.State {
		case "installing", "uninstalling", "updating":
			continue
		}
		entry, err := m.catalogEntry(inst.ManifestID)
		if errors.Is(err, catalog.ErrNotFound) {
			// Not in the catalog (or the catalog has not synced yet): keep
			// what is stored. An Update clears it if it is gone for good.
			continue
		}
		if err != nil {
			slog.Warn("update check: read the catalog entry", "instance_id", inst.ID, "manifest_id", inst.ManifestID, "err", err)
			continue
		}
		if entry.Version == inst.Version {
			m.setUpdateCheck(inst, store.UpdateCheck{})
			continue
		}
		if inst.Update.Version == entry.Version && inst.Update.Status != "" {
			// This version is classified already. One parked for a newer
			// moose is looked at once more by each brain run: this brain
			// may be the newer moose.
			if inst.Update.Status != store.UpdateNeedsNewerMoose || m.parkedSeen[inst.ID] == entry.Version {
				continue
			}
		}
		newMan, _, err := m.catalogLoad(ctx, inst.ManifestID)
		if errors.Is(err, manifest.ErrUnsupportedManifestVersion) {
			m.parkedSeen[inst.ID] = entry.Version
			m.setUpdateCheck(inst, needsNewerMoose(entry.Version))
			found++
			continue
		}
		if err != nil {
			slog.Warn("update check: load the new version", "instance_id", inst.ID, "manifest_id", inst.ManifestID, "new_version", entry.Version, "err", err)
			continue
		}
		oldMan, err := m.loadInstanceManifest(inst.ID)
		if err != nil {
			slog.Warn("update check: read the installed manifest", "instance_id", inst.ID, "err", err)
			continue
		}
		if !baseRead {
			if _, base, err = m.hostIdentity(ctx); err != nil {
				slog.Warn("update check: read the remap; trying again at the next sync", "err", err)
				return
			}
			baseRead = true
		}
		grants, err := m.store.GetServiceGrants(inst.ID)
		if err != nil {
			slog.Warn("update check: read service grants", "instance_id", inst.ID, "err", err)
			continue
		}
		cfg, err := m.store.GetInstanceConfig(inst.ID)
		if err != nil {
			slog.Warn("update check: read config", "instance_id", inst.ID, "err", err)
			continue
		}
		check := classifyUpdate(inst, oldMan, newMan, grants, cfg, base)
		m.setUpdateCheck(inst, check)
		found++
		slog.Info("update found", "instance_id", inst.ID, "manifest_id", inst.ManifestID, "version", inst.Version, "new_version", check.Version, "update_class", check.Status)
	}
	if found > 0 {
		slog.Info("update check done", "count", found)
	}
}

func needsNewerMoose(version string) store.UpdateCheck {
	return store.UpdateCheck{Version: version, Status: store.UpdateNeedsNewerMoose,
		Reason: "This version needs a newer moose. It will be offered once moose has updated itself"}
}

// classifyUpdate sorts an update into its class (UPDATES.md # 4). The order
// is: a refusal (another isolation tier or identity, a new required setting
// moose cannot fill in), then a managed-service major move (#635), then wider
// access (#633). Anything else can be applied.
func classifyUpdate(inst store.Instance, oldMan, newMan *manifest.Manifest, grants []store.ServiceGrant, cfg []store.InstanceConfig, base int) store.UpdateCheck {
	c := store.UpdateCheck{Version: newMan.Version}
	blocked := func(reason string) store.UpdateCheck {
		c.Status, c.Reason = store.UpdateBlocked, reason
		return c
	}

	// The tier and the runtime identity are fixed for the life of an instance
	// (APP_ISOLATION.md # User-namespace tiers).
	// service_user too: it swaps the identity that owns the data.
	if err := tierKept(inst, newMan, base); err != nil {
		return blocked(err.Error())
	}
	if newMan.ServiceUser != oldMan.ServiceUser {
		return blocked(ErrTierChange.Error())
	}

	// A new required setting with no default that the instance has no value
	// for: moose cannot make one up.
	have := map[string]bool{}
	for _, v := range cfg {
		have[v.AppEnv] = true
	}
	for _, f := range newMan.Config {
		if f.Required && f.Default == "" && !have[f.AppEnv] {
			name := f.Title
			if name == "" {
				name = f.AppEnv
			}
			return blocked(fmt.Sprintf("This version needs a new setting, %q, that moose cannot fill in for you. To use it, uninstall the app and install it again", name))
		}
	}

	// A managed service that moves to another version needs its data moved
	// (#635). A new one is fine: the update provisions it.
	for _, g := range grants {
		dep, ok := newMan.Services[g.LogicalName]
		if !ok {
			continue
		}
		kind, version := normalizeEngine(dep.Type, dep.Version)
		if kind != g.Kind || version != g.Version {
			c.Status = store.UpdateServiceMajor
			c.Reason = fmt.Sprintf("This version needs %s %s for its data, and the app has %s %s. moose cannot move the data to a new database version yet", kind, version, g.Kind, g.Version)
			return c
		}
	}

	if wider := widenedAccess(oldMan, newMan); len(wider) > 0 {
		c.Status = store.UpdateNeedsApproval
		c.Reason = "This version asks for more access: " + strings.Join(wider, ", ") + ". Updates that ask for more access are not applied yet"
		return c
	}
	c.Status = store.UpdateAvailable
	return c
}

// widenedAccess lists, in plain words, what the new manifest grants that the
// old one does not (UPDATES.md # Auto-apply unless permissions expand): a new
// permission key or a wider value, and a new access.public_paths entry. Empty
// means the same or narrower.
func widenedAccess(oldMan, newMan *manifest.Manifest) []string {
	o, n := oldMan.Permissions, newMan.Permissions
	var out []string
	if n.Internet && !o.Internet {
		out = append(out, "the internet")
	}
	if n.LAN && !o.LAN {
		out = append(out, "your home network")
	}
	if n.GPU && !o.GPU {
		out = append(out, "the graphics card")
	}
	for _, d := range n.Devices {
		if !slices.Contains(o.Devices, d) {
			out = append(out, "the device "+d)
		}
	}
	oldFolders := map[string]manifest.Folder{}
	for _, f := range o.Folders {
		oldFolders[f.Folder] = f
	}
	for _, f := range n.Folders {
		name := folderDir[f.Folder]
		if name == "" {
			name = f.Folder
		}
		was, ok := oldFolders[f.Folder]
		switch {
		case !ok:
			out = append(out, "your "+name+" folder")
		case f.Mode == "write" && was.Mode != "write":
			out = append(out, "changes to your "+name+" folder")
		case f.Scope != "pick-subfolder" && was.Scope == "pick-subfolder":
			out = append(out, "all of your "+name+" folder")
		}
	}
	for _, p := range newMan.Access.PublicPaths {
		if !slices.Contains(oldMan.Access.PublicPaths, p) {
			out = append(out, "anyone can open "+p)
		}
	}
	return out
}
