package lifecycle

// The app update transaction (APP_LIFECYCLE.md # update + rollback,
// UPDATES.md # 4, #631). It moves one instance to the catalog's new version
// and, when the new version fails, puts the old version back on its old data.
//
// The order of the steps is set by what the pre-update snapshot needs
// (snapshot.go): the app must be stopped while its data is copied, and the
// snapshot reads the manifest on disk to find the data volumes. So:
//
//  1. Under the instance's lock and one of the global install/update slots,
//     load and classify the new version, and admit it. Nothing has changed.
//  2. Write the row `updating`, then pull the new images while the old
//     version still runs (the downtime is only the switch). For an app in the
//     image tier, read the new images' users and refuse a change.
//  3. Keep the previous generation: copies of manifest.yml, compose.yml,
//     compose.override.yml and .env as <name>.prev, and the image pins in
//     instance_images_prev.
//  4. Stop the old version, then take the snapshot of its data.
//  5. Write the new manifest, compose, pins, secrets, managed services,
//     override and .env, then `compose up -d` and wait for it to be healthy.
//  6. Healthy: flip the route and write version, name and `running` in one
//     store write.
//
// A failure before step 4 leaves the old version running and changes nothing
// but the failure record. A failure after it rolls back: stop, restore the
// snapshot, put the previous generation back, start the old version. A good
// rollback ends in `running` with the failure recorded on the row; a rollback
// that cannot bring the old version back ends in `update_failed`.
//
// Progress is written to update.json in the instance dir, so a brain that
// stops in the middle knows on its next start how far the update got and
// puts the old version back (recoverUpdates, called by Reconcile).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"

	"gopkg.in/yaml.v3"
)

// updateHealthMargin is how long past the app's start period an update waits
// for the new version to become healthy, so a slow app is not rolled back for
// being slow (UPDATES.md # 4 step 6). With the default start period of 60s the
// budget is 120s, the same as install.
const updateHealthMargin = 60 * time.Second

// ErrNoUpdate is returned when the catalog has no other version of the app:
// the app is on the catalog's version, it is a Door-2 app (no catalog entry),
// or the catalog no longer lists it.
var ErrNoUpdate = errors.New("there is no update for this app")

// ErrUpdateNotRunning is returned for an app that is not running. An update
// starts from a running app, so the box can tell a good update from a broken
// one by its health.
var ErrUpdateNotRunning = errors.New("start the app before you update it")

// ErrImageUserChange refuses an update of an app in the image tier whose new
// image runs as another user: the owner of its data follows that user
// (APP_ISOLATION.md # User-namespace tiers).
var ErrImageUserChange = errors.New("this version of the app runs as a different user inside its container, so its data would need a new owner. To use it, uninstall the app and install it again")

// updateFiles are the files of the current generation an update keeps a copy
// of, as <name>.prev, next to the current ones (APP_LIFECYCLE.md # on-disk
// layout per instance).
var updateFiles = []string{"manifest.yml", "compose.yml", "compose.override.yml", ".env"}

const updateRecordFile = "update.json"

// updateRecord is how far an update got, in update.json. It is written before
// each step that changes something a rollback must undo.
type updateRecord struct {
	From string `json:"from"` // the version the instance ran
	To   string `json:"to"`   // the version it moves to
	// PrevKept: the .prev copies and the previous pins are complete.
	PrevKept bool `json:"prev_kept,omitempty"`
	// SnapshotTaken: the snapshot of From is complete; a rollback restores it.
	SnapshotTaken bool `json:"snapshot_taken,omitempty"`
	// Writing: the new files may be on disk; a rollback puts the .prev copies
	// and the previous pins back.
	Writing bool `json:"writing,omitempty"`
	// AddedServices and AddedSecrets are what the new version added; a
	// rollback drops them.
	AddedServices []string `json:"added_services,omitempty"`
	AddedSecrets  []string `json:"added_secrets,omitempty"`
	// AddedConfig is the config fields the new version adds that got their
	// default, by app_env.
	AddedConfig []string `json:"added_config,omitempty"`
	// NewImages are the new version's pins; DroppedImages the previous
	// generation this update replaced. Both are removed from the image store
	// when nothing uses them any more.
	NewImages     []store.InstanceImage `json:"new_images,omitempty"`
	DroppedImages []store.InstanceImage `json:"dropped_images,omitempty"`
}

// updateHealthBudget is how long an update waits for the new version: its
// health_probe start_period (the default when it declares none) plus the
// margin, and never less than healthWait.
func (m *Manager) updateHealthBudget(man *manifest.Manifest) time.Duration {
	sp := m.defaultStartPeriod
	if man.HealthProbe != nil && man.HealthProbe.StartPeriod > 0 {
		sp = man.HealthProbe.StartPeriod
	}
	return max(m.healthWait, sp+m.updateMargin)
}

// Update moves an instance to the catalog's version of its app
// (APP_LIFECYCLE.md # update + rollback). See the file comment for the steps.
// It returns an error the dashboard can show as it is: plain words, with the
// detail in the log.
func (m *Manager) Update(ctx context.Context, id string, progress func(step string)) error {
	step := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	defer m.lockInstance(id)()
	release, err := m.acquireHeavy(ctx)
	if err != nil {
		return err
	}
	defer release()

	inst, err := m.store.Get(id)
	if err != nil {
		return err
	}
	if inst.State != "running" {
		return ErrUpdateNotRunning
	}
	if inst.Version == manifest.CustomVersion {
		return ErrNoUpdate
	}

	// 1. Load, classify and admit the new version. Nothing changes here.
	step("checking_update")
	entry, err := m.catalogEntry(inst.ManifestID)
	if errors.Is(err, catalog.ErrNotFound) {
		m.setUpdateCheck(inst, store.UpdateCheck{})
		return ErrNoUpdate
	}
	if err != nil {
		slog.Warn("update: read the catalog entry", "instance_id", id, "manifest_id", inst.ManifestID, "err", err)
		return errors.New("moose could not read the app store. Try again later")
	}
	if entry.Version == inst.Version {
		m.setUpdateCheck(inst, store.UpdateCheck{})
		return ErrNoUpdate
	}
	newMan, newCompose, err := m.catalogLoad(ctx, inst.ManifestID)
	if errors.Is(err, manifest.ErrUnsupportedManifestVersion) {
		check := needsNewerMoose(entry.Version)
		m.setUpdateCheck(inst, check)
		return errors.New(check.Reason)
	}
	if err != nil {
		slog.Warn("update: load the new version", "instance_id", id, "manifest_id", inst.ManifestID, "err", err)
		return errors.New("moose could not download the new version from the app store. Try again later")
	}
	if newMan.Version == inst.Version {
		m.setUpdateCheck(inst, store.UpdateCheck{})
		return ErrNoUpdate
	}
	oldMan, err := m.loadInstanceManifest(id)
	if err != nil {
		return fmt.Errorf("read the app's manifest: %w", err)
	}
	oldCompose, err := os.ReadFile(m.composeFile(id))
	if err != nil {
		return fmt.Errorf("read the app's compose file: %w", err)
	}
	wk, base, err := m.hostIdentity(ctx)
	if err != nil {
		return err
	}
	grants, err := m.store.GetServiceGrants(id)
	if err != nil {
		return err
	}
	cfg, err := m.store.GetInstanceConfig(id)
	if err != nil {
		return err
	}
	check := classifyUpdate(inst, oldMan, newMan, grants, cfg, base)
	m.setUpdateCheck(inst, check)
	if check.Status != store.UpdateAvailable {
		slog.Info("update refused", "instance_id", id, "version", inst.Version, "new_version", newMan.Version, "update_class", check.Status)
		return errors.New(check.Reason)
	}
	if err := m.admitUpdate(ctx, newMan, newCompose); err != nil {
		slog.Warn("update refused by admission", "instance_id", id, "new_version", newMan.Version, "err", err)
		m.recordUpdateFailure(inst, newMan.Version, store.UpdateFailedRefused, "The new version did not pass moose's safety checks: "+err.Error())
		return fmt.Errorf("the new version did not pass moose's safety checks: %w", err)
	}

	// 2. The row says updating from here, then the pull, with the old
	// version still running.
	if err := m.store.SetState(id, "updating"); err != nil {
		return fmt.Errorf("set state updating: %w", err)
	}
	inst.State = "updating"
	m.emitState(inst, "running")
	rec := updateRecord{From: inst.Version, To: newMan.Version}
	if err := m.writeUpdateRecord(id, rec); err != nil {
		m.endBeforeStop(inst, rec, store.UpdateFailedStart, "moose could not start the update", err)
		return errors.New("moose could not start the update")
	}
	slog.Info("app update started", "instance_id", id, "name", inst.Name, "version", inst.Version, "new_version", newMan.Version)

	step("resolving_digests")
	pins, err := resolveImages(ctx, m.docker, newMan, newCompose, m.offlineInstall)
	if err != nil {
		const msg = "moose could not download the new version of the app. The old version is still running"
		m.endBeforeStop(inst, rec, store.UpdateFailedPull, msg, err)
		return errors.New(msg)
	}
	rec.NewImages = toInstanceImages(pins)
	oldPins, err := m.store.GetInstanceImages(id)
	if err != nil {
		m.endBeforeStop(inst, rec, store.UpdateFailedStart, "moose could not read the app's images", err)
		return errors.New("moose could not read the app's images")
	}

	var newIDs map[string]imageIDs
	if inst.UsernsTier == store.UsernsTierImage && base > 0 {
		newIDs, err = m.checkImageUsersKept(ctx, inst, oldCompose, newCompose, oldPins, pins, base)
		if err != nil {
			if errors.Is(err, ErrImageUserChange) {
				m.setUpdateCheck(inst, store.UpdateCheck{Version: newMan.Version, Status: store.UpdateBlocked, Reason: ErrImageUserChange.Error()})
			}
			m.endBeforeStop(inst, rec, store.UpdateFailedRefused, plainOr(err, "moose could not read who the new version runs as"), err)
			return errors.New(plainOr(err, "moose could not read who the new version runs as"))
		}
	}
	iso, err := m.updateIsolation(ctx, inst, newMan, wk, base, newIDs)
	if err != nil {
		m.endBeforeStop(inst, rec, store.UpdateFailedRefused, plainOr(err, "moose could not prepare the new version"), err)
		return errors.New(plainOr(err, "moose could not prepare the new version"))
	}

	// 3. Keep the previous generation.
	step("keeping_previous")
	if rec.DroppedImages, err = m.store.GetInstancePrevImages(id); err != nil {
		m.endBeforeStop(inst, rec, store.UpdateFailedStart, "moose could not keep a copy of the old version", err)
		return errors.New("moose could not keep a copy of the old version")
	}
	if err := m.keepPrevious(id, oldPins); err != nil {
		m.endBeforeStop(inst, rec, store.UpdateFailedStart, "moose could not keep a copy of the old version", err)
		return errors.New("moose could not keep a copy of the old version")
	}
	rec.PrevKept = true
	if err := m.writeUpdateRecord(id, rec); err != nil {
		m.endBeforeStop(inst, rec, store.UpdateFailedStart, "moose could not keep a copy of the old version", err)
		return errors.New("moose could not keep a copy of the old version")
	}

	// 4. Stop the old version, then copy its data.
	step("stopping")
	host := m.routeHost(inst)
	if err := m.caddy.AddSplashRoute(ctx, id, host, oldMan.Name, "updating"); err != nil {
		slog.Warn("update: caddy splash flip failed (continuing)", "instance_id", id, "host", host, "err", err)
	}
	if out, err := m.docker.ComposeStop(ctx, m.instanceDir(id), "moose-"+id); err != nil {
		return m.restartAfterStop(ctx, inst, oldMan, rec, store.UpdateFailedStart,
			"moose could not stop the old version, so it did not update the app", fmt.Errorf("compose stop: %w\n%s", err, out))
	}
	step("taking_snapshot")
	if _, err := m.TakeSnapshot(ctx, id); err != nil {
		msg := "moose could not keep a copy of the app's data, so it did not update the app"
		if errors.Is(err, ErrSnapshotNoSpace) {
			msg = "There is not enough free space on the drive to keep a copy of the app's data, so moose did not update the app. Free up some space and try again"
		}
		return m.restartAfterStop(ctx, inst, oldMan, rec, store.UpdateFailedSnapshot, msg, err)
	}
	rec.SnapshotTaken = true
	rec.Writing = true // set before the first new file is written
	if err := m.writeUpdateRecord(id, rec); err != nil {
		rec.Writing = false
		return m.rollbackUpdate(ctx, inst, oldMan, rec, store.UpdateFailedStart, "moose could not prepare the new version", err)
	}

	// 5. Write the new generation and start it.
	step("writing_instance_dir")
	if err := m.writeNewGeneration(ctx, inst, newMan, newCompose, pins, iso, base, &rec, step); err != nil {
		return m.rollbackUpdate(ctx, inst, oldMan, rec, store.UpdateFailedStart, "moose could not set up the new version", err)
	}
	step("compose_up")
	budget := m.updateHealthBudget(newMan)
	upCtx, cancelUp := context.WithTimeout(ctx, budget)
	out, upErr := m.docker.ComposeUp(upCtx, m.instanceDir(id), "moose-"+id)
	cancelUp()
	if upErr != nil {
		return m.rollbackUpdate(ctx, inst, oldMan, rec, store.UpdateFailedStart,
			fmt.Sprintf("The new version (%s) did not start", newMan.Version), fmt.Errorf("compose up: %w\n%s", upErr, out))
	}
	m.connectCaddy(ctx, id)
	step("waiting_healthy")
	if err := m.waitHealthy(ctx, id, newMan.MainService, budget); err != nil {
		return m.rollbackUpdate(ctx, inst, oldMan, rec, store.UpdateFailedHealth,
			fmt.Sprintf("The new version (%s) did not become healthy", newMan.Version), err)
	}

	// 6. Healthy: the route, then the row in one write.
	step("flipping_route")
	upstream := fmt.Sprintf("moose-%s-%s:%d", id, newMan.MainService, newMan.MainPort)
	if err := m.caddy.AddRoute(ctx, m.buildRouteConfig(inst, newMan, host, upstream)); err != nil {
		slog.Warn("update: caddy upstream flip failed (continuing)", "instance_id", id, "host", host, "upstream", upstream, "err", err)
	}
	if err := m.store.FinishInstanceUpdate(id, newMan.Version, newMan.Name); err != nil {
		// The row cannot say the new version runs, so the brain would not
		// know what it runs. Put the old version back, as on any failure.
		return m.rollbackUpdate(ctx, inst, oldMan, rec, store.UpdateFailedStart, "moose could not record the update", err)
	}
	m.removeUpdateRecord(id)
	m.reclaimImages(ctx, id, rec.DroppedImages)
	prev := inst.State
	inst.State, inst.Version, inst.Name = "running", newMan.Version, newMan.Name
	m.emitState(inst, prev)
	m.publishUpdateChanged(id)
	slog.Info("app updated", "instance_id", id, "name", newMan.Name, "version", rec.From, "new_version", newMan.Version)
	return nil
}

// plainOr is err's text when err is one of the brain's plain sentences for
// the user, and msg otherwise (a raw error stays in the log).
func plainOr(err error, msg string) string {
	for _, plain := range []error{ErrImageUserChange, ErrNoGPU} {
		if errors.Is(err, plain) {
			return plain.Error()
		}
	}
	return msg
}

// admitUpdate runs the install's checks on the new version: the compose
// admission and the manifest checks, the same for both doors.
func (m *Manager) admitUpdate(ctx context.Context, man *manifest.Manifest, composeBytes []byte) error {
	if err := validateConfigServices(man, composeBytes); err != nil {
		return err
	}
	if err := m.admit(ctx, composeBytes); err != nil {
		return err
	}
	if err := admission.CheckManifest(man); err != nil {
		return err
	}
	return admission.CheckManifestCompose(man, composeBytes)
}

// checkImageUsersKept reads the users the old and the new images run as and
// refuses the update when a bind dir both versions use would need a new
// owner (APP_ISOLATION.md # User-namespace tiers). It returns the new users,
// which give the bind dirs of the new version their owners.
func (m *Manager) checkImageUsersKept(ctx context.Context, inst store.Instance, oldCompose, newCompose []byte, oldPins []store.InstanceImage, newPins []servicePin, base int) (map[string]imageIDs, error) {
	old := make([]servicePin, len(oldPins))
	for i, p := range oldPins {
		old[i] = servicePin{Service: p.Service, Image: p.Image, Digest: p.Digest, ref: storedRef(p)}
	}
	oldIDs, err := m.resolveImageUsers(ctx, inst.ID, old)
	if err != nil {
		return nil, fmt.Errorf("read the old image users: %w", err)
	}
	newIDs, err := m.resolveImageUsers(ctx, inst.ID, newPins)
	if err != nil {
		return nil, err
	}
	owners := func(compose []byte, ids map[string]imageIDs) (map[string]hostOwner, error) {
		dirs, err := bindDirsByService(compose)
		if err != nil {
			return nil, err
		}
		it := isolation{tier: store.UsernsTierImage, remapBase: base, imageIDs: ids}
		return it.bindDirOwners(dirs)
	}
	before, err := owners(oldCompose, oldIDs)
	if err != nil {
		return nil, fmt.Errorf("old bind dir owners: %w", err)
	}
	after, err := owners(newCompose, newIDs)
	if err != nil {
		return nil, err
	}
	for dir, own := range after {
		if was, ok := before[dir]; ok && was != own {
			slog.Warn("update refused: the image user of a bind dir changes", "instance_id", inst.ID, "dir", dir,
				"uid", own.uid, "gid", own.gid)
			return nil, ErrImageUserChange
		}
	}
	return newIDs, nil
}

// updateIsolation is the isolation the new version's override is written
// with. The tier is the instance's, fixed at install. The identity follows
// the same rules as install, from what the instance already has: its
// allocated service identity, its owner's home, and the folder sources the
// installer elected, read back from the old override (mountsFromOverride).
func (m *Manager) updateIsolation(ctx context.Context, inst store.Instance, man *manifest.Manifest, wk protocol.WellKnownIdentityResponse, base int, ids map[string]imageIDs) (isolation, error) {
	iso := isolation{uid: os.Geteuid(), gid: os.Getegid(), sharedBase: m.sharedRoot, tier: inst.UsernsTier, remapBase: base, imageIDs: ids}
	if man.Permissions.GPU {
		g, err := m.host.SystemGPU(ctx)
		if err != nil {
			return iso, fmt.Errorf("query host GPU capability: %w", err)
		}
		if !g.Present {
			return iso, ErrNoGPU
		}
		if g.RenderGID <= 0 {
			return iso, fmt.Errorf("host reported a GPU with no render group (render_gid=%d)", g.RenderGID)
		}
		iso.gpu = g
	}
	switch {
	case len(man.Permissions.Folders) > 0:
		override, err := os.ReadFile(filepath.Join(m.instanceDir(inst.ID), "compose.override.yml"))
		if err != nil {
			return iso, fmt.Errorf("read the override: %w", err)
		}
		mounts, err := mountsFromOverride(override, man, m.sharedRoot)
		if err != nil {
			return iso, err
		}
		iso.sharedGID, iso.mounts = wk.MooseSharedGID, mounts
		if inst.Scope == store.ScopeHousehold {
			iso.uid, iso.gid = wk.MooseAppUID, wk.MooseAppGID
		} else {
			owner, err := m.store.GetUser(inst.OwnerUserID)
			if err != nil {
				return iso, fmt.Errorf("read the app's owner: %w", err)
			}
			rh, err := m.host.ResolveHome(ctx, owner.Username)
			if err != nil {
				return iso, fmt.Errorf("resolve owner home: %w", err)
			}
			iso.uid, iso.gid, iso.home = rh.UID, rh.GID, rh.HomePath
		}
	case man.ServiceUser:
		// classifyUpdate refuses a version that turns service_user on or off,
		// so the instance has its identity from install.
		if inst.ServiceUID == 0 {
			return iso, ErrTierChange
		}
		iso.uid, iso.gid = inst.ServiceUID, inst.ServiceGID
	}
	return iso, nil
}

// mountsFromOverride reads back the folder binds an install wrote: each
// `<host path>:/moose/<folder>:<mode>` volume of the old override, for the
// folders the new manifest still declares. The host path is kept as it is,
// so the update binds exactly the source the installer elected. The mode
// comes from the new manifest when the override is written.
func mountsFromOverride(override []byte, man *manifest.Manifest, sharedBase string) ([]FolderMount, error) {
	var doc struct {
		Services map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(override, &doc); err != nil {
		return nil, fmt.Errorf("parse the override: %w", err)
	}
	declared := map[string]bool{}
	for _, f := range man.Permissions.Folders {
		declared[f.Folder] = true
	}
	names := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	var out []FolderMount
	for _, name := range names {
		for _, v := range doc.Services[name].Volumes {
			i := strings.LastIndex(v, ":")
			if i < 0 {
				continue
			}
			rest := v[:i]
			j := strings.LastIndex(rest, ":")
			if j < 0 {
				continue
			}
			src, dest := rest[:j], rest[j+1:]
			folder, ok := strings.CutPrefix(dest, "/moose/")
			if !ok || !declared[folder] || seen[folder] {
				continue
			}
			seen[folder] = true
			source := sourcePersonal
			if src == sharedBase || strings.HasPrefix(src, sharedBase+"/") {
				source = sourceShared
			}
			out = append(out, FolderMount{Folder: folder, Source: source, hostPath: src})
		}
	}
	return out, nil
}

// writeNewGeneration writes everything the new version runs from: manifest
// and compose, the image pins, the defaults of the config fields it adds, the
// secrets it adds, the managed services it adds, the bind dirs, the override
// and the .env. rec is saved before a
// service is provisioned or a secret stored, so a rollback knows what to
// drop.
func (m *Manager) writeNewGeneration(ctx context.Context, inst store.Instance, man *manifest.Manifest, composeBytes []byte, pins []servicePin, iso isolation, base int, rec *updateRecord, step func(string)) error {
	id := inst.ID
	if err := m.writeInstanceDir(id, man, composeBytes); err != nil {
		return fmt.Errorf("instance dir: %w", err)
	}
	if err := m.store.SetInstanceImages(id, toInstanceImages(pins)); err != nil {
		return fmt.Errorf("persist digests: %w", err)
	}

	// A config field the new version adds gets its default, as the install
	// form prefills it. A field with no default and no value is left empty:
	// classifyUpdate refused a required one.
	cfg, err := m.store.GetInstanceConfig(id)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	hasValue := map[string]bool{}
	for _, c := range cfg {
		hasValue[c.AppEnv] = true
	}
	var defaults []store.InstanceConfig
	for _, f := range man.Config {
		if f.Default != "" && !hasValue[f.AppEnv] {
			defaults = append(defaults, store.InstanceConfig{AppEnv: f.AppEnv, Value: f.Default, Secret: f.Secret})
			rec.AddedConfig = append(rec.AddedConfig, f.AppEnv)
		}
	}
	if len(defaults) > 0 {
		if err := m.writeUpdateRecord(id, *rec); err != nil {
			return err
		}
		if err := m.store.SetInstanceConfig(id, append(cfg, defaults...)); err != nil {
			return fmt.Errorf("persist config: %w", err)
		}
	}

	// New declared secrets are generated; the ones the instance has are kept
	// as they are, so a token-signing secret never changes under the app.
	secrets, err := m.store.GetInstanceSecrets(id)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}
	have := map[string]bool{}
	for _, s := range secrets {
		have[s.Name] = true
	}
	var missing []manifest.Secret
	for _, d := range man.Secrets {
		if !have[d.Name] {
			missing = append(missing, d)
			rec.AddedSecrets = append(rec.AddedSecrets, d.Name)
		}
	}
	if len(missing) > 0 {
		if err := m.writeUpdateRecord(id, *rec); err != nil {
			return err
		}
		added, err := generateSecrets(missing)
		if err != nil {
			return fmt.Errorf("generate secrets: %w", err)
		}
		if err := m.store.SetInstanceSecrets(id, append(secrets, added...)); err != nil {
			return fmt.Errorf("persist secrets: %w", err)
		}
	}

	// A managed service the old version did not have is provisioned, as at
	// install. classifyUpdate refused a move of an existing one.
	grants, err := m.store.GetServiceGrants(id)
	if err != nil {
		return fmt.Errorf("load service grants: %w", err)
	}
	granted := map[string]bool{}
	for _, g := range grants {
		granted[g.LogicalName] = true
	}
	newServices := map[string]manifest.ServiceDep{}
	for name, dep := range man.Services {
		if !granted[name] {
			newServices[name] = dep
			rec.AddedServices = append(rec.AddedServices, name)
		}
	}
	if len(newServices) > 0 {
		step("provisioning_services")
		sort.Strings(rec.AddedServices)
		if err := m.writeUpdateRecord(id, *rec); err != nil {
			return err
		}
		added, err := m.provisionServices(ctx, id, man.ID, newServices, base)
		if err != nil {
			return fmt.Errorf("provision services: %w", err)
		}
		if err := m.store.SetServiceGrants(id, append(grants, added...)); err != nil {
			m.dropServiceGrants(context.Background(), id, added)
			return fmt.Errorf("persist service grants: %w", err)
		}
	}

	step("generating_override")
	if err := m.prepareBindDirs(id, composeBytes, iso); err != nil {
		return err
	}
	lim, err := m.store.GetResourceLimits(id)
	if err != nil {
		return fmt.Errorf("get resource limits: %w", err)
	}
	if err := m.writeOverride(id, man, composeBytes, pins, iso, lim); err != nil {
		return fmt.Errorf("override: %w", err)
	}
	if err := m.writeEnv(id, inst.Slug, iso); err != nil {
		return fmt.Errorf("env: %w", err)
	}
	// The per-app network exists from install; this only makes sure.
	if err := m.docker.NetworkCreate(ctx, "moose-app-"+id, !man.Permissions.Internet); err != nil {
		return fmt.Errorf("create network: %w", err)
	}
	return nil
}

// --- the previous generation ----------------------------------------------

// keepPrevious copies the current files to <name>.prev and the current pins
// to instance_images_prev. It replaces any older previous generation: one
// generation of rollback.
func (m *Manager) keepPrevious(id string, pins []store.InstanceImage) error {
	dir := m.instanceDir(id)
	for _, name := range updateFiles {
		if err := copyFileAtomic(filepath.Join(dir, name), filepath.Join(dir, name+".prev")); err != nil {
			return err
		}
	}
	return m.store.SetInstancePrevImages(id, pins)
}

// putPreviousBack makes the previous generation current again: the .prev
// copies over the current files, and the previous pins as the current pins.
// It can run again after a failure.
func (m *Manager) putPreviousBack(id string) error {
	dir := m.instanceDir(id)
	for _, name := range updateFiles {
		if err := copyFileAtomic(filepath.Join(dir, name+".prev"), filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	prev, err := m.store.GetInstancePrevImages(id)
	if err != nil {
		return err
	}
	if len(prev) == 0 {
		return errors.New("the previous image pins are missing")
	}
	return m.store.SetInstanceImages(id, prev)
}

// dropPrevious removes the previous generation: after a rollback it is the
// same as the current one.
func (m *Manager) dropPrevious(id string) {
	dir := m.instanceDir(id)
	for _, name := range updateFiles {
		if err := os.Remove(filepath.Join(dir, name+".prev")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("update: remove a previous-generation file", "instance_id", id, "src", name+".prev", "err", err)
		}
	}
	if err := m.store.SetInstancePrevImages(id, nil); err != nil {
		slog.Warn("update: clear the previous image pins", "instance_id", id, "err", err)
	}
}

func copyFileAtomic(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(src); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// --- the progress record -----------------------------------------------------

func (m *Manager) writeUpdateRecord(id string, rec updateRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(m.instanceDir(id), updateRecordFile)
	if err := writeFileSynced(p+".tmp", func(w io.Writer) error { _, err := w.Write(b); return err }); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// readUpdateRecord reads update.json. ok is false when there is none.
func (m *Manager) readUpdateRecord(id string) (rec updateRecord, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(m.instanceDir(id), updateRecordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, false, fmt.Errorf("read the update record: %w", err)
	}
	return rec, true, nil
}

func (m *Manager) removeUpdateRecord(id string) {
	if err := os.Remove(filepath.Join(m.instanceDir(id), updateRecordFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("update: remove the update record", "instance_id", id, "err", err)
	}
}

// --- ending an update that did not go through ---------------------------------

// endBeforeStop ends an update that failed while the old version still ran:
// nothing of the old version changed. The row goes back to running and the
// failure is recorded.
func (m *Manager) endBeforeStop(inst store.Instance, rec updateRecord, kind, msg string, cause error) {
	slog.Warn("app update failed; the old version still runs", "instance_id", inst.ID, "version", rec.From, "new_version", rec.To, "step", kind, "err", cause)
	if rec.PrevKept {
		m.dropPrevious(inst.ID)
	}
	m.removeUpdateRecord(inst.ID)
	if err := m.store.SetState(inst.ID, "running"); err != nil {
		slog.Warn("update: set state running", "instance_id", inst.ID, "err", err)
	}
	inst.State = "running"
	m.emitState(inst, "updating")
	m.recordUpdateFailure(inst, rec.To, kind, msg)
	m.reclaimImages(context.Background(), inst.ID, append(rec.NewImages, rec.DroppedImages...))
}

// restartAfterStop ends an update that failed after the old version stopped
// and before anything new was written: the old version is started again on
// its own files and data.
func (m *Manager) restartAfterStop(ctx context.Context, inst store.Instance, oldMan *manifest.Manifest, rec updateRecord, kind, msg string, cause error) error {
	slog.Warn("app update failed; starting the old version again", "instance_id", inst.ID, "version", rec.From, "new_version", rec.To, "step", kind, "err", cause)
	// The old version is stopped: start it again even when the caller's
	// context is gone, as rollbackUpdate does.
	ctx = context.WithoutCancel(ctx)
	m.finishRecord(inst.ID, rec)
	if err := m.startOld(ctx, inst, oldMan); err != nil {
		m.endUpdateFailed(ctx, inst, oldMan, rec.To, kind, msg+". The old version did not start again either", err)
		return errors.New(msg)
	}
	m.endRolledBack(inst, rec, kind, msg)
	return errors.New(msg)
}

// rollbackUpdate puts the old version back after the new one was written:
// stop, restore the snapshot, put the previous generation back, drop what the
// new version added, start the old version.
func (m *Manager) rollbackUpdate(ctx context.Context, inst store.Instance, oldMan *manifest.Manifest, rec updateRecord, kind, msg string, cause error) error {
	slog.Warn("app update failed; rolling back", "instance_id", inst.ID, "version", rec.From, "new_version", rec.To, "step", kind, "err", cause)
	// The rollback runs to its end even when the caller's context is gone.
	ctx = context.WithoutCancel(ctx)
	if err := m.undoUpdate(ctx, inst.ID, rec); err != nil {
		slog.Error("app update rollback failed", "instance_id", inst.ID, "version", rec.From, "err", err)
		m.endUpdateFailed(ctx, inst, oldMan, rec.To, store.UpdateFailedRestore,
			msg+". moose could not put the old version back", nil)
		return errors.New(msg + ". moose could not put the old version back")
	}
	m.finishRecord(inst.ID, rec)
	if err := m.startOld(ctx, inst, oldMan); err != nil {
		m.endUpdateFailed(ctx, inst, oldMan, rec.To, kind, msg+". The old version did not start again either", err)
		return errors.New(msg + ". The old version did not start again either")
	}
	m.endRolledBack(inst, rec, kind, msg+". moose put the old version back")
	return errors.New(msg + ". moose put the old version back")
}

// undoUpdate is the part of a rollback that changes data and files. It can
// run again after a failure: the snapshot restore, the copies and the drops
// all can.
func (m *Manager) undoUpdate(ctx context.Context, id string, rec updateRecord) error {
	if rec.SnapshotTaken || rec.Writing {
		if out, err := m.docker.ComposeStop(ctx, m.instanceDir(id), "moose-"+id); err != nil {
			slog.Warn("update rollback: compose stop", "instance_id", id, "err", err, "output", out)
		}
	}
	if rec.SnapshotTaken {
		if err := m.RestoreSnapshot(ctx, id, rec.From); err != nil {
			return fmt.Errorf("restore the snapshot: %w", err)
		}
	}
	if rec.PrevKept && rec.Writing {
		if err := m.putPreviousBack(id); err != nil {
			return fmt.Errorf("put the previous version back: %w", err)
		}
	}
	if len(rec.AddedServices) > 0 {
		grants, err := m.store.GetServiceGrants(id)
		if err != nil {
			return err
		}
		var keep, drop []store.ServiceGrant
		for _, g := range grants {
			if slices.Contains(rec.AddedServices, g.LogicalName) {
				drop = append(drop, g)
			} else {
				keep = append(keep, g)
			}
		}
		if len(drop) > 0 {
			m.dropServiceGrants(ctx, id, drop)
			if err := m.store.SetServiceGrants(id, keep); err != nil {
				return err
			}
		}
	}
	if len(rec.AddedConfig) > 0 {
		cfg, err := m.store.GetInstanceConfig(id)
		if err != nil {
			return err
		}
		var keep []store.InstanceConfig
		for _, c := range cfg {
			if !slices.Contains(rec.AddedConfig, c.AppEnv) {
				keep = append(keep, c)
			}
		}
		if len(keep) != len(cfg) {
			if err := m.store.SetInstanceConfig(id, keep); err != nil {
				return err
			}
		}
	}
	if len(rec.AddedSecrets) > 0 {
		secrets, err := m.store.GetInstanceSecrets(id)
		if err != nil {
			return err
		}
		var keep []store.InstanceSecret
		for _, s := range secrets {
			if !slices.Contains(rec.AddedSecrets, s.Name) {
				keep = append(keep, s)
			}
		}
		if len(keep) != len(secrets) {
			if err := m.store.SetInstanceSecrets(id, keep); err != nil {
				return err
			}
		}
	}
	return nil
}

// finishRecord ends a rollback whose undo is done: the record and the
// previous generation (now the current one) go, and the new version's images
// go when nothing uses them.
func (m *Manager) finishRecord(id string, rec updateRecord) {
	if rec.PrevKept {
		m.dropPrevious(id)
	}
	m.removeUpdateRecord(id)
	m.reclaimImages(context.Background(), id, append(rec.NewImages, rec.DroppedImages...))
}

// startOld starts the old version after a failed update, the way Start does:
// `compose up -d`, Caddy's network, the health wait and the route.
func (m *Manager) startOld(ctx context.Context, inst store.Instance, man *manifest.Manifest) error {
	id := inst.ID
	budget := m.updateHealthBudget(man)
	upCtx, cancel := context.WithTimeout(ctx, budget)
	out, err := m.composeUpInstance(upCtx, id, pullRetryDelays)
	cancel()
	if err != nil {
		return fmt.Errorf("compose up: %w\n%s", err, out)
	}
	m.connectCaddy(ctx, id)
	if err := m.waitHealthy(ctx, id, man.MainService, budget); err != nil {
		return err
	}
	host := m.routeHost(inst)
	upstream := fmt.Sprintf("moose-%s-%s:%d", id, man.MainService, man.MainPort)
	if err := m.caddy.AddRoute(ctx, m.buildRouteConfig(inst, man, host, upstream)); err != nil {
		slog.Warn("update: caddy upstream flip failed (continuing)", "instance_id", id, "host", host, "upstream", upstream, "err", err)
	}
	return nil
}

// endRolledBack ends an update whose old version runs again: the row is
// running, with the failure recorded on it.
func (m *Manager) endRolledBack(inst store.Instance, rec updateRecord, kind, msg string) {
	if err := m.store.SetState(inst.ID, "running"); err != nil {
		slog.Warn("update: set state running", "instance_id", inst.ID, "err", err)
	}
	inst.State = "running"
	m.emitState(inst, "updating")
	m.recordUpdateFailure(inst, rec.To, kind, msg)
	slog.Info("app update rolled back", "instance_id", inst.ID, "version", rec.From, "new_version", rec.To, "step", kind)
}

// endUpdateFailed ends an update whose old version is not running: the row
// is update_failed and the route shows the "failed" splash. When the undo did
// not finish, update.json stays, and a Start finishes it (finishRollback).
func (m *Manager) endUpdateFailed(ctx context.Context, inst store.Instance, man *manifest.Manifest, version, kind, msg string, cause error) {
	if cause != nil {
		slog.Error("app update failed and the old version is not running", "instance_id", inst.ID, "new_version", version, "step", kind, "err", cause)
	}
	host := m.routeHost(inst)
	if err := m.caddy.AddSplashRoute(ctx, inst.ID, host, man.Name, "failed"); err != nil {
		slog.Warn("update: caddy splash flip failed", "instance_id", inst.ID, "host", host, "err", err)
	}
	if err := m.store.SetState(inst.ID, "update_failed"); err != nil {
		slog.Warn("update: set state update_failed", "instance_id", inst.ID, "err", err)
	}
	inst.State = "update_failed"
	m.emitState(inst, "updating")
	m.recordUpdateFailure(inst, version, kind, msg)
}

// finishRollback is the first part of a Start from update_failed: when the
// rollback did not finish, it runs the undo again. The caller holds the lock.
func (m *Manager) finishRollback(ctx context.Context, inst store.Instance) error {
	rec, ok, err := m.readUpdateRecord(inst.ID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := m.undoUpdate(ctx, inst.ID, rec); err != nil {
		slog.Error("update rollback failed again", "instance_id", inst.ID, "version", rec.From, "err", err)
		return errors.New("moose still cannot put the old version of the app back. See the app's logs")
	}
	m.finishRecord(inst.ID, rec)
	slog.Info("update rollback finished", "instance_id", inst.ID, "version", rec.From)
	return nil
}

// recoverUpdates puts back the old version of every instance the brain left
// in `updating` (it stopped in the middle of an update). It runs at the start
// of the startup pass, before anything else is running, so it takes no lock.
// Afterwards the row is running (the pass then brings the containers up if
// they are not) or update_failed.
func (m *Manager) recoverUpdates(ctx context.Context) {
	insts, err := m.store.List()
	if err != nil {
		slog.Warn("reconcile: list instances for interrupted updates", "err", err)
		return
	}
	for _, inst := range insts {
		if inst.State != "updating" {
			continue
		}
		const msg = "The update did not finish, so moose put the old version back"
		rec, ok, err := m.readUpdateRecord(inst.ID)
		if err != nil {
			slog.Error("reconcile: interrupted update has an unreadable record", "instance_id", inst.ID, "err", err)
			m.markUpdateFailedAtStartup(inst, inst.Update.Version, "The update did not finish, and moose cannot tell how far it got")
			continue
		}
		if !ok {
			// No record: the update stopped before it changed anything.
			slog.Info("reconcile: interrupted update changed nothing", "instance_id", inst.ID)
			m.endRolledBack(inst, updateRecord{To: inst.Update.Version}, store.UpdateFailedInterrupted, msg)
			continue
		}
		if err := m.undoUpdate(ctx, inst.ID, rec); err != nil {
			slog.Error("reconcile: could not put the old version back", "instance_id", inst.ID, "version", rec.From, "err", err)
			m.markUpdateFailedAtStartup(inst, rec.To, "The update did not finish, and moose could not put the old version back")
			continue
		}
		m.finishRecord(inst.ID, rec)
		if rec.SnapshotTaken || rec.Writing {
			// The new version may be up. Bring the old one up now; the pass
			// below would leave containers that are up alone.
			if out, err := m.composeUpInstance(ctx, inst.ID, nil); err != nil {
				slog.Warn("reconcile: compose up after an interrupted update", "instance_id", inst.ID, "err", err, "output", out)
			}
		}
		slog.Info("reconcile: interrupted update rolled back", "instance_id", inst.ID, "version", rec.From, "new_version", rec.To)
		m.endRolledBack(inst, rec, store.UpdateFailedInterrupted, msg)
	}
}

// markUpdateFailedAtStartup is endUpdateFailed for the startup pass, which
// writes the splash itself, after its other work.
func (m *Manager) markUpdateFailedAtStartup(inst store.Instance, version, msg string) {
	if err := m.store.SetState(inst.ID, "update_failed"); err != nil {
		slog.Warn("reconcile: set state update_failed", "instance_id", inst.ID, "err", err)
	}
	inst.State = "update_failed"
	m.emitState(inst, "updating")
	m.recordUpdateFailure(inst, version, store.UpdateFailedRestore, msg)
}

// --- the row's update fields -------------------------------------------------

// setUpdateCheck stores what a check found, when it differs from the row.
func (m *Manager) setUpdateCheck(inst store.Instance, check store.UpdateCheck) {
	if inst.Update == check {
		return
	}
	if err := m.store.SetInstanceUpdateCheck(inst.ID, check); err != nil {
		slog.Warn("update check: store the result", "instance_id", inst.ID, "err", err)
		return
	}
	m.publishUpdateChanged(inst.ID)
}

func (m *Manager) recordUpdateFailure(inst store.Instance, version, kind, msg string) {
	f := store.UpdateFailure{Version: version, Kind: kind, Message: msg, At: time.Now()}
	if err := m.store.RecordInstanceUpdateFailure(inst.ID, f); err != nil {
		slog.Warn("update: record the failure", "instance_id", inst.ID, "err", err)
	}
	m.publishUpdateChanged(inst.ID)
}

func (m *Manager) publishUpdateChanged(id string) {
	m.bus.Publish(events.AppUpdateChanged, map[string]any{"instance_id": id})
}
