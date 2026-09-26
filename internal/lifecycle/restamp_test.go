package lifecycle

// Re-stamping the apps that use an account (INSTALL_SETUP.md piece 4): a
// config edit with bindings writes both at once, an AI account's new key
// reaches every app bound to it, a deleted AI account's values leave the
// override, and an email account's edit or delete reaches the .env. Running
// apps are recreated, stopped ones are not, and one failing app does not stop
// the others.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

// aiRoleManifest has one AI slot (ai.openai: key and chat model) and one
// plain field.
const aiRoleManifest = `
id: aiapp
manifest_version: 1
name: AI App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [aiapp]
permissions:
  internet: true
  lan: false
config:
  - app_env: OPENAI_API_KEY
    title: "OpenAI key"
    description: "d"
    secret: true
    role: ai.openai.api_key
  - app_env: OPENAI_MODEL
    title: "Model"
    description: "d"
    role: ai.openai.model.chat
  - app_env: PLAIN
    title: "Plain"
    description: "d"
`

// installAIApp installs aiapp bound to account ai_1, with a plain value. The
// manifest id is set to id, because instance ids carry the manifest id and a
// time to the second, so two installs of one app in a test would collide.
func installAIApp(t *testing.T, e *testEnv, id string) store.Instance {
	t.Helper()
	man := strings.Replace(strings.Replace(aiRoleManifest, "id: aiapp", "id: "+id, 1), "[aiapp]", "["+id+"]", 1)
	e.writeCatalogApp(t, id, mailCompose, man)
	e.docker.digests[testImage] = testDigest
	cfg := []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-from-account", Secret: true},
		{AppEnv: "OPENAI_MODEL", Value: "gpt-4o"},
		{AppEnv: "PLAIN", Value: "keep"},
	}
	bindings := []store.AIBinding{{Slot: "ai.openai", AccountID: "ai_1", Models: map[string][]string{"model.chat": {"gpt-4o"}}}}
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, id),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", cfg, bindings, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst
}

// composeUps counts the compose ups of one instance.
func composeUps(e *testEnv, id string) int {
	n := 0
	for _, c := range e.docker.Calls() {
		if c.method == "ComposeUp" && len(c.args) == 2 && c.args[1] == "moose-"+id {
			n++
		}
	}
	return n
}

func TestSetConfigAndAIBindingsStoresBoth(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	inst := installAIApp(t, e, "aiapp")
	before := composeUps(e, inst.ID)

	// Clear the slot: no binding, no slot values, the plain value stays.
	cfg := []store.InstanceConfig{{AppEnv: "PLAIN", Value: "keep"}}
	if err := e.m.SetConfigAndAIBindings(context.Background(), inst.ID, cfg, []string{"ai.openai"}, nil); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := e.store.ListInstanceAIBindings(inst.ID); len(got) != 0 {
		t.Fatalf("binding survived a clear: %+v", got)
	}
	env := readOverrideEnv(t, e, inst.ID).Services["app"].Environment
	if _, ok := env["OPENAI_API_KEY"]; ok || env["PLAIN"] != "keep" {
		t.Fatalf("override env = %v", env)
	}
	if composeUps(e, inst.ID) != before+1 {
		t.Fatalf("a running app must be recreated once")
	}
}

func TestRestampAIAccount(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	running := installAIApp(t, e, "aione")
	stopped := installAIApp(t, e, "aitwo")
	rebound := installAIApp(t, e, "aithree")
	broken := installAIApp(t, e, "aifour")
	if err := e.store.SetState(stopped.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	// rebound's slot now points at another account, as if the user picked one
	// between the edit and the job.
	now := time.Unix(1_700_000_000, 0)
	if err := e.store.CreateAIAccount(store.AIAccount{ID: "ai_2", OwnerUserID: "u_admin", ProviderID: "openai", Label: "Other", APIKey: "sk-2", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetInstanceAIBindings(rebound.ID, []store.AIBinding{{Slot: "ai.openai", AccountID: "ai_2"}}); err != nil {
		t.Fatal(err)
	}
	// broken has lost its manifest copy, so it cannot be updated.
	if err := os.Remove(filepath.Join(e.stateDir, "instances", broken.ID, "manifest.yml")); err != nil {
		t.Fatal(err)
	}
	ups := map[string]int{}
	for _, i := range []store.Instance{running, stopped, rebound} {
		ups[i.ID] = composeUps(e, i.ID)
	}

	var updates []AISlotValues
	for _, i := range []store.Instance{broken, running, stopped, rebound} {
		// A new key, and no model value: the model field is cleared.
		updates = append(updates, AISlotValues{InstanceID: i.ID, Slot: "ai.openai", Values: map[string]string{"OPENAI_API_KEY": "sk-new"}})
	}
	err := e.m.RestampAIAccount(context.Background(), "ai_1", updates)
	if err == nil || !strings.Contains(err.Error(), "could not update AI App") {
		t.Fatalf("err = %v; want it to name the broken app", err)
	}

	for _, i := range []store.Instance{running, stopped} {
		env := readOverrideEnv(t, e, i.ID).Services["app"].Environment
		if env["OPENAI_API_KEY"] != "sk-new" || env["PLAIN"] != "keep" {
			t.Fatalf("%s override env = %v", i.ID, env)
		}
		if _, ok := env["OPENAI_MODEL"]; ok {
			t.Fatalf("%s kept a slot value the update did not give", i.ID)
		}
		cfg, _ := e.store.GetInstanceConfig(i.ID)
		for _, c := range cfg {
			if c.AppEnv == "OPENAI_API_KEY" && (c.Value != "sk-new" || !c.Secret) {
				t.Fatalf("%s stored key = %+v", i.ID, c)
			}
		}
	}
	if composeUps(e, running.ID) != ups[running.ID]+1 {
		t.Fatal("the running app was not recreated")
	}
	if composeUps(e, stopped.ID) != ups[stopped.ID] {
		t.Fatal("the stopped app was recreated")
	}
	if env := readOverrideEnv(t, e, rebound.ID).Services["app"].Environment; env["OPENAI_API_KEY"] != "sk-from-account" {
		t.Fatalf("a rebound slot was rewritten: %v", env)
	}
	if composeUps(e, rebound.ID) != ups[rebound.ID] {
		t.Fatal("an app with nothing to change was recreated")
	}
}

func TestRestampConfigAfterAIAccountDelete(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	inst := installAIApp(t, e, "aiapp")
	before := composeUps(e, inst.ID)
	if err := e.store.DeleteAIAccountAndValues("ai_1", "u_admin", map[string][]string{inst.ID: {"OPENAI_API_KEY", "OPENAI_MODEL"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RestampConfig(context.Background(), []string{inst.ID, "gone"}); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	env := readOverrideEnv(t, e, inst.ID).Services["app"].Environment
	if len(env) != 1 || env["PLAIN"] != "keep" {
		t.Fatalf("override env = %v", env)
	}
	if composeUps(e, inst.ID) != before+1 {
		t.Fatal("the running app was not recreated")
	}
}

func TestRestampMail(t *testing.T) {
	e := newTestEnv(t)
	if err := e.createMailProvider(testProvider()); err != nil {
		t.Fatal(err)
	}
	inst, _ := installMailApp(t, e, "mp_test")
	envPath := filepath.Join(e.stateDir, "instances", inst.ID, ".env")
	before := composeUps(e, inst.ID)

	edited := testProvider()
	edited.Host = "smtp.new.example"
	if !MailEnvChanged(testProvider(), edited) {
		t.Fatal("a host change must change the env")
	}
	relabel := testProvider()
	relabel.Label, relabel.ProviderType = "Renamed", "fastmail"
	if MailEnvChanged(testProvider(), relabel) {
		t.Fatal("a label or preset change must not change the env")
	}
	if err := e.store.UpdateMailProvider(edited); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RestampMail(context.Background(), []string{inst.ID}); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	raw, _ := os.ReadFile(envPath)
	if !strings.Contains(string(raw), "MOOSE_MAIL_HOST=smtp.new.example") || strings.Contains(string(raw), "smtp.fastmail.com") {
		t.Fatalf(".env after edit:\n%s", raw)
	}
	if composeUps(e, inst.ID) != before+1 {
		t.Fatal("the running app was not recreated after the edit")
	}

	// Delete: the binding goes with the account, and the lines go too.
	if err := e.store.DeleteMailProvider("mp_test", "u_admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RestampMail(context.Background(), []string{inst.ID}); err != nil {
		t.Fatalf("restamp after delete: %v", err)
	}
	raw, _ = os.ReadFile(envPath)
	if strings.Contains(string(raw), "MOOSE_MAIL_") {
		t.Fatalf(".env after delete still has mail lines:\n%s", raw)
	}
	if composeUps(e, inst.ID) != before+2 {
		t.Fatal("the running app was not recreated after the delete")
	}
}
