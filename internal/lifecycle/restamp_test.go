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

	"github.com/onmoose/os/internal/manifest"
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

func TestUpdateConfigStoresValuesAndBindings(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	inst := installAIApp(t, e, "aiapp")
	before := composeUps(e, inst.ID)

	// Clear the slot: no binding, no slot values, the plain value stays.
	cfg := []store.InstanceConfig{{AppEnv: "PLAIN", Value: "keep"}}
	clear := func(*manifest.Manifest, []store.InstanceConfig, []store.AIBinding) (ConfigChange, error) {
		return ConfigChange{Values: cfg, Slots: []string{"ai.openai"}}, nil
	}
	if err := e.m.UpdateConfig(context.Background(), inst.ID, clear); err != nil {
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

// keyResolver resolves a binding the way the API does for this manifest:
// the account's key as it is in the store now, and the stored model.
func keyResolver(e *testEnv) SlotResolver {
	return func(_ *manifest.Manifest, b store.AIBinding, _ map[string]string) (map[string]string, error) {
		acct, err := e.store.GetAIAccount(b.AccountID)
		if err != nil {
			return nil, err
		}
		out := map[string]string{"OPENAI_API_KEY": acct.APIKey}
		if m := b.Models["model.chat"]; len(m) > 0 {
			out["OPENAI_MODEL"] = m[0]
		}
		return out, nil
	}
}

// setAccountKey changes an account's key in the store, as a saved edit does.
func setAccountKey(t *testing.T, e *testEnv, id, key string) {
	t.Helper()
	a, err := e.store.GetAIAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	a.APIKey = key
	if err := e.store.UpdateAIAccount(a); err != nil {
		t.Fatal(err)
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

	setAccountKey(t, e, "ai_1", "sk-new")
	ids := []string{broken.ID, running.ID, stopped.ID, rebound.ID}
	err := e.m.RestampAIAccount(context.Background(), "ai_1", ids, keyResolver(e))
	if err == nil || !strings.Contains(err.Error(), "could not update AI App") {
		t.Fatalf("err = %v; want it to name the broken app", err)
	}

	for _, i := range []store.Instance{running, stopped} {
		env := readOverrideEnv(t, e, i.ID).Services["app"].Environment
		if env["OPENAI_API_KEY"] != "sk-new" || env["OPENAI_MODEL"] != "gpt-4o" || env["PLAIN"] != "keep" {
			t.Fatalf("%s override env = %v", i.ID, env)
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

// Two quick edits start two jobs. The older one may run last; it reads the
// account at commit time, so the app ends on the newer key either way.
func TestRestampAIAccountOlderJobKeepsNewerKey(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	inst := installAIApp(t, e, "aiapp")
	setAccountKey(t, e, "ai_1", "sk-first-edit")
	setAccountKey(t, e, "ai_1", "sk-second-edit")
	// The second edit's job runs first, then the first edit's job.
	for i := 0; i < 2; i++ {
		if err := e.m.RestampAIAccount(context.Background(), "ai_1", []string{inst.ID}, keyResolver(e)); err != nil {
			t.Fatalf("restamp %d: %v", i, err)
		}
	}
	if env := readOverrideEnv(t, e, inst.ID).Services["app"].Environment; env["OPENAI_API_KEY"] != "sk-second-edit" {
		t.Fatalf("override env = %v; want the newer key", env)
	}
}

func TestRestampConfigAfterAIAccountDelete(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	inst := installAIApp(t, e, "aiapp")
	before := composeUps(e, inst.ID)
	fields := func(string, string) ([]string, error) { return []string{"OPENAI_API_KEY", "OPENAI_MODEL"}, nil }
	if _, err := e.store.DeleteAIAccountAndValues("ai_1", "u_admin", fields); err != nil {
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

// Dropping a deleted email account's lines needs only the binding and the
// .env, so it works even when the app's manifest copy cannot be read.
func TestRestampMailWithoutManifest(t *testing.T) {
	e := newTestEnv(t)
	if err := e.createMailProvider(testProvider()); err != nil {
		t.Fatal(err)
	}
	inst, _ := installMailApp(t, e, "mp_test")
	dir := filepath.Join(e.stateDir, "instances", inst.ID)
	if err := os.Remove(filepath.Join(dir, "manifest.yml")); err != nil {
		t.Fatal(err)
	}
	if err := e.store.DeleteMailProvider("mp_test", "u_admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RestampMail(context.Background(), []string{inst.ID}); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if strings.Contains(string(raw), "MOOSE_MAIL_") {
		t.Fatalf(".env still has mail lines:\n%s", raw)
	}
}

// deleteAccount deletes an AI account and the values its bindings gave, as
// the API's delete does, naming the fields of this test's one slot.
func deleteAccount(t *testing.T, e *testEnv, id string) {
	t.Helper()
	fields := func(string, string) ([]string, error) { return []string{"OPENAI_API_KEY", "OPENAI_MODEL"}, nil }
	if _, err := e.store.DeleteAIAccountAndValues(id, "u_admin", fields); err != nil {
		t.Fatal(err)
	}
}

// An account delete that lands after a re-stamp read the account and before
// it writes does not get the deleted key written back.
func TestRestampAIAccountDeleteBetweenReadAndWrite(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	inst := installAIApp(t, e, "aiapp")
	read := keyResolver(e)
	racing := func(man *manifest.Manifest, b store.AIBinding, cur map[string]string) (map[string]string, error) {
		vals, err := read(man, b, cur)
		deleteAccount(t, e, "ai_1") // the delete commits before the write
		return vals, err
	}
	if err := e.m.RestampAIAccount(context.Background(), "ai_1", []string{inst.ID}, racing); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	cfg, _ := e.store.GetInstanceConfig(inst.ID)
	for _, c := range cfg {
		if c.AppEnv == "OPENAI_API_KEY" {
			t.Fatalf("the deleted key came back: %+v", cfg)
		}
	}
}

// A config save whose unchanged slot lost its account while it ran writes
// nothing and fails with a plain message, so the deleted key stays gone.
func TestUpdateConfigDeleteBetweenReadAndWrite(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	inst := installAIApp(t, e, "aiapp")
	edit := func(_ *manifest.Manifest, current []store.InstanceConfig, _ []store.AIBinding) (ConfigChange, error) {
		// A plain field edit; the slot's values ride along from current.
		out := []store.InstanceConfig{}
		for _, c := range current {
			if c.AppEnv == "PLAIN" {
				c.Value = "changed"
			}
			out = append(out, c)
		}
		deleteAccount(t, e, "ai_1") // the delete commits before the write
		return ConfigChange{Values: out}, nil
	}
	err := e.m.UpdateConfig(context.Background(), inst.ID, edit)
	if err == nil || !strings.Contains(err.Error(), "was deleted while saving") {
		t.Fatalf("err = %v; want the plain deleted-while-saving message", err)
	}
	cfg, _ := e.store.GetInstanceConfig(inst.ID)
	for _, c := range cfg {
		if c.AppEnv == "OPENAI_API_KEY" || c.Value == "changed" {
			t.Fatalf("a refused save wrote values: %+v", cfg)
		}
	}
}
