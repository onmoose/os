package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/onmoose/os/internal/admission"
)

// Door 2 (custom compose) is admin-only (APP_ISOLATION.md # Trust tiers,
// DECISIONS.md 2026-06-02). These tests lock the role gate on
// POST /api/v1/apps/custom and its failure audit, confirm the store door stays
// member-allowed, and lock that admission stays door-symmetric.
//
// The api harness builds the server with life=nil and admission.Check shells
// out to `docker compose config -q`, so a *successful* custom install can't run
// here (it would reach the job goroutine, or depend on docker). Each test
// instead asserts a synchronous, docker-free boundary: the guard rejects before
// synthesize/admission, an admin clears the guard and is stopped only at the
// pure-Go synthesize pre-check, and the shared admission policy rejects the
// forbidden primitives.

func TestInstallCustomMemberForbiddenAndAudits(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_m", "mara", "pw123456")
	h.loginAs("mara", "pw123456")

	resp := h.do("POST", "/api/v1/apps/custom", map[string]any{
		"name":      "my app",
		"compose":   "services:\n  web:\n    image: nginx:1.27\n",
		"main_port": 80,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member custom install = %d, want 403", resp.StatusCode)
	}

	ar := h.do("GET", "/api/v1/audit", nil)
	body := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, ar)
	found := false
	for _, e := range body.Events {
		if e.Action == "app.custom.create" && !e.Success {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("want an app.custom.create success=false audit row for the rejected member")
	}
}

func TestInstallCustomAdminPassesAdminGate(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")

	resp := h.do("POST", "/api/v1/apps/custom", map[string]any{
		"name":    "my app",
		"compose": "services:\n  web:\n    image: nginx:1.27\n",
		// main_port omitted (0) → Synthesize fails before admission/job.
	})
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("admin custom install got 403 — the admin gate must let admins through")
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("admin custom install = %d, want 422 (synthesize pre-check)", resp.StatusCode)
	}
}

func TestInstallStoreStillMemberAllowed(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_m", "mara", "pw123456")
	h.loginAs("mara", "pw123456")

	resp := h.do("POST", "/api/v1/apps", map[string]any{
		"manifest_id": "does-not-exist",
		"scope":       "personal",
	})
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("member store install got 403 — the store door must stay member-allowed")
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member store install (bogus manifest) = %d, want 404", resp.StatusCode)
	}
}

func TestCustomCatalogAdmissionDoorSymmetry(t *testing.T) {
	primitives := map[string]string{
		"privileged":    "services:\n  web:\n    image: nginx\n    privileged: true\n",
		"cap_add":       "services:\n  web:\n    image: nginx\n    cap_add: [SYS_ADMIN]\n",
		"host ports":    "services:\n  web:\n    image: nginx\n    ports: [\"8080:80\"]\n",
		"absolute bind": "services:\n  web:\n    image: nginx\n    volumes: [\"/etc/passwd:/etc/passwd:ro\"]\n",
	}
	for name, compose := range primitives {
		t.Run(name, func(t *testing.T) {
			if err := admission.CheckStructure(context.Background(), []byte(compose)); err == nil {
				t.Fatalf("admission accepted %s — both doors must reject it", name)
			}
		})
	}
}
