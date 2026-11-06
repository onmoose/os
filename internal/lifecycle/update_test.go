package lifecycle

// The app update (#631, APP_LIFECYCLE.md # update + rollback) against the
// fakes: a good update, each failure and its rollback, every refusal, the
// crash recovery in the startup pass, and the update check.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

const (
	upImage1  = "traefik/whoami:v1.10.3"
	upImage2  = "traefik/whoami:v1.10.4"
	upDigest1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	upDigest2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func upCompose(image string) string {
	return "services:\n  app:\n    image: " + image + "\n    volumes:\n      - ./data:/data\n"
}

// upManifest is the test app at a version, with its one image promised by
// digest, data in ./data and a cache inside it. extra is appended as is.
func upManifest(version, image, digest, extra string) string {
	return fmt.Sprintf(`
id: upapp
manifest_version: 1
name: Up App
version: %q
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [upapp]
storage:
  data_volumes: [./data]
  cache_volumes: [./data/cache]
images:
  %s: %s
permissions:
  internet: false
  lan: false
%s`, version, image, digest, extra)
}

// publish writes a version of the test app into the catalog.
func (e *testEnv) publish(t *testing.T, version, image, digest, extra string) {
	t.Helper()
	e.writeCatalogApp(t, "upapp", upCompose(image), upManifest(version, image, digest, extra))
}

// installUp installs version 1.0 of the test app and writes some data.
func installUp(t *testing.T, e *testEnv, extra string) store.Instance {
	t.Helper()
	e.publish(t, "1.0", upImage1, upDigest1, extra)
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "upapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	writeSnapFile(t, e.dataFile(inst.ID, "app.db"), "v1 data", 0o644)
	return inst
}

func (e *testEnv) dataFile(id, name string) string {
	return filepath.Join(e.m.instanceDir(id), "data", name)
}

// runningVersion is the image the instance's compose names on disk now.
func (e *testEnv) onDisk(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(e.m.composeFile(id))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newVersionMigrates makes the new version write its data when it starts, the
// way a real app runs its migration, and optionally never become healthy.
func (e *testEnv) newVersionMigrates(t *testing.T, healthy bool) {
	t.Helper()
	e.docker.composeUp = func(_ context.Context, dir, _ string) (string, error) {
		b, _ := os.ReadFile(filepath.Join(dir, "compose.yml"))
		if strings.Contains(string(b), upImage2) {
			writeSnapFile(t, filepath.Join(dir, "data", "app.db"), "v2 migrated", 0o644)
			writeSnapFile(t, filepath.Join(dir, "data", "v2-only"), "new", 0o644)
		}
		return "", nil
	}
	e.docker.inspect = func(id, _ string) (bool, string, error) {
		if !healthy && strings.Contains(e.onDisk(t, id), upImage2) {
			return true, "unhealthy", nil
		}
		return true, "healthy", nil
	}
}

func (e *testEnv) row(t *testing.T, id string) store.Instance {
	t.Helper()
	r, err := e.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *testEnv) exists(id, name string) bool {
	_, err := os.Stat(filepath.Join(e.m.instanceDir(id), name))
	return err == nil
}

func pinDigests(t *testing.T, imgs []store.InstanceImage) []string {
	t.Helper()
	var out []string
	for _, i := range imgs {
		out = append(out, i.Digest)
	}
	return out
}

func TestUpdateGood(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.newVersionMigrates(t, true)
	e.publish(t, "2.0", upImage2, upDigest2, "")
	e.m.CheckUpdates(context.Background())
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable || r.Update.Version != "2.0" {
		t.Fatalf("check found %+v, want 2.0 available", r.Update)
	}

	var steps []string
	if err := e.m.Update(context.Background(), inst.ID, func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatalf("update: %v", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "2.0" || r.Update != (store.UpdateCheck{}) || r.UpdateFailure != (store.UpdateFailure{}) {
		t.Fatalf("row after update: state %q version %q update %+v failure %+v", r.State, r.Version, r.Update, r.UpdateFailure)
	}
	if !strings.Contains(e.onDisk(t, inst.ID), upImage2) {
		t.Fatal("compose.yml on disk is not the new version")
	}
	man, err := e.m.loadInstanceManifest(inst.ID)
	if err != nil || man.Version != "2.0" {
		t.Fatalf("manifest on disk: %v, %v", man, err)
	}
	pins, _ := e.store.GetInstanceImages(inst.ID)
	prev, _ := e.store.GetInstancePrevImages(inst.ID)
	if !slices.Equal(pinDigests(t, pins), []string{upDigest2}) || !slices.Equal(pinDigests(t, prev), []string{upDigest1}) {
		t.Fatalf("pins %v, previous pins %v", pins, prev)
	}
	for _, f := range updateFiles {
		if !e.exists(inst.ID, f+".prev") {
			t.Errorf("%s.prev is missing after a good update", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.m.instanceDir(inst.ID), "compose.yml.prev")); !strings.Contains(string(b), upImage1) {
		t.Error("compose.yml.prev is not the old version")
	}
	if e.exists(inst.ID, updateRecordFile) {
		t.Error("update.json is left after a good update")
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v2 migrated" {
		t.Errorf("data after a good update = %q, want the new version's", got)
	}
	if !slices.Contains(snapshotFiles(t, e, inst.ID), "pre-update-1.0.json") {
		t.Error("no snapshot of 1.0 was kept")
	}
	if got := e.caddy.route(inst.ID); got != "upstream:moose-"+inst.ID+"-app:80" {
		t.Errorf("route = %q", got)
	}
	// Pull before stop, stop before snapshot (the snapshot reads files, so
	// the order of the docker calls is the check here).
	ms := e.docker.methods()
	pull, stop := slices.Index(ms, "Pull"), slices.Index(ms, "ComposeStop")
	if pull < 0 || stop < 0 || pull > stop {
		t.Errorf("want the pull before the stop, got %v", ms)
	}
	if !slices.Contains(steps, "taking_snapshot") || !slices.Contains(steps, "waiting_healthy") {
		t.Errorf("steps = %v", steps)
	}
}

func TestUpdatePullFailureChangesNothing(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.publish(t, "2.0", upImage2, upDigest2, "")
	e.docker.pullErr["traefik/whoami@"+upDigest2] = errors.New("manifest unknown")
	before := len(e.docker.Calls())

	err := e.m.Update(context.Background(), inst.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "could not download") {
		t.Fatalf("update error = %v", err)
	}
	for _, c := range e.docker.Calls()[before:] {
		if c.method == "ComposeStop" || c.method == "ComposeUp" {
			t.Fatalf("a failed pull ran %s: the old version must never stop", c.method)
		}
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" || r.UpdateFailure.Kind != store.UpdateFailedPull || r.UpdateFailure.Version != "2.0" {
		t.Fatalf("row: state %q version %q failure %+v", r.State, r.Version, r.UpdateFailure)
	}
	if !strings.Contains(e.onDisk(t, inst.ID), upImage1) || e.exists(inst.ID, "compose.yml.prev") || e.exists(inst.ID, updateRecordFile) {
		t.Fatal("a failed pull changed the files")
	}
	if pins, _ := e.store.GetInstanceImages(inst.ID); !slices.Equal(pinDigests(t, pins), []string{upDigest1}) {
		t.Fatalf("pins = %v", pins)
	}
}

func TestUpdateHealthFailureRollsBack(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.newVersionMigrates(t, false)
	e.publish(t, "2.0", upImage2, upDigest2, "")

	err := e.m.Update(context.Background(), inst.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "did not become healthy") || !strings.Contains(err.Error(), "put the old version back") {
		t.Fatalf("update error = %v", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" {
		t.Fatalf("after rollback: state %q version %q, want running 1.0", r.State, r.Version)
	}
	if r.UpdateFailure.Kind != store.UpdateFailedHealth || r.UpdateFailure.Version != "2.0" || r.UpdateFailure.At.IsZero() {
		t.Fatalf("failure = %+v", r.UpdateFailure)
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v1 data" {
		t.Errorf("data after rollback = %q, want the old data", got)
	}
	if _, err := os.Stat(e.dataFile(inst.ID, "v2-only")); !os.IsNotExist(err) {
		t.Error("a file the new version wrote survived the rollback")
	}
	if !strings.Contains(e.onDisk(t, inst.ID), upImage1) {
		t.Error("the old compose is not back")
	}
	if man, _ := e.m.loadInstanceManifest(inst.ID); man == nil || man.Version != "1.0" {
		t.Error("the old manifest is not back")
	}
	if pins, _ := e.store.GetInstanceImages(inst.ID); !slices.Equal(pinDigests(t, pins), []string{upDigest1}) {
		t.Errorf("pins = %v", pins)
	}
	if prev, _ := e.store.GetInstancePrevImages(inst.ID); len(prev) != 0 || e.exists(inst.ID, "compose.yml.prev") || e.exists(inst.ID, updateRecordFile) {
		t.Error("the rollback left the previous generation or its record")
	}
	if got := e.caddy.route(inst.ID); got != "upstream:moose-"+inst.ID+"-app:80" {
		t.Errorf("route = %q", got)
	}
	// The failed version's image goes; the old one stays.
	removed := false
	for _, c := range e.docker.Calls() {
		if c.method == "RemoveImage" {
			if strings.Contains(c.String(), upDigest1) {
				t.Fatal("the rollback removed the old version's image")
			}
			removed = removed || strings.Contains(c.String(), upDigest2)
		}
	}
	if !removed {
		t.Error("the failed version's image was not removed")
	}
}

func TestUpdateRollbackStartFailsIsUpdateFailed(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.newVersionMigrates(t, false)
	e.publish(t, "2.0", upImage2, upDigest2, "")
	// Neither version becomes healthy.
	e.docker.inspect = func(string, string) (bool, string, error) { return true, "unhealthy", nil }

	if err := e.m.Update(context.Background(), inst.ID, nil); err == nil || !strings.Contains(err.Error(), "did not start again") {
		t.Fatalf("update error = %v", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "update_failed" || r.Version != "1.0" || r.UpdateFailure.Kind != store.UpdateFailedHealth {
		t.Fatalf("row: state %q version %q failure %+v", r.State, r.Version, r.UpdateFailure)
	}
	if got := e.caddy.route(inst.ID); got != "splash:failed" {
		t.Errorf("route = %q, want the failed splash", got)
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v1 data" {
		t.Errorf("data = %q: the undo ran, so the old data is back", got)
	}
	// Start is the retry, and it starts the old version.
	e.docker.inspect = nil
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start from update_failed: %v", err)
	}
	if r := e.row(t, inst.ID); r.State != "running" || r.Version != "1.0" {
		t.Fatalf("after start: state %q version %q", r.State, r.Version)
	}
}

func TestUpdateRestoreFailsKeepsRecordForRetry(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.publish(t, "2.0", upImage2, upDigest2, "")
	snapTar := filepath.Join(e.m.snapshotsDir(inst.ID), "pre-update-1.0.tar")
	// The new version starts, loses the snapshot's tar, and never goes healthy.
	e.docker.composeUp = func(_ context.Context, dir, _ string) (string, error) {
		b, _ := os.ReadFile(filepath.Join(dir, "compose.yml"))
		if strings.Contains(string(b), upImage2) {
			_ = os.Remove(snapTar)
		}
		return "", nil
	}
	e.docker.inspect = func(id, _ string) (bool, string, error) {
		if strings.Contains(e.onDisk(t, id), upImage2) {
			return true, "unhealthy", nil
		}
		return true, "healthy", nil
	}

	if err := e.m.Update(context.Background(), inst.ID, nil); err == nil || !strings.Contains(err.Error(), "could not put the old version back") {
		t.Fatalf("update error = %v", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "update_failed" || r.UpdateFailure.Kind != store.UpdateFailedRestore {
		t.Fatalf("row: state %q failure %+v", r.State, r.UpdateFailure)
	}
	if !e.exists(inst.ID, updateRecordFile) || !e.exists(inst.ID, "compose.yml.prev") {
		t.Fatal("the record and the previous generation must stay for a retry")
	}
	// A retry runs the undo again; with the tar gone it still cannot.
	if err := e.m.Start(context.Background(), inst.ID); err == nil {
		t.Fatal("start went on although the old data could not be put back")
	}
	if r := e.row(t, inst.ID); r.State != "update_failed" {
		t.Fatalf("state after the failed retry = %q", r.State)
	}
}

func TestUpdateNoSnapshotSpaceRestartsOld(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.publish(t, "2.0", upImage2, upDigest2, "")
	e.m.diskSpace = func(string) (uint64, uint64, error) { return 1 << 10, 1 << 40, nil }
	before := len(e.docker.Calls())

	err := e.m.Update(context.Background(), inst.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("update error = %v", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" || r.UpdateFailure.Kind != store.UpdateFailedSnapshot || !strings.Contains(r.UpdateFailure.Message, "free space") {
		t.Fatalf("row: state %q version %q failure %+v", r.State, r.Version, r.UpdateFailure)
	}
	var ms []string
	for _, c := range e.docker.Calls()[before:] {
		ms = append(ms, c.method)
	}
	stop, up := slices.Index(ms, "ComposeStop"), slices.Index(ms, "ComposeUp")
	if stop < 0 || up < stop {
		t.Fatalf("want a stop, then the old version up again: %v", ms)
	}
	if !strings.Contains(e.onDisk(t, inst.ID), upImage1) || e.exists(inst.ID, "compose.yml.prev") || e.exists(inst.ID, updateRecordFile) {
		t.Fatal("a refused snapshot changed the files")
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v1 data" {
		t.Fatalf("data = %q", got)
	}
}

// TestUpdateRefusals: each class the box does not apply is stored by the
// check with a plain reason, and Update refuses it before anything runs.
func TestUpdateRefusals(t *testing.T) {
	v2 := func(extra string) string { return upManifest("2.0", upImage2, upDigest2, extra) }
	for name, tc := range map[string]struct {
		v1     string // extra for 1.0
		v2     string // the whole 2.0 manifest
		remap  bool
		status string
		reason string
	}{
		"internet widened": {
			v2:     strings.Replace(v2(""), "internet: false", "internet: true", 1),
			status: store.UpdateNeedsApproval, reason: "the internet",
		},
		"public path added": {
			v2:     v2("access:\n  public_paths: [/api]\n"),
			status: store.UpdateNeedsApproval, reason: "anyone can open /api",
		},
		"folder added": {
			v2:     strings.Replace(v2(""), "  lan: false\n", "  lan: false\n  folders:\n    - folder: photos\n      mode: read\n", 1),
			status: store.UpdateNeedsApproval, reason: "Photos",
		},
		"managed service major": {
			v1:     upDatabase,
			v2:     v2(strings.Replace(upDatabase, `"16"`, `"17"`, 1)),
			status: store.UpdateServiceMajor, reason: "postgres 17",
		},
		"new required setting": {
			v2:     v2("config:\n  - app_env: API_TOKEN\n    title: API token\n    description: x\n    required: true\n"),
			status: store.UpdateBlocked, reason: "API token",
		},
		"tier change": {
			v2: v2("root_setup: true\n"), remap: true,
			status: store.UpdateBlocked, reason: "different kind of isolation",
		},
		"service user change": {
			v2:     v2("service_user: true\n"),
			status: store.UpdateBlocked, reason: "different kind of isolation",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			if tc.remap {
				e.remapped()
			}
			inst := installUp(t, e, tc.v1)
			e.writeCatalogApp(t, "upapp", upCompose(upImage2), tc.v2)

			e.m.CheckUpdates(context.Background())
			r := e.row(t, inst.ID)
			if r.Update.Status != tc.status || r.Update.Version != "2.0" || !strings.Contains(r.Update.Reason, tc.reason) {
				t.Fatalf("check = %+v, want status %s with %q", r.Update, tc.status, tc.reason)
			}
			pulls := len(e.docker.pulled())
			err := e.m.Update(context.Background(), inst.ID, nil)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("update error = %v, want the reason", err)
			}
			if len(e.docker.pulled()) != pulls {
				t.Fatal("a refused update pulled images")
			}
			if r := e.row(t, inst.ID); r.State != "running" || r.Version != "1.0" {
				t.Fatalf("row: state %q version %q", r.State, r.Version)
			}
		})
	}
}

func TestUpdateUnsupportedManifestVersion(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.m.catalogEntry = func(string) (catalog.Entry, error) { return catalog.Entry{ID: "upapp", Version: "3.0"}, nil }
	e.m.catalogLoad = func(context.Context, string) (*manifest.Manifest, []byte, error) {
		return nil, nil, fmt.Errorf("%w: manifest_version 2", manifest.ErrUnsupportedManifestVersion)
	}
	e.m.CheckUpdates(context.Background())
	r := e.row(t, inst.ID)
	if r.Update.Status != store.UpdateNeedsNewerMoose || r.Update.Version != "3.0" || !strings.Contains(r.Update.Reason, "newer moose") {
		t.Fatalf("check = %+v", r.Update)
	}
	if err := e.m.Update(context.Background(), inst.ID, nil); err == nil || !strings.Contains(err.Error(), "newer moose") {
		t.Fatalf("update error = %v", err)
	}

	// A newer brain reads that manifest: its first check classifies the
	// parked version again. A brain run asks once, not on every sync.
	loads := 0
	e.m.catalogLoad = func(context.Context, string) (*manifest.Manifest, []byte, error) {
		loads++
		return nil, nil, fmt.Errorf("%w: manifest_version 2", manifest.ErrUnsupportedManifestVersion)
	}
	e.m.CheckUpdates(context.Background())
	if loads != 0 {
		t.Fatalf("this brain run loaded the parked version again (%d loads)", loads)
	}
	e.m.parkedSeen = nil // a new brain run
	e.publish(t, "3.0", upImage2, upDigest2, "")
	e.m.catalogLoad = e.cat.Load
	e.m.CheckUpdates(context.Background())
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable || r.Update.Version != "3.0" {
		t.Fatalf("a newer brain still parks it: %+v", r.Update)
	}
}

func TestUpdateNoUpdateAndDoor2(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	if err := e.m.Update(context.Background(), inst.ID, nil); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("same version: %v, want ErrNoUpdate", err)
	}
	e.docker.digests[testImage] = testDigest // a Door-2 pull is trusted on first use
	custom, err := e.m.InstallCustom(context.Background(), CustomSpec{
		Name: "Pasted", Compose: whoamiCompose, MainPort: 80, Permissions: manifest.Permissions{Internet: true},
	}, Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil)
	if err != nil {
		t.Fatalf("install custom: %v", err)
	}
	loads := 0
	e.m.catalogLoad = func(ctx context.Context, id string) (*manifest.Manifest, []byte, error) {
		loads++
		return e.cat.Load(ctx, id)
	}
	e.m.CheckUpdates(context.Background())
	if r := e.row(t, custom.ID); r.Update != (store.UpdateCheck{}) || loads != 0 {
		t.Fatalf("Door-2 app: check %+v after %d loads", r.Update, loads)
	}
	if err := e.m.Update(context.Background(), custom.ID, nil); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("Door-2 update: %v, want ErrNoUpdate", err)
	}
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Update(context.Background(), inst.ID, nil); !errors.Is(err, ErrUpdateNotRunning) {
		t.Fatalf("stopped app: %v, want ErrUpdateNotRunning", err)
	}
}

// TestUpdateImageUserChangeRefused: an app in the image tier whose new image
// runs as another user is refused after the pull, before the old version
// stops, and the check remembers it.
func TestUpdateImageUserChangeRefused(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	recordChowns(e)
	e.docker.imageUsers = map[string]string{
		"traefik/whoami@" + upDigest1: "1000:1000",
		"traefik/whoami@" + upDigest2: "2000:2000",
	}
	inst := installUp(t, e, "image_user: true\n")
	if inst.UsernsTier != store.UsernsTierImage {
		t.Fatalf("tier = %q", inst.UsernsTier)
	}
	e.publish(t, "2.0", upImage2, upDigest2, "image_user: true\n")
	before := len(e.docker.Calls())

	err := e.m.Update(context.Background(), inst.ID, nil)
	if !errors.Is(err, ErrImageUserChange) && (err == nil || err.Error() != ErrImageUserChange.Error()) {
		t.Fatalf("update error = %v, want the image-user refusal", err)
	}
	for _, c := range e.docker.Calls()[before:] {
		if c.method == "ComposeStop" {
			t.Fatal("the old version stopped for a refused update")
		}
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" || r.Update.Status != store.UpdateBlocked || r.UpdateFailure.Kind != store.UpdateFailedRefused {
		t.Fatalf("row: state %q version %q check %+v failure %+v", r.State, r.Version, r.Update, r.UpdateFailure)
	}

	// The same user in the new image updates.
	e.docker.imageUsers["traefik/whoami@"+upDigest2] = "1000:1000"
	e.publish(t, "2.1", upImage2, upDigest2, "image_user: true\n")
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update with the same image user: %v", err)
	}
}

const upDatabase = "services:\n  database:\n    type: postgres\n    version: \"16\"\n"

// TestUpdateNewManagedService: a managed service the old version did not have
// is provisioned in the update, and dropped again when the update rolls back.
func TestUpdateNewManagedService(t *testing.T) {
	t.Run("kept on success", func(t *testing.T) {
		e := newTestEnv(t)
		inst := installUp(t, e, "")
		e.newVersionMigrates(t, true)
		e.publish(t, "2.0", upImage2, upDigest2, upDatabase)
		if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
			t.Fatalf("update: %v", err)
		}
		grants, _ := e.store.GetServiceGrants(inst.ID)
		if len(grants) != 1 || grants[0].LogicalName != "database" {
			t.Fatalf("grants = %+v", grants)
		}
		env, _ := os.ReadFile(filepath.Join(e.m.instanceDir(inst.ID), ".env"))
		if !strings.Contains(string(env), "MOOSE_SERVICE_DATABASE_DSN=") {
			t.Fatal("the new service's credentials are not in the .env")
		}
	})
	t.Run("dropped on rollback", func(t *testing.T) {
		e := newTestEnv(t)
		inst := installUp(t, e, "")
		e.newVersionMigrates(t, false)
		e.publish(t, "2.0", upImage2, upDigest2, upDatabase+"secrets:\n  - name: signing\n")
		if err := e.m.Update(context.Background(), inst.ID, nil); err == nil {
			t.Fatal("want a failed update")
		}
		if grants, _ := e.store.GetServiceGrants(inst.ID); len(grants) != 0 {
			t.Fatalf("grants after rollback = %+v, want none", grants)
		}
		if secrets, _ := e.store.GetInstanceSecrets(inst.ID); len(secrets) != 0 {
			t.Fatalf("secrets after rollback = %+v, want none", secrets)
		}
		dropped := false
		for _, c := range e.docker.Calls() {
			dropped = dropped || (c.method == "RunOneOff" && strings.Contains(c.String(), "DROP DATABASE"))
		}
		if !dropped {
			t.Fatal("the new database was not dropped")
		}
		env, _ := os.ReadFile(filepath.Join(e.m.instanceDir(inst.ID), ".env"))
		if strings.Contains(string(env), "MOOSE_SERVICE_DATABASE") {
			t.Fatal("the old .env is not back")
		}
	})
}

// TestUpdateNewConfigDefault: a config field the new version adds gets its
// default, and loses it again on a rollback.
func TestUpdateNewConfigDefault(t *testing.T) {
	field := "config:\n  - app_env: LOG_LEVEL\n    title: Log level\n    description: x\n    required: true\n    default: info\n"
	for _, healthy := range []bool{true, false} {
		e := newTestEnv(t)
		inst := installUp(t, e, "")
		e.newVersionMigrates(t, healthy)
		e.publish(t, "2.0", upImage2, upDigest2, field)
		e.m.CheckUpdates(context.Background())
		if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable {
			t.Fatalf("a required field with a default blocks the update: %+v", r.Update)
		}
		err := e.m.Update(context.Background(), inst.ID, nil)
		if (err == nil) != healthy {
			t.Fatalf("healthy %v: update error %v", healthy, err)
		}
		cfg, _ := e.store.GetInstanceConfig(inst.ID)
		if healthy && (len(cfg) != 1 || cfg[0].AppEnv != "LOG_LEVEL" || cfg[0].Value != "info") {
			t.Fatalf("after a good update the config is %+v, want LOG_LEVEL=info", cfg)
		}
		if !healthy && len(cfg) != 0 {
			t.Fatalf("after a rollback the config is %+v, want none", cfg)
		}
	}
}

// TestUpdateKeepsSecrets: a secret the instance has keeps its value through
// an update, and a new one is generated.
func TestUpdateKeepsSecrets(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "secrets:\n  - name: auth\n")
	e.newVersionMigrates(t, true)
	before, _ := e.store.GetInstanceSecrets(inst.ID)
	e.publish(t, "2.0", upImage2, upDigest2, "secrets:\n  - name: auth\n  - name: extra\n")
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, _ := e.store.GetInstanceSecrets(inst.ID)
	if len(after) != 2 || after[0] != before[0] {
		t.Fatalf("secrets before %+v after %+v", before, after)
	}
}

// TestReconcileRecoversInterruptedUpdate: a brain that stopped after it wrote
// the new version leaves an updating row; the startup pass restores the
// snapshot and the previous generation and the row is running again.
func TestReconcileRecoversInterruptedUpdate(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	id := inst.ID

	// Do by hand what Update had done when the brain stopped.
	oldPins, _ := e.store.GetInstanceImages(id)
	if err := e.m.keepPrevious(id, oldPins); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.TakeSnapshot(ctx, id); err != nil {
		t.Fatal(err)
	}
	rec := updateRecord{From: "1.0", To: "2.0", PrevKept: true, SnapshotTaken: true, Writing: true}
	if err := e.m.writeUpdateRecord(id, rec); err != nil {
		t.Fatal(err)
	}
	e.publish(t, "2.0", upImage2, upDigest2, "")
	app := mustLoadApp(t, e.m, "upapp")
	if err := e.m.writeInstanceDir(id, app.Manifest, app.Compose); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetInstanceImages(id, []store.InstanceImage{{Service: "app", Image: upImage2, Digest: upDigest2}}); err != nil {
		t.Fatal(err)
	}
	writeSnapFile(t, e.dataFile(id, "app.db"), "v2 migrated", 0o644)
	if err := e.store.SetState(id, "updating"); err != nil {
		t.Fatal(err)
	}
	e.docker.psManaged[id] = true // the new version is up

	before := len(e.docker.Calls())
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := e.row(t, id)
	if r.State != "running" || r.Version != "1.0" || r.UpdateFailure.Kind != store.UpdateFailedInterrupted || r.UpdateFailure.Version != "2.0" {
		t.Fatalf("row: state %q version %q failure %+v", r.State, r.Version, r.UpdateFailure)
	}
	if got := readSnapFile(t, e.dataFile(id, "app.db")); got != "v1 data" {
		t.Errorf("data = %q, want the old data", got)
	}
	if !strings.Contains(e.onDisk(t, id), upImage1) || e.exists(id, updateRecordFile) || e.exists(id, "compose.yml.prev") {
		t.Error("the old files are not back, or the record is left")
	}
	if pins, _ := e.store.GetInstanceImages(id); !slices.Equal(pinDigests(t, pins), []string{upDigest1}) {
		t.Errorf("pins = %v", pins)
	}
	up := false
	for _, c := range e.docker.Calls()[before:] {
		up = up || c.method == "ComposeUp"
	}
	if !up {
		t.Error("the old version was not brought up again")
	}
	if got := e.caddy.route(id); got != "upstream:moose-"+id+"-app:80" {
		t.Errorf("route = %q", got)
	}
}

// TestReconcileRecoversUpdateWithNoRecord: the brain stopped before the update
// changed anything (no record yet); the row is just running again.
func TestReconcileRecoversUpdateWithNoRecord(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	if err := e.store.SetInstanceUpdateCheck(inst.ID, store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetState(inst.ID, "updating"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.UpdateFailure.Kind != store.UpdateFailedInterrupted || r.UpdateFailure.Version != "2.0" {
		t.Fatalf("row: state %q failure %+v", r.State, r.UpdateFailure)
	}
	if !e.docker.called("ComposeUp") {
		t.Error("the pass did not bring the app up")
	}
}

// TestCheckUpdatesByVersionOnly: a new version is an update, a payload change
// with the same version is none, and each new version is loaded once.
func TestCheckUpdatesByVersionOnly(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	loads := 0
	e.m.catalogLoad = func(ctx context.Context, id string) (*manifest.Manifest, []byte, error) {
		loads++
		return e.cat.Load(ctx, id)
	}
	ctx := context.Background()

	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update != (store.UpdateCheck{}) || loads != 0 {
		t.Fatalf("same version: check %+v, %d loads", r.Update, loads)
	}
	// The same version with other digests (an index digest replaced by the
	// amd64 one, APP_STORE.md) is no update.
	e.publish(t, "1.0", upImage1, upDigest2, "")
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update != (store.UpdateCheck{}) || loads != 0 {
		t.Fatalf("digest change: check %+v, %d loads", r.Update, loads)
	}

	e.publish(t, "2.0", upImage2, upDigest2, "")
	e.m.CheckUpdates(ctx)
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Version != "2.0" || r.Update.Status != store.UpdateAvailable || loads != 1 {
		t.Fatalf("new version: check %+v, %d loads (want 1)", r.Update, loads)
	}
	e.publish(t, "2.1", upImage2, upDigest2, "")
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Version != "2.1" || loads != 2 {
		t.Fatalf("newer version: check %+v, %d loads (want 2)", r.Update, loads)
	}
	// The catalog back on the installed version clears it.
	e.publish(t, "1.0", upImage1, upDigest1, "")
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update != (store.UpdateCheck{}) {
		t.Fatalf("back to the installed version: check %+v", r.Update)
	}
}

// TestCheckUpdatesSkipsAMovedRow: the check loads the new manifest with no
// lock held, so an update may finish, or another operation may hold the app,
// while it loads. Then it stores nothing, and the next check tries again.
func TestCheckUpdatesSkipsAMovedRow(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	ctx := context.Background()
	e.publish(t, "2.0", upImage2, upDigest2, "")

	// An update to 2.0 finishes while the check loads 2.0's manifest.
	e.m.catalogLoad = func(ctx context.Context, id string) (*manifest.Manifest, []byte, error) {
		if err := e.store.FinishInstanceUpdate(inst.ID, "2.0", inst.Name); err != nil {
			t.Fatal(err)
		}
		return e.cat.Load(ctx, id)
	}
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Version != "2.0" || r.Update != (store.UpdateCheck{}) {
		t.Fatalf("after an update finished mid-check: version %s, check %+v, want 2.0 and no update", r.Version, r.Update)
	}

	// Another operation holds the app while the check loads: nothing is
	// stored, and the next check stores the result.
	e.publish(t, "3.0", upImage2, upDigest2, "")
	var unlock func()
	e.m.catalogLoad = func(ctx context.Context, id string) (*manifest.Manifest, []byte, error) {
		unlock = e.m.lockInstance(inst.ID)
		return e.cat.Load(ctx, id)
	}
	e.m.CheckUpdates(ctx)
	unlock()
	if r := e.row(t, inst.ID); r.Update != (store.UpdateCheck{}) {
		t.Fatalf("lock held: check %+v, want nothing stored", r.Update)
	}
	e.m.catalogLoad = e.cat.Load
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Version != "3.0" || r.Update.Status != store.UpdateAvailable {
		t.Fatalf("next check: %+v, want 3.0 available", r.Update)
	}
}

func TestUpdateHealthBudget(t *testing.T) {
	m := &Manager{healthWait: healthWaitTimeout, updateMargin: updateHealthMargin, defaultStartPeriod: manifest.DefaultStartPeriod}
	if got := m.updateHealthBudget(&manifest.Manifest{}); got != 120*time.Second {
		t.Errorf("no probe: %s, want 120s", got)
	}
	slow := &manifest.Manifest{HealthProbe: &manifest.HealthProbe{Path: "/", StartPeriod: 5 * time.Minute}}
	if got := m.updateHealthBudget(slow); got != 6*time.Minute {
		t.Errorf("start_period 5m: %s, want 6m", got)
	}
	fast := &manifest.Manifest{HealthProbe: &manifest.HealthProbe{Path: "/", StartPeriod: 10 * time.Second}}
	if got := m.updateHealthBudget(fast); got != healthWaitTimeout {
		t.Errorf("start_period 10s: %s, want the 120s floor", got)
	}
}

func TestHeavyLimit(t *testing.T) {
	m := &Manager{heavy: make(chan struct{}, maxHeavyOps)}
	var releases []func()
	for range maxHeavyOps {
		r, err := m.acquireHeavy(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.acquireHeavy(ctx); err == nil {
		t.Fatal("a fourth install or update ran next to three")
	}
	releases[0]()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, err := m.acquireHeavy(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		r()
	}()
	wg.Wait()
}

// TestUninstallAfterUpdateRemovesBothGenerations: the previous generation's
// images go at uninstall too, so an update leaks no image.
func TestUninstallAfterUpdateRemovesBothGenerations(t *testing.T) {
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.newVersionMigrates(t, true)
	e.publish(t, "2.0", upImage2, upDigest2, "")
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range e.docker.Calls() {
		if c.method == "RemoveImage" {
			removed = append(removed, c.String())
		}
	}
	for _, d := range []string{upDigest1, upDigest2} {
		if !slices.ContainsFunc(removed, func(s string) bool { return strings.Contains(s, d) }) {
			t.Errorf("image %s was not removed at uninstall: %v", d, removed)
		}
	}
}
