package lifecycle

// Pulling an image from a backup source when upstream fails (#588).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
	"gopkg.in/yaml.v3"
)

// The invented backup sources the tests use. Neither is a real registry.
const (
	testSourceA = "mirror-a.example.test/traefik/whoami"
	testSourceB = "mirror-b.example.test/traefik/whoami"
)

var (
	testUpstreamRef = "traefik/whoami@" + testDigest
	testSourceARef  = testSourceA + "@" + testDigest
	testSourceBRef  = testSourceB + "@" + testDigest
)

// whoamiSourcesManifest is the whoami manifest with the catalog's backup
// sources on its one image, in the published shape.
func whoamiSourcesManifest(sources ...string) string {
	block := fmt.Sprintf("images:\n  %s:\n    digest: %s\n    sources:\n", testImage, testDigest)
	for _, s := range sources {
		block += "      - ref: " + s + "\n"
	}
	return strings.Replace(whoamiManifest(testDigest), fmt.Sprintf("images:\n  %s: %s\n", testImage, testDigest), block, 1)
}

func pullErrorText(ref, msg string) error {
	return fmt.Errorf("pull %s: exit status 1\n%s", ref, msg)
}

// Each kind of upstream error, with the pulls it leads to and the reference it
// ends on. Source A fails and source B serves, so a row that moves shows both
// in order.
func TestPullImageMovesToSources(t *testing.T) {
	fastPullRetries(t)
	for _, tc := range []struct {
		name  string
		msg   string
		moves bool
	}{
		{"unreachable", "dial tcp: lookup registry.example.test: no such host", true},
		{"timeout", "Get \"https://registry.example.test/v2/\": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)", true},
		{"5xx", "Error response from daemon: received unexpected HTTP status: 503 Service Unavailable", true},
		{"manifest unknown", "Error response from daemon: manifest unknown: manifest unknown", true},
		{"401", "Error response from daemon: Head \"https://registry.example.test/v2/x/manifests/sha256:abc\": unauthorized", true},
		{"403", "Error response from daemon: pull access denied for x, repository does not exist or may require 'docker login': denied: requested access to the resource is denied", true},
		{"disk full", "write /var/lib/docker/tmp/GetImageBlob123: no space left on device", false},
		{"daemon down", "Cannot connect to the Docker daemon at tcp://docker-proxy:2375. Is the docker daemon running?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDocker()
			upErr := pullErrorText(testUpstreamRef, tc.msg)
			d.pullErr[testUpstreamRef] = upErr
			d.pullErr[testSourceARef] = pullErrorText(testSourceARef, "manifest unknown")

			ref, err := pullImage(context.Background(), d, testUpstreamRef, []string{testSourceARef, testSourceBRef})
			if !tc.moves {
				if !errors.Is(err, upErr) {
					t.Fatalf("err = %v, want the upstream error", err)
				}
				if got, want := d.pulled(), []string{testUpstreamRef}; !reflect.DeepEqual(got, want) {
					t.Fatalf("pulls = %v, want %v (a local error does not move)", got, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("pull: %v", err)
			}
			if ref != testSourceBRef {
				t.Fatalf("ref = %q, want %q", ref, testSourceBRef)
			}
			if got, want := d.pulled(), []string{testUpstreamRef, testSourceARef, testSourceBRef}; !reflect.DeepEqual(got, want) {
				t.Fatalf("pulls = %v, want %v", got, want)
			}
		})
	}
}

// On a rate limit the box waits once and retries upstream before it moves. A
// source that serves ends the pull there.
func TestPullImageRateLimitMovesAfterFirstWait(t *testing.T) {
	fastPullRetries(t)
	d := newFakeDocker()
	d.pullErr[testUpstreamRef] = errRateLimited

	ref, err := pullImage(context.Background(), d, testUpstreamRef, []string{testSourceARef, testSourceBRef})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if ref != testSourceARef {
		t.Fatalf("ref = %q, want %q", ref, testSourceARef)
	}
	if got, want := d.pulled(), []string{testUpstreamRef, testUpstreamRef, testSourceARef}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pulls = %v, want %v", got, want)
	}
}

// When every source fails after a rate limit, the ladder finishes on upstream,
// and the error is upstream's.
func TestPullImageRateLimitFinishesLadder(t *testing.T) {
	fastPullRetries(t)
	d := newFakeDocker()
	d.pullErr[testUpstreamRef] = errRateLimited
	d.pullErr[testSourceARef] = pullErrorText(testSourceARef, "manifest unknown")

	_, err := pullImage(context.Background(), d, testUpstreamRef, []string{testSourceARef})
	if !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want the upstream rate-limit error", err)
	}
	want := []string{testUpstreamRef, testUpstreamRef, testSourceARef}
	for range pullRetryDelays[1:] {
		want = append(want, testUpstreamRef)
	}
	if got := d.pulled(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pulls = %v, want %v", got, want)
	}
}

// If upstream answers once the rate limit lifts, no source is tried.
func TestPullImageRateLimitClearsBeforeSources(t *testing.T) {
	fastPullRetries(t)
	d := newFakeDocker()
	d.pullFails = 1
	d.pullFailErr = errRateLimited

	ref, err := pullImage(context.Background(), d, testUpstreamRef, []string{testSourceARef})
	if err != nil || ref != testUpstreamRef {
		t.Fatalf("pull = %q, %v; want upstream", ref, err)
	}
	if got := len(d.pulled()); got != 2 {
		t.Fatalf("Pull called %d times, want 2", got)
	}
}

// When every source fails on a non-rate-limit error, the error is upstream's.
func TestPullImageAllSourcesFailReportsUpstream(t *testing.T) {
	d := newFakeDocker()
	upErr := pullErrorText(testUpstreamRef, "manifest unknown")
	d.pullErr[testUpstreamRef] = upErr
	d.pullErr[testSourceARef] = pullErrorText(testSourceARef, "dial tcp: no such host")
	d.pullErr[testSourceBRef] = pullErrorText(testSourceBRef, "denied")

	_, err := pullImage(context.Background(), d, testUpstreamRef, []string{testSourceARef, testSourceBRef})
	if !errors.Is(err, upErr) {
		t.Fatalf("err = %v, want the upstream error", err)
	}
	if got := len(d.pulled()); got != 3 {
		t.Fatalf("Pull called %d times, want 3", got)
	}
}

// A cancelled install never moves to a source.
func TestPullImageCancelledDoesNotMove(t *testing.T) {
	d := newFakeDocker()
	d.pullErr[testUpstreamRef] = pullErrorText(testUpstreamRef, "signal: killed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pullImage(ctx, d, testUpstreamRef, []string{testSourceARef})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got, want := d.pulled(), []string{testUpstreamRef}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pulls = %v, want %v", got, want)
	}
}

// A source this box cannot use is skipped: one that needs a login, and one that
// is not a plain repository.
func TestSourceRefsSkipsUnusable(t *testing.T) {
	got := sourceRefs(testImage, []manifest.ImageSource{
		{Ref: testSourceA, Auth: map[string]any{"kind": "token"}},
		{Ref: testSourceA + ":v1"},
		{Ref: testSourceA + "@sha256:abc"},
		{Ref: ""},
		{Ref: "mirror.example.test:5000/traefik/whoami"},
		{Ref: testSourceB},
	}, testDigest)
	want := []string{"mirror.example.test:5000/traefik/whoami@" + testDigest, testSourceBRef}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sourceRefs = %v, want %v", got, want)
	}
}

// The published shape parses: sources in order, and auth kept so the box can
// see it and skip that source.
func TestManifestParsesSources(t *testing.T) {
	man, err := manifest.Parse([]byte(strings.Replace(whoamiSourcesManifest(testSourceA),
		"      - ref: "+testSourceA+"\n",
		"      - ref: "+testSourceA+"\n      - ref: "+testSourceB+"\n        auth: {kind: token}\n", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	src := man.Images[testImage].Sources
	if len(src) != 2 || src[0].Ref != testSourceA || src[0].Auth != nil || src[1].Ref != testSourceB || src[1].Auth == nil {
		t.Fatalf("sources = %+v", src)
	}
}

func installCatalogWhoami(t *testing.T, e *testEnv) store.Instance {
	t.Helper()
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst
}

// An install whose upstream fails pulls from the first source that works, and
// the override, the stored pin and the uninstall all use that source's name.
func TestInstallFromSource(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiSourcesManifest(testSourceA, testSourceB))
	e.docker.pullErr[testUpstreamRef] = pullErrorText(testUpstreamRef, "manifest unknown")
	e.docker.pullErr[testSourceARef] = pullErrorText(testSourceARef, "dial tcp: no such host")

	inst := installCatalogWhoami(t, e)
	if inst.State != "running" {
		t.Fatalf("state = %q, want running", inst.State)
	}
	if got := overridePin(t, e.stateDir, inst.ID, "whoami"); got != testSourceBRef {
		t.Fatalf("override image = %q, want %q", got, testSourceBRef)
	}
	if got := overridePullPolicy(t, e.stateDir, inst.ID, "whoami"); got != "never" {
		t.Fatalf("override pull_policy = %q, want never", got)
	}
	pins, err := e.store.GetInstanceImages(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].Digest != testDigest || pins[0].Ref != testSourceBRef || pins[0].Image != testImage {
		t.Fatalf("stored pins = %+v", pins)
	}
	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !hasCall(e.docker, "RemoveImage", testSourceBRef) {
		t.Fatalf("uninstall did not remove %s; calls %v", testSourceBRef, e.docker.Calls())
	}
}

// With no sources, an install that fails upstream fails as before, and a
// working one keeps the upstream pin with no stored ref.
func TestInstallWithoutSourcesUnchanged(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	inst := installCatalogWhoami(t, e)
	if got := overridePin(t, e.stateDir, inst.ID, "whoami"); got != testUpstreamRef {
		t.Fatalf("override image = %q, want %q", got, testUpstreamRef)
	}
	pins, _ := e.store.GetInstanceImages(inst.ID)
	if len(pins) != 1 || pins[0].Ref != "" {
		t.Fatalf("stored pins = %+v, want no ref", pins)
	}
	if got, want := e.docker.pulled(), []string{testUpstreamRef}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pulls = %v, want %v", got, want)
	}
}

// Offline mode never tries a source: the offline fallback takes over.
func TestInstallOfflineSkipsSources(t *testing.T) {
	e := newTestEnv(t)
	e.m.offlineInstall = true
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiSourcesManifest(testSourceA))
	e.docker.pullErrAll = fmt.Errorf("dial tcp: registry unreachable")
	e.docker.loaded[testImage] = true

	inst := installCatalogWhoami(t, e)
	if got := overridePin(t, e.stateDir, inst.ID, "whoami"); got != testImage {
		t.Fatalf("override image = %q, want the loaded tag %q", got, testImage)
	}
	for _, p := range e.docker.pulled() {
		if p != testUpstreamRef {
			t.Fatalf("pulled %q in offline mode, want only upstream", p)
		}
	}
}

// A start finds the image gone and pulls it again through the same path. Here
// upstream works again, so the override and the pin move back to upstream.
func TestStartPullsMissingImage(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiSourcesManifest(testSourceA))
	e.docker.pullErr[testUpstreamRef] = pullErrorText(testUpstreamRef, "manifest unknown")
	inst := installCatalogWhoami(t, e)
	if got := overridePin(t, e.stateDir, inst.ID, "whoami"); got != testSourceARef {
		t.Fatalf("install override image = %q", got)
	}
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The image is removed from the box, and upstream is back.
	delete(e.docker.present, testSourceARef)
	delete(e.docker.pullErr, testUpstreamRef)
	before := len(e.docker.pulled())
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got, want := e.docker.pulled()[before:], []string{testUpstreamRef}; !reflect.DeepEqual(got, want) {
		t.Fatalf("start pulls = %v, want %v", got, want)
	}
	if got := overridePin(t, e.stateDir, inst.ID, "whoami"); got != testUpstreamRef {
		t.Fatalf("override image after start = %q, want %q", got, testUpstreamRef)
	}
	if got := overridePullPolicy(t, e.stateDir, inst.ID, "whoami"); got != "never" {
		t.Fatalf("override pull_policy after start = %q, want never", got)
	}
	pins, _ := e.store.GetInstanceImages(inst.ID)
	if len(pins) != 1 || pins[0].Ref != "" {
		t.Fatalf("stored pins after start = %+v, want no ref", pins)
	}

	// A second start finds the image and pulls nothing.
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	before = len(e.docker.pulled())
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := len(e.docker.pulled()) - before; got != 0 {
		t.Fatalf("second start pulled %d times, want 0", got)
	}
}

// A start whose image is gone and cannot be pulled from anywhere fails before
// compose up, with the upstream error.
func TestStartFailsWhenImageCannotBePulled(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiSourcesManifest(testSourceA))
	inst := installCatalogWhoami(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	delete(e.docker.present, testUpstreamRef)
	e.docker.pullErrAll = fmt.Errorf("dial tcp: registry unreachable")
	ups := countCalls(e.docker.Calls(), "ComposeUp")

	err := e.m.Start(context.Background(), inst.ID)
	if err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("start err = %v, want the pull error", err)
	}
	if got := countCalls(e.docker.Calls(), "ComposeUp"); got != ups {
		t.Fatalf("compose up ran %d more times, want 0", got-ups)
	}
}

func overridePullPolicy(t *testing.T, stateDir, id, service string) string {
	t.Helper()
	ov, err := os.ReadFile(filepath.Join(stateDir, "instances", id, "compose.override.yml"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			PullPolicy string `yaml:"pull_policy"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(ov, &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	return doc.Services[service].PullPolicy
}

func hasCall(d *fakeDocker, method, arg string) bool {
	for _, c := range d.Calls() {
		if c.method == method && len(c.args) > 0 && fmt.Sprint(c.args[0]) == arg {
			return true
		}
	}
	return false
}

// With no waits (the reconcile pass at boot), a rate limit tries the sources
// at once and fails without sleeping when they fail too.
func TestPullImageNoWaitsOnRateLimit(t *testing.T) {
	old := pullRetryDelays
	pullRetryDelays = []time.Duration{time.Hour}
	t.Cleanup(func() { pullRetryDelays = old })
	d := newFakeDocker()
	d.pullErr[testUpstreamRef] = errRateLimited
	d.pullErr[testSourceARef] = pullErrorText(testSourceARef, "manifest unknown")

	start := time.Now()
	_, err := pullImageWaits(context.Background(), d, testUpstreamRef, []string{testSourceARef}, nil)
	if !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want the upstream rate-limit error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("pull waited %s, want no wait", time.Since(start))
	}
	if got, want := d.pulled(), []string{testUpstreamRef, testSourceARef}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pulls = %v, want %v", got, want)
	}
}

// The reconcile pass pulls a missing image with no backoff wait, so one
// rate-limited registry cannot use up the boot budget every app shares.
func TestReconcilePullsMissingImageWithoutWaiting(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiSourcesManifest(testSourceA))
	inst := installCatalogWhoami(t, e)
	old := pullRetryDelays
	pullRetryDelays = []time.Duration{time.Hour}
	t.Cleanup(func() { pullRetryDelays = old })
	delete(e.docker.present, testUpstreamRef)
	e.docker.pullErr[testUpstreamRef] = errRateLimited

	done := make(chan error, 1)
	go func() { done <- e.m.ensureImages(context.Background(), inst.ID, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ensureImages: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("ensureImages waited on the backoff ladder")
	}
	if got := overridePin(t, e.stateDir, inst.ID, "whoami"); got != testSourceARef {
		t.Fatalf("override image = %q, want %q", got, testSourceARef)
	}
}

// A stored pin that missed an earlier save is repaired on the next start, even
// when the image is present and nothing is pulled.
func TestStartRepairsStalePin(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiSourcesManifest(testSourceA))
	e.docker.pullErr[testUpstreamRef] = pullErrorText(testUpstreamRef, "manifest unknown")
	inst := installCatalogWhoami(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The override names the source; the store lost it, as after a failed save.
	if err := e.store.SetInstanceImages(inst.ID, []store.InstanceImage{{Service: "whoami", Image: testImage, Digest: testDigest}}); err != nil {
		t.Fatal(err)
	}
	before := len(e.docker.pulled())
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := len(e.docker.pulled()) - before; got != 0 {
		t.Fatalf("start pulled %d times, want 0", got)
	}
	pins, _ := e.store.GetInstanceImages(inst.ID)
	if len(pins) != 1 || pins[0].Ref != testSourceARef {
		t.Fatalf("stored pins = %+v, want ref %q", pins, testSourceARef)
	}
}
