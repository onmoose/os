package api

// resolveElections is the authoritative write-path validation of per-folder
// install elections (the install-plan endpoint is advisory). Pure function —
// tested directly against parsed manifests.

import (
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// jellyfinManifest parses the shared folders fixture: movies (write,
// pick-subfolder, default Movies/Family) + music (read, whole).
func jellyfinManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	man, err := manifest.Parse([]byte(foldersManifestYML))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return man
}

// mountFor returns the resolved mount for a folder name, or fails.
func mountFor(t *testing.T, mounts []lifecycle.FolderMount, folder string) lifecycle.FolderMount {
	t.Helper()
	for _, m := range mounts {
		if m.Folder == folder {
			return m
		}
	}
	t.Fatalf("no mount for %q in %v", folder, mounts)
	return lifecycle.FolderMount{}
}

// wantStatus asserts err is a huma error carrying the given HTTP status.
func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with status %d, got nil", status)
	}
	se, ok := err.(huma.StatusError)
	if !ok {
		t.Fatalf("want huma.StatusError, got %T: %v", err, err)
	}
	if se.GetStatus() != status {
		t.Fatalf("want status %d, got %d: %v", status, se.GetStatus(), err)
	}
}

func TestResolveElections_DefaultsPersonal(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolveElections(man, store.ScopePersonal, nil)
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("want one mount per declared folder (2), got %d", len(mounts))
	}
	movies := mountFor(t, mounts, "movies")
	if movies.Source != sourcePersonal {
		t.Errorf("movies source: want personal default, got %q", movies.Source)
	}
	if movies.Subfolder != "Movies/Family" {
		t.Errorf("movies subfolder: want manifest default Movies/Family, got %q", movies.Subfolder)
	}
	music := mountFor(t, mounts, "music")
	if music.Source != sourcePersonal || music.Subfolder != "" {
		t.Errorf("music: want personal/no-subfolder, got %+v", music)
	}
}

func TestResolveElections_DefaultsHousehold(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolveElections(man, store.ScopeHousehold, nil)
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	for _, m := range mounts {
		if m.Source != sourceShared {
			t.Errorf("%s: household forces shared, got %q", m.Folder, m.Source)
		}
	}
}

func TestResolveElections_PersonalMayElectShared(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolveElections(man, store.ScopePersonal,
		[]FolderElection{{Folder: "movies", Source: sourceShared}})
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if got := mountFor(t, mounts, "movies").Source; got != sourceShared {
		t.Errorf("movies source: want shared, got %q", got)
	}
}

func TestResolveElections_SubfolderOverride(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolveElections(man, store.ScopePersonal,
		[]FolderElection{{Folder: "movies", Subfolder: "Movies/Kids"}})
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if got := mountFor(t, mounts, "movies").Subfolder; got != "Movies/Kids" {
		t.Errorf("movies subfolder: want override Movies/Kids, got %q", got)
	}
}

func TestResolveElections_Rejections(t *testing.T) {
	man := jellyfinManifest(t)
	cases := []struct {
		name      string
		scope     string
		elections []FolderElection
	}{
		{"household cannot elect personal", store.ScopeHousehold,
			[]FolderElection{{Folder: "movies", Source: sourcePersonal}}},
		{"invalid source value", store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Source: "everyone"}}},
		{"subfolder on a whole-scope folder", store.ScopePersonal,
			[]FolderElection{{Folder: "music", Subfolder: "Jazz"}}},
		{"subfolder escapes the folder", store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Subfolder: "../etc"}}},
		{"absolute subfolder", store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Subfolder: "/etc"}}},
		{"election for an undeclared folder", store.ScopePersonal,
			[]FolderElection{{Folder: "photos", Source: sourcePersonal}}},
		{"duplicate election", store.ScopePersonal,
			[]FolderElection{{Folder: "movies"}, {Folder: "movies"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveElections(man, tc.scope, tc.elections)
			wantStatus(t, err, http.StatusUnprocessableEntity)
		})
	}
}
