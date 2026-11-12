package lifecycle

// Changing an app's folders after install (#659).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"

	"gopkg.in/yaml.v3"
)

func (e *testEnv) composeUps() int {
	n := 0
	for _, c := range e.docker.Calls() {
		if c.method == "ComposeUp" {
			n++
		}
	}
	return n
}

func overrideApp(t *testing.T, e *testEnv, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "instances", id, "compose.override.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Services["app"]
}

// A running app gets its new folders: the override binds them with the
// manifest's modes, the shared group comes and goes with a shared folder,
// .env names the folders it binds, the app is recreated, and the pins file
// pins the new source. InstanceFolders reads the choice back.
func TestSetFolders_RunningApp(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	owner := Owner{UserID: "u_alex", Username: "alex"}
	installFolders(t, e, store.ScopePersonal, owner, choiceManifest, []FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects"},
	})
	insts, _ := e.store.List()
	id := insts[0].ID
	home := filepath.Join(e.host.homeRoot, "alex")

	ups := e.composeUps()
	next := []FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects/site"},
		{Folder: "notes", Source: sourceShared, Path: "Notes"},
		{Source: sourcePersonal, Path: "Recipes", Extra: "Recipes", NewFolder: true},
	}
	before, changed, err := e.m.SetFolders(ctx, id, "", next, nil)
	if err != nil || !changed {
		t.Fatalf("SetFolders: changed=%v err=%v", changed, err)
	}
	if len(before) != 1 || before[0].Path != "Projects" {
		t.Errorf("before = %+v", before)
	}
	app := overrideApp(t, e, id)
	for _, v := range []string{
		home + "/Projects/site:/moose/projects:rw",
		filepath.Join(e.m.sharedRoot, "Notes") + ":/moose/notes:ro",
		home + "/Recipes:/moose/extra/Recipes:ro",
	} {
		if !hasString(app["volumes"], v) {
			t.Errorf("volumes: want %q, got %v", v, app["volumes"])
		}
	}
	if !hasString(app["group_add"], "2001") {
		t.Errorf("group_add: want the shared group 2001, got %v", app["group_add"])
	}
	if got := app["user"]; got != "3000:3000" {
		t.Errorf("user changed: %v", got)
	}
	env, _ := os.ReadFile(filepath.Join(e.stateDir, "instances", id, ".env"))
	for _, line := range []string{"MOOSE_FOLDER_PROJECTS=/moose/projects", "MOOSE_FOLDER_NOTES=/moose/notes", "MOOSE_FOLDER_EXTRA=/moose/extra"} {
		if !strings.Contains(string(env), line) {
			t.Errorf(".env has no %s:\n%s", line, env)
		}
	}
	if e.composeUps() != ups+1 {
		t.Errorf("compose up ran %d times, want once", e.composeUps()-ups)
	}
	pins, _ := os.ReadFile(filepath.Join(e.stateDir, "instances", id, folderPinRecord))
	if !strings.Contains(string(pins), "Projects/site") {
		t.Errorf("the pins record does not name the new folder:\n%s", pins)
	}
	got, err := e.m.InstanceFolders(ctx, id)
	if err != nil || !SameFolders(got, next) {
		t.Errorf("InstanceFolders = %+v, %v; want %+v", got, err, next)
	}

	// The same set again changes nothing and recreates nothing.
	ups = e.composeUps()
	if _, changed, err := e.m.SetFolders(ctx, id, "", got, nil); err != nil || changed {
		t.Errorf("same set: changed=%v err=%v", changed, err)
	}
	if e.composeUps() != ups {
		t.Error("a set with no change recreated the app")
	}

	// Dropping the shared folder drops the shared group.
	if _, _, err := e.m.SetFolders(ctx, id, "", []FolderMount{{Folder: "projects", Source: sourcePersonal, Path: "Projects"}}, nil); err != nil {
		t.Fatal(err)
	}
	app = overrideApp(t, e, id)
	if _, ok := app["group_add"]; ok {
		t.Errorf("group_add kept with no shared folder: %v", app["group_add"])
	}
	env, _ = os.ReadFile(filepath.Join(e.stateDir, "instances", id, ".env"))
	if strings.Contains(string(env), "MOOSE_FOLDER_NOTES") || strings.Contains(string(env), "MOOSE_FOLDER_EXTRA") {
		t.Errorf(".env still names a folder the app lost:\n%s", env)
	}
}

// A stopped app gets the new files and is not started.
func TestSetFolders_StoppedAppIsNotStarted(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	installFolders(t, e, store.ScopePersonal, Owner{UserID: "u_alex", Username: "alex"}, choiceManifest, []FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects"},
	})
	insts, _ := e.store.List()
	id := insts[0].ID
	if err := e.m.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	ups := e.composeUps()
	if _, changed, err := e.m.SetFolders(ctx, id, "", []FolderMount{{Folder: "projects", Source: sourcePersonal, Path: "Documents"}}, nil); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if e.composeUps() != ups {
		t.Error("a stopped app was started")
	}
	if !hasString(overrideApp(t, e, id)["volumes"], filepath.Join(e.host.homeRoot, "alex", "Documents")+":/moose/projects:rw") {
		t.Error("the override does not bind the new folder")
	}
}

// A refused pin at the recreate is the job's error in its plain words.
func TestSetFolders_PinRefusal(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	installFolders(t, e, store.ScopePersonal, Owner{UserID: "u_alex", Username: "alex"}, choiceManifest, []FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects"},
	})
	insts, _ := e.store.List()
	id := insts[0].ID
	home := filepath.Join(e.host.homeRoot, "alex")
	// The new folder is a link by the time the pin walks it: prepare made
	// it, then something swapped it.
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "Documents")); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.m.SetFolders(ctx, id, "", []FolderMount{{Folder: "projects", Source: sourcePersonal, Path: "Documents"}}, nil)
	var refusal *folderRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v; want a folder refusal", err)
	}
}

// A rollback by hand puts the previous generation's files back. A folder
// change made after the update must hold in them too, or the rollback would
// give the app back a folder the owner took away.
func TestSetFolders_SurvivesRollback(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	// The snapshot checks the real disk; this test is not about space.
	e.m.diskSpace = func(string) (uint64, uint64, error) { return 1 << 40, 2 << 40, nil }
	addUser(t, e, "u_alex", "alex")
	e.newVersionMigrates(t, true)
	e.publish(t, "1.0", upImage1, upDigest1, choiceUpFolders)
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, "upapp"), Owner{UserID: "u_alex", Username: "alex"}, store.ScopePersonal,
		[]FolderMount{
			{Folder: "documents", Source: sourcePersonal, Path: "Documents/Taxes"},
			{Folder: "notes", Source: sourcePersonal, Path: "Notes"},
		}, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	e.publish(t, "2.0", upImage2, upDigest2, choiceUpFolders)
	e.m.CheckUpdates(ctx)
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	// After the update the owner points documents elsewhere and drops notes.
	if _, _, err := e.m.SetFolders(ctx, inst.ID, "", []FolderMount{{Folder: "documents", Source: sourcePersonal, Path: "Documents/Work"}}, nil); err != nil {
		t.Fatalf("SetFolders: %v", err)
	}
	if err := e.m.Rollback(ctx, inst.ID, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	home := filepath.Join(e.host.homeRoot, "alex")
	want := []string{home + "/Documents/Work:/moose/documents:ro"}
	if got := e.overrideVolumes(t, inst.ID); !slices.Equal(got, want) {
		t.Fatalf("after the rollback the app binds %v, want %v", got, want)
	}
	env, _ := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, ".env"))
	if strings.Contains(string(env), "MOOSE_FOLDER_NOTES") {
		t.Errorf("the rollback brought back the notes variable:\n%s", env)
	}
}

// A choice checked against one version is not applied to another: an update
// that ran between the save and its job may need other folders.
func TestSetFolders_VersionChanged(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	installFolders(t, e, store.ScopePersonal, Owner{UserID: "u_alex", Username: "alex"}, choiceManifest, []FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects"},
	})
	insts, _ := e.store.List()
	id := insts[0].ID
	before := overrideApp(t, e, id)["volumes"]
	_, changed, err := e.m.SetFolders(ctx, id, "0.9", []FolderMount{{Folder: "projects", Source: sourcePersonal, Path: "Documents"}}, nil)
	if !errors.Is(err, ErrFoldersVersionChanged) || changed {
		t.Fatalf("changed=%v err=%v; want ErrFoldersVersionChanged", changed, err)
	}
	if got := overrideApp(t, e, id)["volumes"]; !slices.Equal(toStrings(got), toStrings(before)) {
		t.Errorf("the override changed: %v", got)
	}
}

// A save that cannot write every file puts back the ones it wrote, so the
// override and .env never disagree and a retry of the same choice still
// sees a change.
func TestSetFolders_FailedWriteRestores(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	addUser(t, e, "u_alex", "alex")
	installFolders(t, e, store.ScopePersonal, Owner{UserID: "u_alex", Username: "alex"}, choiceManifest, []FolderMount{
		{Folder: "projects", Source: sourcePersonal, Path: "Projects"},
	})
	insts, _ := e.store.List()
	id := insts[0].ID
	dir := filepath.Join(e.stateDir, "instances", id)
	ovBefore, _ := os.ReadFile(filepath.Join(dir, "compose.override.yml"))
	// .env cannot be written: a folder is in its place.
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o755); err != nil {
		t.Fatal(err)
	}
	ups := e.composeUps()
	next := []FolderMount{{Folder: "projects", Source: sourcePersonal, Path: "Documents"}}
	if _, changed, err := e.m.SetFolders(ctx, id, "", next, nil); !errors.Is(err, errFoldersNotWritten) || changed {
		t.Fatalf("changed=%v err=%v; want errFoldersNotWritten", changed, err)
	}
	if ovAfter, _ := os.ReadFile(filepath.Join(dir, "compose.override.yml")); string(ovAfter) != string(ovBefore) {
		t.Errorf("the override was not put back:\n%s", ovAfter)
	}
	if e.composeUps() != ups {
		t.Error("a failed save recreated the app")
	}
	got, err := e.m.InstanceFolders(ctx, id)
	if err != nil || SameFolders(got, next) {
		t.Errorf("a retry would see no change: %+v %v", got, err)
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}
