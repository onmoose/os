package api

// Per-app access-mode endpoint (#306). Driven over a real store + lifecycle
// Manager (reusing configServer's seeded instance); hosted is simulated by
// setting the profile + box-id on both the Server and the Manager. Stopped-state
// instances let SetExposure persist without touching the nil caddy driver.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/onmoose/moose/internal/audit"
	"github.com/onmoose/moose/internal/catalog"
	"github.com/onmoose/moose/internal/profile"
	"github.com/onmoose/moose/internal/store"
)

func hostedExposureServer(t *testing.T, owner, scope, state string) (*Server, string, string) {
	t.Helper()
	s, id, instDir := configServer(t, owner, scope, state, nil)
	s.profile = profile.Hosted
	s.boxID = "cindy-fox"
	// The success response re-DTOs the app, which enriches from the catalog; an
	// empty catalog is enough (the app simply has no icon), but a nil one panics.
	s.catalog = catalog.New(t.TempDir())
	s.life.SetEnvironment(profile.Hosted, "cindy-fox")
	return s, id, instDir
}

func putExposure(t *testing.T, s *Server, ctx context.Context, id, exposure string) (*struct{ Body InstanceDTO }, error) {
	t.Helper()
	return s.setAppExposure(ctx, &struct {
		ID   string `path:"id"`
		Body struct {
			Exposure string `json:"exposure" enum:"restricted,public"`
		}
	}{ID: id, Body: struct {
		Exposure string `json:"exposure" enum:"restricted,public"`
	}{Exposure: exposure}})
}

func auditedExposure(t *testing.T, s *Server, success bool) bool {
	t.Helper()
	rows, err := s.store.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, r := range rows {
		if r.Action == audit.ActionAppExposureSet && r.Success == success {
			return true
		}
	}
	return false
}

func TestSetAppExposure_Hosted_TogglesAndAudits(t *testing.T) {
	s, id, _ := hostedExposureServer(t, "u_owner", store.ScopePersonal, "stopped")

	out, err := putExposure(t, s, adminCtx("u_admin"), id, store.ExposureRestricted)
	if err != nil {
		t.Fatalf("put restricted: %v", err)
	}
	if out.Body.Exposure != store.ExposureRestricted {
		t.Errorf("DTO exposure = %q, want restricted", out.Body.Exposure)
	}
	if row, _ := s.store.Get(id); row.Exposure != store.ExposureRestricted {
		t.Errorf("stored exposure = %q, want restricted", row.Exposure)
	}
	if !auditedExposure(t, s, true) {
		t.Error("expected an app.exposure.set success audit")
	}

	out, err = putExposure(t, s, adminCtx("u_admin"), id, store.ExposurePublic)
	if err != nil {
		t.Fatalf("put public: %v", err)
	}
	if out.Body.Exposure != store.ExposurePublic {
		t.Errorf("DTO exposure = %q, want public", out.Body.Exposure)
	}
}

func TestSetAppExposure_Appliance_404(t *testing.T) {
	// configServer leaves the appliance profile; the endpoint must 404 there —
	// exposure is a hosted-only concept.
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "stopped", nil)
	_, err := putExposure(t, s, adminCtx("u_admin"), id, store.ExposureRestricted)
	if err == nil {
		t.Fatal("want 404 on the appliance")
	}
	assertStatus(t, err, http.StatusNotFound)
}

func TestSetAppExposure_InvalidValue_422(t *testing.T) {
	s, id, _ := hostedExposureServer(t, "u_owner", store.ScopePersonal, "stopped")
	_, err := putExposure(t, s, adminCtx("u_admin"), id, "bogus")
	if err == nil {
		t.Fatal("want 422 for an out-of-range exposure")
	}
	assertStatus(t, err, http.StatusUnprocessableEntity)
}

func TestSetAppExposure_NoAuth_401(t *testing.T) {
	s, id, _ := hostedExposureServer(t, "u_owner", store.ScopePersonal, "stopped")
	_, err := putExposure(t, s, context.Background(), id, store.ExposureRestricted)
	if err == nil {
		t.Fatal("want 401 for an unauthenticated request")
	}
	assertStatus(t, err, http.StatusUnauthorized)
}

func TestSetAppExposure_NonAdminHousehold_403(t *testing.T) {
	s, id, _ := hostedExposureServer(t, "u_owner", store.ScopeHousehold, "stopped")
	_, err := putExposure(t, s, memberCtx("u_owner"), id, store.ExposureRestricted)
	if err == nil {
		t.Fatal("want 403 for a member acting on a household app")
	}
	assertStatus(t, err, http.StatusForbidden)
}

func TestSetAppExposure_FailureAudits(t *testing.T) {
	// A running app whose manifest.yml is gone: SetExposure persists the column but
	// fails to rebuild the route, so the endpoint 500s and audits the failure.
	s, id, instDir := hostedExposureServer(t, "u_owner", store.ScopePersonal, "running")
	if err := os.Remove(filepath.Join(instDir, "manifest.yml")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	_, err := putExposure(t, s, adminCtx("u_admin"), id, store.ExposureRestricted)
	if err == nil {
		t.Fatal("want an error when the route rebuild fails")
	}
	assertStatus(t, err, http.StatusInternalServerError)
	if !auditedExposure(t, s, false) {
		t.Error("expected an app.exposure.set failure audit")
	}
}

// The detail response carries the manifest's declared public paths, so the
// dashboard can say that part of an "Only me" app is open (#415). It reads the
// INSTANCE's manifest copy — the same file the route builder reads — so the
// label cannot claim a narrower app than Caddy is serving.
func TestGetApp_CarriesDeclaredPublicPaths(t *testing.T) {
	s, id, instDir := hostedExposureServer(t, "u_owner", store.ScopePersonal, "running")
	man := `
id: cfgapp
manifest_version: 1
name: Cfg App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
access:
  public_paths: ["/v1/*"]
`
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(man), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	out, err := s.getApp(adminCtx("u_admin"), &struct {
		ID string `path:"id"`
	}{ID: id})
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got := out.Body.PublicPaths; len(got) != 1 || got[0] != "/v1/*" {
		t.Fatalf("public_paths = %v, want the manifest's declaration", got)
	}
}

// An app that declares nothing carries nothing, so the toggle keeps meaning
// exactly what it says for almost every app.
func TestGetApp_NoPublicPathsWhenUndeclared(t *testing.T) {
	s, id, _ := hostedExposureServer(t, "u_owner", store.ScopePersonal, "running")
	out, err := s.getApp(adminCtx("u_admin"), &struct {
		ID string `path:"id"`
	}{ID: id})
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if len(out.Body.PublicPaths) != 0 {
		t.Fatalf("public_paths = %v, want none", out.Body.PublicPaths)
	}
}

// The exposure toggle echoes the app so the dashboard can render without a
// refetch. That echo must carry the declared public paths too: exposure alone
// would render "Only me" for an app that is partly open.
func TestSetAppExposure_EchoCarriesPublicPaths(t *testing.T) {
	s, id, instDir := hostedExposureServer(t, "u_owner", store.ScopePersonal, "stopped")
	man := `
id: cfgapp
manifest_version: 1
name: Cfg App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
access:
  public_paths: ["/v1/*"]
`
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(man), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	out, err := putExposure(t, s, adminCtx("u_admin"), id, store.ExposureRestricted)
	if err != nil {
		t.Fatalf("set exposure: %v", err)
	}
	if got := out.Body.PublicPaths; len(got) != 1 || got[0] != "/v1/*" {
		t.Fatalf("exposure echo public_paths = %v, want the manifest's declaration", got)
	}
}
