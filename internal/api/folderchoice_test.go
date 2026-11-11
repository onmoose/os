package api

import (
	"net/http"
	"strings"
	"testing"
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
