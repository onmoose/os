package lifecycle

// Changing an app's folders after install (DASHBOARD.md # Changing folders
// after install, BRAIN_UI_PROTOCOL.md `PUT /api/v1/apps/{id}/folders`, #659).
// The API checks the new choice against the folder limit, as at install
// (internal/api resolveElections). Lifecycle reads the current choice back
// from the override, prepares any new source, rewrites the folder binds in
// the override and the folder variables in .env, and recreates a running
// app. The pins are refreshed by the compose up, so the new folders are
// pinned by inode like every other start (#604).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"

	"gopkg.in/yaml.v3"
)

// ErrFoldersBusy is returned when the app is installing, updating or being
// removed, so its folders cannot change now. The words are the user's.
var ErrFoldersBusy = errors.New("this app is busy right now. Try again when it has finished")

// InstanceFolders reads an installed app's current folders back from its
// override: one FolderMount per declared folder it binds, in manifest order,
// then the extra folders in the order the override has them. Each carries
// its Source and its Path relative to the owner's home or the shared tree. A
// declared folder that is not in the list is skipped.
func (m *Manager) InstanceFolders(ctx context.Context, id string) ([]FolderMount, error) {
	inst, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	home, err := m.ownerHome(ctx, inst)
	if err != nil {
		return nil, err
	}
	return m.currentFolders(inst.ID, man.Permissions.Folders, home)
}

// ownerHome is the home of a personal app's owner, "" for a household app.
func (m *Manager) ownerHome(ctx context.Context, inst store.Instance) (string, error) {
	if inst.Scope != store.ScopePersonal {
		return "", nil
	}
	u, err := m.store.GetUser(inst.OwnerUserID)
	if err != nil {
		return "", fmt.Errorf("read the app's owner: %w", err)
	}
	rh, err := m.host.ResolveHome(ctx, u.Username)
	if err != nil {
		return "", fmt.Errorf("resolve the owner's home: %w", err)
	}
	return rh.HomePath, nil
}

// currentFolders turns the override's folder binds into relative choices.
func (m *Manager) currentFolders(id string, declared []manifest.Folder, home string) ([]FolderMount, error) {
	raw, err := os.ReadFile(filepath.Join(m.instanceDir(id), "compose.override.yml"))
	if err != nil {
		return nil, fmt.Errorf("read override: %w", err)
	}
	binds, extras, err := folderBinds(raw, m.sharedRoot)
	if err != nil {
		return nil, err
	}
	rel := func(mt FolderMount) (FolderMount, error) {
		shared, p, ok := m.folderSource(mt.hostPath, home)
		if !ok {
			return FolderMount{}, fmt.Errorf("folder bind %q is outside the home and the shared folders", mt.hostPath)
		}
		mt.Source = sourcePersonal
		if shared {
			mt.Source = sourceShared
		}
		mt.Path, mt.hostPath = p, ""
		return mt, nil
	}
	out := make([]FolderMount, 0, len(declared)+len(extras))
	for _, f := range declared {
		b, ok := binds[f.Folder]
		if !ok {
			continue
		}
		mt, err := rel(b)
		if err != nil {
			return nil, err
		}
		out = append(out, mt)
	}
	for _, x := range extras {
		mt, err := rel(x)
		if err != nil {
			return nil, err
		}
		out = append(out, mt)
	}
	return out, nil
}

// SameFolders reports whether two folder lists bind the same folders at the
// same places. The order of the extra folders does not matter.
func SameFolders(a, b []FolderMount) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(mt FolderMount) string {
		return mt.Folder + "\x00" + mt.Extra + "\x00" + mt.Source + "\x00" + mt.relPath()
	}
	ka := make([]string, len(a))
	kb := make([]string, len(b))
	for i := range a {
		ka[i], kb[i] = key(a[i]), key(b[i])
	}
	slices.Sort(ka)
	slices.Sort(kb)
	return slices.Equal(ka, kb)
}

// SetFolders points an installed app's folders at mounts, the full new set
// the API resolved and checked (#659). It returns the folders as they were
// before, for the audit record, and changed false when mounts binds what the
// app binds now (nothing is written then).
//
// Under the app's lock it prepares each new source as install does (a new
// top-level folder gets its made_folders row), then rewrites the folder binds
// and the shared group in the override and the folder variables in .env.
// Brain commits first: the files hold the new folders before the recreate,
// so a failed compose up leaves the app marked pending-recreate and the
// reconcile pass brings it to them, as for a config edit. A stopped or failed
// app picks them up at its next start. step reports progress.
func (m *Manager) SetFolders(ctx context.Context, id string, mounts []FolderMount, step func(string)) (before []FolderMount, changed bool, err error) {
	if step == nil {
		step = func(string) {}
	}
	defer m.lockInstance(id)()
	inst, err := m.store.Get(id)
	if err != nil {
		return nil, false, err
	}
	switch inst.State {
	case "running", "stopped", "failed":
	default:
		return nil, false, ErrFoldersBusy
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return nil, false, fmt.Errorf("load manifest: %w", err)
	}
	if !man.Permissions.FolderApp() {
		return nil, false, errors.New("this app does not use folders, so it cannot be given one")
	}

	step("preparing_folders")
	var owner Owner
	iso := isolation{sharedBase: m.sharedRoot}
	if inst.Scope == store.ScopePersonal {
		u, err := m.store.GetUser(inst.OwnerUserID)
		if err != nil {
			return nil, false, fmt.Errorf("read the app's owner: %w", err)
		}
		owner = Owner{UserID: u.ID, Username: u.Username}
		rh, err := m.host.ResolveHome(ctx, u.Username)
		if err != nil {
			return nil, false, fmt.Errorf("resolve the owner's home: %w", err)
		}
		iso.home = rh.HomePath
	}
	before, err = m.currentFolders(id, man.Permissions.Folders, iso.home)
	if err != nil {
		return nil, false, err
	}
	if SameFolders(before, mounts) {
		return before, false, nil
	}
	wk, err := m.host.WellKnownIdentity(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("read the shared group: %w", err)
	}
	iso.sharedGID, iso.mounts = wk.MooseSharedGID, mounts

	// Only a source the app does not bind yet needs preparing. One it binds
	// now was prepared when it was chosen, and its pin is checked again at
	// the compose up.
	var fresh []FolderMount
	for _, mt := range mounts {
		if !slices.ContainsFunc(before, func(b FolderMount) bool {
			return b.Source == mt.Source && b.relPath() == mt.relPath()
		}) {
			fresh = append(fresh, mt)
		}
	}
	if err := m.prepareFolderSources(ctx, id, owner, iso, fresh); err != nil {
		if errors.Is(err, ErrFolderAlreadyThere) {
			return before, false, ErrFolderAlreadyThere
		}
		slog.Error("change folders: prepare a folder", "instance_id", id, "err", err)
		return before, false, errors.New("a folder could not be made ready, so nothing was changed. Try again")
	}

	step("generating_override")
	if err := m.rewriteFolderBinds(id, man.Permissions.ExtraFolders, modesByFolder(man.Permissions.Folders), iso); err != nil {
		slog.Error("change folders: rewrite the override", "instance_id", id, "err", err)
		return before, false, errors.New("the app's settings could not be written, so nothing was changed. Try again")
	}
	if err := m.writeEnv(id, inst.Slug, iso); err != nil {
		slog.Error("change folders: rewrite .env", "instance_id", id, "err", err)
		return before, true, errors.New("the app's settings could not be written. Try again")
	}
	slog.Info("app folders changed", "instance_id", id, "name", inst.Name, "count", len(mounts))
	if inst.State != "running" {
		return before, true, nil
	}

	step("compose_up")
	if err := m.recreateRunning(ctx, inst); err != nil {
		var refusal *folderRefusal
		if errors.As(err, &refusal) {
			return before, true, refusal
		}
		slog.Error("change folders: recreate", "instance_id", id, "err", err)
		return before, true, fmt.Errorf("%s did not restart with the new folders. moose will try again", inst.Name)
	}
	step("waiting_healthy")
	if err := m.waitHealthy(ctx, id, man.MainService, m.healthWait); err != nil {
		slog.Warn("change folders: not healthy after restart", "instance_id", id, "err", err)
		return before, true, fmt.Errorf("%s restarted with the new folders, but it has not come up yet. Check its logs", inst.Name)
	}
	return before, true, nil
}

// modesByFolder maps each declared folder to its mode, read or write.
func modesByFolder(fs []manifest.Folder) map[string]string {
	out := make(map[string]string, len(fs))
	for _, f := range fs {
		out[f.Folder] = f.Mode
	}
	return out
}

// rewriteFolderBinds sets every service's volumes in the override to the
// folder binds of iso.mounts, as writeOverride makes them, and adds or drops
// the shared group to match. writeOverride writes only folder binds into the
// override's volumes, and the same list on every service, so the whole list
// is replaced. Every other key is left as it is.
func (m *Manager) rewriteFolderBinds(id, extraMode string, modes map[string]string, iso isolation) error {
	path := filepath.Join(m.instanceDir(id), "compose.override.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read override: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse override: %w", err)
	}
	services, _ := doc["services"].(map[string]any)
	if services == nil {
		return fmt.Errorf("override for %s has no services", id)
	}
	volumes := make([]string, 0, len(iso.mounts))
	needShared := false
	for _, mt := range iso.mounts {
		mode := modes[mt.Folder]
		if mt.Extra != "" {
			mode = extraMode
		}
		suffix := ":ro"
		if mode == "write" {
			suffix = ":rw"
		}
		volumes = append(volumes, iso.hostSource(mt)+":"+containerDest(mt)+suffix)
		if mt.Source == sourceShared {
			needShared = true
		}
	}
	shared := strconv.Itoa(iso.sharedGID)
	for name, svcAny := range services {
		svc, _ := svcAny.(map[string]any)
		if svc == nil {
			continue
		}
		if len(volumes) > 0 {
			svc["volumes"] = slices.Clone(volumes)
		} else {
			delete(svc, "volumes")
		}
		var groups []string
		if list, ok := svc["group_add"].([]any); ok {
			for _, g := range list {
				s := fmt.Sprint(g)
				if s != shared {
					groups = append(groups, s)
				}
			}
		}
		// The shared group goes first, as writeOverride puts it before the
		// GPU's render group.
		if needShared {
			groups = append([]string{shared}, groups...)
		}
		if len(groups) > 0 {
			svc["group_add"] = groups
		} else {
			delete(svc, "group_add")
		}
		services[name] = svc
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o644)
}
