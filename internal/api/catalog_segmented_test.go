package api

import (
	"net/http"
	"testing"

	"github.com/onmoose/os/internal/catalog"
)

// toolsManifestYML is a second listed app in a distinct category, so the segmented
// endpoints have a category union and a filter to exercise (rich is media/photos).
const toolsManifestYML = `id: widget
manifest_version: 1
name: Widget
version: "1.0"
description:
  short: "a handy widget"
categories: [tools]
compose_file: compose.yml
main_service: app
main_port: 80
`

// TestCatalogHome: the landing lists the sorted union of the browsable apps'
// categories. Featured is empty off a disk catalog (no store curation on disk).
func TestCatalogHome(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "rich", richManifestYML)
	writeManifestFixture(t, h.catalogDir, "widget", toolsManifestYML)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/catalog/home", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	home := decodeJSON[catalog.Home](t, resp)
	want := []string{"media", "photos", "tools"}
	if len(home.Categories) != len(want) {
		t.Fatalf("categories = %v, want %v", home.Categories, want)
	}
	for i, c := range want {
		if home.Categories[i].ID != c {
			t.Fatalf("categories = %v, want %v (sorted union)", home.Categories, want)
		}
		// A disk catalog carries no authored vocabulary, so every label is the
		// readable-id fallback rather than blank.
		if home.Categories[i].Label == "" {
			t.Errorf("category %q has an empty label; want the readable-id fallback", c)
		}
	}
	if len(home.Featured) != 0 {
		t.Fatalf("disk catalog has no curation; featured = %v, want empty", home.Featured)
	}
}

// TestCatalogCategory: ?name= filters to that category; an unknown category is 404.
func TestCatalogCategory(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "rich", richManifestYML)
	writeManifestFixture(t, h.catalogDir, "widget", toolsManifestYML)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/catalog/category?name=tools", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	cat := decodeJSON[catalog.CategoryPage](t, resp)
	if cat.Category != "tools" || len(cat.Apps) != 1 || cat.Apps[0].ID != "widget" {
		t.Fatalf("category tools = %+v, want just widget", cat)
	}

	nope := h.do("GET", "/api/v1/catalog/category?name=ghosts", nil)
	if nope.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown category: want 404, got %d", nope.StatusCode)
	}
	nope.Body.Close()
}

// TestCatalogSearch: ?q= matches name/tagline/categories; a blank query is empty.
func TestCatalogSearch(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "rich", richManifestYML)
	writeManifestFixture(t, h.catalogDir, "widget", toolsManifestYML)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/catalog/search?q=widget", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	out := decodeJSON[struct {
		Apps []catalog.Entry `json:"apps"`
	}](t, resp)
	if len(out.Apps) != 1 || out.Apps[0].ID != "widget" {
		t.Fatalf("search widget = %+v, want just widget", out.Apps)
	}

	blank := h.do("GET", "/api/v1/catalog/search?q=", nil)
	if blank.StatusCode != http.StatusOK {
		t.Fatalf("blank search: want 200, got %d", blank.StatusCode)
	}
	empty := decodeJSON[struct {
		Apps []catalog.Entry `json:"apps"`
	}](t, blank)
	if len(empty.Apps) != 0 {
		t.Fatalf("blank search returned %d apps, want 0", len(empty.Apps))
	}
}

// TestCatalogSegmentedRequiresAuth: the segmented routes sit behind the same auth
// middleware as the rest of /api/v1 — an unauthenticated browse is 401, not a data
// leak.
func TestCatalogSegmentedRequiresAuth(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "rich", richManifestYML)
	jar, _ := newJar()
	h.jar = jar

	for _, path := range []string{
		"/api/v1/catalog/home",
		"/api/v1/catalog/category?name=media",
		"/api/v1/catalog/search?q=rich",
	} {
		resp := h.do("GET", path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
