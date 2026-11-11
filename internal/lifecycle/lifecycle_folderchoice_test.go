package lifecycle

// Folders the user chooses (#655, APP_MANIFEST.md # folders): a chosen path,
// the projects folder, a skipped optional folder, extra folders at
// /moose/extra/<Name>, the row for a folder made through moose, and what an
// update does with each of them.

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"
)

const choiceManifest = `
id: filesapp
manifest_version: 1
name: Files App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [filesapp]
permissions:
  folders:
    - folder: projects
      mode: write
    - folder: notes
      optional: true
  extra_folders: read
`

func addUser(t *testing.T, e *testEnv, id, name string) {
	t.Helper()
	if err := e.store.CreateUser(store.User{ID: id, Username: name, DisplayName: name, Role: store.RoleMember}); err != nil {
		t.Fatalf("create user: %v", err)
	}
}

// The "Done when" of #657: an app with folder: projects, an optional folder
// and extra_folders installs with a chosen path, and the container sees
// /moose/projects and /moose/extra/<Name>.
func TestInstallFolders_ChosenPathsOptionalAndExtras(t *testing.T) {
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	owner := Owner{UserID: "u_alex", Username: "alex"}
	app, env := installFolders(t, e, store.ScopePersonal, owner, choiceManifest, []FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects/site"},
		// notes was skipped: no mount.
		{Source: sourcePersonal, Path: "Recipes", Extra: "Recipes", NewFolder: true},
		{Source: sourcePersonal, Path: "Documents/Recipes", Extra: "Recipes 2"},
	})

	home := filepath.Join(e.host.homeRoot, "alex")
	want := []string{
		home + "/Projects/site:/moose/projects:rw",
		home + "/Recipes:/moose/extra/Recipes:ro",
		home + "/Documents/Recipes:/moose/extra/Recipes 2:ro",
	}
	for _, v := range want {
		if !hasString(app["volumes"], v) {
			t.Errorf("volumes: want %q, got %v", v, app["volumes"])
		}
	}
	if got := app["user"]; got != "3000:3000" {
		t.Errorf("user: want the owner 3000:3000, got %v", got)
	}
	for _, line := range []string{"MOOSE_FOLDER_PROJECTS=/moose/projects", "MOOSE_FOLDER_EXTRA=/moose/extra"} {
		if !strings.Contains(env, line) {
			t.Errorf("env missing %s:\n%s", line, env)
		}
	}
	if strings.Contains(env, "MOOSE_FOLDER_NOTES") {
		t.Errorf("a skipped folder must not set its variable:\n%s", env)
	}
	var prepared []string
	for _, c := range e.host.callsTo("PrepareUserFolder") {
		prepared = append(prepared, c.args[1].(string))
	}
	if wantPrep := []string{"Projects/site", "Recipes", "Documents/Recipes"}; !slices.Equal(prepared, wantPrep) {
		t.Errorf("PrepareUserFolder paths = %v, want %v", prepared, wantPrep)
	}
	// Recipes was made through moose, so it has a row; the rest do not.
	if got, err := e.store.MadeFolders("u_alex"); err != nil || !slices.Equal(got, []string{"Recipes"}) {
		t.Errorf("made folders = %v, %v; want [Recipes]", got, err)
	}
}

// An app that declares only extra_folders is a folder app even before the
// user adds a folder: it runs as the owner and its tier is host.
func TestInstallFolders_ExtraFoldersOnlyIsAFolderApp(t *testing.T) {
	e := newTestEnv(t)
	man := strings.Replace(choiceManifest, "  folders:\n    - folder: projects\n      mode: write\n    - folder: notes\n      optional: true\n", "", 1)
	app, env := installFolders(t, e, store.ScopePersonal, Owner{UserID: "u_alex", Username: "alex"}, man, nil)
	if got := app["user"]; got != "3000:3000" {
		t.Errorf("user: want the owner 3000:3000, got %v", got)
	}
	if _, ok := app["volumes"]; ok {
		t.Errorf("volumes: want none, got %v", app["volumes"])
	}
	if strings.Contains(env, "MOOSE_FOLDER_EXTRA") {
		t.Errorf("MOOSE_FOLDER_EXTRA set with no extra folder:\n%s", env)
	}
}

const choiceUpFolders = "  folders:\n    - folder: documents\n      mode: read\n    - folder: notes\n      optional: true\n  extra_folders: read\n"

// An update keeps every chosen folder and extra folder where it is, leaves a
// new optional folder off, and keeps a skipped folder skipped while it stays
// optional (UPDATES.md # What an allowed update turns on, #655).
func TestUpdateKeepsChosenFoldersAndLeavesOptionalOff(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	e.newVersionMigrates(t, true)
	e.publish(t, "1.0", upImage1, upDigest1, choiceUpFolders)
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, "upapp"), Owner{UserID: "u_alex", Username: "alex"}, store.ScopePersonal,
		[]FolderMount{
			{Folder: "documents", Source: sourcePersonal, Path: "Documents/Taxes"},
			{Source: sourcePersonal, Path: "Recipes", Extra: "Recipes", NewFolder: true},
		}, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	home := filepath.Join(e.host.homeRoot, "alex")
	before := []string{home + "/Documents/Taxes:/moose/documents:ro", home + "/Recipes:/moose/extra/Recipes:ro"}
	if got := e.overrideVolumes(t, inst.ID); !slices.Equal(got, before) {
		t.Fatalf("1.0 binds %v, want %v", got, before)
	}

	// 2.0 adds an optional music folder. Nothing in it asks for more.
	v2 := strings.Replace(choiceUpFolders, "  extra_folders: read\n", "    - folder: music\n      optional: true\n  extra_folders: read\n", 1)
	e.publish(t, "2.0", upImage2, upDigest2, v2)
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable || len(r.Update.Wants) != 0 {
		t.Fatalf("check = %+v; want available with no wants", r.Update)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := e.overrideVolumes(t, inst.ID); !slices.Equal(got, before) {
		t.Fatalf("2.0 binds %v, want the same as 1.0: %v", got, before)
	}
}

// A folder the user skipped that the new version makes required waits for
// the owner's yes, and the yes binds its suggestion.
func TestUpdateSkippedFolderBecomesRequired(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	e.newVersionMigrates(t, true)
	e.publish(t, "1.0", upImage1, upDigest1, choiceUpFolders)
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, "upapp"), Owner{UserID: "u_alex", Username: "alex"}, store.ScopePersonal,
		[]FolderMount{{Folder: "documents", Source: sourcePersonal, Path: "Documents"}}, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	v2 := strings.Replace(choiceUpFolders, "    - folder: notes\n      optional: true\n", "    - folder: notes\n      default: Obsidian\n", 1)
	e.publish(t, "2.0", upImage2, upDigest2, v2)
	e.m.CheckUpdates(ctx)
	r := e.row(t, inst.ID)
	want := []store.AccessWant{{Kind: store.WantFolder, Folder: "notes", Mode: "read", Change: store.FolderNew, Source: sourcePersonal}}
	if r.Update.Status != store.UpdateNeedsApproval || !slices.Equal(r.Update.Wants, want) {
		t.Fatalf("check = %+v; want needs_approval for notes", r.Update)
	}
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_alex"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	home := filepath.Join(e.host.homeRoot, "alex")
	got := e.overrideVolumes(t, inst.ID)
	wantVols := []string{home + "/Documents:/moose/documents:ro", home + "/Notes/Obsidian:/moose/notes:ro"}
	if !slices.Equal(got, wantVols) {
		t.Fatalf("2.0 binds %v, want %v", got, wantVols)
	}
}
