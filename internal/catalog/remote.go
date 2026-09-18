package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onmoose/os/internal/manifest"
)

// remote.go is the box's thin-client catalog: it consumes the control plane's
// public-read catalog API (cloud specs/CATALOG.md) instead of reading a baked
// directory.
//
// It fetches on two seams, for two different reasons (#434):
//
//   - Browse data — GET /catalog?env=<environment> — is fetched whole, verified,
//     held in memory and projected locally, so the UI never blocks on the
//     network. It is filtered server-side to the box's own surface, so every
//     record it carries is one this box may show.
//   - Install payloads — the manifest and compose of ONE app — are fetched only
//     when the box actually installs that app, by following the URLs on its
//     browse record. Carrying every app's payload in the browse response cost
//     roughly four bytes for every one the store rendered.
//
// The box keeps NO copy of the browse payload on disk. The catalog is a separate
// distribution with its own release cadence, and the box always renders the
// version the endpoint is serving now: a box that has not synced yet, or cannot
// reach the endpoint, shows an empty store rather than an older catalog. That is
// the honest state — browsing a stale catalog was never the same as being able
// to act on it, because installing an app pulls images over the same network,
// and a pinned old copy can offer a manifest the store no longer publishes.
// Icons and screenshots are a separate case: those are proxied per request and
// cached on disk with a TTL (see assetTTL), because they are page furniture for
// a snapshot the box is already holding.
//
// An INSTALLED app is a third case again. Its manifest and compose are written
// next to the installation at install time (lifecycle writeInstanceDir), so
// routine box operation never depends on the catalog service and an installed
// app's manifest survives the app being unpublished. Nothing on this path is
// called for an app that is already installed.
//
// It stays the box↔cloud install contract: Load re-parses the fetched manifest
// with the box's own manifest.Parse, so the box remains the sole enforcer of the
// manifest contract.

const (
	// defaultRefreshInterval is how often the box re-syncs the browse payload. The
	// catalog changes rarely (a store publish), and every fetch is a cheap 304
	// when nothing moved (If-None-Match against the catalog's version token), so
	// this is loose by design. Overridable via RemoteOptions.
	defaultRefreshInterval = 15 * time.Minute
	// httpTimeout bounds a single catalog fetch — browse payload, install
	// document, or asset. All three are small, so this is generous.
	httpTimeout = 30 * time.Second
	// maxSnapshotBytes / maxDocumentBytes / maxAssetBytes cap how much a single
	// response body can pull into memory: the timeout bounds wall-clock, not
	// bytes, so a compromised or MITM'd control plane must not be able to
	// pressure box memory with an unbounded body. All three are far above any
	// real payload — browse records are display text, a manifest or compose is a
	// few KB of YAML, assets are icons and screenshots — so a legitimate catalog
	// never trips them; exceeding is treated as a fetch failure.
	maxSnapshotBytes = 32 << 20 // 32 MiB
	maxDocumentBytes = 4 << 20  // 4 MiB
	maxAssetBytes    = 16 << 20 // 16 MiB
	// assetsDir is the AssetCacheDir subtree the proxied icon/screenshot files land
	// in, one directory per app.
	assetsDir = "assets"
	// assetTTL is how long a proxied icon or screenshot is served from disk before
	// it is fetched again. An app's asset URL is stable, so without an expiry the
	// first icon a box ever fetched would be the icon it served forever —
	// republishing artwork would never reach the fleet. A day is short enough that
	// a fixed icon lands on its own, and long enough that browsing the store is
	// not a stream of refetches.
	assetTTL = 24 * time.Hour
)

// RemoteOptions configures the control-plane catalog client. BaseURL is the
// control plane's origin serving the catalog API (the client appends /catalog and
// resolves the record's own URLs against it); Environment is the box's own surface
// ("appliance"|"hosted"), sent as ?env= so the control plane returns only apps this
// box may show; AssetCacheDir holds proxied icons and screenshots (never the
// browse payload). SnapshotFile is a dev/test-only seam — see the field comment.
type RemoteOptions struct {
	BaseURL       string
	Environment   string
	AssetCacheDir string
	// SnapshotFile is a local browse payload to start from, for dev and test lanes
	// that run a brain with no reachable control plane (make dev-app, the QEMU boot
	// proofs, dev/test-health.sh). It is read once at construction and never
	// written: it is an input, not a cache. Such a file inlines each app's manifest
	// and compose (wireApp.Manifest / Compose), because there is no control plane
	// behind it to serve the document routes. Production leaves it empty — a real
	// box gets its catalog from BaseURL and nowhere else.
	SnapshotFile    string
	RefreshInterval time.Duration
	HTTPClient      *http.Client
}

// remoteSource implements source against the control-plane catalog API. Reads
// project from the in-memory browse payload under an RLock; the background sync
// loop swaps a freshly fetched-and-verified one in under the write lock, so a
// read never blocks on the network and never sees a half-applied snapshot. The
// browse payload lives only in memory — nothing writes it to disk.
type remoteSource struct {
	baseURL  string
	env      string
	cacheDir string // assets only
	interval time.Duration
	http     *http.Client

	mu   sync.RWMutex
	snap *snapshot // nil until the first successful sync or seed load
	etag string    // last payload's ETag (quoted), for a cheap If-None-Match 304

	// started guards startRefresh so a repeat call can't spawn a second sync loop.
	started atomic.Bool

	// assetLocks collapses concurrent cache-miss fetches of the same asset: a
	// per-asset mutex serializes the check-fetch-write so N simultaneous requests
	// for one uncached icon do one fetch, not N. assetLocksMu guards the map; the
	// per-asset locks guard each asset's fetch. The map is bounded by the catalog's
	// distinct assets, so it needs no eviction.
	assetLocksMu sync.Mutex
	assetLocks   map[string]*sync.Mutex
}

// snapshot is the immutable, indexed projection of one verified browse payload:
// the apps sorted by name (stable grid order) plus an id lookup. Swapped
// wholesale on each successful sync, so readers holding a *snapshot see a
// consistent view.
type snapshot struct {
	apps []wireApp
	byID map[string]*wireApp
	// home is the authored recommended-apps page carried verbatim from the
	// payload (a curated home.yml via the sync tool).
	home wireHomePage
	// cats is the authored category vocabulary carried verbatim from the payload
	// (a curated categories.yml via the sync tool), in authored order.
	cats []wireCategory
}

func newSnapshot(f catalogFile) *snapshot {
	s := &snapshot{
		apps: append([]wireApp(nil), f.Apps...),
		byID: make(map[string]*wireApp, len(f.Apps)),
		home: f.Home,
		cats: f.Categories,
	}
	sort.Slice(s.apps, func(i, j int) bool { return s.apps[i].Name < s.apps[j].Name })
	for i := range s.apps {
		s.byID[s.apps[i].ID] = &s.apps[i]
	}
	return s
}

// NewRemote builds a control-plane-backed catalog. It does not touch the network
// — call StartRefresh to begin syncing — so a box starts with an empty store and
// fills it when the first sync lands. When SnapshotFile is set (dev and test
// lanes only) it is read here to seed that starting state.
func NewRemote(opts RemoteOptions) *Catalog {
	interval := opts.RefreshInterval
	if interval <= 0 {
		interval = defaultRefreshInterval
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	rs := &remoteSource{
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		env:        opts.Environment,
		cacheDir:   opts.AssetCacheDir,
		interval:   interval,
		http:       client,
		assetLocks: map[string]*sync.Mutex{},
	}
	if err := os.MkdirAll(opts.AssetCacheDir, 0o755); err != nil {
		slog.Error("catalog: create asset cache dir; icons will be fetched per request", "src", opts.AssetCacheDir, "err", err)
	}
	if opts.SnapshotFile != "" {
		rs.loadSnapshotFile(opts.SnapshotFile)
	}
	return &Catalog{src: rs}
}

// loadSnapshotFile seeds the in-memory browse payload from a local file, for the
// dev and test lanes that run a brain against no reachable control plane
// (RemoteOptions.SnapshotFile). It is read here and never written back — the file
// belongs to whoever staged it, not to the brain. Best-effort by design: an absent
// or invalid file leaves the store empty and the sync loop still runs, because
// failing to boot a box over a dev seed would be a worse trade than an empty store.
func (r *remoteSource) loadSnapshotFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("catalog: read local snapshot file failed; starting with empty store", "src", path, "err", err)
		return
	}
	f, err := parseSnapshot(data)
	if err != nil {
		slog.Warn("catalog: local snapshot file invalid; ignoring", "src", path, "err", err)
		return
	}
	r.mu.Lock()
	r.snap = newSnapshot(f)
	r.etag = quoteETag(f.Version)
	r.mu.Unlock()
	slog.Info("catalog: loaded local snapshot file", "src", path, "apps", len(f.Apps), "version", f.Version)
}

// startRefresh runs the background sync loop bound to ctx: one immediate sync (so
// a freshly booted box populates its store promptly) then one per interval. Each
// attempt is independent and best-effort — a failure keeps whatever payload is
// already in memory and the loop retries next tick — so a control plane blip
// during uptime never empties a store that has synced. A second call is a no-op
// (the started guard): one sync loop only, no matter how many times cmd/brain
// wires it.
func (r *remoteSource) startRefresh(ctx context.Context) {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		if err := r.syncOnce(ctx); err != nil {
			slog.Warn("catalog: initial sync failed; store is empty until a sync lands", "err", err)
		}
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.syncOnce(ctx); err != nil {
					slog.Warn("catalog: sync failed; keeping the snapshot in memory", "err", err)
				}
			}
		}
	}()
}

// browseURL is the browse fetch: GET /catalog?env=<this box's surface>. The
// control plane filters to that surface, so the box shows exactly what it is
// given and applies no visibility filter of its own.
func (r *remoteSource) browseURL() string {
	return r.baseURL + "/catalog?env=" + url.QueryEscape(r.env)
}

// syncOnce fetches the browse payload once, verifies it, and swaps it in as the
// new read source. A 304 (the control plane still serves the version the box last
// saw) is the common no-op path. A transport error, a non-200/304 status, or a
// failed verify returns an error and leaves the current payload untouched.
func (r *remoteSource) syncOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.browseURL(), nil)
	if err != nil {
		return err
	}
	r.mu.RLock()
	etag := r.etag
	r.mu.RUnlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("catalog: fetch browse payload: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotModified {
		return nil // nothing changed since the last sync
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("catalog: fetch browse payload: unexpected status %s", resp.Status)
	}
	data, err := readLimited(resp.Body, maxSnapshotBytes)
	if err != nil {
		return fmt.Errorf("catalog: read browse payload: %w", err)
	}
	f, err := parseSnapshot(data)
	if err != nil {
		return err // unreadable format: don't swap in a payload we can't project
	}
	newETag := resp.Header.Get("ETag")
	if newETag == "" {
		newETag = quoteETag(f.Version)
	}
	r.mu.Lock()
	prev := r.etag
	r.snap = newSnapshot(f)
	r.etag = newETag
	r.mu.Unlock()
	if newETag != prev {
		slog.Info("catalog: synced browse payload", "apps", len(f.Apps), "version", f.Version)
	}
	return nil
}

// quoteETag wraps a version token as a strong ETag, so the box's If-None-Match
// matches the header the control plane serves. An empty token yields an empty
// string, which callers read as "no validator to send".
func quoteETag(version string) string {
	if version == "" {
		return ""
	}
	return `"` + version + `"`
}

// current returns the live snapshot under the read lock, or nil when the box has
// not synced yet (empty store).
func (r *remoteSource) current() *snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snap
}

// entryOfApp / detailOfApp project a published app into the box's grid / detail
// shapes. The icon and screenshot URLs are the brain's own asset routes (the
// remote source proxies the underlying control-plane asset behind them), so the UI
// contract is identical to the disk source. Featured/Rank stay off Entry/Detail (a
// card is a card); the segmented Home/Category views select the featured apps and
// project each as a plain Entry (see featured, below).
func entryOfApp(a *wireApp) Entry {
	e := Entry{
		ID:               a.ID,
		Name:             a.Name,
		Version:          a.Version,
		ShortDescription: a.ShortDescription,
		Categories:       a.Categories,
		IconGlyph:        a.IconGlyph,
		Footprint:        a.Footprint,
	}
	if a.IconURL != "" {
		e.IconURL = iconURL(a.ID)
	}
	return e
}

func detailOfApp(a *wireApp) Detail {
	d := Detail{
		Entry:           entryOfApp(a),
		LongDescription: a.LongDescription,
		Author:          a.Author,
		License:         a.License,
		Links:           a.Links,
		ChangelogURL:    a.ChangelogURL,
		ExternalCosts:   a.ExternalCosts,
	}
	// The index is the record's own, not the output position: ScreenshotPath
	// resolves by that index, so a record with an empty slot must skip the slot
	// (it would render as a broken image) without renumbering the ones after it.
	for i := range a.ScreenshotURLs {
		if a.ScreenshotURLs[i] == "" {
			continue
		}
		d.ScreenshotURLs = append(d.ScreenshotURLs, screenshotURL(a.ID, i))
	}
	return d
}

// List returns the browse grid: one Entry per app the control plane returned for
// this box's surface, in stable by-name order. There is no environment filter
// here — the ?env= on the fetch is the filter (cloud specs/CATALOG.md #
// Visibility). An empty (never-synced) store returns no entries, not an error.
func (r *remoteSource) List() ([]Entry, error) {
	snap := r.current()
	if snap == nil {
		return nil, nil
	}
	out := make([]Entry, 0, len(snap.apps))
	for i := range snap.apps {
		out = append(out, entryOfApp(&snap.apps[i]))
	}
	return out, nil
}

// featured returns the curated top apps, ascending by rank (the sync tool
// guarantees a featured app carries a rank; guard nil anyway). It reads
// Featured/Rank straight off the browse record — the fields the box carries but
// keeps off Entry/Detail (wire.go). An empty or never-synced store has no
// featured apps.
func (r *remoteSource) featured() ([]Entry, error) {
	snap := r.current()
	if snap == nil {
		return nil, nil
	}
	var top []*wireApp
	for i := range snap.apps {
		if snap.apps[i].Featured {
			top = append(top, &snap.apps[i])
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return rankOf(top[i]) < rankOf(top[j]) })
	out := make([]Entry, len(top))
	for i, a := range top {
		out[i] = entryOfApp(a)
	}
	return out, nil
}

// home returns the landing page's spotlight app and category groups. An app the
// home block names but the response does not carry — because this surface does
// not advertise it — drops out of its slot (the spotlight goes nil, or the app is
// skipped within its group), and a group left with no apps is dropped entirely
// rather than rendered empty. An empty or never-synced store has neither.
func (r *remoteSource) home() (*Entry, []HomeGroupView, error) {
	snap := r.current()
	if snap == nil {
		return nil, nil, nil
	}
	var spotlight *Entry
	if a, ok := snap.byID[snap.home.Spotlight]; ok {
		e := entryOfApp(a)
		spotlight = &e
	}
	var groups []HomeGroupView
	for _, g := range snap.home.Groups {
		var apps []Entry
		for _, id := range g.Apps {
			if a, ok := snap.byID[id]; ok {
				apps = append(apps, entryOfApp(a))
			}
		}
		if len(apps) > 0 {
			groups = append(groups, HomeGroupView{Category: g.Category, Apps: apps})
		}
	}
	return spotlight, groups, nil
}

// categories returns the payload's authored category vocabulary in authored
// order. The facade only ever renders the entries whose categories are actually
// present on this surface. A never-synced box has no payload and so no vocabulary.
func (r *remoteSource) categories() ([]Category, error) {
	snap := r.current()
	if snap == nil {
		return nil, nil
	}
	out := make([]Category, 0, len(snap.cats))
	for _, c := range snap.cats {
		out = append(out, Category{ID: c.ID, Label: c.Label})
	}
	return out, nil
}

// rankOf reads an app's curated rank, treating an absent rank as last so a
// misconfigured featured app sinks rather than jumps to the front.
func rankOf(a *wireApp) int {
	if a.Rank == nil {
		return 1 << 30
	}
	return *a.Rank
}

// lookup resolves an app id against the live browse payload. ErrNotFound covers
// both an empty store and an unknown id: from the box's side they are the same
// answer, "this catalog has no such app right now".
//
// Since environment filtering moved to the control plane, an app that LEAVES this
// box's surface (or leaves the catalog) stops resolving here, including for an
// instance already installed from it. The install itself is unaffected — the
// manifest and compose were written next to the installation — but the card loses
// its catalog-supplied icon and falls back to the instance row's own name and
// glyph. See APP_STORE.md # Failure modes.
func (r *remoteSource) lookup(id string) (*wireApp, error) {
	snap := r.current()
	if snap == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	a, ok := snap.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return a, nil
}

// Entry returns the grid summary for one app by id.
func (r *remoteSource) Entry(id string) (Entry, error) {
	a, err := r.lookup(id)
	if err != nil {
		return Entry{}, err
	}
	return entryOfApp(a), nil
}

// Detail returns the full detail-page view of one app.
func (r *remoteSource) Detail(id string) (Detail, error) {
	a, err := r.lookup(id)
	if err != nil {
		return Detail{}, err
	}
	return detailOfApp(a), nil
}

// Load fetches the app's two install documents and returns the parsed manifest
// plus the verbatim compose bytes. The manifest is re-parsed with the box's own
// manifest.Parse — the box, not the cloud, enforces the manifest contract.
//
// This is an install-path call and it goes over the network, so it takes the
// caller's context. Nothing on the routine box paths reaches it: an installed
// app's manifest is read from its own instance directory (lifecycle
// InstanceManifest), not from here.
//
// A record from the local seed seam carries the two documents inline (no control
// plane behind a staged file), and those bytes are used as-is.
func (r *remoteSource) Load(ctx context.Context, id string) (*manifest.Manifest, []byte, error) {
	a, err := r.lookup(id)
	if err != nil {
		return nil, nil, err
	}
	manBytes := []byte(a.Manifest)
	if len(manBytes) == 0 {
		if manBytes, err = r.fetchDocument(ctx, a.ManifestURL, id, "manifest"); err != nil {
			return nil, nil, err
		}
	}
	composeBytes := []byte(a.Compose)
	if len(composeBytes) == 0 {
		if composeBytes, err = r.fetchDocument(ctx, a.ComposeURL, id, "compose"); err != nil {
			return nil, nil, err
		}
	}
	man, err := manifest.Parse(manBytes)
	if err != nil {
		return nil, nil, err
	}
	return man, composeBytes, nil
}

// fetchDocument GETs one install document (manifest or compose) by following the
// URL the browse record carries, capped at maxDocumentBytes. The body is the
// verbatim file, served as application/yaml — there is no decode step and the
// content type is not asserted on, so the control plane can restate it (text/yaml
// -> application/yaml) without breaking a box.
//
// A record with no URL for the document is a catalog integrity problem, not a
// missing app, so it is NOT ErrNotFound: the store offered this app for install
// and then could not say where its install payload lives.
func (r *remoteSource) fetchDocument(ctx context.Context, ref, id, kind string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("catalog: app %q carries no %s URL", id, kind)
	}
	target, err := r.resolveURL(ref)
	if err != nil {
		return nil, fmt.Errorf("catalog: app %q %s URL %q: %w", id, kind, ref, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetch %s for %q: %w", kind, id, err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		// The catalog no longer serves this app's payload — the same answer as an
		// app that is gone, so the API layer maps it to a 404 like any other.
		return nil, fmt.Errorf("%w: %q has no %s", ErrNotFound, id, kind)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: fetch %s for %q: unexpected status %s", kind, id, resp.Status)
	}
	data, err := readLimited(resp.Body, maxDocumentBytes)
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s for %q: %w", kind, id, err)
	}
	return data, nil
}

// IconPath returns a local file path to the app's icon, proxying it from the
// published URL on first request and caching it under AssetCacheDir/assets so
// later requests (and offline browsing) are served locally. ErrNotFound when the
// app is unknown or declares no icon.
func (r *remoteSource) IconPath(id string) (string, error) {
	a, err := r.lookup(id)
	if err != nil {
		return "", err
	}
	if a.IconURL == "" {
		return "", fmt.Errorf("%w: %q has no icon", ErrNotFound, id)
	}
	return r.cachedAsset(id, a.IconURL)
}

// ScreenshotPath returns a local file path to the i-th screenshot (published
// order, 0-based), proxied and cached like the icon. ErrNotFound when the app is
// unknown or the index is out of range.
func (r *remoteSource) ScreenshotPath(id string, i int) (string, error) {
	a, err := r.lookup(id)
	if err != nil {
		return "", err
	}
	if i < 0 || i >= len(a.ScreenshotURLs) || a.ScreenshotURLs[i] == "" {
		return "", fmt.Errorf("%w: %q screenshot %d", ErrNotFound, id, i)
	}
	return r.cachedAsset(id, a.ScreenshotURLs[i])
}

// cachedAsset resolves an asset URL to a local file, fetching it and caching it
// under AssetCacheDir/assets/<id>/<name> for assetTTL. The cache name is derived
// from the URL, not taken from it (see assetCacheName), so a published URL can
// never steer a write outside the cache.
//
// A fetch failure with no cached copy returns a non-ErrNotFound error (500) — the
// app genuinely has this asset; the box just can't reach it right now. A cached
// file past its TTL is refetched, but a refetch that fails serves the expired file
// rather than the error: stale artwork beats a broken image, and the next request
// tries again.
func (r *remoteSource) cachedAsset(id, assetURL string) (string, error) {
	target, err := r.resolveURL(assetURL)
	if err != nil {
		return "", fmt.Errorf("catalog: app %q asset URL %q: %w", id, assetURL, err)
	}
	local, ok := assetCachePath(r.cacheDir, id, assetCacheName(target))
	if !ok {
		return "", fmt.Errorf("%w: %q escapes the asset cache dir", ErrNotFound, id)
	}
	if fresh(local) {
		return local, nil // fast-path cache hit — no lock
	}
	// Serialize concurrent misses for this asset so N simultaneous requests do one
	// fetch, not N; a different asset (different key) proceeds in parallel.
	mu := r.assetLock(local)
	mu.Lock()
	defer mu.Unlock()
	if fresh(local) {
		return local, nil // another goroutine fetched it while we waited on the lock
	}
	data, err := r.fetchAsset(target)
	if err != nil {
		// An expired copy is better than no icon at all.
		if _, statErr := os.Stat(local); statErr == nil {
			slog.Warn("catalog: refresh asset failed; serving the expired copy", "src", local, "url", target, "err", err)
			return local, nil
		}
		return "", err
	}
	if err := writeFileAtomic(local, data); err != nil {
		return "", fmt.Errorf("catalog: cache asset %q: %w", target, err)
	}
	return local, nil
}

// fetchAsset GETs one asset by absolute URL, capped at maxAssetBytes.
func (r *remoteSource) fetchAsset(target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog: fetch asset %q: %w", target, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: fetch asset %q: unexpected status %s", target, resp.Status)
	}
	data, err := readLimited(resp.Body, maxAssetBytes)
	if err != nil {
		return nil, fmt.Errorf("catalog: read asset %q: %w", target, err)
	}
	return data, nil
}

// resolveURL turns a URL from a published record into the absolute URL to fetch.
// An absolute URL is used as-is — assets and documents may move to object storage
// on another origin, which is exactly why the box treats these as opaque — and a
// relative one is resolved against the catalog base URL.
func (r *remoteSource) resolveURL(ref string) (string, error) {
	base, err := url.Parse(r.baseURL + "/")
	if err != nil {
		return "", fmt.Errorf("bad catalog base URL %q: %w", r.baseURL, err)
	}
	u, err := base.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return u.String(), nil
}

// safeExt matches a plain file extension, the only part of a published URL that
// reaches the cache filename.
var safeExt = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)

// assetCachePath places one app's cached asset under <cacheDir>/assets/<id>/<name>
// and refuses any result that leaves that root. The app id is published data too —
// an id carrying ".." would otherwise steer a write out of the cache — so the id is
// cleaned against a virtual root before it is re-based, and the result is checked.
// name comes from assetCacheName and is already a single safe segment.
func assetCachePath(cacheDir, id, name string) (string, bool) {
	base := filepath.Join(cacheDir, assetsDir)
	full := filepath.Join(base, filepath.FromSlash(filepath.Clean("/"+id)), name)
	if !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

// assetCacheName is the cache filename for an asset URL: a short hash of the URL
// plus the URL's own extension. The hash is what makes the name safe — no byte of
// the URL lands in a path segment, so a hostile URL cannot escape the cache dir,
// and two different URLs never collide onto one file. The extension is kept (when
// it is a plain one) because http.ServeFile reads the content type off it, so an
// icon must still end in .png.
func assetCacheName(target string) string {
	sum := sha256.Sum256([]byte(target))
	name := hex.EncodeToString(sum[:8])
	if u, err := url.Parse(target); err == nil {
		if ext := path.Ext(u.Path); safeExt.MatchString(ext) {
			name += strings.ToLower(ext)
		}
	}
	return name
}

// fresh reports whether a cached asset exists and is younger than assetTTL. An
// unreadable file counts as absent, so a broken cache entry is refetched instead
// of served.
func fresh(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < assetTTL
}

// assetLock returns the per-asset mutex for key, creating it on first use. The map
// is guarded by assetLocksMu; the returned lock guards that asset's fetch+cache.
func (r *remoteSource) assetLock(key string) *sync.Mutex {
	r.assetLocksMu.Lock()
	defer r.assetLocksMu.Unlock()
	mu, ok := r.assetLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		r.assetLocks[key] = mu
	}
	return mu
}

// drain reads and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// readLimited reads up to max bytes from rd, erroring if the body would exceed it
// — so an unbounded or hostile response body can't be pulled wholesale into
// memory (the fetch timeout bounds wall-clock, not bytes). It reads max+1 to
// detect the overflow.
func readLimited(rd io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(rd, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return data, nil
}

// writeFileAtomic writes data to a temp file in the destination directory and
// renames it into place, so a concurrent reader (or a crash mid-write) never sees
// a half-written file — each proxied asset must be all or nothing. The rename also
// resets the file's mtime, which is what expires it (see fresh / assetTTL).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
