package lifecycle

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/manifest"
	"github.com/onmoose/moose/internal/store"
)

func TestRepoOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"redis", "redis"},
		{"redis:7", "redis"},
		{"traefik/whoami:v1.10.3", "traefik/whoami"},
		{"ghcr.io/foo/bar", "ghcr.io/foo/bar"},
		{"ghcr.io/foo/bar:tag", "ghcr.io/foo/bar"},
		// Registry with port: the `:5000` must NOT be treated as a tag.
		{"ghcr.io:5000/foo/bar:v1", "ghcr.io:5000/foo/bar"},
		{"ghcr.io:5000/foo/bar", "ghcr.io:5000/foo/bar"},
		// Digest form strips at `@`, regardless of any preceding tag.
		{"redis@sha256:deadbeef", "redis"},
		// Tag AND digest: both come off, so the pin stays canonical `name@sha256:…`.
		{"nextcloud:34.0.1-apache@sha256:deadbeef", "nextcloud"},
		{"ghcr.io:5000/foo/bar:v1@sha256:deadbeef", "ghcr.io:5000/foo/bar"},
	} {
		if got := repoOf(tc.in); got != tc.want {
			t.Errorf("repoOf(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestDigestOf(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  string
		hasOK bool
	}{
		{"redis", "", false},
		{"redis:7", "", false},
		{"redis@sha256:abc", "sha256:abc", true},
	} {
		got, ok := digestOf(tc.in)
		if ok != tc.hasOK || got != tc.want {
			t.Errorf("digestOf(%q)=(%q,%v) want (%q,%v)", tc.in, got, ok, tc.want, tc.hasOK)
		}
	}
}

func TestServiceImages(t *testing.T) {
	got, err := serviceImages([]byte(`
services:
  web: {image: nginx:1}
  db:  {image: postgres:16}
`))
	if err != nil {
		t.Fatalf("serviceImages: %v", err)
	}
	want := map[string]string{"web": "nginx:1", "db": "postgres:16"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	if _, err := serviceImages([]byte(`services: {bad: {}}`)); err == nil {
		t.Fatalf("service without image must error")
	}
}

func TestServicePinPinnedRef(t *testing.T) {
	// PinnedRef returns the override ref verbatim: the digest form online,
	// the original tag in the offline-local case (resolveImages sets it).
	digestPin := servicePin{Image: "traefik/whoami:v1.10.3", Digest: "sha256:abc", ref: "traefik/whoami@sha256:abc"}
	if got := digestPin.PinnedRef(); got != "traefik/whoami@sha256:abc" {
		t.Fatalf("PinnedRef (digest) = %q", got)
	}
	tagPin := servicePin{Image: "traefik/whoami:v1.10.3", Digest: "sha256:abc", ref: "traefik/whoami:v1.10.3"}
	if got := tagPin.PinnedRef(); got != "traefik/whoami:v1.10.3" {
		t.Fatalf("PinnedRef (offline tag) = %q", got)
	}
}

// TestAllocateSlug drives the real Manager.allocateSlug against a real store
// so the test catches regressions in either layer.
func TestAllocateSlug(t *testing.T) {
	newMgr := func(t *testing.T) *Manager {
		t.Helper()
		s, err := store.Open(filepath.Join(t.TempDir(), "moose.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return &Manager{store: s}
	}
	mark := func(t *testing.T, s *store.Store, slug string) {
		t.Helper()
		if err := s.Create(store.Instance{
			ID: slug + "-id", ManifestID: "x", Name: "X", Slug: slug,
			Version: "1", State: "running",
			OwnerUserID: "u_admin", Scope: store.ScopeHousehold,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed slug %q: %v", slug, err)
		}
	}

	t.Run("first choice wins", func(t *testing.T) {
		m := newMgr(t)
		slug, err := m.allocateSlug(&manifest.Manifest{ID: "whoami"}, store.ScopeHousehold, "")
		if err != nil || slug != "whoami" {
			t.Fatalf("slug=%q err=%v", slug, err)
		}
	})

	t.Run("falls back to -2 on conflict", func(t *testing.T) {
		m := newMgr(t)
		mark(t, m.store, "whoami")
		slug, err := m.allocateSlug(&manifest.Manifest{ID: "whoami"}, store.ScopeHousehold, "")
		if err != nil || slug != "whoami-2" {
			t.Fatalf("slug=%q err=%v", slug, err)
		}
	})

	t.Run("reserved is skipped even when free", func(t *testing.T) {
		m := newMgr(t)
		// "api" is reserved → allocator must skip it and try -2/-3 (also free).
		slug, err := m.allocateSlug(&manifest.Manifest{ID: "api"}, store.ScopeHousehold, "")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if slug == "api" {
			t.Fatalf("returned reserved slug %q", slug)
		}
	})

	t.Run("exhaustion errors", func(t *testing.T) {
		m := newMgr(t)
		mark(t, m.store, "whoami")
		mark(t, m.store, "whoami-2")
		mark(t, m.store, "whoami-3")
		if _, err := m.allocateSlug(&manifest.Manifest{ID: "whoami"}, store.ScopeHousehold, ""); err == nil {
			t.Fatalf("expected exhaustion error")
		}
	})

	t.Run("preferred slugs are tried in order", func(t *testing.T) {
		m := newMgr(t)
		mark(t, m.store, "whoami")
		slug, err := m.allocateSlug(&manifest.Manifest{
			ID: "whoami", PreferredSlugs: []string{"whoami", "hello"},
		}, store.ScopeHousehold, "")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		// Either "whoami-2" (exhaust first cand suffixes first) or "hello"
		// is consistent with allocateSlug's loop shape. Document by asserting
		// what the implementation does: -2 before next candidate.
		if slug != "whoami-2" {
			t.Fatalf("slug=%q, want whoami-2", slug)
		}
	})

	t.Run("personal scope gets the bare name first-come", func(t *testing.T) {
		m := newMgr(t)
		// First-come, first-served: a personal install on an empty store takes
		// the bare slug, not `immich--alex`. The owner suffix is reserved for
		// disambiguating a collision (DASHBOARD.md # instance naming).
		slug, err := m.allocateSlug(&manifest.Manifest{ID: "immich"}, store.ScopePersonal, "alex")
		if err != nil || slug != "immich" {
			t.Fatalf("slug=%q err=%v, want immich", slug, err)
		}
	})

	t.Run("personal scope suffixes the owner on collision", func(t *testing.T) {
		m := newMgr(t)
		mark(t, m.store, "immich") // someone already holds the bare name
		slug, err := m.allocateSlug(&manifest.Manifest{ID: "immich"}, store.ScopePersonal, "alex")
		if err != nil || slug != "immich--alex" {
			t.Fatalf("slug=%q err=%v, want immich--alex", slug, err)
		}
	})

	t.Run("personal double collision falls back to numeric", func(t *testing.T) {
		m := newMgr(t)
		mark(t, m.store, "immich")       // bare taken
		mark(t, m.store, "immich--alex") // owner-qualified taken
		slug, err := m.allocateSlug(&manifest.Manifest{ID: "immich"}, store.ScopePersonal, "alex")
		if err != nil || slug != "immich-2" {
			t.Fatalf("slug=%q err=%v, want immich-2", slug, err)
		}
	})
}
