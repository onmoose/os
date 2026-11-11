package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

// POST /api/v1/apps refuses a folder path outside the folder limit with a 422
// that names the folder, both in errors[0].location and in the message, so the
// install flow can open the folder step on that row (#655, BRAIN_UI_PROTOCOL.md
// # Folder elections after #655).
func TestInstallFolderLimit422(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "terminal", choiceManifestYML)
	h.setupAdmin("alice", "pass1")

	cases := []struct {
		name   string
		config map[string]any
		want   string
		inText string
	}{
		{"dot folder", map[string]any{"folders": []map[string]any{{"folder": "projects", "path": ".ssh"}}},
			"config.folders.projects", "Projects folder"},
		{"hand-made top folder", map[string]any{"folders": []map[string]any{{"folder": "projects", "path": "scripts"}}},
			"config.folders.projects", "Projects folder"},
		{"required folder skipped", map[string]any{"folders": []map[string]any{{"folder": "projects", "skip": true}}},
			"config.folders.projects", "Projects folder"},
		{"extra folder outside the limit", map[string]any{"extra_folders": []map[string]any{{"path": "bin", "new": true}}},
			"config.extra_folders[0]", "added folder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := h.do("POST", "/api/v1/apps", map[string]any{"manifest_id": "terminal", "scope": "personal", "config": c.config})
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d; want 422", resp.StatusCode)
			}
			body := decodeJSON[struct {
				Errors []struct {
					Location string `json:"location"`
					Message  string `json:"message"`
				} `json:"errors"`
			}](t, resp)
			if len(body.Errors) == 0 || body.Errors[0].Location != c.want {
				t.Fatalf("errors = %+v; want location %q", body.Errors, c.want)
			}
			if !strings.Contains(body.Errors[0].Message, c.inText) {
				t.Fatalf("message %q; want it to contain %q", body.Errors[0].Message, c.inText)
			}
		})
	}
}

// The install plan carries optional and extra_folders (#655).
func TestInstallPlan_OptionalAndExtraFolders(t *testing.T) {
	plan := buildInstallPlan(choiceManifest(t), true)
	p := plan.Permissions
	if p.ExtraFolders != "read" {
		t.Errorf("extra_folders = %q, want read", p.ExtraFolders)
	}
	if len(p.Folders) != 2 || p.Folders[0].Optional || !p.Folders[1].Optional {
		t.Fatalf("folders = %+v; want projects required and notes optional", p.Folders)
	}
	if p.Folders[1].SubfolderDefault != "Obsidian" {
		t.Errorf("notes subfolder_default = %q, want Obsidian", p.Folders[1].SubfolderDefault)
	}
	if plan := buildInstallPlan(jellyfinManifest(t), true); plan.Permissions.ExtraFolders != "" {
		t.Errorf("an app with no extra_folders: got %q", plan.Permissions.ExtraFolders)
	}
}

// GET /api/v1/me/folders lists one level of the caller's own home. The top is
// filtered: the use-case folders (missing when not there) and the folders made
// through moose that are still there; a hand-made folder is not offered, and
// neither is a dot folder or a file (#658, BRAIN_UI_PROTOCOL.md # Folder
// chooser).
func TestListMyFolders(t *testing.T) {
	h := newHarness(t)
	u := h.setupAdmin("alice", "pass1")
	home := filepath.Join(h.homes, u.Username)
	for _, d := range []string{"Documents/Taxes", "Documents/.git", "Photos", "Recipes", "scripts", "Gone"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "Documents", "a.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Recipes", "Deleted"} {
		if err := h.st.AddMadeFolder(u.ID, n); err != nil {
			t.Fatal(err)
		}
	}

	resp := h.do("GET", "/api/v1/me/folders?source=personal", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("top: status %d", resp.StatusCode)
	}
	top := decodeJSON[FolderListDTO](t, resp)
	want := []FolderEntryDTO{
		{Name: "Documents"}, {Name: "Downloads", Missing: true}, {Name: "Movies", Missing: true},
		{Name: "Music", Missing: true}, {Name: "Notes", Missing: true}, {Name: "Photos"},
		{Name: "Projects", Missing: true}, {Name: "Recipes", MadeByMoose: true},
	}
	if !slices.Equal(top.Folders, want) {
		t.Errorf("top = %+v\nwant %+v", top.Folders, want)
	}

	resp = h.do("GET", "/api/v1/me/folders?source=personal&path=Documents", nil)
	if got := decodeJSON[FolderListDTO](t, resp); resp.StatusCode != http.StatusOK || !slices.Equal(got.Folders, []FolderEntryDTO{{Name: "Taxes"}}) {
		t.Errorf("Documents: status %d, %+v; want [Taxes]", resp.StatusCode, got.Folders)
	}

	for _, c := range []struct {
		path   string
		status int
	}{
		{"scripts", http.StatusUnprocessableEntity},
		{"Documents/.git", http.StatusUnprocessableEntity},
		{"Documents/../..", http.StatusUnprocessableEntity},
		{"Movies", http.StatusNotFound},
		{"Documents/a.txt", http.StatusConflict},
	} {
		resp := h.do("GET", "/api/v1/me/folders?source=personal&path="+url.QueryEscape(c.path), nil)
		if resp.StatusCode != c.status {
			t.Errorf("path %q: status %d, want %d", c.path, resp.StatusCode, c.status)
		}
		resp.Body.Close()
	}
}

// The shared top level lists the shared use-case folders even with no shared
// tree on disk (the inner loop): each is missing until an app is pointed at
// it.
func TestListMyFolders_SharedTop(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	resp := h.do("GET", "/api/v1/me/folders?source=shared", nil)
	got := decodeJSON[FolderListDTO](t, resp)
	if resp.StatusCode != http.StatusOK || len(got.Folders) != len(protocol.UseCaseFolderDirs) {
		t.Fatalf("shared top: status %d, %+v", resp.StatusCode, got.Folders)
	}
	resp = h.do("GET", "/api/v1/me/folders?source=shared&path=Recipes", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("shared non-use-case top: status %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()
}

// A new folder may not take a name that is already in the home (#658, item 4
// of the issue). The user made ~/scripts by hand, so new: true on it is a 422
// on that folder's row, and no made_folders row is written.
func TestInstallNewFolderOnHandMadeName422(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "terminal", choiceManifestYML)
	u := h.setupAdmin("alice", "pass1")
	if err := os.MkdirAll(filepath.Join(h.homes, u.Username, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		config map[string]any
		loc    string
	}{
		{map[string]any{"folders": []map[string]any{{"folder": "projects", "path": "scripts", "new": true}}}, "config.folders.projects"},
		{map[string]any{"extra_folders": []map[string]any{{"path": "scripts/bin", "new": true}}}, "config.extra_folders[0]"},
	} {
		resp := h.do("POST", "/api/v1/apps", map[string]any{"manifest_id": "terminal", "scope": "personal", "config": c.config})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d; want 422", resp.StatusCode)
		}
		body := decodeJSON[struct {
			Errors []struct {
				Location string `json:"location"`
				Message  string `json:"message"`
			} `json:"errors"`
		}](t, resp)
		if len(body.Errors) == 0 || body.Errors[0].Location != c.loc || !strings.Contains(body.Errors[0].Message, "already in your home") {
			t.Errorf("errors = %+v; want %s with 'already in your home'", body.Errors, c.loc)
		}
	}
	if got, err := h.st.MadeFolders(u.ID); err != nil || len(got) != 0 {
		t.Errorf("made folders = %v, %v; want none", got, err)
	}
}

// A home listing cut at 1000 names may leave out an allowed folder, so the
// filtered top takes every allowed name as there rather than marking it
// missing or dropping a folder made through moose (Greptile on #663).
func TestTopLevel_CutListing(t *testing.T) {
	got := topLevel([]string{"Documents"}, map[string]bool{"Recipes": true}, true)
	for _, f := range got {
		if f.Missing {
			t.Errorf("%s marked missing in a cut listing", f.Name)
		}
	}
	if !slices.Contains(got, FolderEntryDTO{Name: "Recipes", MadeByMoose: true}) {
		t.Errorf("Recipes dropped from a cut listing: %+v", got)
	}
	if got := topLevel([]string{"Documents"}, map[string]bool{"Recipes": true}, false); slices.Contains(got, FolderEntryDTO{Name: "Recipes", MadeByMoose: true}) {
		t.Errorf("Recipes offered although a full listing does not have it")
	}
}
