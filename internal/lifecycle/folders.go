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
	"strings"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"

	"gopkg.in/yaml.v3"
)

// ErrFoldersBusy is returned when the app is installing, updating or being
// removed, so its folders cannot change now. The words are the user's.
var ErrFoldersBusy = errors.New("this app is busy right now. Try again when it has finished")

// ErrFoldersVersionChanged is returned when the app was updated or rolled
// back between the save and its job. The words are the user's.
var ErrFoldersVersionChanged = errors.New("this app changed version while you were saving, so nothing was changed. Check its folders and save again")

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
// the API resolved and checked (#659) against the app at version. When the
// app runs another version by the time the lock is held, nothing is changed
// (ErrFoldersVersionChanged); an empty version skips that check. It returns the folders as they were
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
func (m *Manager) SetFolders(ctx context.Context, id, version string, mounts []FolderMount, step func(string)) (before []FolderMount, changed bool, err error) {
	if step == nil {
		step = func(string) {}
	}
	defer m.lockInstance(id)()
	inst, err := m.store.Get(id)
	if err != nil {
		return nil, false, errRead("read the app", id, err)
	}
	switch inst.State {
	case "running", "stopped", "failed":
	default:
		return nil, false, ErrFoldersBusy
	}
	// mounts were checked against the version the app ran when the save was
	// asked for. An update that ran in between may need other folders, so
	// the choice is not applied to it.
	if version != "" && inst.Version != version {
		return nil, false, ErrFoldersVersionChanged
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return nil, false, errRead("load manifest", id, err)
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
			return nil, false, errRead("read the app's owner", id, err)
		}
		owner = Owner{UserID: u.ID, Username: u.Username}
		rh, err := m.host.ResolveHome(ctx, u.Username)
		if err != nil {
			return nil, false, errRead("resolve the owner's home", id, err)
		}
		iso.home = rh.HomePath
	}
	before, err = m.currentFolders(id, man.Permissions.Folders, iso.home)
	if err != nil {
		return nil, false, errRead("read the current folders", id, err)
	}
	if SameFolders(before, mounts) {
		return before, false, nil
	}
	wk, err := m.host.WellKnownIdentity(ctx)
	if err != nil {
		return nil, false, errRead("read the shared group", id, err)
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
	// The files are written together or not at all: a save that stopped
	// half way would leave the override and .env apart, and a retry of the
	// same choice would then see no change and never mend them.
	restore, err := m.keepFolderFiles(id)
	if err != nil {
		slog.Error("change folders: keep the current files", "instance_id", id, "err", err)
		return before, false, errFoldersNotWritten
	}
	writeAll := func() error {
		if err := m.rewriteFolderBinds(id, "compose.override.yml", man.Permissions.ExtraFolders, modesByFolder(man.Permissions.Folders), iso); err != nil {
			return fmt.Errorf("rewrite the override: %w", err)
		}
		if err := m.writeEnv(id, inst.Slug, iso); err != nil {
			return fmt.Errorf("rewrite .env: %w", err)
		}
		// A rollback by hand puts the previous generation's files back
		// (putPreviousBack). They must hold this choice too, or a rollback
		// would give the app back a folder the owner took away (UPDATES.md #
		// The rollback by hand: a setting changed in those days must not
		// come back).
		if err := m.carryFoldersToPrevious(id, before, iso); err != nil {
			return fmt.Errorf("rewrite the previous generation: %w", err)
		}
		return nil
	}
	if err := writeAll(); err != nil {
		slog.Error("change folders: write the files", "instance_id", id, "err", err)
		if rerr := restore(); rerr != nil {
			slog.Error("change folders: put the old files back", "instance_id", id, "err", rerr)
		}
		return before, false, errFoldersNotWritten
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

// errFoldersNotRead is the job's error when SetFolders could not read what it
// needs before it changed anything. The cause goes to the log.
var errFoldersNotRead = errors.New("moose could not read this app's folders, so nothing was changed. Try again")

// errFoldersNotWritten is the job's error when the new folders could not be
// written. The files the save touched are put back first.
var errFoldersNotWritten = errors.New("the app's settings could not be written, so nothing was changed. Try again")

// folderFiles are the files a folder change writes.
var folderFiles = []string{"compose.override.yml", ".env", "compose.override.yml.prev", ".env.prev"}

// keepFolderFiles reads the files a folder change writes, and returns a func
// that writes them back as they were. A file that is not there is left out.
func (m *Manager) keepFolderFiles(id string) (func() error, error) {
	dir := m.instanceDir(id)
	kept := map[string][]byte{}
	for _, name := range folderFiles {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		kept[name] = raw
	}
	return func() error {
		var errs []error
		for name, raw := range kept {
			if err := writeFileAtomic(filepath.Join(dir, name), raw, 0o644); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, nil
}

// errRead logs why SetFolders could not read something and returns the plain
// error the job shows.
func errRead(what, id string, err error) error {
	slog.Error("change folders: "+what, "instance_id", id, "err", err)
	return errFoldersNotRead
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
func (m *Manager) rewriteFolderBinds(id, name, extraMode string, modes map[string]string, iso isolation) error {
	path := filepath.Join(m.instanceDir(id), name)
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

// carryFoldersToPrevious writes a folder change into the previous
// generation's override and .env, when an update kept one for a rollback.
// The previous version gets the new choice for every folder its manifest
// declares, and the extra folders when it declares extra_folders. A folder
// the previous version needs (not optional there) but the new choice leaves
// out keeps the bind it had, before, because the old version cannot start
// without it.
func (m *Manager) carryFoldersToPrevious(id string, before []FolderMount, iso isolation) error {
	dir := m.instanceDir(id)
	if _, err := os.Stat(filepath.Join(dir, "compose.override.yml.prev")); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	prevMan, err := m.loadManifestFile(id, "manifest.yml.prev")
	if err != nil {
		return fmt.Errorf("read the previous manifest: %w", err)
	}
	prevRaw, err := os.ReadFile(filepath.Join(dir, "compose.override.yml.prev"))
	if err != nil {
		return fmt.Errorf("read the previous override: %w", err)
	}
	prevBinds, _, err := folderBinds(prevRaw, m.sharedRoot)
	if err != nil {
		return err
	}
	byFolder := map[string]FolderMount{}
	var extras []FolderMount
	for _, mt := range iso.mounts {
		if mt.Extra != "" {
			extras = append(extras, mt)
			continue
		}
		byFolder[mt.Folder] = mt
	}
	var mounts []FolderMount
	for _, f := range prevMan.Permissions.Folders {
		if mt, ok := byFolder[f.Folder]; ok {
			mounts = append(mounts, mt)
			continue
		}
		if b, ok := prevBinds[f.Folder]; ok && !f.Optional {
			mounts = append(mounts, b)
		}
	}
	if prevMan.Permissions.ExtraFolders != "" {
		mounts = append(mounts, extras...)
	}
	prevIso := iso
	prevIso.mounts = mounts
	if err := m.rewriteFolderBinds(id, "compose.override.yml.prev", prevMan.Permissions.ExtraFolders, modesByFolder(prevMan.Permissions.Folders), prevIso); err != nil {
		return err
	}
	return rewriteEnvFolders(filepath.Join(dir, ".env.prev"), mounts)
}

// rewriteEnvFolders replaces the MOOSE_FOLDER_* lines of an .env file with
// the ones writeEnv makes for mounts, and leaves every other line as it is.
func rewriteEnvFolders(path string, mounts []FolderMount) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if !strings.HasPrefix(l, "MOOSE_FOLDER_") {
			lines = append(lines, l)
		}
	}
	extra := false
	for _, mt := range mounts {
		if mt.Extra != "" {
			extra = true
			continue
		}
		lines = append(lines, "MOOSE_FOLDER_"+strings.ToUpper(mt.Folder)+"="+containerDest(mt))
	}
	if extra {
		lines = append(lines, "MOOSE_FOLDER_EXTRA="+extraDir)
	}
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
