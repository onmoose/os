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
// cmd/brain asks the catalog to call CheckUpdates after every sync that
// reached the catalog service (catalog.OnSync), and once at startup. A version
// already classified is skipped, so a sync that changed nothing costs no
// manifest fetch, and a check that failed is tried again at the next sync.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

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
			// may be the newer moose. One that waits for an approval with
			// no list of what it asks for was classified by a brain from
			// before #633, and is classified again for the list.
			parked := inst.Update.Status == store.UpdateNeedsNewerMoose && m.parkedSeen[inst.ID] != entry.Version
			unlisted := inst.Update.Status == store.UpdateNeedsApproval && len(inst.Update.Wants) == 0
			if !parked && !unlisted {
				continue
			}
		}
		newMan, _, err := m.catalogLoad(ctx, inst.ManifestID)
		if errors.Is(err, manifest.ErrUnsupportedManifestVersion) {
			m.parkedSeen[inst.ID] = entry.Version
			if m.storeCheck(inst, needsNewerMoose(entry.Version)) {
				found++
			}
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
		sources, err := m.instanceFolderSources(inst.ID)
		if err != nil {
			slog.Warn("update check: read the folder binds", "instance_id", inst.ID, "err", err)
			continue
		}
		check := classifyUpdate(inst, oldMan, newMan, grants, cfg, base, sources)
		if !m.storeCheck(inst, check) {
			continue
		}
		found++
		slog.Info("update found", "instance_id", inst.ID, "manifest_id", inst.ManifestID, "version", inst.Version, "new_version", check.Version, "update_class", check.Status)
	}
	if found > 0 {
		slog.Info("update check done", "count", found)
	}
}

// storeCheck stores a check's result for inst, read before the check loaded
// the new manifest. The check takes no lock while it loads, so an update or an
// uninstall may have run since. storeCheck takes the instance's lock only if it
// is free, reads the row again, and stores nothing when the row moved on: an
// operation holds the lock, the row is gone, the installed version changed, or
// the app is now being installed, updated or removed, or the owner's
// approval changed (the check was made without it, #633). The next sync
// checks again. It reports whether it stored the result.
func (m *Manager) storeCheck(inst store.Instance, check store.UpdateCheck) bool {
	unlock, ok := m.tryLockInstance(inst.ID)
	if !ok {
		return false
	}
	defer unlock()
	cur, err := m.store.Get(inst.ID)
	if err != nil || cur.Version != inst.Version || !cur.UpdateApproval.Same(inst.UpdateApproval) {
		return false
	}
	switch cur.State {
	case "installing", "uninstalling", "updating":
		return false
	}
	m.setUpdateCheck(cur, check)
	return true
}

func needsNewerMoose(version string) store.UpdateCheck {
	return store.UpdateCheck{Version: version, Status: store.UpdateNeedsNewerMoose,
		Reason: "This version needs a newer moose. It will be offered once moose has updated itself"}
}

// classifyUpdate sorts an update into its class (UPDATES.md # 4). The order
// is: a refusal (another isolation tier or identity, a new required setting
// moose cannot fill in), then a managed-service major move (#635), then wider
// access that the owner has not accepted (#633). Anything else can be applied.
//
// The access the new version asks for (Wants) is worked out first and stored
// with every class, so a service-major update that also asks for more access
// says so. sources is where each folder the instance binds now comes from
// (instanceFolderSources). An update that asks for more access is available
// when the owner's approval on the row covers it (approvalCovers).
func classifyUpdate(inst store.Instance, oldMan, newMan *manifest.Manifest, grants []store.ServiceGrant, cfg []store.InstanceConfig, base int, sources map[string]string) store.UpdateCheck {
	c := store.UpdateCheck{Version: newMan.Version, Wants: widenedAccess(accessOf(oldMan), newMan, sources, inst.Scope)}
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

	if len(c.Wants) > 0 && !approvalCovers(inst, newMan, sources) {
		c.Status = store.UpdateNeedsApproval
		c.Reason = needsApprovalReason(inst.Scope)
		return c
	}
	c.Status = store.UpdateAvailable
	return c
}

// needsApprovalReason is the sentence an update that asks for more access
// carries. Only the owner of a personal app may allow it, and an admin for a
// household app (UPDATES.md # Who gets prompted).
func needsApprovalReason(scope string) string {
	if scope == store.ScopeHousehold {
		return "This version asks for more access. It waits until an admin allows it"
	}
	return "This version asks for more access. It waits until its owner allows it"
}

// grantedAccess is the access an app has, or the access an owner accepted:
// the manifest's permissions block and its public paths. Its JSON is what an
// approval stores (store.UpdateApproval.Access).
type grantedAccess struct {
	Permissions manifest.Permissions `json:"permissions"`
	PublicPaths []string             `json:"public_paths,omitempty"`
}

func accessOf(man *manifest.Manifest) grantedAccess {
	return grantedAccess{Permissions: man.Permissions, PublicPaths: man.Access.PublicPaths}
}

// approvalCovers reports whether the owner's approval on the row covers the
// new version: it asks for no more than the access the owner accepted. So a
// later version that asks for even more asks again, and one that asks for no
// more (v2 allowed, v2 rolled back, v3 with v2's access) does not.
func approvalCovers(inst store.Instance, newMan *manifest.Manifest, sources map[string]string) bool {
	if inst.UpdateApproval.Access == "" {
		return false
	}
	var ok grantedAccess
	if err := json.Unmarshal([]byte(inst.UpdateApproval.Access), &ok); err != nil {
		slog.Warn("update check: the stored approval cannot be read; asking again", "instance_id", inst.ID, "err", err)
		return false
	}
	return len(widenedAccess(ok, newMan, sources, inst.Scope)) == 0
}

// widenedAccess lists what the new manifest grants that had does not
// (UPDATES.md # Auto-apply unless permissions expand): a new permission key or
// a wider value, and a new access.public_paths entry. Empty means the same or
// narrower. The list is data, not words: the dashboard says each item in the
// install screen's words.
//
// A folder item names the source the update binds: the one the instance binds
// now (sources), or for a new folder the one install picks by default, the
// owner's own folder for a personal app and the household's shared folder for
// a household app (mountsFromOverride binds the same).
func widenedAccess(had grantedAccess, newMan *manifest.Manifest, sources map[string]string, scope string) []store.AccessWant {
	o, n := had.Permissions, newMan.Permissions
	var out []store.AccessWant
	if n.Internet && !o.Internet {
		out = append(out, store.AccessWant{Kind: store.WantInternet})
	}
	if n.LAN && !o.LAN {
		out = append(out, store.AccessWant{Kind: store.WantLAN})
	}
	if n.GPU && !o.GPU {
		out = append(out, store.AccessWant{Kind: store.WantGPU})
	}
	for _, d := range n.Devices {
		if !slices.Contains(o.Devices, d) {
			out = append(out, store.AccessWant{Kind: store.WantDevice, Device: d})
		}
	}
	oldFolders := map[string]manifest.Folder{}
	for _, f := range o.Folders {
		oldFolders[f.Folder] = f
	}
	for _, f := range n.Folders {
		mode := f.Mode
		if mode != "write" {
			mode = "read"
		}
		src := sources[f.Folder]
		if src == "" {
			src = defaultFolderSource(scope)
		}
		w := store.AccessWant{Kind: store.WantFolder, Folder: f.Folder, Mode: mode, Source: src}
		was, ok := oldFolders[f.Folder]
		switch {
		case !ok:
			w.Change = store.FolderNew
		case mode == "write" && was.Mode != "write":
			w.Change = store.FolderWrite
		case f.Scope != "pick-subfolder" && was.Scope == "pick-subfolder":
			w.Change = store.FolderWhole
		default:
			continue
		}
		out = append(out, w)
	}
	for _, p := range newMan.Access.PublicPaths {
		if !slices.Contains(had.PublicPaths, p) {
			out = append(out, store.AccessWant{Kind: store.WantPublicPath, Path: p})
		}
	}
	return out
}

// defaultFolderSource is the folder source install picks by default for a
// scope: the owner's own folder for a personal app, the household's shared
// folder for a household app (api folderSourceMenu).
func defaultFolderSource(scope string) string {
	if scope == store.ScopeHousehold {
		return sourceShared
	}
	return sourcePersonal
}

// instanceFolderSources is where each folder an instance binds comes from,
// read back from its override: personal or shared, by folder. An instance
// with no override yet has none.
func (m *Manager) instanceFolderSources(id string) (map[string]string, error) {
	override, err := os.ReadFile(filepath.Join(m.instanceDir(id), "compose.override.yml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	binds, err := folderBinds(override, m.sharedRoot)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(binds))
	for f, b := range binds {
		out[f] = b.Source
	}
	return out, nil
}

// UpdateRefusal is a refusal of an approval whose text is a plain sentence
// the dashboard can show as it is (ApproveUpdate). The API answers 409 with
// it.
type UpdateRefusal struct{ msg string }

func (e *UpdateRefusal) Error() string { return e.msg }

// Refusals of an approval (ApproveUpdate).
var (
	// ErrNothingToApprove: the app has no update that waits for an approval.
	ErrNothingToApprove error = &UpdateRefusal{"This app has no update that waits for an approval."}
	// ErrApproveStale: the version the person saw is not the one the box
	// offers now.
	ErrApproveStale error = &UpdateRefusal{"A newer version of this app came out. Look again at what it asks for."}
	// ErrApproveChanged: the app store changed what the version asks for
	// since the person looked.
	ErrApproveChanged error = &UpdateRefusal{"What this version asks for changed in the app store. Look again at what it asks for."}
	// ErrApproveBusy: another change of the app is running.
	ErrApproveBusy error = &UpdateRefusal{"The app is busy with another change. Try again in a moment."}
)

// ApproveUpdate stores the owner's yes to the update the box offers for an
// instance, which asks for more access (UPDATES.md # 6 The
// permission-expansion prompt, #633). version is the version the person saw.
// The caller checks who may approve, and then runs the update at once
// (UpdateByPerson), the same as a person's Update.
//
// Under the instance's lock it refuses (an *UpdateRefusal) when the app is
// not running, when the stored update does not wait for an approval, when its
// version is not version, and when the app store's manifest now asks for
// something else than what the person saw (the new list is stored, so the
// next look shows it).
// Otherwise it stores the approval, with the access the new manifest asks for,
// and the update's class becomes available. It returns what the version asks
// for, for the audit record.
//
// The lock is let go before the update takes it again. An update that starts
// in between (the nightly window) is classified again under its own lock, and
// the approval covers only the access the person accepted, so it can only
// apply what they allowed.
func (m *Manager) ApproveUpdate(ctx context.Context, id, version, by string) ([]store.AccessWant, error) {
	unlock, ok := m.tryLockInstance(id)
	if !ok {
		return nil, ErrApproveBusy
	}
	defer unlock()
	inst, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}
	if inst.State != "running" {
		return nil, &UpdateRefusal{"Start the app before you update it."}
	}
	if inst.Update.Status != store.UpdateNeedsApproval {
		return nil, ErrNothingToApprove
	}
	if inst.Update.Version != version {
		return nil, ErrApproveStale
	}
	entry, err := m.catalogEntry(inst.ManifestID)
	if err != nil {
		slog.Warn("approve update: read the catalog entry", "instance_id", id, "manifest_id", inst.ManifestID, "err", err)
		return nil, &UpdateRefusal{"moose could not read the app store. Try again later."}
	}
	if entry.Version != version {
		return nil, ErrApproveStale
	}
	newMan, _, err := m.catalogLoad(ctx, inst.ManifestID)
	if err != nil {
		slog.Warn("approve update: load the new version", "instance_id", id, "manifest_id", inst.ManifestID, "new_version", version, "err", err)
		return nil, &UpdateRefusal{"moose could not download the new version from the app store. Try again later."}
	}
	if newMan.Version != version {
		return nil, ErrApproveStale
	}
	oldMan, err := m.loadInstanceManifest(id)
	if err != nil {
		return nil, fmt.Errorf("read the app's manifest: %w", err)
	}
	_, base, err := m.hostIdentity(ctx)
	if err != nil {
		return nil, err
	}
	grants, err := m.store.GetServiceGrants(id)
	if err != nil {
		return nil, err
	}
	cfg, err := m.store.GetInstanceConfig(id)
	if err != nil {
		return nil, err
	}
	sources, err := m.instanceFolderSources(id)
	if err != nil {
		return nil, fmt.Errorf("read the app's folders: %w", err)
	}
	// Classify again as if no approval were stored: the person approves
	// what the version asks for now.
	fresh := inst
	fresh.UpdateApproval = store.UpdateApproval{}
	check := classifyUpdate(fresh, oldMan, newMan, grants, cfg, base, sources)
	if check.Status != store.UpdateNeedsApproval {
		m.setUpdateCheck(inst, check)
		if check.Reason != "" {
			return nil, &UpdateRefusal{check.Reason + "."}
		}
		return nil, ErrNothingToApprove
	}
	if !slices.Equal(check.Wants, inst.Update.Wants) {
		m.setUpdateCheck(inst, check)
		slog.Info("approve update refused: the version asks for something else now", "instance_id", id, "new_version", version)
		return nil, ErrApproveChanged
	}
	access, err := json.Marshal(accessOf(newMan))
	if err != nil {
		return nil, err
	}
	approval := store.UpdateApproval{Version: version, Access: string(access), By: by, At: time.Now()}
	if err := m.store.ApproveInstanceUpdate(id, approval); err != nil {
		return nil, fmt.Errorf("store the approval: %w", err)
	}
	inst.UpdateApproval, inst.UpdateDeclined = approval, ""
	m.setUpdateCheck(inst, classifyUpdate(inst, oldMan, newMan, grants, cfg, base, sources))
	m.publishUpdateChanged(id)
	slog.Info("app update approved", "instance_id", id, "name", inst.Name, "version", inst.Version, "new_version", version, "user_id", by)
	return check.Wants, nil
}
