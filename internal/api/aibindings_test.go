package api

// Filling AI slots from accounts at install (INSTALL_SETUP.md # 1 and # 5):
// how a binding resolves into app_env values, every 422 it can raise, and the
// install handler's ownership check and audit.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// aiAppManifestYML has a native slot (protocol acme), a compatible slot with a
// model list and a single embedding model, and one plain field.
const aiAppManifestYML = `
id: aiapp
manifest_version: 1
name: AI Demo
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [aiapp]
config:
  - app_env: ACME_API_KEY
    title: "Acme key"
    description: "d"
    secret: true
    role: ai.acme.api_key
  - app_env: ACME_BASE_URL
    title: "Acme address"
    description: "d"
    role: ai.acme.base_url
  - app_env: ACME_MODEL
    title: "Acme model"
    description: "d"
    role: ai.acme.model.chat
  - app_env: CUSTOM_API_KEY
    title: "Custom key"
    description: "d"
    secret: true
    role: ai.openai_compatible.api_key
  - app_env: CUSTOM_BASE_URL
    title: "Custom address"
    description: "d"
    role: ai.openai_compatible.base_url
  - app_env: CUSTOM_MODELS
    title: "Custom models"
    description: "d"
    role: ai.openai_compatible.models.chat
    separator: ";"
  - app_env: CUSTOM_EMBED
    title: "Custom embedding model"
    description: "d"
    role: ai.openai_compatible.model.embedding
  - app_env: SEARCH_KEY
    title: "Search key"
    description: "d"
requires:
  - one_of: [ai]
`

func testAIProviders() []catalog.AIProvider {
	return []catalog.AIProvider{
		{
			ID: "acme", Name: "Acme", NativeProtocol: "acme", OpenAIBaseURL: "https://api.acme.invalid/v1",
			Defaults: map[string]string{"chat": "acme-1", "embedding": "acme-embed"},
			Models: []catalog.AIModel{
				{ID: "acme-1", Name: "Acme 1", Types: []string{"chat"}},
				{ID: "acme-embed", Name: "Acme Embed", Types: []string{"embedding"}},
			},
		},
		{
			ID: "plain", Name: "Plain", OpenAIBaseURL: "https://api.plain.invalid/v1",
			Defaults: map[string]string{"chat": "plain-1"},
			Models:   []catalog.AIModel{{ID: "plain-1", Name: "Plain 1", Types: []string{"chat"}}},
		},
		{ID: "nobase", Name: "No Base", NativeProtocol: "nobase"},
	}
}

// testAccounts is the caller's accounts. "gone" is a provider that has left
// the provider data since the account was made.
var testAccounts = map[string]store.AIAccount{
	"a_acme":       {ID: "a_acme", Label: "Acme", ProviderID: "acme", APIKey: "sk-acme"},
	"a_acme_proxy": {ID: "a_acme_proxy", Label: "Acme proxy", ProviderID: "acme", APIKey: "sk-acme", BaseURL: "https://proxy.invalid/v1"},
	"a_plain":      {ID: "a_plain", Label: "Plain", ProviderID: "plain", APIKey: "sk-plain"},
	"a_nobase":     {ID: "a_nobase", Label: "No Base", ProviderID: "nobase", APIKey: "sk-nb"},
	"a_other":      {ID: "a_other", Label: "Home server", ProviderID: "openai_compatible", BaseURL: "http://192.168.1.10:11434/v1"},
	"a_other_key":  {ID: "a_other_key", Label: "Home server 2", ProviderID: "openai_compatible", APIKey: "sk-local", BaseURL: "http://192.168.1.11/v1"},
	"a_gone":       {ID: "a_gone", Label: "Gone", ProviderID: "gone", APIKey: "sk-gone"},
	"a_gone_url":   {ID: "a_gone_url", Label: "Gone with URL", ProviderID: "gone", APIKey: "sk-gone", BaseURL: "https://gone.invalid/v1"},
}

func lookupTestAccount(id string) (store.AIAccount, error) {
	a, ok := testAccounts[id]
	if !ok {
		return store.AIAccount{}, store.ErrNotFound
	}
	return a, nil
}

func parseAIAppManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	man, err := manifest.Parse([]byte(aiAppManifestYML))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return man
}

// resolveTest runs the install resolution with the test accounts and
// providers, and returns the config as a map plus the bindings.
func resolveTest(t *testing.T, fields map[string]string, bindings ...AIBindingBody) (map[string]string, []store.AIBinding, error) {
	t.Helper()
	cfg, bs, err := resolveInstallWithAI(parseAIAppManifest(t), fields, bindings, lookupTestAccount, testAIProviders())
	if err != nil {
		return nil, nil, err
	}
	out := map[string]string{}
	for _, c := range cfg {
		out[c.AppEnv] = c.Value
		if strings.HasSuffix(c.AppEnv, "_API_KEY") != c.Secret {
			t.Errorf("%s secret flag = %v", c.AppEnv, c.Secret)
		}
	}
	return out, bs, nil
}

func assertValues(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("values = %v; want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("values = %v; want %v", got, want)
		}
	}
}

func TestResolveAIBindings_Values(t *testing.T) {
	t.Run("native with the provider default", func(t *testing.T) {
		got, bs, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.acme", AccountID: "a_acme"})
		if err != nil {
			t.Fatal(err)
		}
		// A native base_url stays blank without an account base URL.
		assertValues(t, got, map[string]string{"ACME_API_KEY": "sk-acme", "ACME_MODEL": "acme-1"})
		if len(bs) != 1 || bs[0].Slot != "ai.acme" || bs[0].AccountID != "a_acme" || bs[0].Models["model.chat"][0] != "acme-1" {
			t.Fatalf("bindings = %+v", bs)
		}
	})
	t.Run("native with an account base URL and a typed model", func(t *testing.T) {
		got, _, err := resolveTest(t, nil, AIBindingBody{Slot: " ai.acme ", AccountID: "a_acme_proxy",
			Models: map[string][]string{"model.chat": {"  acme-next "}}})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{"ACME_API_KEY": "sk-acme", "ACME_BASE_URL": "https://proxy.invalid/v1", "ACME_MODEL": "acme-next"})
	})
	t.Run("compatible through a listed provider, lists joined", func(t *testing.T) {
		got, bs, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_plain",
			Models: map[string][]string{"models.chat": {"plain-1", "plain-2"}, "model.embedding": {"e1"}}})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{
			"CUSTOM_API_KEY": "sk-plain", "CUSTOM_BASE_URL": "https://api.plain.invalid/v1",
			"CUSTOM_MODELS": "plain-1;plain-2", "CUSTOM_EMBED": "e1",
		})
		if m := bs[0].Models; len(m["models.chat"]) != 2 || m["model.embedding"][0] != "e1" {
			t.Fatalf("stored models = %+v", m)
		}
	})
	t.Run("compatible through a native provider, defaults for both types", func(t *testing.T) {
		got, _, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_acme"})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{
			"CUSTOM_API_KEY": "sk-acme", "CUSTOM_BASE_URL": "https://api.acme.invalid/v1",
			"CUSTOM_MODELS": "acme-1", "CUSTOM_EMBED": "acme-embed",
		})
	})
	t.Run("compatible with an account base URL override", func(t *testing.T) {
		got, _, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_acme_proxy"})
		if err != nil {
			t.Fatal(err)
		}
		if got["CUSTOM_BASE_URL"] != "https://proxy.invalid/v1" {
			t.Fatalf("base URL = %q", got["CUSTOM_BASE_URL"])
		}
	})
	t.Run("Other without a key injects no key", func(t *testing.T) {
		got, _, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_other",
			Models: map[string][]string{"models.chat": {"llama3"}, "model.embedding": {"nomic"}}})
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, got, map[string]string{
			"CUSTOM_BASE_URL": "http://192.168.1.10:11434/v1", "CUSTOM_MODELS": "llama3", "CUSTOM_EMBED": "nomic",
		})
	})
	t.Run("Other with a key", func(t *testing.T) {
		got, _, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_other_key",
			Models: map[string][]string{"models.chat": {"llama3"}, "model.embedding": {"nomic"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got["CUSTOM_API_KEY"] != "sk-local" || got["CUSTOM_BASE_URL"] != "http://192.168.1.11/v1" {
			t.Fatalf("values = %v", got)
		}
	})
	t.Run("provider gone, compatible slot, account has a base URL", func(t *testing.T) {
		got, _, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_gone_url",
			Models: map[string][]string{"models.chat": {"g1"}, "model.embedding": {"g2"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got["CUSTOM_BASE_URL"] != "https://gone.invalid/v1" || got["CUSTOM_API_KEY"] != "sk-gone" {
			t.Fatalf("values = %v", got)
		}
	})
	t.Run("two slots at once, plus a plain field", func(t *testing.T) {
		got, bs, err := resolveTest(t, map[string]string{"SEARCH_KEY": "k"},
			AIBindingBody{Slot: "ai.acme", AccountID: "a_acme"},
			AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_acme"})
		if err != nil {
			t.Fatal(err)
		}
		if got["SEARCH_KEY"] != "k" || got["ACME_API_KEY"] != "sk-acme" || got["CUSTOM_API_KEY"] != "sk-acme" || len(bs) != 2 {
			t.Fatalf("values = %v bindings = %+v", got, bs)
		}
	})
}

func TestResolveAIBindings_422(t *testing.T) {
	compat := func(acct string, models map[string][]string) AIBindingBody {
		return AIBindingBody{Slot: "ai.openai_compatible", AccountID: acct, Models: models}
	}
	cases := []struct {
		name   string
		fields map[string]string
		bs     []AIBindingBody
		msg    string
	}{
		{"unknown slot", nil, []AIBindingBody{{Slot: "ai.nope", AccountID: "a_acme"}}, `config.ai_bindings: this app has no AI slot "ai.nope"`},
		{"slot twice", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme"}, {Slot: "ai.acme", AccountID: "a_acme"}}, "config.ai_bindings: slot ai.acme is given more than once"},
		{"no account id", nil, []AIBindingBody{{Slot: "ai.acme"}}, "config.ai_bindings: pick an AI account for slot ai.acme"},
		{"missing account", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_nope"}}, "config.ai_bindings: no such AI account"},
		{"listed provider on the wrong native slot", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_plain"}}, `config.ai_bindings: the account "Plain" does not work with this app's ai.acme slot`},
		{"Other on a native slot", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_other"}}, `the account "Home server" does not work with this app's ai.acme slot`},
		{"no compatible endpoint", nil, []AIBindingBody{compat("a_nobase", nil)}, `the account "No Base" does not work with this app's ai.openai_compatible slot`},
		{"provider gone, native slot", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_gone_url"}}, `config.ai_bindings: the provider of the account "Gone with URL" is not in the provider list now, so it cannot fill the ai.acme slot`},
		{"provider gone, no base URL", nil, []AIBindingBody{compat("a_gone", nil)}, `config.ai_bindings: the provider of the account "Gone" is not in the provider list now. Give the account its own base URL, or pick another account`},
		{"undeclared model setting", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme", Models: map[string][]string{"models.chat": {"x"}}}}, "config.ai_bindings: the ai.acme slot of this app does not take models.chat"},
		{"no default for a compatible account", nil, []AIBindingBody{compat("a_other", map[string][]string{"models.chat": {"x"}})}, "config.ai_bindings: pick a model for Custom embedding model"},
		{"no default for a type the provider lacks", nil, []AIBindingBody{compat("a_plain", nil)}, "config.ai_bindings: pick a model for Custom embedding model"},
		{"two ids for one model", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme", Models: map[string][]string{"model.chat": {"a", "b"}}}}, "config.ai_bindings: pick exactly one model for Acme model"},
		{"no id for one model", nil, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme", Models: map[string][]string{"model.chat": {}}}}, "config.ai_bindings: pick exactly one model for Acme model"},
		{"empty list", nil, []AIBindingBody{compat("a_acme", map[string][]string{"models.chat": {}})}, "config.ai_bindings: pick at least one model for Custom models"},
		{"blank id", nil, []AIBindingBody{compat("a_acme", map[string][]string{"models.chat": {"  "}})}, "config.ai_bindings: a model name for Custom models is empty"},
		{"control character", nil, []AIBindingBody{compat("a_acme", map[string][]string{"models.chat": {"a\nb"}})}, "config.ai_bindings: a model name for Custom models must not contain line breaks or control characters"},
		{"same id twice", nil, []AIBindingBody{compat("a_acme", map[string][]string{"models.chat": {"a", " a"}})}, `config.ai_bindings: model "a" is picked twice for Custom models`},
		{"separator in an id", nil, []AIBindingBody{compat("a_acme", map[string][]string{"models.chat": {"a;b"}})}, `config.ai_bindings: model "a;b" for Custom models contains ";", which this app uses to separate models`},
		{"binding and a typed value on one slot", map[string]string{"ACME_MODEL": "typed"}, []AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme"}}, "config.fields: ACME_MODEL is filled from an AI account, so do not also send a value for it"},
		{"requires still checked", map[string]string{"SEARCH_KEY": "k"}, nil, "config.fields: pick at least one AI provider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := resolveTest(t, c.fields, c.bs...)
			if err == nil {
				t.Fatalf("want 422 %q, got nil", c.msg)
			}
			assert422(t, err, c.msg)
		})
	}
}

// A separator inside a single-model id is fine: only a list is split.
func TestResolveAIBindings_SeparatorOnlyForLists(t *testing.T) {
	got, _, err := resolveTest(t, nil, AIBindingBody{Slot: "ai.openai_compatible", AccountID: "a_acme",
		Models: map[string][]string{"model.embedding": {"a;b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got["CUSTOM_EMBED"] != "a;b" {
		t.Fatalf("CUSTOM_EMBED = %q", got["CUSTOM_EMBED"])
	}
}

// A binding satisfies requires: [ai], and an empty typed value on a bound
// slot is the same as no value. Typed values for role fields without a binding
// still work, for a box with no provider data.
func TestResolveAIBindings_RequiresAndPlainFallback(t *testing.T) {
	if _, _, err := resolveTest(t, map[string]string{"ACME_API_KEY": ""}, AIBindingBody{Slot: "ai.acme", AccountID: "a_acme"}); err != nil {
		t.Fatalf("binding alone must satisfy requires: %v", err)
	}
	got, bs, err := resolveTest(t, map[string]string{"ACME_API_KEY": "sk-typed", "CUSTOM_MODELS": "a;b"})
	if err != nil {
		t.Fatalf("typed role values: %v", err)
	}
	if got["ACME_API_KEY"] != "sk-typed" || got["CUSTOM_MODELS"] != "a;b" || len(bs) != 0 {
		t.Fatalf("typed values = %v bindings = %+v", got, bs)
	}
}

// A failed account lookup other than not-found is a 500, not a 422.
func TestResolveAIBindings_LookupError500(t *testing.T) {
	boom := func(string) (store.AIAccount, error) { return store.AIAccount{}, errors.New("disk on fire") }
	_, _, err := resolveInstallWithAI(parseAIAppManifest(t), nil,
		[]AIBindingBody{{Slot: "ai.acme", AccountID: "a_acme"}}, boom, testAIProviders())
	assertStatus(t, err, http.StatusInternalServerError)
}

// aiInstallHarness is a harness whose catalog holds aiapp, and whose provider
// data holds acme, so POST /api/v1/apps resolves bindings up to the job. The
// install reads the app from the lifecycle's catalog directory and the
// providers from s.catalog. The job body is never reached: every request here
// is refused first.
func aiInstallHarness(t *testing.T) *harness {
	t.Helper()
	snap := `{"schema_version":2,"version":"v1","apps":[],
		"ai_providers":[{"id":"acme","name":"Acme","native_protocol":"acme","openai_base_url":"https://api.acme.invalid/v1",
		 "defaults":{"chat":"acme-1"},"models":[{"id":"acme-1","name":"Acme 1","types":["chat"]}]}]}`
	seed := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seed, []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := catalog.NewRemote(catalog.RemoteOptions{
		BaseURL: "http://127.0.0.1:1", Environment: "hosted",
		AssetCacheDir: t.TempDir(), SnapshotFile: seed,
	})
	h := newHarness(t, func(s *Server) { s.catalog = cat })
	writeManifestFixture(t, h.catalogDir, "aiapp", aiAppManifestYML)
	return h
}

// Another user's account id is refused like a missing one, and the refused
// install is audited as a failure.
func TestInstallAIBindingOtherUsersAccount422(t *testing.T) {
	h := aiInstallHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")
	now := time.Now()
	if err := h.st.CreateAIAccount(store.AIAccount{
		ID: "ai_bob", OwnerUserID: "u_bob", ProviderID: "acme", Label: "Bob's", APIKey: "sk-bob", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"ai_bob", "ai_missing"} {
		code, raw := h.doRaw("POST", "/api/v1/apps", map[string]any{
			"manifest_id": "aiapp",
			"config":      map[string]any{"ai_bindings": []map[string]any{{"slot": "ai.acme", "account_id": id}}},
		})
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "config.ai_bindings: no such AI account") {
			t.Fatalf("install with account %s = %d %s; want 422 no such AI account", id, code, raw)
		}
	}
	if !h.hasAuditEvent(audit.ActionAppInstall, "", false) {
		t.Fatal("refused install was not audited as a failure")
	}
	// Bob's own account still resolves for bob, so the refusal above was about
	// the owner, not the account. It fails later, on the plain-value clash.
	h.loginAs("bob", "bobpass")
	code, raw := h.doRaw("POST", "/api/v1/apps", map[string]any{
		"manifest_id": "aiapp",
		"config": map[string]any{
			"ai_bindings": []map[string]any{{"slot": "ai.acme", "account_id": "ai_bob"}},
			"fields":      map[string]string{"ACME_API_KEY": "typed"},
		},
	})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "ACME_API_KEY is filled from an AI account") {
		t.Fatalf("own account with a clashing value = %d %s", code, raw)
	}
}

// A failed user delete puts back the bindings to the user's AI accounts along
// with the accounts.
func TestDeleteUserRestoresAIBindings(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", deleteFailUser, "pw-bob")
	h.loginAs(deleteFailUser, "pw-bob")
	acct := h.createAIAccount(aiAccountBody("bob ai"))
	if err := h.st.Create(store.Instance{
		ID: "i_1", ManifestID: "aiapp", Name: "AI Demo", Slug: "aiapp--bob", Version: "1.0",
		State: "running", OwnerUserID: "u_bob", Scope: store.ScopePersonal, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetInstanceAIBindings("i_1", []store.AIBinding{{Slot: "ai.acme", AccountID: acct.ID, Models: map[string][]string{"model.chat": {"acme-1"}}}}); err != nil {
		t.Fatal(err)
	}

	h.loginAs("alice", "pass1")
	h.elevate("pass1")
	if code, _ := h.doRaw("DELETE", "/api/v1/users/u_bob", nil); code != http.StatusBadGateway {
		t.Fatalf("delete with host failure = %d; want 502", code)
	}
	got, err := h.st.ListInstanceAIBindings("i_1")
	if err != nil || len(got) != 1 || got[0].AccountID != acct.ID || got[0].Models["model.chat"][0] != "acme-1" {
		t.Fatalf("rollback lost the AI bindings: %+v (%v)", got, err)
	}
}
