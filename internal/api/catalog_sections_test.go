package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/catalog"
)

// sectionsHarness is the normal harness with a remote catalog seeded from a
// local snapshot file that carries landing sections and two packs. The art
// URLs are relative, so they resolve against a fake asset server standing in
// for the catalog base.
func sectionsHarness(t *testing.T, opts ...func(*Server)) *harness {
	t.Helper()
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, "png:"+r.URL.Path)
	}))
	t.Cleanup(assets.Close)

	seed := filepath.Join(t.TempDir(), "seed.json")
	snap := `{"schema_version":2,"version":"v1",
	"apps":[
		{"id":"notes","name":"Notes","version":"1","short_description":"write things down","footprint":{"image_download_bytes":0,"image_disk_bytes":0}},
		{"id":"pics","name":"Pics","version":"1","short_description":"a photo library","footprint":{"image_download_bytes":0,"image_disk_bytes":0}},
		{"id":"vault","name":"Vault","version":"1","root_setup":true,"footprint":{"image_download_bytes":0,"image_disk_bytes":0}}
	],
	"packs":[
		{"id":"family","title":"Family cloud","description":"Photos and notes.","illustration_url":"/art/family.png","apps":["pics","notes"],"keywords":["share with family"]},
		{"id":"locked","title":"Locked away","description":"Needs the remap.","apps":["notes","vault"]}
	],
	"home":{"spotlight":"pics","groups":[],"sections":[
		{"type":"search","suggestions":["photos"]},
		{"type":"discover","slides":[{"app":"pics","size":"hero","illustration_url":"/art/pics.png"},{"app":"vault","size":"side"}]},
		{"type":"packs","title":"Starter packs","packs":["family","locked"]}
	]}}`
	if err := os.WriteFile(seed, []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := catalog.NewRemote(catalog.RemoteOptions{
		BaseURL: assets.URL, Environment: "hosted",
		AssetCacheDir: t.TempDir(), SnapshotFile: seed,
	})
	// The catalog goes in first: withRemap builds its manager from s.catalog.
	return newHarness(t, append([]func(*Server){func(s *Server) { s.catalog = cat }}, opts...)...)
}

// TestCatalogHomeSections: /catalog/home carries the resolved sections, with
// apps as full entries and packs as full pack records.
func TestCatalogHomeSections(t *testing.T) {
	h := sectionsHarness(t)
	h.setupAdmin("alice", "pass1")

	home := decodeJSON[catalog.Home](t, h.getOK("/api/v1/catalog/home"))
	if len(home.Sections) != 3 {
		t.Fatalf("sections = %+v, want 3", home.Sections)
	}
	disc := home.Sections[1]
	if disc.Type != "discover" || len(disc.Slides) != 2 || disc.Slides[0].App.Name != "Pics" {
		t.Fatalf("discover = %+v", disc)
	}
	packs := home.Sections[2].Packs
	if len(packs) != 2 || packs[0].ID != "family" || packs[0].Apps[0].ID != "pics" {
		t.Fatalf("packs = %+v", packs)
	}

	// The slide's art and the pack's art are served by the brain.
	for _, u := range []string{disc.Slides[0].IllustrationURL, packs[0].IllustrationURL} {
		resp := h.getOK(u)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.HasPrefix(string(b), "png:/art/") {
			t.Errorf("GET %s = %q, want the proxied art", u, b)
		}
	}
	resp := h.do("GET", "/api/v1/catalog/illustration?key=0000000000000000", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown illustration key: want 404, got %d", resp.StatusCode)
	}
}

// TestCatalogPackRoute: a pack on this box is served with its apps; an unknown
// one is a 404.
func TestCatalogPackRoute(t *testing.T) {
	h := sectionsHarness(t)
	h.setupAdmin("alice", "pass1")

	p := decodeJSON[catalog.Pack](t, h.getOK("/api/v1/catalog/pack?id=family"))
	if p.Title != "Family cloud" || !slices.Equal(orderedIDs(p.Apps), []string{"pics", "notes"}) {
		t.Fatalf("pack = %+v", p)
	}
	for _, id := range []string{"nope", ""} {
		resp := h.do("GET", "/api/v1/catalog/pack?id="+id, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("pack %q: want 404, got %d", id, resp.StatusCode)
		}
	}
}

// TestCatalogPackRouteWithoutRemap: on a box with no remap, a pack holding an
// app that needs it is not on this box: 404 on its route, and gone from the
// landing and from search.
func TestCatalogPackRouteWithoutRemap(t *testing.T) {
	h := sectionsHarness(t, withRemap(t, false))
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/catalog/pack?id=locked", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("locked pack on a box with no remap: want 404, got %d", resp.StatusCode)
	}
	home := decodeJSON[catalog.Home](t, h.getOK("/api/v1/catalog/home"))
	if got := home.Sections[2].Packs; len(got) != 1 || got[0].ID != "family" {
		t.Errorf("packs section = %+v, want only family", got)
	}
	if got := home.Sections[1].Slides; len(got) != 1 || got[0].App.ID != "pics" {
		t.Errorf("slides = %+v, want only pics", got)
	}
	res := decodeJSON[catalog.SearchResult](t, h.getOK("/api/v1/catalog/search?q=locked"))
	if len(res.Packs) != 0 {
		t.Errorf("search found %+v, want no pack", res.Packs)
	}
}

// TestCatalogSearchPacks: /catalog/search returns the apps, then the packs that
// match on title or keyword.
func TestCatalogSearchPacks(t *testing.T) {
	h := sectionsHarness(t)
	h.setupAdmin("alice", "pass1")

	res := decodeJSON[catalog.SearchResult](t, h.getOK("/api/v1/catalog/search?q=share+with+family"))
	if len(res.Packs) != 1 || res.Packs[0].ID != "family" {
		t.Fatalf("packs = %+v, want family", res.Packs)
	}
	if !slices.Equal(orderedIDs(res.Apps), []string{"pics", "notes"}) {
		t.Errorf("apps = %v, want the pack's apps", orderedIDs(res.Apps))
	}
}

// orderedIDs is entryIDs without the sort: these tests check order too.
func orderedIDs(es []catalog.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}
