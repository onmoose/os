package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// validManifest is a minimal manifest.yml the box's manifest.Parse accepts, so
// Load exercises the real re-parse path a remote install depends on.
func validManifest(id, name string) string {
	return "id: " + id + `
manifest_version: 1
name: ` + name + `
version: "1.0"
compose_file: compose.yml
main_service: web
main_port: 80
`
}

// testApps is the fixture browse payload: one app with artwork, one without, both
// pointing at the two document routes the catalog serves per app.
func testApps() []wireApp {
	return []wireApp{
		{
			ID:               "alpha",
			Name:             "Alpha",
			Version:          "1.0",
			ShortDescription: "the first app",
			LongDescription:  "# Alpha\nlong body",
			Categories:       []string{"tools"},
			IconGlyph:        "box",
			IconURL:          "/catalog/assets/alpha/icon.png",
			ScreenshotURLs:   []string{"/catalog/assets/alpha/screenshots/0.png"},
			ManifestURL:      "/catalog/apps/alpha/manifest",
			ComposeURL:       "/catalog/apps/alpha/compose",
		},
		{
			ID:          "beta",
			Name:        "Beta",
			Version:     "2.0",
			ManifestURL: "/catalog/apps/beta/manifest",
			ComposeURL:  "/catalog/apps/beta/compose",
		},
	}
}

// makeSnapshot marshals apps into a served GET /catalog body and returns the body
// plus the ETag the catalog serves it with.
func makeSnapshot(t *testing.T, apps []wireApp) (body []byte, etag string) {
	t.Helper()
	version, err := contentToken(apps)
	if err != nil {
		t.Fatal(err)
	}
	f := catalogFile{SchemaVersion: wireSchemaVersion, Version: version, Apps: apps}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b, `"` + version + `"`
}

// seedApps returns testApps with each app's install payload inlined, the shape a
// staged local seed file carries (MOOSE_CATALOG_FILE) because it has no control
// plane behind it to serve the document routes.
func seedApps() []wireApp {
	apps := testApps()
	for i := range apps {
		apps[i].Manifest = validManifest(apps[i].ID, apps[i].Name)
		apps[i].Compose = "services:\n  web:\n    image: " + apps[i].ID + ":1\n"
	}
	return apps
}

// fakeCP is a controllable catalog-service fake. It serves the browse
// payload (honouring If-None-Match), the two per-app document routes, and per-app
// assets, and counts hits so tests can assert what is fetched and when.
type fakeCP struct {
	mu    sync.Mutex
	body  []byte
	etag  string
	asset []byte
	// lastEnv is the ?env= of the most recent browse fetch.
	lastEnv      string
	syncHits     int
	assetHits    int
	docHits      int
	failSync     bool // 500 every browse fetch
	failAsset    bool // 500 every asset fetch
	missingDocs  bool // 404 every document fetch
	failDocument bool // 500 every document fetch
}

func newFakeCP(t *testing.T, apps []wireApp) *fakeCP {
	body, etag := makeSnapshot(t, apps)
	return &fakeCP{body: body, etag: etag, asset: []byte("\x89PNG-fake-bytes")}
}

func (f *fakeCP) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/catalog":
			f.syncHits++
			f.lastEnv = r.URL.Query().Get("env")
			if f.failSync {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("ETag", f.etag)
			if r.Header.Get("If-None-Match") == f.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Write(f.body)
		case strings.HasSuffix(r.URL.Path, "/manifest"), strings.HasSuffix(r.URL.Path, "/compose"):
			f.docHits++
			if f.missingDocs {
				http.Error(w, "gone", http.StatusNotFound)
				return
			}
			if f.failDocument {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/catalog/apps/"), "/")[0]
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			if strings.HasSuffix(r.URL.Path, "/manifest") {
				fmt.Fprint(w, validManifest(id, strings.ToUpper(id[:1])+id[1:]))
				return
			}
			fmt.Fprintf(w, "services:\n  web:\n    image: %s:1\n", id)
		case strings.HasPrefix(r.URL.Path, "/catalog/assets/"):
			f.assetHits++
			if f.failAsset {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Write(f.asset)
		default:
			http.NotFound(w, r)
		}
	}))
}

// newRemote builds a remoteSource (not the Catalog facade) so tests can drive
// syncOnce and the projections directly, without a background loop racing them.
// cacheDir is the ASSET cache — the browse payload is never written anywhere.
func newRemote(baseURL, env, cacheDir string) *remoteSource {
	c := NewRemote(RemoteOptions{BaseURL: baseURL, Environment: env, AssetCacheDir: cacheDir})
	return c.src.(*remoteSource)
}

// newRemoteFromFile builds a remoteSource seeded from a local snapshot file, the
// dev/test seam (MOOSE_CATALOG_FILE).
func newRemoteFromFile(baseURL, env, cacheDir, snapshotFile string) *remoteSource {
	c := NewRemote(RemoteOptions{
		BaseURL: baseURL, Environment: env,
		AssetCacheDir: cacheDir, SnapshotFile: snapshotFile,
	})
	return c.src.(*remoteSource)
}

func TestRemoteSyncAndProject(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}

	// The box asks for its own surface and shows exactly what it gets back: the
	// visibility filter is the catalog service's now, not a second box-side pass.
	cp.mu.Lock()
	env := cp.lastEnv
	cp.mu.Unlock()
	if env != "appliance" {
		t.Errorf("browse fetch sent env=%q, want appliance", env)
	}
	list, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %+v, want both served apps", list)
	}
	if list[0].IconURL != "/api/v1/catalog/alpha/icon" {
		t.Fatalf("icon URL must be the brain route, got %q", list[0].IconURL)
	}
	if list[1].IconURL != "" {
		t.Fatalf("beta declares no icon; IconURL = %q, want empty", list[1].IconURL)
	}

	if _, err := r.Entry("beta"); err != nil {
		t.Fatalf("Entry(beta): %v", err)
	}
	d, err := r.Detail("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if d.LongDescription == "" || len(d.ScreenshotURLs) != 1 {
		t.Fatalf("alpha detail incomplete: %+v", d)
	}

	// Nothing about browsing touched the document routes: the whole point of the
	// split is that an app's install payload is fetched only when it is installed.
	cp.mu.Lock()
	docs := cp.docHits
	cp.mu.Unlock()
	if docs != 0 {
		t.Errorf("browsing fetched %d install documents, want 0", docs)
	}

	// Load follows the record's own URLs and re-parses the manifest with the box's
	// own parser.
	man, compose, err := r.Load(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if man.ID != "alpha" || !strings.Contains(string(compose), "alpha:1") {
		t.Fatalf("Load(alpha) wrong: id=%q compose=%q", man.ID, compose)
	}
	cp.mu.Lock()
	docs = cp.docHits
	cp.mu.Unlock()
	if docs != 2 {
		t.Errorf("Load fetched %d documents, want 2 (manifest + compose)", docs)
	}
}

// TestRemoteLoadFollowsAbsoluteDocumentURL: a document URL on another origin is
// followed as given. The box treats every published URL as opaque, so the control
// plane can move documents (or assets) to object storage without a box change.
func TestRemoteLoadFollowsAbsoluteDocumentURL(t *testing.T) {
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			fmt.Fprint(w, validManifest("alpha", "Alpha"))
			return
		}
		fmt.Fprint(w, "services:\n  web:\n    image: alpha:1\n")
	}))
	defer docs.Close()

	apps := testApps()
	apps[0].ManifestURL = docs.URL + "/store/alpha/manifest"
	apps[0].ComposeURL = docs.URL + "/store/alpha/compose"

	cp := newFakeCP(t, apps)
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "hosted", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	man, _, err := r.Load(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Load must follow an absolute document URL: %v", err)
	}
	if man.ID != "alpha" {
		t.Errorf("manifest id = %q", man.ID)
	}
	cp.mu.Lock()
	hits := cp.docHits
	cp.mu.Unlock()
	if hits != 0 {
		t.Errorf("catalog origin served %d documents, want 0 (they live elsewhere)", hits)
	}
}

// TestRemoteLoadFailures: an app the catalog no longer serves a payload for is
// ErrNotFound (404 to the caller), while a reachable-but-broken document route is
// a plain error (500) — the app exists, the box just could not get its payload.
func TestRemoteLoadFailures(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "hosted", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	cp.mu.Lock()
	cp.missingDocs = true
	cp.mu.Unlock()
	if _, _, err := r.Load(context.Background(), "alpha"); err == nil {
		t.Fatal("want an error when the document route 404s")
	} else if !strings.Contains(err.Error(), ErrNotFound.Error()) {
		t.Errorf("a 404 document must be ErrNotFound, got %v", err)
	}

	cp.mu.Lock()
	cp.missingDocs, cp.failDocument = false, true
	cp.mu.Unlock()
	if _, _, err := r.Load(context.Background(), "alpha"); err == nil {
		t.Fatal("want an error when the document route 500s")
	} else if strings.Contains(err.Error(), ErrNotFound.Error()) {
		t.Errorf("a 500 document must NOT be ErrNotFound, got %v", err)
	}

	// A record that names no document URL is a catalog integrity problem, not a
	// missing app.
	apps := testApps()
	apps[0].ManifestURL = ""
	cp2 := newFakeCP(t, apps)
	srv2 := cp2.server()
	defer srv2.Close()
	r2 := newRemote(srv2.URL, "hosted", t.TempDir())
	if err := r2.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r2.Load(context.Background(), "alpha"); err == nil {
		t.Fatal("want an error for a record with no manifest URL")
	} else if strings.Contains(err.Error(), ErrNotFound.Error()) {
		t.Errorf("a record with no manifest URL must NOT be ErrNotFound, got %v", err)
	}
}

// TestRemoteKeepsNoSnapshotOnDisk pins the rule that the box holds the catalog in
// memory only. A synced source survives a later failed sync (the payload it
// already has stays), but nothing about it outlives the process: a fresh source
// over the very same directory starts empty, because there is no file to read.
//
// The store being empty rather than stale is the intended behavior, not a
// degradation to work around — see APP_STORE.md # Failure modes.
func TestRemoteKeepsNoSnapshotOnDisk(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	dir := t.TempDir()

	r := newRemote(srv.URL, "hosted", dir)
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l, _ := r.List(); len(l) != 2 {
		t.Fatalf("List after sync = %d apps, want 2", len(l))
	}
	srv.Close() // catalog now unreachable

	// A failed sync leaves the payload this source already holds untouched.
	if err := r.syncOnce(context.Background()); err == nil {
		t.Fatal("syncOnce against dead server should error")
	}
	if l, _ := r.List(); len(l) != 2 {
		t.Fatalf("List after failed sync = %d apps, want the 2 already in memory", len(l))
	}

	// A new source over the same directory — the restart case — has nothing to
	// read back, so it browses empty until a sync lands.
	r2 := newRemote(srv.URL, "hosted", dir)
	if l, _ := r2.List(); len(l) != 0 {
		t.Fatalf("a restarted box must start empty, got %d apps: the payload was persisted somewhere", len(l))
	}

	// And no snapshot-shaped file was written under the asset cache dir either.
	found, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("browse payload written to disk: %v", found)
	}
}

// TestRemoteSnapshotFileSeed covers the dev/test seam (MOOSE_CATALOG_FILE): a
// local file seeds the store at construction, the brain never writes to it, and
// its inlined install payload lets an air-gapped lane install with no control
// plane to fetch documents from.
func TestRemoteSnapshotFileSeed(t *testing.T) {
	body, _ := makeSnapshot(t, seedApps())
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// No catalog service at all: the seed is the whole store.
	r := newRemoteFromFile("http://127.0.0.1:1", "hosted", t.TempDir(), path)
	if l, _ := r.List(); len(l) != 2 {
		t.Fatalf("seeded List = %d apps, want 2", len(l))
	}
	// The inlined payload is what makes an air-gapped install work: Load must not
	// touch the network when the record carries the documents.
	man, compose, err := r.Load(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Load from an inlined seed must not need the network: %v", err)
	}
	if man.ID != "alpha" || !strings.Contains(string(compose), "alpha:1") {
		t.Fatalf("seeded Load wrong: id=%q compose=%q", man.ID, compose)
	}
	if err := r.syncOnce(context.Background()); err == nil {
		t.Fatal("syncOnce against no catalog service should error")
	}
	if l, _ := r.List(); len(l) != 2 {
		t.Fatalf("List after failed sync = %d apps, want the seeded 2", len(l))
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("the seed file was written to; it is an input, not a cache")
	}
}

// TestRemoteSnapshotFileInvalid: a missing or corrupt seed leaves the store empty
// and the source usable, rather than failing construction. A dev seed must not be
// able to stop a box from booting.
func TestRemoteSnapshotFileInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bad, filepath.Join(dir, "absent.json")} {
		r := newRemoteFromFile("http://127.0.0.1:1", "hosted", t.TempDir(), path)
		if l, _ := r.List(); len(l) != 0 {
			t.Errorf("seed %q: List = %d apps, want empty", path, len(l))
		}
	}
}

func TestRemoteNeverSyncedIsEmpty(t *testing.T) {
	cp := newFakeCP(t, testApps())
	cp.failSync = true
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err == nil {
		t.Fatal("want sync error from failing catalog service")
	}
	if l, _ := r.List(); len(l) != 0 {
		t.Fatalf("never-synced store must be empty, got %d", len(l))
	}
	if _, err := r.Entry("alpha"); err == nil {
		t.Fatal("Entry on empty store should be ErrNotFound")
	}
}

// TestRemoteRefusesUnreadableSchema: a payload stamped with a format the box
// cannot project is refused and never becomes the read source. This is the one
// refusal left on the browse path — the index digest is gone (#434).
func TestRemoteRefusesUnreadableSchema(t *testing.T) {
	cp := newFakeCP(t, testApps())
	cp.body = []byte(`{"schema_version":99,"version":"v1","apps":[]}`)
	cp.etag = `"v1"`
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err == nil {
		t.Fatal("a payload with an unreadable schema version must be refused")
	}
	if l, _ := r.List(); len(l) != 0 {
		t.Fatal("a refused payload must not become the read source")
	}
}

func TestRemoteAssetProxyAndCache(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	p1, err := r.IconPath("alpha")
	if err != nil {
		t.Fatalf("IconPath: %v", err)
	}
	// The extension survives into the cache name, because http.ServeFile reads
	// the content type off it.
	if filepath.Ext(p1) != ".png" {
		t.Errorf("cached icon %q lost its extension; the browser would get the wrong type", p1)
	}
	p2, err := r.IconPath("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatalf("icon path not stable: %q vs %q", p1, p2)
	}
	cp.mu.Lock()
	hits := cp.assetHits
	cp.mu.Unlock()
	if hits != 1 {
		t.Fatalf("asset fetched %d times, want 1 (second served from cache)", hits)
	}

	// Unknown app / no-icon app / out-of-range screenshot are ErrNotFound.
	if _, err := r.IconPath("beta"); err == nil {
		t.Fatal("beta has no icon; want ErrNotFound")
	}
	if _, err := r.IconPath("nope"); err == nil {
		t.Fatal("unknown app; want ErrNotFound")
	}
	if _, err := r.ScreenshotPath("alpha", 5); err == nil {
		t.Fatal("out-of-range screenshot; want ErrNotFound")
	}
	if _, err := r.ScreenshotPath("alpha", 0); err != nil {
		t.Fatalf("ScreenshotPath(alpha, 0): %v", err)
	}
}

// TestRemoteAssetFollowsAbsoluteURL is the artwork half of
// TestRemoteLoadFollowsAbsoluteDocumentURL, and it is the production path: the
// catalog publishes icons and screenshots on an object-storage origin, not on its
// own host. The box must follow the URL it is given and cache the bytes the same
// way, without the catalog origin serving a single asset byte.
func TestRemoteAssetFollowsAbsoluteURL(t *testing.T) {
	var bucketHits int
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucketHits++
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "PNG-"+r.URL.Path)
	}))
	defer bucket.Close()

	apps := testApps()
	apps[0].IconURL = bucket.URL + "/moose-catalog-assets/alpha/icon.png"
	apps[0].ScreenshotURLs = []string{bucket.URL + "/moose-catalog-assets/alpha/screenshots/0.png"}

	cp := newFakeCP(t, apps)
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	icon, err := r.IconPath("alpha")
	if err != nil {
		t.Fatalf("IconPath must follow an absolute asset URL: %v", err)
	}
	if got, err := os.ReadFile(icon); err != nil || !strings.HasPrefix(string(got), "PNG-") {
		t.Fatalf("cached icon = %q, %v; want the bucket's bytes", got, err)
	}
	if filepath.Ext(icon) != ".png" {
		t.Errorf("cached icon %q lost its extension", icon)
	}
	if _, err := r.ScreenshotPath("alpha", 0); err != nil {
		t.Fatalf("ScreenshotPath must follow an absolute asset URL: %v", err)
	}

	if bucketHits != 2 {
		t.Errorf("bucket served %d assets, want 2 (the icon and the screenshot)", bucketHits)
	}
	cp.mu.Lock()
	hits := cp.assetHits
	cp.mu.Unlock()
	if hits != 0 {
		t.Errorf("catalog origin served %d assets, want 0 (they live elsewhere)", hits)
	}
}

// TestAssetCacheNameContainsNoPublishedPath pins the safety property of the cache
// naming: nothing a publisher controls reaches a path segment, so a hostile asset
// URL cannot steer a write out of the cache dir. Distinct URLs still get distinct
// files.
func TestAssetCacheNameContainsNoPublishedPath(t *testing.T) {
	hostile := "https://evil.invalid/a/../../../../etc/passwd"
	name := assetCacheName(hostile)
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		t.Fatalf("cache name %q carries path syntax from the URL", name)
	}
	if assetCacheName("https://a.invalid/icon.png") == assetCacheName("https://b.invalid/icon.png") {
		t.Error("two different asset URLs collide onto one cache file")
	}
	// An odd extension is dropped rather than carried into the filename.
	if got := assetCacheName("https://a.invalid/icon.p n g"); filepath.Ext(got) != "" {
		t.Errorf("cache name %q kept an implausible extension", got)
	}
}

// TestAssetCachePathContainsTheAppID: the app id on a published record is
// publisher data like every other field, so an id carrying ".." must not steer a
// cache write out of the asset dir.
func TestAssetCachePathContainsTheAppID(t *testing.T) {
	base := filepath.Join("/var/cache", assetsDir)
	for _, id := range []string{"../../etc", "..", "a/../../b", "/etc"} {
		got, ok := assetCachePath("/var/cache", id, "deadbeef.png")
		if ok && !strings.HasPrefix(got, base+string(os.PathSeparator)) {
			t.Errorf("id %q resolved to %q, outside %q", id, got, base)
		}
	}
	got, ok := assetCachePath("/var/cache", "alpha", "deadbeef.png")
	if !ok || got != filepath.Join(base, "alpha", "deadbeef.png") {
		t.Errorf("a plain id must resolve normally, got %q ok=%v", got, ok)
	}
}

// TestDetailSkipsEmptyScreenshotSlots: a record with a blank screenshot URL must
// not put a URL on the detail page for it — ScreenshotPath answers ErrNotFound
// for that index, so the gallery would render a broken image. The slots that do
// have artwork keep their OWN index, because that is what ScreenshotPath resolves
// by; renumbering them would point each one at the wrong picture.
func TestDetailSkipsEmptyScreenshotSlots(t *testing.T) {
	apps := testApps()
	apps[0].ScreenshotURLs = []string{"", "/catalog/assets/alpha/screenshots/1.png"}
	d := detailOfApp(&apps[0])
	if len(d.ScreenshotURLs) != 1 {
		t.Fatalf("ScreenshotURLs = %v, want just the one real screenshot", d.ScreenshotURLs)
	}
	if d.ScreenshotURLs[0] != screenshotURL("alpha", 1) {
		t.Errorf("ScreenshotURLs[0] = %q, want index 1 (the slot the artwork is in)", d.ScreenshotURLs[0])
	}

	// And the route that URL names must resolve to that same artwork.
	cp2 := newFakeCP(t, apps)
	srv2 := cp2.server()
	defer srv2.Close()
	r := newRemote(srv2.URL, "hosted", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ScreenshotPath("alpha", 1); err != nil {
		t.Errorf("ScreenshotPath(alpha, 1): %v", err)
	}
	if _, err := r.ScreenshotPath("alpha", 0); err == nil {
		t.Error("the empty slot must stay ErrNotFound")
	}
}

// TestRemoteAssetExpires covers the asset TTL. An app's asset URL is stable, so
// without an expiry the first icon a box fetched would be the icon it served
// forever, and republished artwork would never reach the fleet. Aging the cached
// file past assetTTL must produce a refetch.
func TestRemoteAssetExpires(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := r.IconPath("alpha")
	if err != nil {
		t.Fatal(err)
	}

	// A second request inside the TTL is served from disk.
	if _, err := r.IconPath("alpha"); err != nil {
		t.Fatal(err)
	}
	cp.mu.Lock()
	hits := cp.assetHits
	cp.mu.Unlock()
	if hits != 1 {
		t.Fatalf("asset fetched %d times inside the TTL, want 1", hits)
	}

	// Age the cached file past the TTL: the next request refetches.
	old := time.Now().Add(-assetTTL - time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := r.IconPath("alpha"); err != nil {
		t.Fatal(err)
	}
	cp.mu.Lock()
	hits = cp.assetHits
	cp.mu.Unlock()
	if hits != 2 {
		t.Fatalf("asset fetched %d times after expiry, want 2 (the expired copy must be refreshed)", hits)
	}
}

// TestRemoteExpiredAssetSurvivesFailedRefresh: once an asset is expired but the
// catalog is unreachable, the box serves the stale file rather than a
// broken image. The browse payload has no such fallback; artwork does.
func TestRemoteExpiredAssetSurvivesFailedRefresh(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := r.IconPath("alpha")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-assetTTL - time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}

	cp.mu.Lock()
	cp.failAsset = true
	cp.mu.Unlock()

	got, err := r.IconPath("alpha")
	if err != nil {
		t.Fatalf("an expired asset with a failing refresh must still be served: %v", err)
	}
	if got != p {
		t.Fatalf("IconPath = %q, want the expired copy %q", got, p)
	}
}

func TestRemoteSnapshotSizeCapRejects(t *testing.T) {
	cp := newFakeCP(t, testApps())
	// Serve a body far over the browse cap; parse must never be reached.
	cp.body = make([]byte, maxSnapshotBytes+1)
	cp.etag = `"oversize"`
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err == nil {
		t.Fatal("oversize browse payload must fail (size cap)")
	}
	if l, _ := r.List(); len(l) != 0 {
		t.Fatal("oversize browse payload must not become the read source")
	}
}

func TestRemoteAssetFetchCollapsesConcurrent(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Fire many concurrent first-time icon requests: the per-asset lock must
	// collapse them into a single upstream fetch.
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.IconPath("alpha"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("IconPath: %v", err)
	}
	cp.mu.Lock()
	hits := cp.assetHits
	cp.mu.Unlock()
	if hits != 1 {
		t.Fatalf("asset fetched %d times under %d concurrent requests, want 1", hits, n)
	}
}

func TestRemoteStartRefreshIsIdempotent(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "appliance", t.TempDir())
	// The guard is an atomic CAS, independent of the loop; assert it directly so
	// the test doesn't race the background goroutine's first sync.
	if r.started.Load() {
		t.Fatal("started should be false before startRefresh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.startRefresh(ctx)
	if !r.started.Load() {
		t.Fatal("started must be true after first startRefresh")
	}
	// A second call must be a no-op (no second goroutine); CAS already false→true,
	// so a repeat returns immediately.
	r.startRefresh(ctx) // must not panic or spawn a second loop
}

func TestRemote304KeepsSnapshot(t *testing.T) {
	cp := newFakeCP(t, testApps())
	srv := cp.server()
	defer srv.Close()

	r := newRemote(srv.URL, "hosted", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second sync sends If-None-Match and gets a 304; the payload stays intact.
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatalf("304 path should not error: %v", err)
	}
	if l, _ := r.List(); len(l) != 2 {
		t.Fatalf("List after 304 = %d apps, want 2", len(l))
	}
	cp.mu.Lock()
	hits := cp.syncHits
	cp.mu.Unlock()
	if hits != 2 {
		t.Fatalf("sync hit the catalog %d times, want 2", hits)
	}
}

// TestBrowseURLEscapesEnv: the environment goes on the query string, so a value
// with URL syntax in it can't rewrite the request path.
func TestBrowseURLEscapesEnv(t *testing.T) {
	r := newRemote("https://moose.invalid", "hosted&x=1 /../y", t.TempDir())
	got := r.browseURL()
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("browseURL %q does not parse: %v", got, err)
	}
	if u.Path != "/catalog" {
		t.Errorf("browse path = %q, want /catalog (from %q)", u.Path, got)
	}
	if u.Query().Get("env") != "hosted&x=1 /../y" {
		t.Errorf("env round-trip = %q", u.Query().Get("env"))
	}
}
