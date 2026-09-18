package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// rankPtr is the *int helper the featured fixtures need (Rank is a pointer so an
// unranked app omits the field).
func rankPtr(i int) *int { return &i }

// segApps is the segmentation fixture: two featured apps at different ranks, plus
// one unfeatured app, so featured order and the category/search projections all
// have something to bite on.
//
//	alpha — appliance+hosted, categories {tools, media}, featured rank 2
//	beta  — hosted only,      categories {media},        featured rank 1
//	gamma — appliance+hosted, categories {tools},        not featured
func segApps() []wireApp {
	return []wireApp{
		{
			ID: "alpha", Name: "Alpha", Version: "1.0",
			ShortDescription: "the first app",
			Categories:       []string{"tools", "media"},
			IconURL:          "/catalog/assets/alpha/icon.png",
			Featured:         true, Rank: rankPtr(2),
			ManifestURL: "/catalog/apps/alpha/manifest",
			ComposeURL:  "/catalog/apps/alpha/compose",
		},
		{
			ID: "beta", Name: "Beta", Version: "2.0",
			ShortDescription: "streams media",
			Categories:       []string{"media"},
			Featured:         true, Rank: rankPtr(1),
			ManifestURL: "/catalog/apps/beta/manifest",
			ComposeURL:  "/catalog/apps/beta/compose",
		},
		{
			ID: "gamma", Name: "Gamma", Version: "3.0",
			ShortDescription: "a utility",
			Categories:       []string{"tools"},
			ManifestURL:      "/catalog/apps/gamma/manifest",
			ComposeURL:       "/catalog/apps/gamma/compose",
		},
	}
}

// segAppEnvs is which surfaces each fixture app is advertised on. It lives in the
// test, not on the wire record, because environment filtering is the catalog
// service's job now (#434): GET /catalog returns only the apps for the ?env= it
// was asked for, and the box shows what it is given. advertisedIn is what the
// fake catalog below applies, so these tests still prove the two surfaces render
// differently — they just prove it through the seam that now does the filtering.
var segAppEnvs = map[string][]string{
	"alpha": {"appliance", "hosted"},
	"beta":  {"hosted"},
	"gamma": {"appliance", "hosted"},
}

// advertisedIn returns the apps the catalog would serve to a box on env.
func advertisedIn(apps []wireApp, env string) []wireApp {
	var out []wireApp
	for _, a := range apps {
		for _, e := range segAppEnvs[a.ID] {
			if e == env {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// syncedCatalog builds the Catalog facade over a freshly synced remote source for
// env, so the segmented methods (which live on the facade) run against a real
// snapshot without a background loop racing them.
func syncedCatalog(t *testing.T, apps []wireApp, env string) *Catalog {
	t.Helper()
	cp := newFakeCP(t, advertisedIn(apps, env))
	srv := cp.server()
	t.Cleanup(srv.Close)
	rs := newRemote(srv.URL, env, t.TempDir())
	if err := rs.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}
	return &Catalog{src: rs}
}

func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// homeFixture is the authored landing-page fixture for the home-projection
// tests: a hosted-only spotlight (dropped on appliance) and two groups, one of
// which (media) empties out entirely on appliance because its only app is
// hosted-only.
func homeFixture() wireHomePage {
	return wireHomePage{
		Spotlight: "beta",
		Groups: []wireHomeGroup{
			{Category: "tools", Apps: []string{"alpha", "gamma"}},
			{Category: "media", Apps: []string{"beta"}},
		},
	}
}

// makeSnapshotHome is makeSnapshot plus a home block, for the tests that
// exercise the landing-page projection.
func makeSnapshotHome(t *testing.T, apps []wireApp, home wireHomePage) (body []byte, etag string) {
	t.Helper()
	version, err := contentToken(apps)
	if err != nil {
		t.Fatal(err)
	}
	f := catalogFile{SchemaVersion: wireSchemaVersion, Version: version, Apps: apps, Home: home}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b, `"` + version + `"`
}

// newFakeCPHome is newFakeCP plus a home block on the served snapshot.
func newFakeCPHome(t *testing.T, apps []wireApp, home wireHomePage) *fakeCP {
	body, etag := makeSnapshotHome(t, apps, home)
	return &fakeCP{body: body, etag: etag, asset: []byte("\x89PNG-fake-bytes")}
}

// syncedCatalogWithHome is syncedCatalog plus a home block on the served
// snapshot.
func syncedCatalogWithHome(t *testing.T, apps []wireApp, home wireHomePage, env string) *Catalog {
	t.Helper()
	cp := newFakeCPHome(t, advertisedIn(apps, env), home)
	srv := cp.server()
	t.Cleanup(srv.Close)
	rs := newRemote(srv.URL, env, t.TempDir())
	if err := rs.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}
	return &Catalog{src: rs}
}

// catIDs projects the category vocabulary entries to their ids, for assertions
// about which categories are present rather than how they are labelled.
func catIDs(cats []Category) []string {
	out := make([]string, len(cats))
	for i, c := range cats {
		out[i] = c.ID
	}
	return out
}

func TestHomeSegmentsByEnv(t *testing.T) {
	// Appliance: categories are the union over appliance-visible apps (alpha,
	// gamma); featured is only the appliance-visible featured app (alpha) — beta is
	// hosted-only and must not leak into the appliance landing.
	appliance, err := syncedCatalog(t, segApps(), "appliance").Home()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"media", "tools"}; !reflect.DeepEqual(catIDs(appliance.Categories), want) {
		t.Fatalf("appliance categories = %v, want %v (sorted union)", catIDs(appliance.Categories), want)
	}
	if got := ids(appliance.Featured); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("appliance featured = %v, want [alpha] (beta is hosted-only)", got)
	}

	// Hosted: both featured apps show, ascending by rank (beta rank 1 before alpha
	// rank 2), regardless of name order.
	hosted, err := syncedCatalog(t, segApps(), "hosted").Home()
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hosted.Featured); !reflect.DeepEqual(got, []string{"beta", "alpha"}) {
		t.Fatalf("hosted featured = %v, want [beta alpha] (by rank)", got)
	}
}

// TestHomeProjectsSpotlightAndGroups covers the authored landing page (a
// curated home.yml, carried on the synced snapshot): the spotlight and each group's
// apps are filtered to this box's environment exactly like every other
// projection — an app not advertised here drops out of its slot (the
// spotlight goes nil, or the app is skipped within its group), and a group
// left with no advertised apps is dropped entirely rather than rendered empty.
func TestHomeProjectsSpotlightAndGroups(t *testing.T) {
	home := homeFixture()

	appliance, err := syncedCatalogWithHome(t, segApps(), home, "appliance").Home()
	if err != nil {
		t.Fatal(err)
	}
	if appliance.Spotlight != nil {
		t.Fatalf("appliance spotlight = %+v, want nil (beta is hosted-only)", appliance.Spotlight)
	}
	if len(appliance.Groups) != 1 || appliance.Groups[0].Category != "tools" {
		t.Fatalf("appliance groups = %+v, want just the tools group (media empties out)", appliance.Groups)
	}
	if got := ids(appliance.Groups[0].Apps); !reflect.DeepEqual(got, []string{"alpha", "gamma"}) {
		t.Fatalf("tools group apps = %v, want [alpha gamma]", got)
	}

	hosted, err := syncedCatalogWithHome(t, segApps(), home, "hosted").Home()
	if err != nil {
		t.Fatal(err)
	}
	if hosted.Spotlight == nil || hosted.Spotlight.ID != "beta" {
		t.Fatalf("hosted spotlight = %+v, want beta", hosted.Spotlight)
	}
	if len(hosted.Groups) != 2 {
		t.Fatalf("hosted groups = %+v, want both groups (beta is advertised on hosted)", hosted.Groups)
	}
	if got := ids(hosted.Groups[1].Apps); !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("hosted media group apps = %v, want [beta]", got)
	}
}

// TestHomeNoBlockYieldsNoSpotlightOrGroups covers a snapshot with no authored
// home block (the box has never synced a landing page, or the sync tool
// published one with an empty home.yml): the landing carries neither a
// spotlight nor groups, while the categories/featured projections it shares
// with the home block are unaffected.
func TestHomeNoBlockYieldsNoSpotlightOrGroups(t *testing.T) {
	h, err := syncedCatalog(t, segApps(), "appliance").Home()
	if err != nil {
		t.Fatal(err)
	}
	if h.Spotlight != nil {
		t.Fatalf("spotlight = %+v, want nil with no home block", h.Spotlight)
	}
	if len(h.Groups) != 0 {
		t.Fatalf("groups = %+v, want none with no home block", h.Groups)
	}
	if want := []string{"media", "tools"}; !reflect.DeepEqual(catIDs(h.Categories), want) {
		t.Fatalf("categories = %v, want %v (unaffected by the absent home block)", catIDs(h.Categories), want)
	}
	if got := ids(h.Featured); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("featured = %v, want [alpha] (unaffected by the absent home block)", got)
	}
}

func TestCategoryFiltersAndFolds(t *testing.T) {
	c := syncedCatalog(t, segApps(), "appliance")

	// "tools" on appliance: alpha + gamma, and the featured row rides along.
	tools, err := c.Category("tools")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(tools.Apps); !reflect.DeepEqual(got, []string{"alpha", "gamma"}) {
		t.Fatalf("category tools apps = %v, want [alpha gamma]", got)
	}
	if got := ids(tools.Featured); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("category tools featured = %v, want [alpha]", got)
	}

	// Match is case-insensitive: "Media" resolves the "media" category (alpha only
	// on appliance — beta is hosted-only).
	media, err := c.Category("Media")
	if err != nil {
		t.Fatalf("Category(Media) should fold to media: %v", err)
	}
	if got := ids(media.Apps); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("category Media apps = %v, want [alpha]", got)
	}

	// An unknown / empty-on-this-surface category is ErrNotFound, not an empty page.
	if _, err := c.Category("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Category(nope) = %v, want ErrNotFound", err)
	}
}

func TestSearchMatchesAndFiltersEnv(t *testing.T) {
	appliance := syncedCatalog(t, segApps(), "appliance")

	// A blank query returns nothing — search narrows, it does not dump the catalog.
	if got, _ := appliance.Search("   "); got != nil {
		t.Fatalf("blank Search = %v, want nil", got)
	}

	// Name match.
	if got, _ := appliance.Search("alpha"); !reflect.DeepEqual(ids(got), []string{"alpha"}) {
		t.Fatalf("Search(alpha) = %v, want [alpha]", ids(got))
	}

	// Category match: "media" hits alpha via its category (gamma is tools-only).
	if got, _ := appliance.Search("MEDIA"); !reflect.DeepEqual(ids(got), []string{"alpha"}) {
		t.Fatalf("Search(MEDIA) = %v, want [alpha] (category match, case-insensitive)", ids(got))
	}

	// Env filter: "beta" is hosted-only, so an appliance search never surfaces it.
	if got, _ := appliance.Search("beta"); got != nil {
		t.Fatalf("appliance Search(beta) = %v, want nil (hosted-only)", ids(got))
	}
	if got, _ := syncedCatalog(t, segApps(), "hosted").Search("beta"); !reflect.DeepEqual(ids(got), []string{"beta"}) {
		t.Fatalf("hosted Search(beta) = %v, want [beta]", ids(got))
	}
}

func TestSegmentedEmptyStoreNoError(t *testing.T) {
	// A never-synced store (disk source with an empty dir stands in for "no apps"):
	// Home is empty but not an error, Category is ErrNotFound, Search is nil.
	c := New(t.TempDir())
	h, err := c.Home()
	if err != nil {
		t.Fatalf("empty Home errored: %v", err)
	}
	if len(h.Categories) != 0 || len(h.Featured) != 0 {
		t.Fatalf("empty Home = %+v, want no categories/featured", h)
	}
	if _, err := c.Category("tools"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty Category = %v, want ErrNotFound", err)
	}
	if got, _ := c.Search("x"); got != nil {
		t.Fatalf("empty Search = %v, want nil", got)
	}
}

// makeSnapshotFull is makeSnapshotHome plus a category vocabulary — for the
// label tests, where what the snapshot says a category is called is the subject.
func makeSnapshotFull(t *testing.T, apps []wireApp, home wireHomePage, cats []wireCategory) (body []byte, etag string) {
	t.Helper()
	version, err := contentToken(apps)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(catalogFile{SchemaVersion: wireSchemaVersion, Version: version, Apps: apps, Home: home, Categories: cats})
	if err != nil {
		t.Fatal(err)
	}
	return b, `"` + version + `"`
}

func syncedCatalogWithCats(t *testing.T, apps []wireApp, home wireHomePage, cats []wireCategory, env string) *Catalog {
	t.Helper()
	body, etag := makeSnapshotFull(t, advertisedIn(apps, env), home, cats)
	cp := &fakeCP{body: body, etag: etag, asset: []byte("\x89PNG-fake-bytes")}
	srv := cp.server()
	t.Cleanup(srv.Close)
	rs := newRemote(srv.URL, env, t.TempDir())
	if err := rs.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}
	return &Catalog{src: rs}
}

// The authored label reaches every place the box names a category: the pills, the
// landing row headings, and the category page. Before the vocabulary was on the
// wire the UI derived display text from the id, and disagreed with the other store
// surface doing the same ("Developer-tools" against "developer tools").
func TestCategoryLabelsComeFromTheSnapshot(t *testing.T) {
	apps := segApps()
	home := wireHomePage{Spotlight: "alpha", Groups: []wireHomeGroup{{Category: "tools", Apps: []string{"gamma"}}}}
	cats := []wireCategory{
		{ID: "tools", Label: "Developer tools"},
		{ID: "media", Label: "Media"},
	}
	c := syncedCatalogWithCats(t, apps, home, cats, "appliance")

	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	// Authored order, not alphabetical: "tools" is authored before "media", which
	// sorts the other way, so a sorted result would fail here.
	if got := catIDs(h.Categories); !reflect.DeepEqual(got, []string{"tools", "media"}) {
		t.Fatalf("categories = %v, want authored order [tools media]", got)
	}
	if h.Categories[0].Label != "Developer tools" {
		t.Errorf("pill label = %q, want %q (authored, not derived from the id)", h.Categories[0].Label, "Developer tools")
	}
	if len(h.Groups) != 1 || h.Groups[0].Label != "Developer tools" {
		t.Fatalf("group = %+v, want one row labelled %q", h.Groups, "Developer tools")
	}

	page, err := c.Category("tools")
	if err != nil {
		t.Fatal(err)
	}
	if page.Label != "Developer tools" {
		t.Errorf("category page label = %q, want %q", page.Label, "Developer tools")
	}
}

// A box with no vocabulary — never synced, or a snapshot published before the
// field existed — still renders something readable rather than a blank pill, and
// still shows every browsable category. The fallback matches the other store
// surfaces' own, so they agree even in the degraded case.
func TestCategoryLabelFallsBackToAReadableID(t *testing.T) {
	apps := segApps()
	c := syncedCatalogWithCats(t, apps, wireHomePage{}, nil, "appliance")

	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	if got := catIDs(h.Categories); !reflect.DeepEqual(got, []string{"media", "tools"}) {
		t.Fatalf("categories = %v, want every browsable category [media tools]", got)
	}
	if h.Categories[0].Label != "Media" || h.Categories[1].Label != "Tools" {
		t.Errorf("fallback labels = %q/%q, want Media/Tools", h.Categories[0].Label, h.Categories[1].Label)
	}
}

// A category an app claims but the vocabulary omits must still get a pill: dropping
// it would hide a browsable app behind no entry point at all.
func TestCategoryMissingFromVocabularyStillGetsAPill(t *testing.T) {
	apps := segApps()
	cats := []wireCategory{{ID: "media", Label: "Photos and video"}}
	c := syncedCatalogWithCats(t, apps, wireHomePage{}, cats, "appliance")

	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	// Authored entries first, then the unknown ones sorted — a deterministic order,
	// not the map's.
	if got := catIDs(h.Categories); !reflect.DeepEqual(got, []string{"media", "tools"}) {
		t.Fatalf("categories = %v, want [media tools]", got)
	}
	if h.Categories[0].Label != "Photos and video" {
		t.Errorf("authored label = %q, want %q", h.Categories[0].Label, "Photos and video")
	}
	if h.Categories[1].Label != "Tools" {
		t.Errorf("unknown-category label = %q, want the readable fallback %q", h.Categories[1].Label, "Tools")
	}
}
