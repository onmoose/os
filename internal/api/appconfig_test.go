package api

// User-supplied app config (APP_MANIFEST.md # D4): GET reads the form schema +
// state (secrets masked, never returned); PUT applies a partial update and
// audits success/failure. Resolver validation is unit-tested directly; the
// handlers are exercised over a real store + lifecycle Manager (like
// appsecrets_test) since a real read/restamp needs an installed instance dir.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/audit"
	"github.com/onmoose/moose/internal/catalog"
	"github.com/onmoose/moose/internal/events"
	"github.com/onmoose/moose/internal/lifecycle"
	"github.com/onmoose/moose/internal/manifest"
	"github.com/onmoose/moose/internal/store"
)

const configManifestYAML = `
id: cfgapp
manifest_version: 1
name: Cfg App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [cfgapp]
config:
  - app_env: OPENAI_API_KEY
    title: "OpenAI key"
    description: "From platform.openai.com."
    secret: true
    required: true
  - app_env: OPENAI_MODEL
    title: "Model"
    description: "Which model."
    type: enum
    options: ["gpt-4o", "gpt-4o-mini"]
    default: "gpt-4o-mini"
  - app_env: ENDPOINT
    title: "Endpoint"
    description: "Base URL."
`

const configOverrideYAML = `
services:
  app:
    image: x
    cap_drop: [ALL]
`

// parseConfigManifest parses the test manifest for the pure resolver tests.
func parseConfigManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	man, err := manifest.Parse([]byte(configManifestYAML))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return man
}

// --- pure resolvers ------------------------------------------------------

func TestResolveInstallConfig(t *testing.T) {
	man := parseConfigManifest(t)

	t.Run("happy: required set, optional omitted", func(t *testing.T) {
		out, err := resolveInstallConfig(man, map[string]string{"OPENAI_API_KEY": "sk-1", "OPENAI_MODEL": "gpt-4o"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// ENDPOINT left blank → not injected; 2 values resolved.
		if len(out) != 2 {
			t.Fatalf("resolved %d values, want 2: %+v", len(out), out)
		}
		bySecret := map[string]bool{}
		for _, c := range out {
			bySecret[c.AppEnv] = c.Secret
		}
		if !bySecret["OPENAI_API_KEY"] {
			t.Errorf("OPENAI_API_KEY should be secret")
		}
	})

	bad := map[string]map[string]string{
		"missing required": {"OPENAI_MODEL": "gpt-4o"},
		"required blank":   {"OPENAI_API_KEY": "", "OPENAI_MODEL": "gpt-4o"},
		"enum off-list":    {"OPENAI_API_KEY": "sk-1", "OPENAI_MODEL": "gpt-5"},
		"unknown app_env":  {"OPENAI_API_KEY": "sk-1", "NOPE": "x"},
	}
	for name, fields := range bad {
		t.Run("reject "+name, func(t *testing.T) {
			if _, err := resolveInstallConfig(man, fields); err == nil {
				t.Fatalf("resolveInstallConfig accepted invalid input %v", fields)
			}
		})
	}
}

func TestResolvePutConfig(t *testing.T) {
	man := parseConfigManifest(t)
	current := []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-old", Secret: true},
		{AppEnv: "OPENAI_MODEL", Value: "gpt-4o", Secret: false},
	}

	t.Run("partial keeps an unsent secret", func(t *testing.T) {
		// Only change the model; the required secret is satisfied by its stored
		// value and survives untouched.
		out, err := resolvePutConfig(man, current, map[string]string{"OPENAI_MODEL": "gpt-4o-mini"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		val := map[string]string{}
		for _, c := range out {
			val[c.AppEnv] = c.Value
		}
		if val["OPENAI_API_KEY"] != "sk-old" {
			t.Errorf("secret not preserved: %v", val)
		}
		if val["OPENAI_MODEL"] != "gpt-4o-mini" {
			t.Errorf("model not updated: %v", val)
		}
	})

	t.Run("clear an optional", func(t *testing.T) {
		cur := append(current, store.InstanceConfig{AppEnv: "ENDPOINT", Value: "https://x"})
		out, err := resolvePutConfig(man, cur, map[string]string{"ENDPOINT": ""})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		for _, c := range out {
			if c.AppEnv == "ENDPOINT" {
				t.Errorf("cleared ENDPOINT still present: %v", out)
			}
		}
	})

	t.Run("reject clearing a required", func(t *testing.T) {
		if _, err := resolvePutConfig(man, current, map[string]string{"OPENAI_API_KEY": ""}); err == nil {
			t.Fatal("clearing a required field was accepted")
		}
	})

	t.Run("reject enum off-list", func(t *testing.T) {
		if _, err := resolvePutConfig(man, current, map[string]string{"OPENAI_MODEL": "gpt-5"}); err == nil {
			t.Fatal("off-list enum was accepted")
		}
	})

	t.Run("reject unknown app_env", func(t *testing.T) {
		if _, err := resolvePutConfig(man, current, map[string]string{"NOPE": "x"}); err == nil {
			t.Fatal("unknown app_env was accepted")
		}
	})
}

// --- buildInstallPlan schema (no values) ---------------------------------

func TestBuildInstallPlanConfig(t *testing.T) {
	man := parseConfigManifest(t)
	plan := buildInstallPlan(man, true)
	if len(plan.Config) != 3 {
		t.Fatalf("plan.Config = %d fields, want 3", len(plan.Config))
	}
	// Schema carries the secret/required flags and enum options/default, never a value.
	var key, model InstallPlanConfigField
	for _, c := range plan.Config {
		switch c.AppEnv {
		case "OPENAI_API_KEY":
			key = c
		case "OPENAI_MODEL":
			model = c
		}
	}
	if !key.Secret || !key.Required {
		t.Errorf("OPENAI_API_KEY schema wrong: %+v", key)
	}
	if model.Type != "enum" || len(model.Options) != 2 || model.Default != "gpt-4o-mini" {
		t.Errorf("OPENAI_MODEL schema wrong: %+v", model)
	}

	// A config-less manifest omits the block entirely.
	plain, _ := manifest.Parse([]byte("id: x\nmanifest_version: 1\nname: X\nversion: \"1\"\ncompose_file: c.yml\nmain_service: s\nmain_port: 80\n"))
	if got := buildInstallPlan(plain, true); len(got.Config) != 0 {
		t.Errorf("config-less plan has %d fields, want 0", len(got.Config))
	}
}

// --- GET / PUT handlers --------------------------------------------------

// configServer builds a Server over a temp store + lifecycle Manager, seeds an
// installed cfgapp instance (owner, scope, state), its manifest, override, and
// the given stored config values.
func configServer(t *testing.T, ownerID, scope, state string, cfg []store.InstanceConfig) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	life := lifecycle.NewManager(st, catalog.New(t.TempDir()), nil, nil, nil, events.NewBus(), dir)

	// Seed the owner + an admin actor so the elevation-class audit insert (FK to
	// users) succeeds; without these the audit silently fails its FK constraint.
	for _, u := range []store.User{
		{ID: ownerID, Username: ownerID, Role: store.RoleMember},
		{ID: "u_admin", Username: "admin", Role: store.RoleAdmin},
	} {
		if err := st.CreateUser(u); err != nil {
			t.Fatalf("seed user %s: %v", u.ID, err)
		}
	}

	const id = "inst_cfg"
	if err := st.Create(store.Instance{
		ID: id, ManifestID: "cfgapp", Name: "Cfg App", Slug: "cfgapp",
		Version: "1.0", State: state, OwnerUserID: ownerID, Scope: scope,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	instDir := filepath.Join(dir, "instances", id)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatalf("mkdir instance dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(configManifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "compose.override.yml"), []byte(configOverrideYAML), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}
	if cfg != nil {
		if err := st.SetInstanceConfig(id, cfg); err != nil {
			t.Fatalf("seed config: %v", err)
		}
	}
	return &Server{store: st, life: life, auditor: audit.New(st), jobs: newJobs()}, id, instDir
}

func getConfig(t *testing.T, s *Server, ctx context.Context, id string) (AppConfigDTO, error) {
	t.Helper()
	out, err := s.getAppConfig(ctx, &struct {
		ID string `path:"id"`
	}{ID: id})
	if err != nil {
		return AppConfigDTO{}, err
	}
	return out.Body, nil
}

func TestGetAppConfig_MasksSecretsAndPrefillsDefaults(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-secret", Secret: true},
		{AppEnv: "ENDPOINT", Value: "https://x", Secret: false},
	})
	body, err := getConfig(t, s, memberCtx("u_owner"), id)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	byEnv := map[string]AppConfigFieldDTO{}
	for _, f := range body.Fields {
		byEnv[f.AppEnv] = f
	}
	// Secret: value never returned, but reported as set.
	if k := byEnv["OPENAI_API_KEY"]; k.Value != "" || !k.Set {
		t.Errorf("secret field leaked or unset: %+v", k)
	}
	// Non-secret never-set: pre-filled with the manifest default, set=false.
	if m := byEnv["OPENAI_MODEL"]; m.Value != "gpt-4o-mini" || m.Set {
		t.Errorf("unset non-secret should pre-fill default: %+v", m)
	}
	// Non-secret set: returns the stored value.
	if e := byEnv["ENDPOINT"]; e.Value != "https://x" || !e.Set {
		t.Errorf("set non-secret wrong: %+v", e)
	}
}

func TestGetAppConfig_OtherMember_404(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	if _, err := getConfig(t, s, memberCtx("u_intruder"), id); err == nil {
		t.Fatal("want error for another member's personal app")
	} else {
		assertStatus(t, err, http.StatusNotFound)
	}
}

func TestGetAppConfig_NoIdentity_401(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	if _, err := getConfig(t, s, context.Background(), id); err == nil {
		t.Fatal("want 401 for unauthenticated request")
	} else {
		assertStatus(t, err, http.StatusUnauthorized)
	}
}

func awaitJob(t *testing.T, s *Server, jobID string) Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := s.jobs.get(jobID)
		if !ok {
			t.Fatalf("job %s not found", jobID)
		}
		if j.snapshot().Status != "running" {
			return j.snapshot()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", jobID)
	return Job{}
}

func putConfig(t *testing.T, s *Server, ctx context.Context, id string, fields map[string]string) (*struct{ Body Job }, error) {
	t.Helper()
	return s.updateAppConfig(ctx, &struct {
		ID   string `path:"id"`
		Body struct {
			Fields map[string]string `json:"fields"`
		}
	}{ID: id, Body: struct {
		Fields map[string]string `json:"fields"`
	}{Fields: fields}})
}

// auditedConfigUpdate reports whether an app.config.update event with the given
// success flag exists in the store.
func auditedConfigUpdate(t *testing.T, s *Server, success bool) bool {
	t.Helper()
	rows, err := s.store.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, r := range rows {
		if r.Action == audit.ActionAppConfigUpdate && r.Success == success {
			return true
		}
	}
	return false
}

func TestUpdateAppConfig_Success_RestampsAndAudits(t *testing.T) {
	// Stopped so SetConfig skips ComposeUp (no docker driver in this server).
	s, id, instDir := configServer(t, "u_owner", store.ScopePersonal, "stopped", []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-old", Secret: true},
		{AppEnv: "OPENAI_MODEL", Value: "gpt-4o", Secret: false},
	})
	out, err := putConfig(t, s, adminCtx("u_admin"), id, map[string]string{"OPENAI_MODEL": "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	job := awaitJob(t, s, out.Body.ID)
	if job.Status != "completed" {
		t.Fatalf("job status = %q, want completed (err=%v)", job.Status, job.Error)
	}
	// The store reflects the update; the required secret survived.
	stored, _ := s.store.GetInstanceConfig(id)
	val := map[string]string{}
	for _, c := range stored {
		val[c.AppEnv] = c.Value
	}
	if val["OPENAI_MODEL"] != "gpt-4o-mini" || val["OPENAI_API_KEY"] != "sk-old" {
		t.Fatalf("store not updated correctly: %v", val)
	}
	// The override env block was restamped.
	raw, _ := os.ReadFile(filepath.Join(instDir, "compose.override.yml"))
	if !containsAll(string(raw), "OPENAI_MODEL", "gpt-4o-mini", "OPENAI_API_KEY") {
		t.Fatalf("override not restamped:\n%s", raw)
	}
	if !auditedConfigUpdate(t, s, true) {
		t.Errorf("success was not audited")
	}
}

func TestUpdateAppConfig_RejectClearRequired_Audits422(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-old", Secret: true},
	})
	if _, err := putConfig(t, s, adminCtx("u_admin"), id, map[string]string{"OPENAI_API_KEY": ""}); err == nil {
		t.Fatal("clearing a required field was accepted")
	} else {
		assertStatus(t, err, http.StatusUnprocessableEntity)
	}
	if !auditedConfigUpdate(t, s, false) {
		t.Errorf("rejected update was not audited as failure")
	}
}

func TestUpdateAppConfig_NoIdentity_401(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "stopped", nil)
	if _, err := putConfig(t, s, context.Background(), id, map[string]string{"OPENAI_API_KEY": "sk-x"}); err == nil {
		t.Fatal("want 401 for unauthenticated request")
	} else {
		assertStatus(t, err, http.StatusUnauthorized)
	}
}

func TestUpdateAppConfig_ManifestLoadFails_Audits500(t *testing.T) {
	s, id, instDir := configServer(t, "u_owner", store.ScopePersonal, "stopped", nil)
	// Drop the manifest so InstanceManifest fails before any value is resolved;
	// the 500 path must still audit the rejected mutation as a failure.
	if err := os.Remove(filepath.Join(instDir, "manifest.yml")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	if _, err := putConfig(t, s, adminCtx("u_admin"), id, map[string]string{"OPENAI_API_KEY": "sk-x"}); err == nil {
		t.Fatal("want 500 when the manifest can't be loaded")
	} else {
		assertStatus(t, err, http.StatusInternalServerError)
	}
	if !auditedConfigUpdate(t, s, false) {
		t.Errorf("manifest-load failure was not audited")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
