package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// syncedSectionsCatalog serves apps, a home block and packs from a fake catalog
// service, with no environment filter, and syncs a remote source from it.
func syncedSectionsCatalog(t *testing.T, apps []wireApp, home wireHomePage, packs []wirePack) *Catalog {
	t.Helper()
	version, err := contentToken(apps)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(catalogFile{SchemaVersion: wireSchemaVersion, Version: version, Apps: apps, Home: home, Packs: packs})
	if err != nil {
		t.Fatal(err)
	}
	srv := (&fakeCP{body: b, etag: `"` + version + `"`, asset: []byte("\x89PNG-fake-art")}).server()
	t.Cleanup(srv.Close)
	rs := newRemote(srv.URL, "hosted", t.TempDir())
	if err := rs.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce: %v", err)
	}
	return &Catalog{src: rs}
}

func sectionTypes(secs []HomeSection) []string {
	out := make([]string, len(secs))
	for i, s := range secs {
		out[i] = s.Type
	}
	return out
}

func packIDs(packs []Pack) []string {
	out := make([]string, len(packs))
	for i, p := range packs {
		out[i] = p.ID
	}
	return out
}

func slideIDs(slides []Slide) []string {
	out := make([]string, len(slides))
	for i, s := range slides {
		out[i] = s.App.ID
	}
	return out
}

// TestSectionsResolveAgainstTheSnapshot: every id the sections name is
// resolved against the apps and packs the payload carries. An id that is not
// there drops out of its slot, and a slide list, group or section left empty
// drops whole, as does a section type the box does not know.
func TestSectionsResolveAgainstTheSnapshot(t *testing.T) {
	home := wireHomePage{
		Sections: []wireSection{
			{Type: "search", Suggestions: []string{"notes", "media"}},
			{Type: "discover", Slides: []wireSlide{
				{App: "alpha", Size: "hero", Headline: "Alpha, now", Blurb: "Try it.", IllustrationURL: "/catalog/assets/home/alpha.png"},
				{App: "ghost", Size: "side"},
				{App: "beta", Size: "side"},
			}},
			{Type: "discover", Slides: []wireSlide{{App: "ghost", Size: "hero"}}},
			{Type: "intents", Title: "I want to...", Packs: []string{"p-ok", "p-missing-app", "no-such-pack"}},
			{Type: "packs", Packs: []string{"p-missing-app"}},
			{Type: "categories", Title: "Browse", Groups: []wireHomeGroup{
				{Category: "tools", Apps: []string{"alpha", "ghost"}},
				{Category: "empty", Apps: []string{"ghost"}},
			}},
			{Type: "something-new", Title: "Not drawn by this box"},
		},
	}
	packs := []wirePack{
		{ID: "p-ok", Title: "OK pack", Description: "Two apps.", IllustrationURL: "/catalog/assets/packs/ok.png", Apps: []string{"alpha", "gamma"}, Keywords: []string{"tools"}},
		{ID: "p-missing-app", Title: "Broken pack", Apps: []string{"alpha", "ghost"}},
	}
	c := syncedSectionsCatalog(t, segApps(), home, packs)

	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sectionTypes(h.Sections), []string{"search", "discover", "intents", "categories"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("section types = %v, want %v", got, want)
	}
	search, discover, intents, cats := h.Sections[0], h.Sections[1], h.Sections[2], h.Sections[3]

	if !reflect.DeepEqual(search.Suggestions, []string{"notes", "media"}) {
		t.Errorf("suggestions = %v", search.Suggestions)
	}

	if got := slideIDs(discover.Slides); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Errorf("slides = %v, want [alpha beta] (ghost dropped)", got)
	}
	hero := discover.Slides[0]
	if hero.Size != "hero" || hero.Headline != "Alpha, now" || hero.Blurb != "Try it." {
		t.Errorf("hero slide = %+v", hero)
	}
	if hero.App.Name != "Alpha" || hero.App.IconURL != "/api/v1/catalog/alpha/icon" {
		t.Errorf("hero slide app is not a full entry: %+v", hero.App)
	}
	if !strings.HasPrefix(hero.IllustrationURL, "/api/v1/catalog/illustration?key=") {
		t.Errorf("hero illustration = %q, want the brain's own route", hero.IllustrationURL)
	}
	if discover.Slides[1].IllustrationURL != "" {
		t.Errorf("a slide with no art has illustration %q", discover.Slides[1].IllustrationURL)
	}

	if intents.Title != "I want to..." || !reflect.DeepEqual(packIDs(intents.Packs), []string{"p-ok"}) {
		t.Fatalf("intents = %+v, want only p-ok (a pack with a missing app drops whole)", intents)
	}
	p := intents.Packs[0]
	if got := ids(p.Apps); !reflect.DeepEqual(got, []string{"alpha", "gamma"}) {
		t.Errorf("p-ok apps = %v", got)
	}
	if !strings.HasPrefix(p.IllustrationURL, "/api/v1/catalog/illustration?key=") || !reflect.DeepEqual(p.Keywords, []string{"tools"}) {
		t.Errorf("p-ok = %+v", p)
	}

	if len(cats.Groups) != 1 || cats.Groups[0].Category != "tools" || cats.Groups[0].Label != "Tools" {
		t.Fatalf("category groups = %+v, want only tools, labelled", cats.Groups)
	}
	if got := ids(cats.Groups[0].Apps); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("tools group = %v, want [alpha]", got)
	}

	// The pack route sees the same packs.
	if got, err := c.Pack("p-ok"); err != nil || got.Title != "OK pack" {
		t.Errorf("Pack(p-ok) = %+v, %v", got, err)
	}
	for _, id := range []string{"p-missing-app", "no-such-pack", ""} {
		if _, err := c.Pack(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Pack(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

// TestNoSectionsKeepsTheOlderLanding: a catalog with no sections gives a Home
// with none, and the spotlight and groups are still there for the fallback.
func TestNoSectionsKeepsTheOlderLanding(t *testing.T) {
	c := syncedSectionsCatalog(t, segApps(), homeFixture(), nil)
	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	if h.Sections != nil {
		t.Errorf("sections = %+v, want none", h.Sections)
	}
	if h.Spotlight == nil || len(h.Groups) == 0 {
		t.Errorf("spotlight %v and groups %v, want both", h.Spotlight, h.Groups)
	}
}

// TestIllustrationIsProxiedByKey: the art a section or pack names is served
// through the asset cache under its key, and a key the payload does not name
// is not found, so the route cannot fetch any other URL.
func TestIllustrationIsProxiedByKey(t *testing.T) {
	home := wireHomePage{Sections: []wireSection{{Type: "discover", Slides: []wireSlide{
		{App: "alpha", Size: "hero", IllustrationURL: "/catalog/assets/home/alpha.png"},
	}}}}
	packs := []wirePack{{ID: "p", Title: "P", IllustrationURL: "/catalog/assets/packs/p.png", Apps: []string{"gamma"}}}
	c := syncedSectionsCatalog(t, segApps(), home, packs)
	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Pack("p")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{h.Sections[0].Slides[0].IllustrationURL, p.IllustrationURL} {
		key := strings.TrimPrefix(u, "/api/v1/catalog/illustration?key=")
		path, err := c.IllustrationPath(key)
		if err != nil {
			t.Fatalf("IllustrationPath(%q): %v", key, err)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != "\x89PNG-fake-art" {
			t.Errorf("cached art = %q, %v", b, err)
		}
	}
	if _, err := c.IllustrationPath("0000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key = %v, want ErrNotFound", err)
	}
}

// TestWithoutRemapAppsDropsFromSections: on a box with no remap, an app that
// needs it drops from slides and groups, and a pack holding one drops whole,
// from the pack list, the pack route and every section that names it.
func TestWithoutRemapAppsDropsFromSections(t *testing.T) {
	home := wireHomePage{Sections: []wireSection{
		{Type: "search"},
		{Type: "discover", Slides: []wireSlide{{App: "beta", Size: "hero"}, {App: "alpha", Size: "side"}}},
		{Type: "discover", Slides: []wireSlide{{App: "delta", Size: "hero"}}},
		{Type: "packs", Packs: []string{"p-clean", "p-delta"}},
		{Type: "intents", Packs: []string{"p-delta"}},
		{Type: "categories", Groups: []wireHomeGroup{
			{Category: "notes", Apps: []string{"delta"}},
			{Category: "tools", Apps: []string{"alpha", "gamma"}},
		}},
	}}
	packs := []wirePack{
		{ID: "p-clean", Title: "Clean", Apps: []string{"alpha", "gamma"}},
		{ID: "p-delta", Title: "Needs remap", Apps: []string{"alpha", "delta"}},
	}
	full := syncedSectionsCatalog(t, remapApps(), home, packs)
	c := full.WithoutRemapApps()

	h, err := c.Home()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sectionTypes(h.Sections), []string{"search", "discover", "packs", "categories"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("section types = %v, want %v", got, want)
	}
	if got := slideIDs(h.Sections[1].Slides); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("slides = %v, want [alpha] (beta is root_setup)", got)
	}
	if got := packIDs(h.Sections[2].Packs); !reflect.DeepEqual(got, []string{"p-clean"}) {
		t.Errorf("packs section = %v, want [p-clean]", got)
	}
	if g := h.Sections[3].Groups; len(g) != 1 || g[0].Category != "tools" {
		t.Errorf("groups = %+v, want only tools", g)
	}

	if _, err := c.Pack("p-delta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("filtered Pack(p-delta) = %v, want ErrNotFound", err)
	}
	if _, err := full.Pack("p-delta"); err != nil {
		t.Errorf("unfiltered Pack(p-delta) = %v, want the pack", err)
	}
	if got, _ := c.Search("needs remap"); len(got.Packs) != 0 {
		t.Errorf("filtered search found packs %v", packIDs(got.Packs))
	}

	fullHome, err := full.Home()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(fullHome.Sections); got != 6 {
		t.Errorf("unfiltered sections = %d, want all 6", got)
	}
}

// TestSearchFindsPacks: a pack matches on its title or a keyword, as phrases.
// The matching packs' apps come first in the app results, then the apps that
// match on their own text, each once.
func TestSearchFindsPacks(t *testing.T) {
	packs := []wirePack{
		{ID: "photos", Title: "Back up my photos", Apps: []string{"gamma", "alpha"}, Keywords: []string{"photo backup", "gallery"}},
		{ID: "util", Title: "Handy things", Apps: []string{"alpha", "gamma"}, Keywords: []string{"utility"}},
	}
	c := syncedSectionsCatalog(t, segApps(), wireHomePage{}, packs)

	cases := []struct {
		q         string
		packs     []string
		firstApps []string
	}{
		{"phot", []string{"photos"}, []string{"gamma", "alpha"}},         // starts a word of the title
		{"Back up my GALLERY!", []string{"photos"}, nil},                 // holds a whole keyword
		{"photo backup", []string{"photos"}, []string{"gamma", "alpha"}}, // a keyword as typed
		{"hotos", nil, nil}, // inside a word: no pack
		{"p", nil, nil},     // one character: no pack
		{"utility", []string{"util"}, []string{"alpha", "gamma"}},
	}
	for _, tc := range cases {
		got, err := c.Search(tc.q)
		if err != nil {
			t.Fatal(err)
		}
		if gp := packIDs(got.Packs); !reflect.DeepEqual(gp, tc.packs) && (len(gp) != 0 || len(tc.packs) != 0) {
			t.Errorf("Search(%q) packs = %v, want %v", tc.q, gp, tc.packs)
		}
		if tc.firstApps != nil {
			if ga := ids(got.Apps); len(ga) < len(tc.firstApps) || !reflect.DeepEqual(ga[:len(tc.firstApps)], tc.firstApps) {
				t.Errorf("Search(%q) apps = %v, want %v first", tc.q, ga, tc.firstApps)
			}
		}
	}

	// gamma matches "utility" on its own tagline too, but shows once.
	got, _ := c.Search("utility")
	if a := ids(got.Apps); !reflect.DeepEqual(a, []string{"alpha", "gamma"}) {
		t.Errorf("Search(utility) apps = %v, want [alpha gamma] with no repeat", a)
	}
}
