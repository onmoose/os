package lifecycle

// User-supplied config (APP_MANIFEST.md # D4): the brain stamps each declared
// value verbatim under its own app_env into the target service's compose-override
// environment (not the .env the MOOSE_* family uses), at install and on edit.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"
	"gopkg.in/yaml.v3"
)

// A two-service app so the per-field `service:` target is exercised alongside the
// main_service default.
const configCompose = `
services:
  app:
    image: traefik/whoami:v1.10.3
  worker:
    image: traefik/whoami:v1.10.3
`

const configManifest = `
id: configapp
manifest_version: 1
name: Config App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [configapp]
permissions:
  internet: false
  lan: false
config:
  - app_env: OPENAI_API_KEY
    title: "OpenAI API key"
    description: "From platform.openai.com."
    secret: true
    required: true
  - app_env: OPENAI_MODEL
    title: "Model"
    description: "Which model."
  - app_env: WORKER_TOKEN
    title: "Worker token"
    description: "Token for the worker."
    service: worker
  - app_env: ENDPOINT_OPT
    title: "Endpoint"
    description: "Optional base URL."
`

// ovEnv decodes each service's environment block from an instance's override.
type ovEnv struct {
	Services map[string]struct {
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

func readOverrideEnv(t *testing.T, e *testEnv, id string) ovEnv {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "instances", id, "compose.override.yml"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc ovEnv
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	return doc
}

// installConfigApp installs configapp with OPENAI_API_KEY, OPENAI_MODEL, and
// WORKER_TOKEN set; ENDPOINT_OPT left unset.
func installConfigApp(t *testing.T, e *testEnv) store.Instance {
	t.Helper()
	e.writeCatalogApp(t, "configapp", configCompose, configManifest)
	e.docker.digests[testImage] = testDigest
	cfg := []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-secret", Secret: true},
		{AppEnv: "OPENAI_MODEL", Value: "gpt-4o"},
		{AppEnv: "WORKER_TOKEN", Value: "wt-123"},
	}
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "configapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", cfg, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst
}

func TestInstallStampsConfigIntoOverride(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)
	doc := readOverrideEnv(t, e, inst.ID)

	app := doc.Services["app"].Environment
	if app["OPENAI_API_KEY"] != "sk-secret" {
		t.Errorf("app OPENAI_API_KEY = %q, want sk-secret", app["OPENAI_API_KEY"])
	}
	if app["OPENAI_MODEL"] != "gpt-4o" {
		t.Errorf("app OPENAI_MODEL = %q, want gpt-4o", app["OPENAI_MODEL"])
	}
	// A field with an explicit service lands on that service, not main.
	if tok := doc.Services["worker"].Environment["WORKER_TOKEN"]; tok != "wt-123" {
		t.Errorf("worker WORKER_TOKEN = %q, want wt-123", tok)
	}
	if _, ok := app["WORKER_TOKEN"]; ok {
		t.Errorf("WORKER_TOKEN leaked onto the main service")
	}
	// An optional field left unset injects nothing — the app keeps its default.
	if _, ok := app["ENDPOINT_OPT"]; ok {
		t.Errorf("optional unset ENDPOINT_OPT was injected")
	}
	// The persisted store row matches, with the secret flag preserved.
	stored, err := e.store.GetInstanceConfig(inst.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	bySecret := map[string]bool{}
	for _, c := range stored {
		bySecret[c.AppEnv] = c.Secret
	}
	if !bySecret["OPENAI_API_KEY"] || bySecret["OPENAI_MODEL"] {
		t.Errorf("secret flags wrong: %+v", stored)
	}
}

// TestConfigDoesNotLeakIntoEnv: config values land in the override, never the
// .env the MOOSE_* family uses (APP_MANIFEST.md # D4 — no MOOSE_ indirection).
func TestConfigDoesNotLeakIntoEnv(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if strings.Contains(string(raw), "sk-secret") || strings.Contains(string(raw), "OPENAI_API_KEY") {
		t.Errorf("config value leaked into .env:\n%s", raw)
	}
}

// TestSetConfigRestampsAndRestarts: an edit rewrites the override env and, on a
// running instance, recreates the containers. Clearing a value removes it.
func TestSetConfigRestampsAndRestarts(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)

	// New value for the secret; drop WORKER_TOKEN (clear) and keep the model.
	newCfg := []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-new", Secret: true},
		{AppEnv: "OPENAI_MODEL", Value: "gpt-4o-mini"},
	}
	if err := e.m.SetConfig(context.Background(), inst.ID, newCfg); err != nil {
		t.Fatalf("set config: %v", err)
	}
	doc := readOverrideEnv(t, e, inst.ID)
	if got := doc.Services["app"].Environment["OPENAI_API_KEY"]; got != "sk-new" {
		t.Errorf("OPENAI_API_KEY = %q, want sk-new", got)
	}
	if got := doc.Services["app"].Environment["OPENAI_MODEL"]; got != "gpt-4o-mini" {
		t.Errorf("OPENAI_MODEL = %q, want gpt-4o-mini", got)
	}
	// WORKER_TOKEN was cleared, so the worker's env block is gone.
	if _, ok := doc.Services["worker"].Environment["WORKER_TOKEN"]; ok {
		t.Errorf("cleared WORKER_TOKEN still present in override")
	}
	// A running instance is recreated to pick up the new env.
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Errorf("SetConfig did not recreate containers")
	}
	// The store reflects the new set.
	stored, _ := e.store.GetInstanceConfig(inst.ID)
	if len(stored) != 2 {
		t.Errorf("stored config = %v, want 2 rows", stored)
	}
}

// TestReapplyResourceLimitsPreservesConfigEnv: the resource-limit patch
// round-trips the whole override and must not drop the config env block.
func TestReapplyResourceLimitsPreservesConfigEnv(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)

	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{MemoryBytes: 256 << 20}); err != nil {
		t.Fatalf("set limits: %v", err)
	}
	changed, _, err := e.m.reapplyResourceLimits(inst.ID)
	if err != nil {
		t.Fatalf("reapply: %v", err)
	}
	if !changed {
		t.Fatalf("reapply reported no change after setting a cap")
	}
	doc := readOverrideEnv(t, e, inst.ID)
	if got := doc.Services["app"].Environment["OPENAI_API_KEY"]; got != "sk-secret" {
		t.Errorf("config env dropped by resource-limit patch: OPENAI_API_KEY = %q", got)
	}
}

// TestInstallRejectsConfigWithUnknownService: a config field targeting a service
// the compose doesn't define fails install fast, before any state is written.
func TestInstallRejectsConfigWithUnknownService(t *testing.T) {
	e := newTestEnv(t)
	const badManifest = `
id: badcfg
manifest_version: 1
name: Bad Config
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [badcfg]
permissions:
  internet: false
  lan: false
config:
  - app_env: TOKEN
    title: "Token"
    description: "A token."
    service: ghost
`
	e.writeCatalogApp(t, "badcfg", configCompose, badManifest)
	e.docker.digests[testImage] = testDigest
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "badcfg"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "",
		[]store.InstanceConfig{{AppEnv: "TOKEN", Value: "x"}}, nil, nil)
	if err == nil {
		t.Fatal("install accepted a config field targeting an unknown service")
	}
}
