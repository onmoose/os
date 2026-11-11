package api

// resolveElections is the authoritative write-path validation of per-folder
// install elections (the install-plan endpoint is advisory). Pure function —
// tested directly against parsed manifests.

import (
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// jellyfinManifest parses the shared folders fixture: movies (write,
// pick-subfolder, default Family) + music (read, whole).
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

// resolve is resolveElections with no extra folders and no made folders.
func resolve(man *manifest.Manifest, scope string, elections []FolderElection) ([]lifecycle.FolderMount, error) {
	return resolveElections(man, scope, elections, nil, nil)
}

func TestResolveElections_DefaultsPersonal(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolve(man, store.ScopePersonal, nil)
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
	// The suggestion is the folder plus the manifest's default under it.
	if movies.Path != "Movies/Family" {
		t.Errorf("movies path: want the suggestion Movies/Family, got %q", movies.Path)
	}
	music := mountFor(t, mounts, "music")
	if music.Source != sourcePersonal || music.Path != "Music" {
		t.Errorf("music: want personal Music, got %+v", music)
	}
}

func TestResolveElections_DefaultsHousehold(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolve(man, store.ScopeHousehold, nil)
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
	mounts, err := resolve(man, store.ScopePersonal,
		[]FolderElection{{Folder: "movies", Source: sourceShared}})
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if got := mountFor(t, mounts, "movies").Source; got != sourceShared {
		t.Errorf("movies source: want shared, got %q", got)
	}
}

// The form from before #655: a subfolder under the folder. It no longer needs
// scope: pick-subfolder.
func TestResolveElections_SubfolderOverride(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolve(man, store.ScopePersonal,
		[]FolderElection{{Folder: "movies", Subfolder: "Kids"}, {Folder: "music", Subfolder: "Jazz"}})
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if got := mountFor(t, mounts, "movies").Path; got != "Movies/Kids" {
		t.Errorf("movies path: want Movies/Kids, got %q", got)
	}
	if got := mountFor(t, mounts, "music").Path; got != "Music/Jazz" {
		t.Errorf("music path: want Music/Jazz, got %q", got)
	}
}

// Since #655 the user may point a folder at any path inside the folder limit:
// another use-case folder, Projects, or a folder made through moose.
func TestResolveElections_ChosenPath(t *testing.T) {
	man := jellyfinManifest(t)
	made := map[string]bool{"Recipes": true}
	for _, path := range []string{"Documents/Films", "Documents/Taxes 2026", "Projects/site", "Projects/bin", "Recipes", "Recipes/Cakes", "Downloads"} {
		mounts, err := resolveElections(man, store.ScopePersonal, []FolderElection{{Folder: "movies", Path: path}}, nil, made)
		if err != nil {
			t.Fatalf("path %q: %v", path, err)
		}
		if got := mountFor(t, mounts, "movies"); got.Path != path || got.NewFolder {
			t.Errorf("path %q: got %+v", path, got)
		}
	}
}

// new: true makes a top-level folder, and the mount says so, so the brain
// stores its row once host-agent has made it.
func TestResolveElections_NewTopFolder(t *testing.T) {
	man := jellyfinManifest(t)
	mounts, err := resolve(man, store.ScopePersonal, []FolderElection{{Folder: "movies", Path: "Films/Kids", New: true}})
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if got := mountFor(t, mounts, "movies"); got.Path != "Films/Kids" || !got.NewFolder {
		t.Errorf("movies: want a new folder Films/Kids, got %+v", got)
	}
	// new under a use-case folder needs no row: that path is allowed anyway.
	mounts, err = resolve(man, store.ScopePersonal, []FolderElection{{Folder: "movies", Path: "Movies/Kids", New: true}})
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if mountFor(t, mounts, "movies").NewFolder {
		t.Error("a path under a use-case folder must not ask for a row")
	}
}

const choiceManifestYML = `id: terminal
manifest_version: 1
name: Terminal
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
permissions:
  folders:
    - folder: projects
      mode: write
    - folder: notes
      default: Obsidian
      optional: true
  extra_folders: read
`

func choiceManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	man, err := manifest.Parse([]byte(choiceManifestYML))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return man
}

// The "Done when" of #657 on the brain side: projects at a chosen path, an
// optional folder skipped, and extra folders named after their last part,
// with " 2" and " 3" on a clash.
func TestResolveElections_SkipAndExtraFolders(t *testing.T) {
	man := choiceManifest(t)
	mounts, err := resolveElections(man, store.ScopePersonal,
		[]FolderElection{{Folder: "projects", Path: "Projects/site"}, {Folder: "notes", Skip: true}},
		[]ExtraFolderElection{{Path: "Recipes", New: true}, {Path: "Documents/Recipes"}, {Path: "Photos/Recipes", Source: sourceShared}},
		nil)
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	want := []lifecycle.FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects/site"},
		{Source: sourcePersonal, Path: "Recipes", Extra: "Recipes", NewFolder: true},
		{Source: sourcePersonal, Path: "Documents/Recipes", Extra: "Recipes 2"},
		{Source: sourceShared, Path: "Photos/Recipes", Extra: "Recipes 3"},
	}
	if len(mounts) != len(want) {
		t.Fatalf("mounts = %+v, want %+v", mounts, want)
	}
	for i := range want {
		if mounts[i] != want[i] {
			t.Errorf("mount %d = %+v, want %+v", i, mounts[i], want[i])
		}
	}
	// Not skipped, the optional folder takes its suggestion.
	mounts, err = resolve(man, store.ScopePersonal, nil)
	if err != nil {
		t.Fatalf("resolveElections: %v", err)
	}
	if got := mountFor(t, mounts, "notes").Path; got != "Notes/Obsidian" {
		t.Errorf("notes path: want the suggestion Notes/Obsidian, got %q", got)
	}
}

// configLocation is the errors[0].location of a configError.
func configLocation(t *testing.T, err error) string {
	t.Helper()
	var se *huma.ErrorModel
	if !errors.As(err, &se) || len(se.Errors) == 0 {
		t.Fatalf("want a huma error with a location, got %T: %v", err, err)
	}
	return se.Errors[0].Location
}

func TestResolveElections_Rejections(t *testing.T) {
	jelly, choice := jellyfinManifest(t), choiceManifest(t)
	made := map[string]bool{"Recipes": true}
	cases := []struct {
		name      string
		man       *manifest.Manifest
		scope     string
		elections []FolderElection
		extras    []ExtraFolderElection
		location  string
	}{
		{"household cannot elect personal", jelly, store.ScopeHousehold,
			[]FolderElection{{Folder: "movies", Source: sourcePersonal}}, nil, "config.folders.movies"},
		{"invalid source value", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Source: "everyone"}}, nil, "config.folders.movies"},
		{"subfolder escapes the folder", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Subfolder: "../etc"}}, nil, "config.folders.movies"},
		{"absolute subfolder", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Subfolder: "/etc"}}, nil, "config.folders.movies"},
		{"election for an undeclared folder", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "photos", Source: sourcePersonal}}, nil, "config.folders.photos"},
		{"duplicate election", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies"}, {Folder: "movies"}}, nil, "config.folders.movies"},
		{"path and subfolder together", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "Movies/Kids", Subfolder: "Kids"}}, nil, "config.folders.movies"},
		// The folder limit (APP_ISOLATION.md # User content).
		{"the home itself", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "/"}}, nil, "config.folders.movies"},
		{"absolute path", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "/etc"}}, nil, "config.folders.movies"},
		{"dot folder", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: ".ssh"}}, nil, "config.folders.movies"},
		{"dot level deeper", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "Projects/app/.git"}}, nil, "config.folders.movies"},
		{"dot dot", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "Movies/../.ssh"}}, nil, "config.folders.movies"},
		{"empty level", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "Movies//Kids"}}, nil, "config.folders.movies"},
		{"colon", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "Movies/a:b"}}, nil, "config.folders.movies"},
		{"hand-made top folder", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "scripts"}}, nil, "config.folders.movies"},
		{"bin", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "bin"}}, nil, "config.folders.movies"},
		{"new bin", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "bin", New: true}}, nil, "config.folders.movies"},
		{"new Shared", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Path: "Shared", New: true}}, nil, "config.folders.movies"},
		{"shared source outside the shared folders", jelly, store.ScopePersonal,
			[]FolderElection{{Folder: "movies", Source: sourceShared, Path: "Recipes"}}, nil, "config.folders.movies"},
		{"new folder in the shared tree", jelly, store.ScopeHousehold,
			[]FolderElection{{Folder: "movies", Path: "Films", New: true}}, nil, "config.folders.movies"},
		{"skip a required folder", choice, store.ScopePersonal,
			[]FolderElection{{Folder: "projects", Skip: true}}, nil, "config.folders.projects"},
		{"extra folder on an app with no extra_folders", jelly, store.ScopePersonal,
			nil, []ExtraFolderElection{{Path: "Documents"}}, "config.extra_folders[0]"},
		{"extra folder with no path", choice, store.ScopePersonal,
			nil, []ExtraFolderElection{{Path: "Documents"}, {}}, "config.extra_folders[1]"},
		{"extra folder outside the limit", choice, store.ScopePersonal,
			nil, []ExtraFolderElection{{Path: ".config"}}, "config.extra_folders[0]"},
		{"extra folder twice", choice, store.ScopePersonal,
			nil, []ExtraFolderElection{{Path: "Recipes"}, {Path: "Recipes"}}, "config.extra_folders[1]"},
		{"extra personal folder on a household app", choice, store.ScopeHousehold,
			nil, []ExtraFolderElection{{Source: sourcePersonal, Path: "Documents"}}, "config.extra_folders[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveElections(tc.man, tc.scope, tc.elections, tc.extras, made)
			wantStatus(t, err, http.StatusUnprocessableEntity)
			if got := configLocation(t, err); got != tc.location {
				t.Errorf("location = %q, want %q", got, tc.location)
			}
		})
	}
}
