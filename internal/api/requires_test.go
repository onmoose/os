package api

// Roles and requires on config fields (INSTALL_SETUP.md # 1 and # 3): the
// install is refused while a requires group has no filled member, an edit may
// not make a met group unmet, and the install plan and config DTO expose the
// fillable roles, the separator and the effective groups.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// rolesManifestYML is an openclaw-like app: two native key slots, a custom
// OpenAI-compatible slot with a model list, and a pair of plain fields that
// need one of the two.
const rolesManifestYML = `
id: cfgapp
manifest_version: 1
name: Claw Demo
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [cfgapp]
config:
  - app_env: ANTHROPIC_API_KEY
    title: "Anthropic key"
    description: "d"
    secret: true
    role: ai.anthropic.api_key
  - app_env: OPENAI_API_KEY
    title: "OpenAI key"
    description: "d"
    secret: true
    role: ai.openai.api_key
  - app_env: CUSTOM_BASE_URL
    title: "Custom base URL"
    description: "d"
    role: ai.openai_compatible.base_url
  - app_env: CUSTOM_MODELS
    title: "Custom models"
    description: "d"
    role: ai.openai_compatible.models.chat
    separator: ";"
  - app_env: BROKEN
    title: "Broken role"
    description: "d"
    role: ai.openai_compatible.token
  - app_env: SEARCH_KEY
    title: "Search key"
    description: "d"
  - app_env: SEARCH_URL
    title: "Search server"
    description: "d"
requires:
  - one_of: [ai]
  - one_of: [SEARCH_KEY, SEARCH_URL]
  - one_of: [NOT_A_FIELD]
`

func parseRolesManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	man, err := manifest.Parse([]byte(rolesManifestYML))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return man
}

func assert422(t *testing.T, err error, msg string) {
	t.Helper()
	assertStatus(t, err, http.StatusUnprocessableEntity)
	if !strings.Contains(err.Error(), msg) {
		t.Fatalf("422 message = %q; want it to contain %q", err.Error(), msg)
	}
}

func TestResolveInstallConfig_Requires(t *testing.T) {
	man := parseRolesManifest(t)

	t.Run("no AI provider", func(t *testing.T) {
		_, err := resolveInstallConfig(man, map[string]string{"SEARCH_KEY": "k"})
		assert422(t, err, "config.fields: pick at least one AI provider")
	})
	t.Run("a model list alone is not a provider", func(t *testing.T) {
		_, err := resolveInstallConfig(man, map[string]string{"CUSTOM_MODELS": "a;b", "SEARCH_KEY": "k"})
		assert422(t, err, "config.fields: pick at least one AI provider")
	})
	t.Run("plain group unmet", func(t *testing.T) {
		_, err := resolveInstallConfig(man, map[string]string{"ANTHROPIC_API_KEY": "sk-ant"})
		assert422(t, err, "config.fields: fill in at least one of: Search key, Search server")
	})
	t.Run("both met", func(t *testing.T) {
		for _, fields := range []map[string]string{
			{"ANTHROPIC_API_KEY": "sk-ant", "SEARCH_URL": "https://s"},
			{"CUSTOM_BASE_URL": "http://llm:8000/v1", "SEARCH_KEY": "k"},
		} {
			if _, err := resolveInstallConfig(man, fields); err != nil {
				t.Errorf("fields %v: %v", fields, err)
			}
		}
	})
}

func TestResolvePutConfig_RequiresNoWorse(t *testing.T) {
	man := parseRolesManifest(t)
	met := []store.InstanceConfig{
		{AppEnv: "ANTHROPIC_API_KEY", Value: "sk-ant", Secret: true},
		{AppEnv: "SEARCH_KEY", Value: "k"},
	}

	t.Run("clearing the only provider is rejected", func(t *testing.T) {
		_, err := resolvePutConfig(man, met, map[string]string{"ANTHROPIC_API_KEY": ""})
		assert422(t, err, "config.fields: keep at least one AI provider")
	})
	t.Run("clearing the only plain member is rejected", func(t *testing.T) {
		_, err := resolvePutConfig(man, met, map[string]string{"SEARCH_KEY": ""})
		assert422(t, err, "config.fields: keep at least one of these filled in: Search key, Search server")
	})
	t.Run("swapping providers in one edit is fine", func(t *testing.T) {
		if _, err := resolvePutConfig(man, met, map[string]string{"ANTHROPIC_API_KEY": "", "OPENAI_API_KEY": "sk-oa"}); err != nil {
			t.Fatalf("swap: %v", err)
		}
	})
	t.Run("an already unmet group does not block an unrelated edit", func(t *testing.T) {
		// Installed before requires existed: no provider at all.
		old := []store.InstanceConfig{{AppEnv: "SEARCH_KEY", Value: "k"}}
		if _, err := resolvePutConfig(man, old, map[string]string{"SEARCH_URL": "https://s"}); err != nil {
			t.Fatalf("unrelated edit on an app with an unmet group: %v", err)
		}
	})
}

func TestBuildInstallPlan_RolesAndRequires(t *testing.T) {
	plan := buildInstallPlan(parseRolesManifest(t), true)
	byEnv := map[string]InstallPlanConfigField{}
	for _, c := range plan.Config {
		byEnv[c.AppEnv] = c
	}
	want := map[string][2]string{
		"ANTHROPIC_API_KEY": {"ai.anthropic.api_key", ""},
		"CUSTOM_BASE_URL":   {"ai.openai_compatible.base_url", ""},
		"CUSTOM_MODELS":     {"ai.openai_compatible.models.chat", ";"},
		"BROKEN":            {"", ""}, // unknown attribute: a plain field
		"SEARCH_KEY":        {"", ""},
	}
	for env, w := range want {
		if got := byEnv[env]; got.Role != w[0] || got.Separator != w[1] {
			t.Errorf("%s: role=%q separator=%q; want %q %q", env, got.Role, got.Separator, w[0], w[1])
		}
	}
	// NOT_A_FIELD matches nothing, so its group is dropped.
	if len(plan.Requires) != 2 || plan.Requires[0].OneOf[0] != "ai" || len(plan.Requires[1].OneOf) != 2 {
		t.Errorf("requires = %+v; want [ai] and the two search fields", plan.Requires)
	}

	// A manifest without requires omits the block.
	if got := buildInstallPlan(parseConfigManifest(t), true); got.Requires != nil {
		t.Errorf("requires on a manifest without it = %+v", got.Requires)
	}
}

func TestGetAppConfig_RolesAndRequires(t *testing.T) {
	s, id, instDir := configServer(t, "u_owner", store.ScopePersonal, "running", []store.InstanceConfig{
		{AppEnv: "ANTHROPIC_API_KEY", Value: "sk-ant", Secret: true},
	})
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(rolesManifestYML), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := getConfig(t, s, memberCtx("u_owner"), id)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	byEnv := map[string]AppConfigFieldDTO{}
	for _, f := range body.Fields {
		byEnv[f.AppEnv] = f
	}
	if f := byEnv["CUSTOM_MODELS"]; f.Role != "ai.openai_compatible.models.chat" || f.Separator != ";" {
		t.Errorf("CUSTOM_MODELS = %+v", f)
	}
	if f := byEnv["ANTHROPIC_API_KEY"]; f.Role != "ai.anthropic.api_key" || f.Separator != "" || f.Value != "" || !f.Set {
		t.Errorf("ANTHROPIC_API_KEY = %+v", f)
	}
	if f := byEnv["BROKEN"]; f.Role != "" {
		t.Errorf("BROKEN should be a plain field: %+v", f)
	}
	if len(body.Requires) != 2 {
		t.Errorf("requires = %+v; want 2 groups", body.Requires)
	}
}

// The PUT handler turns a worsening edit into a 422 and audits the failure,
// like every other rejected config edit.
func TestUpdateAppConfig_RequiresWorsening_Audits422(t *testing.T) {
	s, id, instDir := configServer(t, "u_owner", store.ScopePersonal, "stopped", []store.InstanceConfig{
		{AppEnv: "ANTHROPIC_API_KEY", Value: "sk-ant", Secret: true},
		{AppEnv: "SEARCH_KEY", Value: "k"},
	})
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(rolesManifestYML), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := putConfig(t, s, adminCtx("u_admin"), id, map[string]string{"ANTHROPIC_API_KEY": ""})
	assert422(t, err, "keep at least one AI provider")
	if !auditedConfigUpdate(t, s, false) {
		t.Errorf("rejected update was not audited as failure")
	}
}

// POST /api/v1/apps refuses an install that leaves a requires group unmet,
// before any job starts, and audits it like the other rejected elections.
func TestInstallUnmetRequires422(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "cfgapp", rolesManifestYML)
	h.setupAdmin("alice", "pass1")

	resp := h.do("POST", "/api/v1/apps", map[string]any{
		"manifest_id": "cfgapp",
		"config":      map[string]any{"fields": map[string]string{"SEARCH_KEY": "k", "CUSTOM_MODELS": "a;b"}},
	})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "pick at least one AI provider") {
		t.Fatalf("install without a provider = %d %s; want 422 pick at least one AI provider", resp.StatusCode, raw)
	}
	if !h.hasAuditEvent(audit.ActionAppInstall, "", false) {
		t.Fatal("app.install failure audit event not found")
	}
}
