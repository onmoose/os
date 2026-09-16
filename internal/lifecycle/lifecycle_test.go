package lifecycle

// Layer 3 — lifecycle scenarios driven against fakeDocker/fakeCaddy/fakeHost
// (docs/dev/testing-brain.md). Each scenario sets up a Manager via newTestEnv,
// scripts the minimum fake state for that path, runs Install/Uninstall/
// Reconcile, and asserts end state + only the one or two driver calls that
// actually matter for that scenario.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/admission"
	"github.com/onmoose/moose/internal/catalog"
	"github.com/onmoose/moose/internal/events"
	"github.com/onmoose/moose/internal/manifest"
	"github.com/onmoose/moose/internal/store"
	"gopkg.in/yaml.v3"
)

const (
	testImage  = "traefik/whoami:v1.10.3"
	testDigest = "sha256:43a68d10b9dfcfc3ffbfe4dd42100dc9aeaf29b3a5636c856337a5940f1b4f1c"
)

type testEnv struct {
	m        *Manager
	docker   *fakeDocker
	caddy    *fakeCaddy
	host     *fakeHost
	store    *store.Store
	stateDir string
	cat      *catalog.Catalog
	catDir   string
}

// newTestEnv wires a Manager backed by fakes + a real temp store/state dir
// and a real catalog rooted at a tmp dir. healthWait is set tight (200ms) so
// timeout scenarios complete in milliseconds.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cat := catalog.New(catDir)
	host := newFakeHost()
	host.homeRoot = t.TempDir()
	cd := newFakeCaddy()
	docker := newFakeDocker()
	bus := events.NewBus()
	m := NewManager(s, cat, host, cd, docker, bus, stateDir)
	// Root the household shared tree under a temp dir so shared-source bind paths
	// and (under a root-run suite) their on-disk preparation never touch the real
	// /srv (mirrors how the fake host roots personal homes under homeRoot).
	m.sharedRoot = t.TempDir()
	// Tests must never shell out to `docker compose config -q`.
	m.SetAdmitter(admission.CheckStructure)
	// Keep timeout small so health-failure tests finish fast.
	m.SetHealthTiming(200*time.Millisecond, 20*time.Millisecond)
	return &testEnv{
		m: m, docker: docker, caddy: cd, host: host,
		store: s, stateDir: stateDir, cat: cat, catDir: catDir,
	}
}

// writeCatalogApp writes a manifest+compose pair under <catDir>/<id>/.
func (e *testEnv) writeCatalogApp(t *testing.T, id, compose, manifestYAML string) {
	t.Helper()
	dir := filepath.Join(e.catDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
}

// whoamiManifest is a minimal Door-1 manifest with a catalog-promised digest.
func whoamiManifest(promisedDigest string) string {
	images := ""
	if promisedDigest != "" {
		images = fmt.Sprintf("images:\n  %s: %s\n", testImage, promisedDigest)
	}
	return `
id: whoami
manifest_version: 1
name: Whoami
version: "1.10"
compose_file: compose.yml
main_service: whoami
main_port: 80
preferred_slugs: [whoami]
permissions:
  internet: false
  lan: false
` + images
}

const whoamiCompose = `
services:
  whoami:
    image: traefik/whoami:v1.10.3
`

// --- 1. Install happy, Door-1 -------------------------------------------

func TestInstallHappyDoor1(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	// No digests entry: Door-1 resolves nothing from the registry — the catalog's
	// promise IS the address it pulls.

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if inst.State != "running" {
		t.Fatalf("end state = %q, want running", inst.State)
	}
	row, _ := e.store.Get(inst.ID)
	if row.State != "running" {
		t.Fatalf("SQLite state = %q, want running", row.State)
	}

	// Override on disk pins the image by digest (APP_LIFECYCLE.md).
	ov, err := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, "compose.override.yml"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(ov, &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	wantPin := "traefik/whoami@" + testDigest
	if got := doc.Services["whoami"].Image; got != wantPin {
		t.Fatalf("override image pin = %q, want %q", got, wantPin)
	}

	// Caddy route flipped from splash → upstream by install end.
	if got := e.caddy.route(inst.ID); got == "" || got[:9] != "upstream:" {
		t.Fatalf("caddy route at end = %q, want upstream:…", got)
	}

	// Ordered key driver calls: Pull → ComposeUp. No ImageInspect — there is no
	// registry-resolved digest to read back when the promise is what we pulled.
	want := []string{"Pull", "ComposeUp"}
	if !containsInOrder(e.docker.methods(), want) {
		t.Fatalf("driver call order missing %v in %v", want, e.docker.methods())
	}
	// The pull went to the digest, never the tag.
	if got := e.docker.pulled(); len(got) != 1 || got[0] != wantPin {
		t.Fatalf("pulled %v, want exactly [%s]", got, wantPin)
	}
}

// --- 2. Install happy, Door-2 -------------------------------------------

func TestInstallHappyDoor2(t *testing.T) {
	e := newTestEnv(t)
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.InstallCustom(context.Background(), CustomSpec{
		Name: "My Whoami", Compose: whoamiCompose, MainPort: 80,
		Permissions: manifest.Permissions{Internet: true},
	}, Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if inst.State != "running" {
		t.Fatalf("state=%q", inst.State)
	}
	// Door-2 has no manifest.Images map → no catalog verification step. We
	// can't assert "the verify branch wasn't entered" from outside, but we
	// can confirm install succeeded with an empty Images map by reading the
	// stored manifest back.
	manBytes, err := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, "manifest.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Images map[string]string `yaml:"images"`
	}
	_ = yaml.Unmarshal(manBytes, &m)
	if len(m.Images) != 0 {
		t.Fatalf("synthesized manifest must not carry catalog digests, got %v", m.Images)
	}
}

// --- 3. Admission rejection ---------------------------------------------

func TestInstallAdmissionRejection(t *testing.T) {
	e := newTestEnv(t)
	bad := `services:
  whoami:
    image: traefik/whoami:v1.10.3
    ports: ["8080:80"]
`
	e.writeCatalogApp(t, "whoami", bad, whoamiManifest(testDigest))

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatalf("want admission rejection")
	}

	// No SQLite row.
	list, _ := e.store.List()
	if len(list) != 0 {
		t.Fatalf("instance rows must be empty, got %d", len(list))
	}
	// No instance dir.
	entries, _ := os.ReadDir(filepath.Join(e.stateDir, "instances"))
	if len(entries) != 0 {
		t.Fatalf("instance dirs must be empty, got %d", len(entries))
	}
	// No Caddy/Docker work attempted (admission runs before any of it).
	if e.caddy.called("AddSplashRoute") || e.caddy.called("AddRoute") {
		t.Fatalf("caddy must not be touched: %v", e.caddy.calls)
	}
	if e.docker.called("Pull") || e.docker.called("ComposeUp") {
		t.Fatalf("docker must not be touched: %v", e.docker.methods())
	}
}

// --- 4. Upstream tag movement (Door-1) ----------------------------------

// An upstream rebuild moves the tag to new bytes after the catalog pinned it —
// routine for library images, which rebuild the same version tag whenever their
// base image is patched. The box must install what the catalog promised and not
// care (APP_STORE.md # Trust model, # Failure modes). Regression test for #331,
// where the tag was pulled and compared instead, turning every upstream rebuild
// into a failed install.
func TestInstallDoor1IgnoresUpstreamTagMove(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	// The tag now resolves to different bytes than the catalog pinned.
	e.docker.digests[testImage] = "sha256:rebuiltupstream"

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install must succeed despite the tag moving: %v", err)
	}

	// The promised bytes are what got pulled, pinned, and recorded.
	wantPin := "traefik/whoami@" + testDigest
	if got := e.docker.pulled(); len(got) != 1 || got[0] != wantPin {
		t.Fatalf("pulled %v, want exactly [%s] — the tag must never be pulled", got, wantPin)
	}
	if got := overridePin(t, e.stateDir, inst.ID, "whoami"); got != wantPin {
		t.Fatalf("override pin = %q, want %q", got, wantPin)
	}
	imgs, err := e.store.GetInstanceImages(inst.ID)
	if err != nil {
		t.Fatalf("InstanceImages: %v", err)
	}
	if len(imgs) != 1 || imgs[0].Digest != testDigest {
		t.Fatalf("stored images = %+v, want one with digest %s", imgs, testDigest)
	}
}

// A compose pinned by digest AND a catalog promise that disagrees is the
// catalog contradicting itself — a curation bug, not upstream drift. There is
// no safe pick between them, so the install fails and rolls back clean.
func TestInstallDigestPromiseContradictionRollsBack(t *testing.T) {
	e := newTestEnv(t)
	const composeRef = "traefik/whoami@sha256:pinnedincompose"
	compose := "services:\n  whoami:\n    image: " + composeRef + "\n    ports: [\"80\"]\n"
	man := fmt.Sprintf(`
id: whoami
manifest_version: 1
name: Whoami
version: "1.10"
compose_file: compose.yml
main_service: whoami
main_port: 80
images:
  %s: sha256:promisedbycatalog
`, composeRef)
	e.writeCatalogApp(t, "whoami", compose, man)

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatalf("want error when the compose pin and the catalog promise disagree")
	}

	// Rollback clean: SQLite empty, instance dir gone, compose up never run.
	list, _ := e.store.List()
	if len(list) != 0 {
		t.Fatalf("instance row must be rolled back, got %v", list)
	}
	if e.docker.called("ComposeUp") {
		t.Fatalf("ComposeUp must not run on a contradictory pin")
	}
	entries, _ := os.ReadDir(filepath.Join(e.stateDir, "instances"))
	if len(entries) != 0 {
		t.Fatalf("instance dir must be cleaned up, got %v", entries)
	}
}

// --- 5. Unpullable image -----------------------------------------------

func TestInstallUnpullableImageRollsBack(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.pullErr[testImage] = fmt.Errorf("registry unreachable")

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatalf("want pull error")
	}

	// Rollback before any network / route / compose work.
	for _, m := range []string{"NetworkCreate", "ComposeUp"} {
		// NetworkCreate for the per-app net specifically; allow no calls.
		if methodsContainArg(e.docker.Calls(), m, "moose-app-") {
			t.Fatalf("%s for per-app net must not run on pull failure", m)
		}
	}
	if e.caddy.called("AddSplashRoute") {
		t.Fatalf("caddy route must not be registered on pull failure")
	}
}

// --- 6. compose up failure ---------------------------------------------

func TestInstallComposeUpFailureRollsBack(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	e.docker.composeUpErr = fmt.Errorf("boom")

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatalf("want compose up error")
	}
	list, _ := e.store.List()
	if len(list) != 0 {
		t.Fatalf("instance row must be rolled back")
	}
	// teardown drops the per-app network.
	if !methodsContainArg(e.docker.Calls(), "NetworkRemove", "moose-app-") {
		t.Fatalf("expected NetworkRemove for per-app net during rollback")
	}
}

// --- 7. Health timeout --------------------------------------------------

func TestInstallHealthTimeoutKeepsInstanceDirAndFlipsSplashToFailed(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	// Inspect always reports not-running → wait times out per healthWait.
	e.docker.inspect = func(string, string) (bool, string, error) { return false, "starting", nil }

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatalf("want health timeout error")
	}

	// State = failed, instance dir kept (NOT rolled back), splash = failed,
	// route still registered.
	list, _ := e.store.List()
	if len(list) != 1 || list[0].State != "failed" {
		t.Fatalf("want one row in state=failed, got %+v", list)
	}
	inst := list[0]
	if _, err := os.Stat(filepath.Join(e.stateDir, "instances", inst.ID)); err != nil {
		t.Fatalf("instance dir must be kept: %v", err)
	}
	if got := e.caddy.route(inst.ID); got != "splash:failed" {
		t.Fatalf("caddy route = %q, want splash:failed", got)
	}
}

// --- 8. Uninstall (some teardown steps fail) ----------------------------

func TestUninstallTearsDownEvenIfStepsFail(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Make ComposeDown fail; uninstall must still proceed to RemoveRoute,
	// Unpublish, NetworkRemove, store.Delete.
	e.docker.composeDownErr = fmt.Errorf("compose down boom")

	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	if _, err := e.store.Get(inst.ID); err != store.ErrNotFound {
		t.Fatalf("store row should be gone, got %v", err)
	}
	if e.caddy.route(inst.ID) != "" {
		t.Fatalf("caddy route lingered: %q", e.caddy.route(inst.ID))
	}
	if e.host.isPublished(inst.Slug) {
		t.Fatalf("mDNS still published")
	}
	if !methodsContainArg(e.docker.Calls(), "NetworkRemove", "moose-app-"+inst.ID) {
		t.Fatalf("per-app NetworkRemove not called: %v", e.docker.methods())
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "instances", inst.ID)); !os.IsNotExist(err) {
		t.Fatalf("instance dir must be removed: %v", err)
	}
}

// --- 8b. Uninstall-time image reclaim -----------------------------------

func TestUninstallReclaimsUnreferencedImage(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	e.docker.calls = nil // focus assertions on the uninstall phase
	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	wantRef := repoOf(testImage) + "@" + testDigest
	if !methodsContainArg(e.docker.Calls(), "RemoveImage", wantRef) {
		t.Fatalf("RemoveImage(%s) not called: %v", wantRef, e.docker.methods())
	}
}

func TestUninstallKeepsImageReferencedByAnotherInstance(t *testing.T) {
	e := newTestEnv(t)
	e.docker.digests[testImage] = testDigest

	// Two distinct catalog apps that happen to share one image.
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.writeCatalogApp(t, "sharer", whoamiCompose, `
id: sharer
manifest_version: 1
name: Sharer
version: "1.0"
compose_file: compose.yml
main_service: whoami
main_port: 80
preferred_slugs: [sharer]
permissions:
  internet: false
  lan: false
images:
  `+testImage+`: `+testDigest+`
`)

	owner := Owner{UserID: "u_admin", Username: "admin"}
	a, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), owner, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install a: %v", err)
	}
	b, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "sharer"), owner, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install b: %v", err)
	}
	wantRef := repoOf(testImage) + "@" + testDigest

	// Uninstalling the first must NOT reclaim the still-shared image.
	e.docker.calls = nil
	if err := e.m.Uninstall(context.Background(), a.ID); err != nil {
		t.Fatalf("uninstall a: %v", err)
	}
	if methodsContainArg(e.docker.Calls(), "RemoveImage", wantRef) {
		t.Fatalf("RemoveImage(%s) called while another instance still uses it: %v", wantRef, e.docker.methods())
	}

	// Uninstalling the last referent reclaims it.
	e.docker.calls = nil
	if err := e.m.Uninstall(context.Background(), b.ID); err != nil {
		t.Fatalf("uninstall b: %v", err)
	}
	if !methodsContainArg(e.docker.Calls(), "RemoveImage", wantRef) {
		t.Fatalf("RemoveImage(%s) not called after last referent removed: %v", wantRef, e.docker.methods())
	}
}

func TestUninstallSucceedsWhenReclaimFails(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	e.docker.removeImageErr = errors.New("image in use")
	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall must succeed even when rmi fails: %v", err)
	}
	if _, err := e.store.Get(inst.ID); err == nil {
		t.Fatal("instance row should be gone after uninstall")
	}
}

// --- 9. Reconcile drift -------------------------------------------------

func TestReconcileBringsRunningInstanceBackUp(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	// Simulate: SQLite says running, but Docker has no containers.
	e.docker.psManaged = map[string]bool{}
	e.docker.calls = nil // clear so we can check what reconcile invoked
	e.caddy.calls = nil

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Per-app network must be re-created before compose up (the override
	// declares it external, so compose fails if the network was pruned).
	if !methodsContainArg(e.docker.Calls(), "NetworkCreate", "moose-app-"+inst.ID) {
		t.Fatalf("NetworkCreate for per-app net not called during reconcile: %v", e.docker.methods())
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("ComposeUp not called for drifted instance: %v", e.docker.methods())
	}
	// Route re-asserted.
	if !e.caddy.called("AddRoute") {
		t.Fatalf("AddRoute not called during reassert: %v", e.caddy.calls)
	}
}

func TestReconcileDriftedInstanceNetworkCreateBeforeComposeUp(t *testing.T) {
	// Regression: the override declares the per-app network as external, so
	// compose up fails if the network no longer exists. The reconciler must
	// ensure the network before calling compose up.
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	e.docker.psManaged = map[string]bool{}
	e.docker.calls = nil

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !containsInOrder(e.docker.methods(), []string{"NetworkCreate", "ComposeUp"}) {
		t.Fatalf("NetworkCreate must precede ComposeUp: %v", e.docker.methods())
	}
}

func TestReconcileDriftedInstanceNetworkCreateFailureSkipsComposeUp(t *testing.T) {
	// If the network cannot be re-created, compose up must not be attempted:
	// it would fail with "network declared as external, but could not be found"
	// and that error is less clear than the NetworkCreate failure itself.
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	e.docker.psManaged = map[string]bool{}
	e.docker.calls = nil
	e.docker.networkCreateErr = fmt.Errorf("docker: network driver error")

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if e.docker.called("ComposeUp") {
		t.Fatalf("ComposeUp must not be called when NetworkCreate fails: %v", e.docker.methods())
	}
}

func TestReconcileStopsStoppedButRunningInstance(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	// Desired state: stopped. Actual: running.
	if err := e.store.SetState(inst.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	e.docker.psManaged = map[string]bool{inst.ID: true}
	e.docker.calls = nil

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeStop", "moose-"+inst.ID) {
		t.Fatalf("ComposeStop not called: %v", e.docker.methods())
	}
}

func TestReconcileTearsDownOrphanContainers(t *testing.T) {
	e := newTestEnv(t)
	// No SQLite row; Docker reports a managed container for unknown instance.
	e.docker.psManaged = map[string]bool{"ghost-id": true}

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// No instance dir on disk → orphan path uses RemoveContainersByInstance,
	// then NetworkRemove for the per-app net.
	if !e.docker.called("RemoveContainersByInstance") {
		t.Fatalf("orphan path must call RemoveContainersByInstance: %v", e.docker.methods())
	}
	if !methodsContainArg(e.docker.Calls(), "NetworkRemove", "moose-app-ghost-id") {
		t.Fatalf("orphan NetworkRemove not called: %v", e.docker.methods())
	}
}

// --- helpers ------------------------------------------------------------

func containsInOrder(haystack, needles []string) bool {
	i := 0
	for _, h := range haystack {
		if i < len(needles) && h == needles[i] {
			i++
		}
	}
	return i == len(needles)
}

// methodsContainArg returns true if any recorded call to `method` has an arg
// whose string form contains substr.
func methodsContainArg(calls []call, method, substr string) bool {
	for _, c := range calls {
		if c.method != method {
			continue
		}
		for _, a := range c.args {
			if s := fmt.Sprintf("%v", a); contains(s, substr) {
				return true
			}
		}
	}
	return false
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- override: main-service container_name pin (#83) ----------------------

// Docker's journald driver tags log lines with the *running* container's
// name; without a pin, compose appends a replica suffix ("-1") and the Logs
// tail's exact CONTAINER_NAME match finds nothing. The override must pin the
// main service to the moose-<id>-<service> stem MainContainerName returns,
// and must leave sidecars unpinned (an explicit container_name forbids
// scaling, which only the main service guarantees).
func TestOverridePinsMainContainerName(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "jobapp", migrateJobCompose, migrateJobManifest)
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "jobapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readInstanceFile(t, e, inst.ID, "compose.override.yml")), &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	want := "moose-" + inst.ID + "-web"
	if got := doc.Services["web"]["container_name"]; got != want {
		t.Fatalf("main service container_name = %v, want %q", got, want)
	}
	for _, svc := range []string{"migrate", "seed"} {
		if v, present := doc.Services[svc]["container_name"]; present {
			t.Fatalf("%s container_name = %v, want unpinned (only the main service is)", svc, v)
		}
	}
	// The pinned name and the name the Logs tail queries must be the same string.
	if name, err := e.m.MainContainerName(inst.ID); err != nil || name != want {
		t.Fatalf("MainContainerName = %q, %v; want %q", name, err, want)
	}
}

// mustLoadApp loads a catalog app's install payload for a test that then calls
// Install. Install takes the payload rather than an id (lifecycle.CatalogApp),
// so that one install reads the manifest and compose exactly once; a test does
// the load the API layer does in production.
func mustLoadApp(t *testing.T, m *Manager, manifestID string) CatalogApp {
	t.Helper()
	app, err := m.LoadCatalogApp(context.Background(), manifestID)
	if err != nil {
		t.Fatalf("load catalog app %q: %v", manifestID, err)
	}
	return app
}

// TestInstallUsesTheLoadedPayloadNotAReFetch pins why Install takes a
// CatalogApp instead of a manifest id (#434, Greptile P1 on #436).
//
// An install reads the catalog in the REQUEST, to validate the folder, mail and
// config elections against the manifest, and then runs the transaction in an
// async job. While the two documents lived in one bulk snapshot the job could
// re-read them cheaply; now each is its own fetch, and a re-read can land on a
// different publication than the one the request validated — installing an app
// whose manifest nothing checked, or pairing one publication's manifest with
// another's compose.
//
// The catalog is republished here BETWEEN the load and the install, with a
// manifest that would produce a visibly different instance. Install must ignore
// it completely: it was handed a payload, and that payload is what it installs.
func TestInstallUsesTheLoadedPayloadNotAReFetch(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	app := mustLoadApp(t, e.m, "whoami")

	// The store republishes whoami under a new name and slug while the install
	// request is still in flight.
	republished := strings.Replace(whoamiManifest(testDigest), "name: Whoami", "name: Republished", 1)
	republished = strings.Replace(republished, "preferred_slugs: [whoami]", "preferred_slugs: [republished]", 1)
	e.writeCatalogApp(t, "whoami", whoamiCompose, republished)

	inst, err := e.m.Install(context.Background(), app,
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if inst.Name != "Whoami" || inst.Slug != "whoami" {
		t.Fatalf("installed name=%q slug=%q, want the loaded payload's Whoami/whoami — "+
			"Install re-read the catalog and picked up a publication nothing validated", inst.Name, inst.Slug)
	}
	// And the manifest persisted next to the installation is that same one.
	man, err := e.m.InstanceManifest(inst.ID)
	if err != nil {
		t.Fatalf("read instance manifest: %v", err)
	}
	if man.Name != "Whoami" {
		t.Errorf("persisted manifest name = %q, want Whoami", man.Name)
	}
}
