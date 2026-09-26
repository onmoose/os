package api

// AI bindings after install, and account changes reaching apps
// (INSTALL_SETUP.md piece 4): the config PUT resolver with bindings, the
// "needs setup" list, the config GET's bindings, and the AI and email account
// edit and delete paths that re-stamp the apps bound to them.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/store"
)

// --- resolvePutWithAI -----------------------------------------------------

// putTest runs the PUT resolution for aiapp with the test accounts and
// providers, from the given current values and bindings.
func putTest(t *testing.T, current map[string]string, bound []store.AIBinding, fields map[string]string, bindings ...AIBindingBody) (map[string]string, putAIResolution, error) {
	t.Helper()
	man := parseAIAppManifest(t)
	var cur []store.InstanceConfig
	for env, v := range current {
		cur = append(cur, store.InstanceConfig{AppEnv: env, Value: v})
	}
	res, err := resolvePutWithAI(man, cur, bound, fields, bindings, lookupTestAccount, testAIProviders())
	if err != nil {
		return nil, putAIResolution{}, err
	}
	return configValues(res.cfg), res, nil
}

// acmeBound is aiapp with its native slot filled from a_acme.
var acmeBound = map[string]string{"ACME_API_KEY": "sk-acme", "ACME_MODEL": "acme-1", "SEARCH_KEY": "s"}
var acmeBinding = []store.AIBinding{{Slot: "ai.acme", AccountID: "a_acme", Models: map[string][]string{"model.chat": {"acme-1"}}}}

func TestResolvePutWithAI(t *testing.T) {
	t.Run("replace a slot with another account", func(t *testing.T) {
		got, res, err := putTest(t, acmeBound, acmeBinding, nil,
			AIBindingBody{Slot: "ai.acme", AccountID: "a_acme_proxy", Models: map[string][]string{"model.chat": {"acme-2"}}})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{"ACME_API_KEY": "sk-acme", "ACME_BASE_URL": "https://proxy.invalid/v1", "ACME_MODEL": "acme-2", "SEARCH_KEY": "s"})
		if len(res.slots) != 1 || res.slots[0] != "ai.acme" || len(res.bindings) != 1 || res.bindings[0].AccountID != "a_acme_proxy" {
			t.Fatalf("resolution = %+v", res)
		}
	})

	t.Run("a new binding clears a field it does not fill", func(t *testing.T) {
		cur := map[string]string{"ACME_API_KEY": "sk", "ACME_BASE_URL": "https://old.invalid", "ACME_MODEL": "m"}
		got, _, err := putTest(t, cur, nil, nil, AIBindingBody{Slot: "ai.acme", AccountID: "a_acme"})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{"ACME_API_KEY": "sk-acme", "ACME_MODEL": "acme-1"})
	})

	t.Run("swap one provider for another in one save", func(t *testing.T) {
		// The requires group stays met, because both halves are checked
		// together.
		got, res, err := putTest(t, acmeBound, acmeBinding, nil,
			AIBindingBody{Slot: "ai.acme", AccountID: ""},
			AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_acme"})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{
			"SEARCH_KEY": "s", "CUSTOM_API_KEY": "sk-acme", "CUSTOM_BASE_URL": "https://api.acme.invalid/v1",
			"CUSTOM_MODELS": "acme-1", "CUSTOM_EMBED": "acme-embed",
		})
		if len(res.slots) != 2 || len(res.bindings) != 1 || res.bindings[0].Slot != "ai.openai_compatible" {
			t.Fatalf("resolution = %+v", res)
		}
	})

	t.Run("typed values for an unbound slot stay possible", func(t *testing.T) {
		got, _, err := putTest(t, nil, nil, map[string]string{"ACME_API_KEY": "typed"})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{"ACME_API_KEY": "typed"})
	})

	t.Run("an empty typed value for a bound field is ignored", func(t *testing.T) {
		got, _, err := putTest(t, acmeBound, acmeBinding, map[string]string{"ACME_API_KEY": "", "SEARCH_KEY": "t"})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{"ACME_API_KEY": "sk-acme", "ACME_MODEL": "acme-1", "SEARCH_KEY": "t"})
	})

	bad := []struct {
		name     string
		fields   map[string]string
		bindings []AIBindingBody
		want     string
	}{
		{"clearing the only provider", nil, []AIBindingBody{{Slot: "ai.acme"}}, "keep at least one AI provider"},
		{"typed value for a listed slot", map[string]string{"ACME_MODEL": "x"}, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme"}}, "ACME_MODEL is filled from an AI account"},
		{"typed value for a bound slot not listed", map[string]string{"ACME_API_KEY": "typed"}, nil, "ACME_API_KEY is filled from an AI account"},
		{"slot twice", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme"}, {Slot: "ai.acme"}}, "given more than once"},
		{"unknown slot", nil, []AIBindingBody{{Slot: "ai.nope"}}, "no AI slot"},
		{"account that does not fit", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_plain"}}, "does not work with this app's ai.acme slot"},
		{"missing account", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_ghost"}}, "no such AI account"},
	}
	for _, tc := range bad {
		t.Run("reject "+tc.name, func(t *testing.T) {
			_, _, err := putTest(t, acmeBound, acmeBinding, tc.fields, tc.bindings...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
			assertStatus(t, err, http.StatusUnprocessableEntity)
		})
	}
}

func TestSetupMissing(t *testing.T) {
	man := parseAIAppManifest(t)
	if got := setupMissing(man, map[string]string{"ACME_MODEL": "m"}); len(got) != 1 || got[0] != "Pick at least one LLM provider." {
		t.Fatalf("missing = %v", got)
	}
	if got := setupMissing(man, map[string]string{"ACME_API_KEY": "k"}); len(got) != 0 {
		t.Fatalf("missing = %v; want none", got)
	}
	cfg := parseConfigManifest(t) // OPENAI_API_KEY is required
	if got := setupMissing(cfg, nil); len(got) != 1 || got[0] != "Fill in OpenAI key." {
		t.Fatalf("missing = %v", got)
	}
}

// --- over HTTP --------------------------------------------------------------

// seedAIApp writes an installed, stopped aiapp owned by ownerID: its row,
// manifest, override and .env, with values and a binding to accountID. The
// harness has no Docker, so a stopped app is what lets a job run to the end.
func (h *harness) seedAIApp(id, name, ownerID, accountID string) {
	h.t.Helper()
	if err := h.st.Create(store.Instance{
		ID: id, ManifestID: "aiapp", Name: name, Slug: id, Version: "1.0", State: "stopped",
		OwnerUserID: ownerID, Scope: store.ScopePersonal, CreatedAt: time.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
	h.seedInstanceManifest(id, aiAppManifestYML)
	dir := filepath.Join(h.stateDir, "instances", id)
	if err := os.WriteFile(filepath.Join(dir, "compose.override.yml"), []byte(configOverrideYAML), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MOOSE_APP_ID="+id+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetInstanceConfig(id, []store.InstanceConfig{
		{AppEnv: "ACME_API_KEY", Value: testAIKey, Secret: true},
		{AppEnv: "ACME_MODEL", Value: "acme-1"},
		{AppEnv: "SEARCH_KEY", Value: "s", Secret: false},
	}); err != nil {
		h.t.Fatal(err)
	}
	if accountID == "" {
		return
	}
	if err := h.st.SetInstanceAIBindings(id, []store.AIBinding{{Slot: "ai.acme", AccountID: accountID, Models: map[string][]string{"model.chat": {"acme-1"}}}}); err != nil {
		h.t.Fatal(err)
	}
}

// storedValues reads an app's config values.
func (h *harness) storedValues(id string) map[string]string {
	h.t.Helper()
	cfg, err := h.st.GetInstanceConfig(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return configValues(cfg)
}

// overrideOf reads an app's compose override.
func (h *harness) overrideOf(id string) string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.stateDir, "instances", id, "compose.override.yml"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

// The config GET shows the app's bindings (another user's without its
// label) and what is missing; the app list flags an app that needs setup.
func TestAppConfigShowsBindingsAndNeedsSetup(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	mine := h.createAIAccount(aiAccountBody("Work"))
	h.seedAIApp("i_ai", "AI Demo", admin.ID, mine.ID)

	code, raw := h.doRaw("GET", "/api/v1/apps/i_ai/config", nil)
	if code != http.StatusOK || strings.Contains(string(raw), testAIKey) {
		t.Fatalf("get config = %d %s", code, raw)
	}
	cfg := decodeRaw[AppConfigDTO](t, raw)
	if len(cfg.AIBindings) != 1 || !cfg.AIBindings[0].Mine || cfg.AIBindings[0].AccountLabel != "Work" ||
		cfg.AIBindings[0].ProviderID != "acme" || cfg.AIBindings[0].Models["model.chat"][0] != "acme-1" {
		t.Fatalf("bindings = %+v", cfg.AIBindings)
	}
	if cfg.NeedsSetup || len(cfg.Missing) != 0 {
		t.Fatalf("a set-up app needs setup: %+v", cfg)
	}

	// Bob's account filling alice's app shows without its label.
	h.addMember("u_bob", "bob", "bobpass")
	now := time.Now()
	if err := h.st.CreateAIAccount(store.AIAccount{ID: "ai_bob", OwnerUserID: "u_bob", ProviderID: "acme", Label: "Bob's secret name", APIKey: "sk-bob", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetInstanceAIBindings("i_ai", []store.AIBinding{{Slot: "ai.acme", AccountID: "ai_bob"}}); err != nil {
		t.Fatal(err)
	}
	code, raw = h.doRaw("GET", "/api/v1/apps/i_ai/config", nil)
	if code != http.StatusOK || strings.Contains(string(raw), "Bob's secret name") {
		t.Fatalf("get config = %d %s", code, raw)
	}
	if b := decodeRaw[AppConfigDTO](t, raw).AIBindings; len(b) != 1 || b[0].Mine || b[0].AccountLabel != "" {
		t.Fatalf("other user's binding = %+v", b)
	}

	// Without a provider value, the app needs setup, on its page and in the list.
	if err := h.st.SetInstanceConfig("i_ai", []store.InstanceConfig{{AppEnv: "ACME_MODEL", Value: "acme-1"}}); err != nil {
		t.Fatal(err)
	}
	cfg = decodeRaw[AppConfigDTO](t, mustOK(t, h, "GET", "/api/v1/apps/i_ai/config"))
	if !cfg.NeedsSetup || len(cfg.Missing) != 1 || cfg.Missing[0] != "Pick at least one LLM provider." {
		t.Fatalf("config = %+v", cfg)
	}
	list := decodeRaw[struct {
		Apps []InstanceDTO `json:"apps"`
	}](t, mustOK(t, h, "GET", "/api/v1/apps"))
	if len(list.Apps) != 1 || !list.Apps[0].NeedsSetup {
		t.Fatalf("list = %+v", list.Apps)
	}
	if app := decodeRaw[InstanceDTO](t, mustOK(t, h, "GET", "/api/v1/apps/i_ai")); !app.NeedsSetup {
		t.Fatalf("app = %+v", app)
	}
}

func mustOK(t *testing.T, h *harness, method, path string) []byte {
	t.Helper()
	code, raw := h.doRaw(method, path, nil)
	if code != http.StatusOK {
		t.Fatalf("%s %s = %d %s", method, path, code, raw)
	}
	return raw
}

// A config PUT with a binding stores the binding and the values it gives,
// and a PUT that clears the only provider is refused and audited.
func TestAppConfigPutBinding(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	work := h.createAIAccount(aiAccountBody("Work"))
	proxy := aiAccountBody("Proxy")
	proxy["base_url"] = "https://proxy.invalid/v1"
	other := h.createAIAccount(proxy)
	h.seedAIApp("i_ai", "AI Demo", admin.ID, work.ID)

	code, raw := h.doRaw("PUT", "/api/v1/apps/i_ai/config", map[string]any{
		"ai_bindings": []map[string]any{{"slot": "ai.acme", "account_id": other.ID}},
	})
	if code != http.StatusOK {
		t.Fatalf("put = %d %s", code, raw)
	}
	if job := awaitJob(t, h.apiSrv, decodeRaw[Job](t, raw).ID); job.Status != "completed" {
		t.Fatalf("job = %s %+v", job.Status, job.Error)
	}
	if v := h.storedValues("i_ai"); v["ACME_BASE_URL"] != "https://proxy.invalid/v1" || v["ACME_MODEL"] != "acme-1" || v["SEARCH_KEY"] != "s" {
		t.Fatalf("values = %v", v)
	}
	if b, _ := h.st.ListInstanceAIBindings("i_ai"); len(b) != 1 || b[0].AccountID != other.ID {
		t.Fatalf("bindings = %+v", b)
	}
	if !strings.Contains(h.overrideOf("i_ai"), "https://proxy.invalid/v1") {
		t.Fatalf("override not rewritten:\n%s", h.overrideOf("i_ai"))
	}

	code, raw = h.doRaw("PUT", "/api/v1/apps/i_ai/config", map[string]any{
		"ai_bindings": []map[string]any{{"slot": "ai.acme", "account_id": ""}},
	})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "keep at least one AI provider") {
		t.Fatalf("clearing the only provider = %d %s", code, raw)
	}
	if !h.hasAuditEvent(audit.ActionAppConfigUpdate, "i_ai", false) {
		t.Fatal("refused edit was not audited")
	}
}

// An AI account's key change reaches every app bound to it in a job; a
// label change starts no job; a provider change while apps use it is a 409;
// the list names the apps.
func TestAIAccountEditReachesApps(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	acct := h.createAIAccount(aiAccountBody("Work"))
	h.seedAIApp("i_one", "Zed", admin.ID, acct.ID)
	h.seedAIApp("i_two", "Alpha", admin.ID, acct.ID)

	list := decodeRaw[struct {
		Accounts []AIAccountDTO `json:"accounts"`
	}](t, mustOK(t, h, "GET", "/api/v1/ai-accounts"))
	if u := list.Accounts[0].UsedBy; len(u) != 2 || u[0].Name != "Alpha" || u[1].InstanceID != "i_one" {
		t.Fatalf("used_by = %+v", u)
	}

	// A rename starts no job.
	code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/"+acct.ID, map[string]any{"provider_id": "acme", "label": "Renamed"})
	if code != http.StatusOK {
		t.Fatalf("rename = %d %s", code, raw)
	}
	if saved := decodeRaw[AIAccountSavedDTO](t, raw); saved.JobID != "" || saved.Label != "Renamed" || len(saved.UsedBy) != 2 {
		t.Fatalf("rename answer = %+v", saved)
	}

	// A new key starts one job that rewrites both apps.
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/"+acct.ID, map[string]any{"provider_id": "acme", "label": "Renamed", "api_key": "sk-new-key"})
	if code != http.StatusOK {
		t.Fatalf("key change = %d %s", code, raw)
	}
	assertNoKey(t, raw)
	saved := decodeRaw[AIAccountSavedDTO](t, raw)
	if saved.JobID == "" {
		t.Fatal("a key change on a used account must start a job")
	}
	if job := awaitJob(t, h.apiSrv, saved.JobID); job.Status != "completed" {
		t.Fatalf("job = %s %+v", job.Status, job.Error)
	}
	for _, id := range []string{"i_one", "i_two"} {
		if v := h.storedValues(id); v["ACME_API_KEY"] != "sk-new-key" || v["ACME_MODEL"] != "acme-1" || v["SEARCH_KEY"] != "s" {
			t.Fatalf("%s values = %v", id, v)
		}
		if !strings.Contains(h.overrideOf(id), "sk-new-key") {
			t.Fatalf("%s override not rewritten", id)
		}
	}

	// The provider cannot change while apps use the account.
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/"+acct.ID, map[string]any{"provider_id": "openai_compatible", "label": "Renamed", "base_url": "http://10.0.0.2/v1"})
	if code != http.StatusConflict || !strings.Contains(string(raw), "its provider cannot change") {
		t.Fatalf("provider change = %d %s", code, raw)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountUpdate, acct.ID, false) {
		t.Fatal("refused provider change was not audited")
	}
}

// Deleting an AI account clears its values from the apps in the store at
// once, and a job rewrites them. An unused account answers 204.
func TestAIAccountDeleteClearsApps(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	acct := h.createAIAccount(aiAccountBody("Work"))
	unused := h.createAIAccount(aiAccountBody("Unused"))
	h.seedAIApp("i_ai", "AI Demo", admin.ID, acct.ID)
	h.elevate("pass1")

	if code, raw := h.doRaw("DELETE", "/api/v1/ai-accounts/"+unused.ID, nil); code != http.StatusNoContent || len(raw) != 0 {
		t.Fatalf("unused delete = %d %s", code, raw)
	}

	code, raw := h.doRaw("DELETE", "/api/v1/ai-accounts/"+acct.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("delete = %d %s", code, raw)
	}
	del := decodeRaw[AccountDeletedDTO](t, raw)
	if v := h.storedValues("i_ai"); len(v) != 1 || v["SEARCH_KEY"] != "s" {
		t.Fatalf("values right after delete = %v", v)
	}
	if job := awaitJob(t, h.apiSrv, del.JobID); job.Status != "completed" {
		t.Fatalf("job = %s %+v", job.Status, job.Error)
	}
	if strings.Contains(h.overrideOf("i_ai"), testAIKey) {
		t.Fatalf("override still holds the deleted key:\n%s", h.overrideOf("i_ai"))
	}
	if !h.hasAuditEvent(audit.ActionAIAccountDelete, acct.ID, true) {
		t.Fatal("delete was not audited")
	}
}

// An email account edit that changes the MOOSE_MAIL_* lines reaches the
// bound apps in a job, a rename does not, and a delete drops the lines.
func TestMailProviderEditAndDeleteReachApps(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	p := h.createProvider("Mail")
	h.seedAIApp("i_mail", "Mailer", admin.ID, "")
	if err := h.st.SetInstanceMailBinding("i_mail", p.ID); err != nil {
		t.Fatal(err)
	}
	envOf := func() string {
		raw, err := os.ReadFile(filepath.Join(h.stateDir, "instances", "i_mail", ".env"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	list := decodeRaw[struct {
		Providers []MailProviderDTO `json:"providers"`
	}](t, mustOK(t, h, "GET", "/api/v1/mail-providers"))
	if u := list.Providers[0].UsedBy; len(u) != 1 || u[0].Name != "Mailer" {
		t.Fatalf("used_by = %+v", u)
	}

	body := providerBody("Renamed")
	code, raw := h.doRaw("PUT", "/api/v1/mail-providers/"+p.ID, body)
	if code != http.StatusOK || decodeRaw[MailProviderSavedDTO](t, raw).JobID != "" {
		t.Fatalf("rename = %d %s", code, raw)
	}

	body["host"] = "smtp.new.example"
	code, raw = h.doRaw("PUT", "/api/v1/mail-providers/"+p.ID, body)
	saved := decodeRaw[MailProviderSavedDTO](t, raw)
	if code != http.StatusOK || saved.JobID == "" || strings.Contains(string(raw), "s3cret-pass") {
		t.Fatalf("host change = %d %s", code, raw)
	}
	if job := awaitJob(t, h.apiSrv, saved.JobID); job.Status != "completed" {
		t.Fatalf("job = %s %+v", job.Status, job.Error)
	}
	if env := envOf(); !strings.Contains(env, "MOOSE_MAIL_HOST=smtp.new.example") || !strings.Contains(env, "MOOSE_APP_ID=i_mail") {
		t.Fatalf(".env after edit:\n%s", env)
	}

	h.elevate("pass1")
	code, raw = h.doRaw("DELETE", "/api/v1/mail-providers/"+p.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("delete = %d %s", code, raw)
	}
	if job := awaitJob(t, h.apiSrv, decodeRaw[AccountDeletedDTO](t, raw).JobID); job.Status != "completed" {
		t.Fatalf("job = %s %+v", job.Status, job.Error)
	}
	if env := envOf(); strings.Contains(env, "MOOSE_MAIL_") || !strings.Contains(env, "MOOSE_APP_ID=i_mail") {
		t.Fatalf(".env after delete:\n%s", env)
	}
}
