package api

// The three flags of a terminal app (#660): personal_only and hosted_only on
// the install path and its plan, access.owner_only on the exposure toggle and
// the app's DTO.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

const terminalAppYML = `id: term
manifest_version: 1
name: Term
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
personal_only: true
hosted_only: true
access:
  owner_only: true
`

func hosted(s *Server) {
	s.profile = profile.Hosted
	s.boxID = "cindy-fox"
}

func installAudited(t *testing.T, st *store.Store) bool {
	t.Helper()
	rows, err := st.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, r := range rows {
		if r.Action == audit.ActionAppInstall && !r.Success {
			return true
		}
	}
	return false
}

func TestInstall_HostedOnlyOnAppliance(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "term", terminalAppYML)
	h.setupAdmin("alice", "pass1")

	plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/term/install-plan"))
	if plan.Unavailable != unavailableHostedOnly {
		t.Errorf("plan unavailable = %q, want %q", plan.Unavailable, unavailableHostedOnly)
	}
	resp := h.do("POST", "/api/v1/apps", map[string]any{"manifest_id": "term", "scope": "personal"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("install on the appliance: %d, want 409", resp.StatusCode)
	}
	if !installAudited(t, h.st) {
		t.Error("the refused install was not audited")
	}
}

// make dev-app's switch (MOOSE_DEV_ALLOW_HOSTED_ONLY) on the API side: the
// appliance plan offers the app and a personal install is accepted as a job.
// personal_only still refuses a household install. The lifecycle backstop has
// its own switch and its own test (lifecycle TestInstall_HostedOnlyDevSwitch).
func TestInstall_HostedOnlyDevSwitch(t *testing.T) {
	var srv *Server
	h := newHarness(t, func(s *Server) { srv = s })
	writeManifestFixture(t, h.catalogDir, "term", terminalAppYML)
	h.setupAdmin("alice", "pass1")
	srv.SetAllowHostedOnly(true)

	plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/term/install-plan"))
	if plan.Unavailable != "" {
		t.Errorf("plan unavailable = %q with the dev switch, want none", plan.Unavailable)
	}
	resp := h.do("POST", "/api/v1/apps", map[string]any{"manifest_id": "term", "scope": "household"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("household install with the dev switch: %d, want 409 (personal_only)", resp.StatusCode)
	}
	resp = h.do("POST", "/api/v1/apps", map[string]any{"manifest_id": "term", "scope": "personal"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("personal install with the dev switch: %d, want 202", resp.StatusCode)
	}
}

func TestInstall_PersonalOnlyHosted(t *testing.T) {
	// First-run setup is an appliance path, so the box turns hosted after it.
	var srv *Server
	h := newHarness(t, func(s *Server) { srv = s })
	writeManifestFixture(t, h.catalogDir, "term", terminalAppYML)
	h.setupAdmin("alice", "pass1")
	hosted(srv)

	plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/term/install-plan"))
	if plan.Unavailable != "" {
		t.Errorf("plan unavailable = %q on hosted, want none", plan.Unavailable)
	}
	if !slices.Equal(plan.ScopeOptions, []string{store.ScopePersonal}) || plan.ScopeDefault != store.ScopePersonal {
		t.Errorf("an admin's scope menu = %v (default %q), want personal only", plan.ScopeOptions, plan.ScopeDefault)
	}

	resp := h.do("POST", "/api/v1/apps", map[string]any{"manifest_id": "term", "scope": "household"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("household install: %d, want 409", resp.StatusCode)
	}
	if !installAudited(t, h.st) {
		t.Error("the refused install was not audited")
	}

	// No scope asked for: an admin's default is household, but this app is
	// personal, so the install is accepted as a job.
	resp = h.do("POST", "/api/v1/apps", map[string]any{"manifest_id": "term"})
	if resp.StatusCode != http.StatusAccepted {
		resp.Body.Close()
		t.Fatalf("install with no scope: %d, want 202", resp.StatusCode)
	}
	var job Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// This harness cannot run a whole install, so the job fails later on.
	// What matters is that it was not refused as a household install: the
	// scope the handler chose was personal.
	for i := 0; i < 200 && (job.Status == "running" || job.Status == ""); i++ {
		time.Sleep(20 * time.Millisecond)
		job = decodeJSON[Job](t, h.getOK("/api/v1/jobs/"+job.ID))
	}
	if job.Error != nil && strings.Contains(job.Error.Message, "not for the whole household") {
		t.Fatalf("the install with no scope ran as household: %s", job.Error.Message)
	}
}

func TestSetAppExposure_OwnerOnly409(t *testing.T) {
	s, id, instDir := hostedExposureServer(t, "u_owner", store.ScopePersonal, "stopped")
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(terminalAppYML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	_, err := putExposure(t, s, adminCtx("u_admin"), id, store.ExposurePublic)
	if err == nil {
		t.Fatal("making an owner_only app public was allowed")
	}
	assertStatus(t, err, http.StatusConflict)
	if !auditedExposure(t, s, false) {
		t.Error("the refused toggle was not audited")
	}

	out, err := putExposure(t, s, adminCtx("u_admin"), id, store.ExposureRestricted)
	if err != nil {
		t.Fatalf("set restricted: %v", err)
	}
	if !out.Body.ExposureLocked || out.Body.Exposure != store.ExposureRestricted {
		t.Fatalf("echo exposure %q locked %v, want restricted and locked", out.Body.Exposure, out.Body.ExposureLocked)
	}
}

// A row left public is reported as it is served: restricted and locked.
func TestWithPublicPaths_ReportsLock(t *testing.T) {
	s, id, instDir := hostedExposureServer(t, "u_owner", store.ScopePersonal, "stopped")
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(terminalAppYML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	dto := InstanceDTO{ID: id, Exposure: store.ExposurePublic}
	s.withPublicPaths(&dto)
	if !dto.ExposureLocked || dto.Exposure != store.ExposureRestricted {
		t.Fatalf("dto exposure %q locked %v, want restricted and locked", dto.Exposure, dto.ExposureLocked)
	}

	s.profile = profile.Appliance
	dto = InstanceDTO{ID: id, Exposure: store.ExposurePublic}
	s.withPublicPaths(&dto)
	if dto.ExposureLocked || dto.Exposure != store.ExposurePublic {
		t.Fatalf("appliance dto exposure %q locked %v, want public, not locked", dto.Exposure, dto.ExposureLocked)
	}
}
