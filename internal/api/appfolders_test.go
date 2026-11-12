package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/store"
)

// seedFolderApp writes an installed, stopped Terminal app (choiceManifestYML)
// owned by ownerID: its row, manifest, an override that binds projects at
// <home>/<projects> and leaves the optional notes folder off, and a .env. The
// harness has no Docker, so a stopped app is what lets the job run to the end.
func (h *harness) seedFolderApp(id, ownerID, username, scope, projects string) {
	h.t.Helper()
	if err := h.st.Create(store.Instance{
		ID: id, ManifestID: "terminal", Name: "Terminal", Slug: id, Version: "1.0", State: "stopped",
		OwnerUserID: ownerID, Scope: scope, CreatedAt: time.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
	h.seedInstanceManifest(id, choiceManifestYML)
	dir := filepath.Join(h.stateDir, "instances", id)
	src := filepath.Join(h.homes, username, filepath.FromSlash(projects))
	if scope == store.ScopeHousehold {
		src = filepath.Join(h.apiSrv.life.SharedRoot(), filepath.FromSlash(projects))
	}
	ov := "services:\n  app:\n    user: \"1000:1000\"\n    volumes:\n      - " + src + ":/moose/projects:rw\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.override.yml"), []byte(ov), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MOOSE_FOLDER_PROJECTS=/moose/projects\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) envOf(id string) string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.stateDir, "instances", id, ".env"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

// The Done when of #659 on the brain side: GET shows where each folder
// points, a PUT points projects somewhere else, turns the optional notes
// folder on and adds a new extra folder, the job rewrites the override and
// .env, records the new top folder, and the change is audited with the old
// and new places. The same body again is 204 with no job and no record.
func TestAppFoldersChange(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "terminal", choiceManifestYML)
	u := h.setupAdmin("alice", "pass1")
	h.seedFolderApp("i_term", u.ID, u.Username, store.ScopePersonal, "Projects")

	resp := h.do("GET", "/api/v1/apps/i_term/folders", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: status %d", resp.StatusCode)
	}
	got := decodeJSON[AppFoldersDTO](t, resp)
	if !got.CanChange || got.ExtraFoldersMode != "read" || len(got.Folders) != 2 || len(got.ExtraFolders) != 0 {
		t.Fatalf("GET = %+v", got)
	}
	if f := got.Folders[0]; f.Folder != "projects" || f.Source != "personal" || f.Path != "Projects" || f.Skipped {
		t.Errorf("projects = %+v", f)
	}
	if f := got.Folders[1]; f.Folder != "notes" || !f.Skipped || !f.Optional || f.SubfolderDefault != "Obsidian" {
		t.Errorf("notes = %+v", f)
	}

	body := map[string]any{
		"folders": []map[string]any{
			{"folder": "projects", "source": "personal", "path": "Documents/Code"},
			{"folder": "notes", "source": "personal", "path": "Notes/Obsidian"},
		},
		"extra_folders": []map[string]any{{"source": "personal", "path": "Recipes", "new": true}},
	}
	resp = h.do("PUT", "/api/v1/apps/i_term/folders", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PUT: status %d", resp.StatusCode)
	}
	job := decodeJSON[Job](t, resp)
	if job.Kind != "app-folders" {
		t.Errorf("job kind = %q", job.Kind)
	}
	if status, jerr := waitJob(t, h.apiSrv, job.ID); status != "completed" {
		t.Fatalf("job = %s %+v", status, jerr)
	}

	home := filepath.Join(h.homes, u.Username)
	ov := h.overrideOf("i_term")
	for _, want := range []string{
		home + "/Documents/Code:/moose/projects:rw",
		home + "/Notes/Obsidian:/moose/notes:ro",
		home + "/Recipes:/moose/extra/Recipes:ro",
	} {
		if !strings.Contains(ov, want) {
			t.Errorf("override has no %q:\n%s", want, ov)
		}
	}
	if strings.Contains(ov, home+"/Projects:") {
		t.Errorf("override still binds the old folder:\n%s", ov)
	}
	env := h.envOf("i_term")
	for _, want := range []string{"MOOSE_FOLDER_PROJECTS=/moose/projects", "MOOSE_FOLDER_NOTES=/moose/notes", "MOOSE_FOLDER_EXTRA=/moose/extra"} {
		if !strings.Contains(env, want) {
			t.Errorf(".env has no %q:\n%s", want, env)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "Recipes")); err != nil {
		t.Errorf("the new folder was not made: %v", err)
	}
	if made, _ := h.st.MadeFolders(u.ID); len(made) != 1 || made[0] != "Recipes" {
		t.Errorf("made_folders = %v, want [Recipes]", made)
	}
	recs := auditsOf(t, h.apiSrv, "app.folders_change")
	if len(recs) != 1 || !recs[0].Success || recs[0].TargetID != "i_term" {
		t.Fatalf("audit = %+v", recs)
	}
	for _, want := range []string{`"old":"personal:Projects"`, `"new":"personal:Documents/Code"`, `"folder":"extra/Recipes"`} {
		if !strings.Contains(recs[0].Metadata, want) {
			t.Errorf("audit metadata %s has no %s", recs[0].Metadata, want)
		}
	}

	// The same choice again changes nothing. Recipes now has a row, so it
	// is sent without new, as the dashboard does after a save.
	body["extra_folders"] = []map[string]any{{"source": "personal", "path": "Recipes"}}
	resp = h.do("PUT", "/api/v1/apps/i_term/folders", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("same PUT: status %d, want 204", resp.StatusCode)
	}
	if n := len(auditsOf(t, h.apiSrv, "app.folders_change")); n != 1 {
		t.Errorf("a no-op PUT wrote a record: %d records", n)
	}
}

// PUT has the gate of an update's yes: an admin may see another person's
// personal app's folders but not change them (403, audited), a member on a
// household app is refused too, and a member on another person's app sees
// nothing (404).
func TestAppFoldersGate(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "terminal", choiceManifestYML)
	admin := h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "pass2")
	h.addMember("u_carol", "carol", "pass3")
	h.seedFolderApp("i_bob", "u_bob", "bob", store.ScopePersonal, "Projects")
	h.seedFolderApp("i_house", admin.ID, admin.Username, store.ScopeHousehold, "Projects")
	put := map[string]any{"folders": []map[string]any{{"folder": "projects", "path": "Documents"}}}

	resp := h.do("GET", "/api/v1/apps/i_bob/folders", nil)
	if got := decodeJSON[AppFoldersDTO](t, resp); resp.StatusCode != http.StatusOK || got.CanChange {
		t.Fatalf("admin GET on bob's app: %d can_change=%v; want 200 false", resp.StatusCode, got.CanChange)
	}
	resp = h.do("PUT", "/api/v1/apps/i_bob/folders", put)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin PUT on bob's app: %d, want 403", resp.StatusCode)
	}
	if recs := auditsOf(t, h.apiSrv, "app.folders_change"); len(recs) != 1 || recs[0].Success {
		t.Fatalf("the 403 was not audited as a failure: %+v", recs)
	}

	h.loginAs("bob", "pass2")
	resp = h.do("PUT", "/api/v1/apps/i_house/folders", map[string]any{"folders": []map[string]any{{"folder": "projects", "source": "shared", "path": "Documents"}}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member PUT on a household app: %d, want 403", resp.StatusCode)
	}
	h.loginAs("carol", "pass3")
	resp = h.do("PUT", "/api/v1/apps/i_bob/folders", put)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member PUT on another person's app: %d, want 404", resp.StatusCode)
	}
}

// An app with no folders gets 409, audited: its tier is fixed for life. A
// choice outside the folder limit is a 422 on that folder's row, not
// audited (a validation answer), and changes nothing.
func TestAppFoldersRefusals(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "terminal", choiceManifestYML)
	u := h.setupAdmin("alice", "pass1")
	h.seedInstance("i_plain", "plain", "plain", u.ID, store.ScopePersonal)
	h.seedInstanceManifest("i_plain", "id: plain\nmanifest_version: 1\nname: Plain\nversion: \"1\"\ncompose_file: compose.yml\nmain_service: app\nmain_port: 80\n")
	resp := h.do("PUT", "/api/v1/apps/i_plain/folders", map[string]any{"folders": []map[string]any{}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("folderless app: %d, want 409", resp.StatusCode)
	}
	resp = h.do("GET", "/api/v1/apps/i_plain/folders", nil)
	if got := decodeJSON[AppFoldersDTO](t, resp); resp.StatusCode != http.StatusOK || len(got.Folders) != 0 || got.CanChange {
		t.Fatalf("folderless GET: %d %+v; want 200 and no folders", resp.StatusCode, got)
	}

	h.seedFolderApp("i_term", u.ID, u.Username, store.ScopePersonal, "Projects")
	before := h.overrideOf("i_term")
	for _, c := range []struct {
		name, location string
		folders        []map[string]any
	}{
		{"skip a required folder", "config.folders.projects", []map[string]any{{"folder": "projects", "skip": true}}},
		{"a dot folder", "config.folders.projects", []map[string]any{{"folder": "projects", "path": "Projects/.ssh"}}},
	} {
		resp := h.do("PUT", "/api/v1/apps/i_term/folders", map[string]any{"folders": c.folders})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422", c.name, resp.StatusCode)
		}
		body := decodeJSON[struct {
			Errors []struct {
				Location string `json:"location"`
			} `json:"errors"`
		}](t, resp)
		if len(body.Errors) == 0 || body.Errors[0].Location != c.location {
			t.Errorf("%s: errors = %+v, want %s", c.name, body.Errors, c.location)
		}
	}
	if h.overrideOf("i_term") != before {
		t.Error("a refused PUT changed the override")
	}
	if recs := auditsOf(t, h.apiSrv, "app.folders_change"); len(recs) != 1 || recs[0].Success {
		t.Errorf("want one failure record (the 409), got %+v", recs)
	}
}

// A folder the app binds now may be kept even when its top folder is not one
// the rules allow today (a hand-made folder chosen before the rule): it is
// the user's choice already, and the PUT changes another folder.
func TestAppFoldersKeepsCurrentTop(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "terminal", choiceManifestYML)
	u := h.setupAdmin("alice", "pass1")
	h.seedFolderApp("i_term", u.ID, u.Username, store.ScopePersonal, "scripts/work")
	resp := h.do("PUT", "/api/v1/apps/i_term/folders", map[string]any{"folders": []map[string]any{
		{"folder": "projects", "path": "scripts/work"},
		{"folder": "notes", "path": "Notes"},
	}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	job := decodeJSON[Job](t, resp)
	if status, jerr := waitJob(t, h.apiSrv, job.ID); status != "completed" {
		t.Fatalf("job = %s %+v", status, jerr)
	}
}

// keepExtraNames keeps the name an extra folder has, so removing one folder
// does not rename the other under the app, and a new folder takes a free name.
func TestKeepExtraNames(t *testing.T) {
	current := []lifecycle.FolderMount{
		{Source: "personal", Path: "a/Recipes", Extra: "Recipes"},
		{Source: "personal", Path: "b/Recipes", Extra: "Recipes 2"},
	}
	mounts := []lifecycle.FolderMount{
		{Folder: "projects", Source: "personal", Path: "Projects"},
		{Source: "personal", Path: "b/Recipes", Extra: "Recipes"},
		{Source: "personal", Path: "c/Recipes", Extra: "Recipes 2"},
	}
	keepExtraNames(current, mounts)
	if mounts[1].Extra != "Recipes 2" || mounts[2].Extra != "Recipes" || mounts[0].Extra != "" {
		t.Errorf("names = %q %q %q; want \"\" \"Recipes 2\" \"Recipes\"", mounts[0].Extra, mounts[1].Extra, mounts[2].Extra)
	}
}
