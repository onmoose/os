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
	"github.com/onmoose/os/internal/catalog"
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
		{"clearing the only provider", nil, []AIBindingBody{{Slot: "ai.acme"}}, "keep at least one LLM provider"},
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
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "keep at least one LLM provider") {
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

// --- ordering: values resolved at commit time -----------------------------

// An older re-stamp job that runs after a newer edit writes the newer key,
// because it reads the account when it commits.
func TestAIAccountRestampJobUsesLatestKey(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	acct := h.createAIAccount(aiAccountBody("Work"))
	h.seedAIApp("i_ai", "AI Demo", admin.ID, acct.ID)

	// The newest edit is saved; then the job of an older edit runs.
	stored, err := h.st.GetAIAccount(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.APIKey = "sk-newest"
	if err := h.st.UpdateAIAccount(stored); err != nil {
		t.Fatal(err)
	}
	job := h.apiSrv.restampAIAccountJob(acct.ID, []string{"i_ai"}, "")
	if done := awaitJob(t, h.apiSrv, job.ID); done.Status != "completed" {
		t.Fatalf("job = %s %+v", done.Status, done.Error)
	}
	if v := h.storedValues("i_ai"); v["ACME_API_KEY"] != "sk-newest" {
		t.Fatalf("values = %v; want the newest key", v)
	}
}

// A config save resolves its binding again when it commits, so an account
// edited after the request was checked gives the app its new key.
func TestConfigResolverUsesAccountAtCommit(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	work := h.createAIAccount(aiAccountBody("Work"))
	other := h.createAIAccount(aiAccountBody("Other"))
	h.seedAIApp("i_ai", "AI Demo", admin.ID, work.ID)
	providers, err := h.apiSrv.catalog.AIProviders()
	if err != nil {
		t.Fatal(err)
	}
	account := func(id string) (store.AIAccount, error) {
		a, err := h.st.GetAIAccount(id)
		if err == nil && a.OwnerUserID != admin.ID {
			return store.AIAccount{}, store.ErrNotFound
		}
		return a, err
	}
	resolve := configResolver(nil, []AIBindingBody{{Slot: "ai.acme", AccountID: other.ID}}, account, providers)

	// The request was checked; now the account is edited before the job commits.
	stored, _ := h.st.GetAIAccount(other.ID)
	stored.APIKey = "sk-edited-meanwhile"
	if err := h.st.UpdateAIAccount(stored); err != nil {
		t.Fatal(err)
	}
	if err := h.apiSrv.life.UpdateConfig(t.Context(), "i_ai", resolve); err != nil {
		t.Fatalf("update: %v", err)
	}
	if v := h.storedValues("i_ai"); v["ACME_API_KEY"] != "sk-edited-meanwhile" {
		t.Fatalf("values = %v; want the key the account has at commit", v)
	}

	// An account deleted in between fails the save with a plain message and
	// writes nothing.
	resolve = configResolver(nil, []AIBindingBody{{Slot: "ai.acme", AccountID: work.ID}}, account, providers)
	if err := h.st.DeleteAIAccount(work.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	err = h.apiSrv.life.UpdateConfig(t.Context(), "i_ai", resolve)
	if err == nil || err.Error() != "config.ai_bindings: no such AI account" {
		t.Fatalf("err = %v; want the plain no such AI account", err)
	}
	if b, _ := h.st.ListInstanceAIBindings("i_ai"); len(b) != 1 || b[0].AccountID != other.ID {
		t.Fatalf("bindings changed on a failed save: %+v", b)
	}
}

// A delete after the slot was rebound to another account keeps the new
// account's values, and answers 204 because no app still used the account.
func TestAIAccountDeleteAfterRebindKeepsNewValues(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	old := h.createAIAccount(aiAccountBody("Old"))
	fresh := h.createAIAccount(aiAccountBody("Fresh"))
	h.seedAIApp("i_ai", "AI Demo", admin.ID, old.ID)
	// Rebound to fresh just before the delete.
	if err := h.st.SetInstanceConfigAndAIBindings("i_ai", []store.InstanceConfig{
		{AppEnv: "ACME_API_KEY", Value: "sk-fresh", Secret: true}, {AppEnv: "ACME_MODEL", Value: "acme-1"},
	}, []string{"ai.acme"}, []store.AIBinding{{Slot: "ai.acme", AccountID: fresh.ID}}, nil); err != nil {
		t.Fatal(err)
	}
	h.elevate("pass1")
	if code, raw := h.doRaw("DELETE", "/api/v1/ai-accounts/"+old.ID, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", code, raw)
	}
	if v := h.storedValues("i_ai"); v["ACME_API_KEY"] != "sk-fresh" || v["ACME_MODEL"] != "acme-1" {
		t.Fatalf("values = %v; the new account's values must stay", v)
	}
}

// A delete is refused, and audited as a failure, when a bound app's manifest
// copy cannot be read: its key would otherwise stay in the app.
func TestAIAccountDeleteRefusedWhenManifestUnreadable(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	acct := h.createAIAccount(aiAccountBody("Work"))
	h.seedAIApp("i_ai", "Broken App", admin.ID, acct.ID)
	if err := os.Remove(filepath.Join(h.stateDir, "instances", "i_ai", "manifest.yml")); err != nil {
		t.Fatal(err)
	}
	h.elevate("pass1")
	code, raw := h.doRaw("DELETE", "/api/v1/ai-accounts/"+acct.ID, nil)
	if code != http.StatusInternalServerError || !strings.Contains(string(raw), "the LLM provider settings of Broken App could not be found") {
		t.Fatalf("delete = %d %s", code, raw)
	}
	if _, err := h.st.GetAIAccount(acct.ID); err != nil {
		t.Fatalf("the account was deleted anyway: %v", err)
	}
	if v := h.storedValues("i_ai"); v["ACME_API_KEY"] != testAIKey {
		t.Fatalf("values changed on a refused delete: %v", v)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountDelete, acct.ID, false) {
		t.Fatal("refused delete was not audited")
	}
}

// --- account changes that need no provider data ---------------------------

// A key change reaches the apps even when the box has no provider data (right
// after a boot, or with the catalog service down): the key, the stored model
// and an account base URL need none.
func TestAIAccountKeyChangeWithoutProviderData(t *testing.T) {
	h := newHarness(t) // a disk catalog: the provider list is empty
	admin := h.setupAdmin("alice", "pass1")
	now := time.Now()
	if err := h.st.CreateAIAccount(store.AIAccount{ID: "ai_1", OwnerUserID: admin.ID, ProviderID: "acme", Label: "Work", APIKey: testAIKey, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	h.seedAIApp("i_ai", "AI Demo", admin.ID, "ai_1")

	code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/ai_1", map[string]any{"provider_id": "acme", "label": "Work", "api_key": "sk-new-key"})
	if code != http.StatusOK {
		t.Fatalf("key change = %d %s", code, raw)
	}
	saved := decodeRaw[AIAccountSavedDTO](t, raw)
	if done := awaitJob(t, h.apiSrv, saved.JobID); done.Status != "completed" {
		t.Fatalf("job = %s %+v", done.Status, done.Error)
	}
	if v := h.storedValues("i_ai"); v["ACME_API_KEY"] != "sk-new-key" || v["ACME_MODEL"] != "acme-1" || v["SEARCH_KEY"] != "s" {
		t.Fatalf("values = %v", v)
	}
	if _, ok := h.storedValues("i_ai")["ACME_BASE_URL"]; ok {
		t.Fatal("a native base URL was filled without an account base URL")
	}
}

// Removing an account's base URL is refused while an app's compatible slot
// would have nothing to take in its place, so a key changed in the same edit
// is never saved without reaching the app. With the provider's address in
// the data, the removal goes through and the app gets that address.
func TestAIAccountBaseURLRemoval(t *testing.T) {
	bindCompatible := func(h *harness, id, accountID string) {
		t.Helper()
		if err := h.st.SetInstanceAIBindings(id, []store.AIBinding{{Slot: "ai.openai_compatible", AccountID: accountID, Models: map[string][]string{"models.chat": {"a"}}}}); err != nil {
			t.Fatal(err)
		}
		if err := h.st.SetInstanceConfig(id, []store.InstanceConfig{{AppEnv: "CUSTOM_BASE_URL", Value: "https://proxy.invalid/v1"}}); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"provider_id": "acme", "label": "Work", "api_key": "sk-new-key"}

	t.Run("no provider data", func(t *testing.T) {
		h := newHarness(t) // a disk catalog: the provider list is empty
		admin := h.setupAdmin("alice", "pass1")
		now := time.Now()
		if err := h.st.CreateAIAccount(store.AIAccount{ID: "ai_1", OwnerUserID: admin.ID, ProviderID: "acme", Label: "Work", APIKey: testAIKey, BaseURL: "https://proxy.invalid/v1", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		h.seedAIApp("i_ai", "AI Demo", admin.ID, "")
		bindCompatible(h, "i_ai", "ai_1")

		code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/ai_1", body)
		if code != http.StatusConflict {
			t.Fatalf("removal = %d %s; want 409", code, raw)
		}
		if a, _ := h.st.GetAIAccount("ai_1"); a.APIKey != testAIKey || a.BaseURL == "" {
			t.Fatalf("account = %+v; want it unchanged", a)
		}
		// The key alone still changes.
		body := map[string]any{"provider_id": "acme", "label": "Work", "api_key": "sk-new-key", "base_url": "https://proxy.invalid/v1"}
		if code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/ai_1", body); code != http.StatusOK {
			t.Fatalf("key change = %d %s", code, raw)
		}
	})

	t.Run("provider has no address", func(t *testing.T) {
		h, _ := aiHarness(t) // acme has no openai_base_url in this data
		admin := h.setupAdmin("alice", "pass1")
		acct := h.createAIAccount(map[string]any{"provider_id": "acme", "label": "Work", "api_key": testAIKey, "base_url": "https://proxy.invalid/v1"})
		h.seedAIApp("i_ai", "AI Demo", admin.ID, "")
		bindCompatible(h, "i_ai", acct.ID)

		if code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/"+acct.ID, body); code != http.StatusConflict {
			t.Fatalf("removal = %d %s; want 409", code, raw)
		}
	})

	t.Run("provider address in the data", func(t *testing.T) {
		h, _ := aiHarness(t)
		admin := h.setupAdmin("alice", "pass1")
		acct := h.createAIAccount(map[string]any{"provider_id": "plain", "label": "Work", "api_key": testAIKey, "base_url": "https://proxy.invalid/v1"})
		h.seedAIApp("i_ai", "AI Demo", admin.ID, "")
		bindCompatible(h, "i_ai", acct.ID)

		code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/"+acct.ID, map[string]any{"provider_id": "plain", "label": "Work", "api_key": "sk-new-key"})
		if code != http.StatusOK {
			t.Fatalf("removal = %d %s", code, raw)
		}
		if done := awaitJob(t, h.apiSrv, decodeRaw[AIAccountSavedDTO](t, raw).JobID); done.Status != "completed" {
			t.Fatalf("job = %s %+v", done.Status, done.Error)
		}
		if v := h.storedValues("i_ai"); v["CUSTOM_BASE_URL"] != "https://api.example.invalid/v1" || v["CUSTOM_API_KEY"] != "sk-new-key" {
			t.Fatalf("values = %v", v)
		}
	})
}

func TestRestampSlotRules(t *testing.T) {
	man := parseAIAppManifest(t)
	slots := fillableSlots(man)
	restamp := func(fields []slotField, b store.AIBinding, acct store.AIAccount, providers []catalog.AIProvider, removedURL string, cur map[string]string) map[string]string {
		t.Helper()
		got, err := restampSlot(fields, b, acct, providers, removedURL, cur)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	b := store.AIBinding{Slot: "ai.openai_compatible", Models: map[string][]string{"models.chat": {"a", "b"}}}
	cur := map[string]string{"CUSTOM_BASE_URL": "https://kept.invalid/v1", "CUSTOM_EMBED": "kept-embed"}

	// No provider data, no account base URL: the stored base URL and the
	// stored embedding value are kept, the model list comes from the binding.
	got := restamp(slots["ai.openai_compatible"], b, testAccounts["a_acme"], nil, "", cur)
	assertValues(t, got, map[string]string{"CUSTOM_API_KEY": "sk-acme", "CUSTOM_BASE_URL": "https://kept.invalid/v1", "CUSTOM_MODELS": "a;b", "CUSTOM_EMBED": "kept-embed"})

	// With provider data the provider's endpoint wins over the stored one.
	got = restamp(slots["ai.openai_compatible"], b, testAccounts["a_acme"], testAIProviders(), "", cur)
	if got["CUSTOM_BASE_URL"] != "https://api.acme.invalid/v1" {
		t.Fatalf("values = %v", got)
	}
	// An account base URL wins over both.
	got = restamp(slots["ai.openai_compatible"], b, testAccounts["a_acme_proxy"], testAIProviders(), "", cur)
	if got["CUSTOM_BASE_URL"] != "https://proxy.invalid/v1" {
		t.Fatalf("values = %v", got)
	}
	// A native slot's base URL stays blank without an account base URL.
	got = restamp(slots["ai.acme"], store.AIBinding{Slot: "ai.acme"}, testAccounts["a_acme"], nil, "", map[string]string{"ACME_BASE_URL": "https://old.invalid", "ACME_MODEL": "m"})
	assertValues(t, got, map[string]string{"ACME_API_KEY": "sk-acme", "ACME_MODEL": "m"})

	// The edit removed the account's base URL, the app still holds it, and
	// there is no provider data: the app fails instead of keeping the
	// removed endpoint.
	if _, err := restampSlot(slots["ai.openai_compatible"], b, testAccounts["a_acme"], nil, "https://kept.invalid/v1", cur); err == nil {
		t.Fatal("the removed base URL was kept")
	}
	// A removed URL the app does not hold does not fail it.
	got = restamp(slots["ai.openai_compatible"], b, testAccounts["a_acme"], nil, "https://other.invalid/v1", cur)
	if got["CUSTOM_BASE_URL"] != "https://kept.invalid/v1" {
		t.Fatalf("values = %v", got)
	}
	// With provider data the removed URL is replaced, so nothing fails.
	got = restamp(slots["ai.openai_compatible"], b, testAccounts["a_acme"], testAIProviders(), "https://kept.invalid/v1", cur)
	if got["CUSTOM_BASE_URL"] != "https://api.acme.invalid/v1" {
		t.Fatalf("values = %v", got)
	}
}

// A binding that recorded its fields is cleared by those names, so the
// delete needs no manifest. A binding from before that record, whose manifest
// no longer has the slot, refuses the delete.
func TestAIAccountDeleteUsesRecordedFields(t *testing.T) {
	h, _ := aiHarness(t)
	admin := h.setupAdmin("alice", "pass1")
	acct := h.createAIAccount(aiAccountBody("Work"))
	legacy := h.createAIAccount(aiAccountBody("Legacy"))
	h.seedAIApp("i_new", "Recorded", admin.ID, "")
	if err := h.st.SetInstanceAIBindings("i_new", []store.AIBinding{{Slot: "ai.acme", AccountID: acct.ID, Envs: []string{"ACME_API_KEY", "ACME_BASE_URL", "ACME_MODEL"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.stateDir, "instances", "i_new", "manifest.yml")); err != nil {
		t.Fatal(err)
	}
	h.elevate("pass1")
	code, raw := h.doRaw("DELETE", "/api/v1/ai-accounts/"+acct.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("delete = %d %s", code, raw)
	}
	if v := h.storedValues("i_new"); len(v) != 1 || v["SEARCH_KEY"] != "s" {
		t.Fatalf("values = %v", v)
	}

	// A legacy row (no recorded fields) on an app whose manifest lost the slot.
	h.seedAIApp("i_old", "Updated App", admin.ID, "")
	if err := h.st.SetInstanceAIBindings("i_old", []store.AIBinding{{Slot: "ai.gone", AccountID: legacy.ID}}); err != nil {
		t.Fatal(err)
	}
	code, raw = h.doRaw("DELETE", "/api/v1/ai-accounts/"+legacy.ID, nil)
	if code != http.StatusInternalServerError || !strings.Contains(string(raw), "settings of Updated App could not be found") {
		t.Fatalf("delete = %d %s", code, raw)
	}
	if _, err := h.st.GetAIAccount(legacy.ID); err != nil {
		t.Fatalf("the account was deleted anyway: %v", err)
	}
}

// An install and a config save record the fields each binding fills.
func TestBindingsRecordTheirFields(t *testing.T) {
	_, res, err := putTest(t, nil, nil, nil, AIBindingBody{Slot: "ai.acme", AccountID: "a_acme"})
	if err != nil {
		t.Fatal(err)
	}
	if e := res.bindings[0].Envs; len(e) != 3 || e[0] != "ACME_API_KEY" || e[2] != "ACME_MODEL" {
		t.Fatalf("recorded envs = %v", e)
	}
}
