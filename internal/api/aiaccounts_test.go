package api

// API-boundary tests for AI provider accounts: per-user ownership, the
// elevation fence on delete only, the key never leaving the box, audit on
// success and failure, the validation rules, and the user-delete rollback.

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/store"
)

// testAIKey is the key every listed-provider body carries. Tests look for it
// in raw response bodies to prove it never comes back.
const testAIKey = "sk-acme-very-secret-key"

func aiAccountBody(label string) map[string]any {
	return map[string]any{"provider_id": "acme", "label": label, "api_key": testAIKey}
}

// createAIAccount creates an account via the API and returns its DTO. It
// also checks the raw body holds no key.
func (h *harness) createAIAccount(body map[string]any) AIAccountDTO {
	h.t.Helper()
	resp := h.do("POST", "/api/v1/ai-accounts", body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("create ai account %v: %d %s", body["label"], resp.StatusCode, raw)
	}
	assertNoKey(h.t, raw)
	return decodeRaw[AIAccountDTO](h.t, raw)
}

func decodeRaw[T any](t *testing.T, raw []byte) T {
	t.Helper()
	return decodeJSON[T](t, &http.Response{Body: io.NopCloser(strings.NewReader(string(raw)))})
}

// assertNoKey fails when a response body holds a key or an api_key field.
func assertNoKey(t *testing.T, raw []byte) {
	t.Helper()
	for _, bad := range []string{testAIKey, "sk-new-key", "api_key"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("response holds %q: %s", bad, raw)
		}
	}
}

// doRaw sends a request and returns the status and the raw body.
func (h *harness) doRaw(method, path string, body any) (int, []byte) {
	h.t.Helper()
	resp := h.do(method, path, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw
}

// A member manages their own accounts: add and edit without a re-prompt, and
// list them. The key never comes back, and an empty key on edit keeps it.
func TestAIAccountsMemberManagesOwn(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	a := h.createAIAccount(aiAccountBody("  Work  ")) // no h.elevate
	if a.Label != "Work" || a.ProviderID != "acme" || !a.KeySet || a.BaseURL != "" || a.CreatedAt == 0 || a.UpdatedAt == 0 {
		t.Fatalf("created = %+v", a)
	}
	stored, err := h.st.GetAIAccount(a.ID)
	if err != nil || stored.OwnerUserID != "u_bob" || stored.APIKey != testAIKey {
		t.Fatalf("stored = %+v (%v); want owner u_bob with the key", stored, err)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountCreate, a.ID, true) {
		t.Error("create did not audit success")
	}

	// Edit without a key keeps the stored one.
	code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/"+a.ID, map[string]any{
		"provider_id": "acme", "label": "Work 2", "base_url": "https://proxy.example.com/v1",
	})
	if code != http.StatusOK {
		t.Fatalf("edit own without elevation = %d %s; want 200", code, raw)
	}
	assertNoKey(t, raw)
	if got, _ := h.st.GetAIAccount(a.ID); got.APIKey != testAIKey || got.Label != "Work 2" || got.BaseURL != "https://proxy.example.com/v1" {
		t.Fatalf("after edit without key = %+v", got)
	}
	// Edit with a key replaces it.
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/"+a.ID, map[string]any{
		"provider_id": "acme", "label": "Work 2", "api_key": "sk-new-key",
	})
	if code != http.StatusOK {
		t.Fatalf("edit with key = %d %s", code, raw)
	}
	assertNoKey(t, raw)
	if got, _ := h.st.GetAIAccount(a.ID); got.APIKey != "sk-new-key" || got.BaseURL != "" {
		t.Fatalf("after edit with key = %+v", got)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountUpdate, a.ID, true) {
		t.Error("update did not audit success")
	}

	code, raw = h.doRaw("GET", "/api/v1/ai-accounts", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	assertNoKey(t, raw)
	list := decodeRaw[struct {
		Accounts []AIAccountDTO `json:"accounts"`
	}](t, raw)
	if len(list.Accounts) != 1 || list.Accounts[0].Label != "Work 2" || !list.Accounts[0].KeySet {
		t.Fatalf("list = %+v", list.Accounts)
	}

	// Audit metadata carries no key either.
	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Metadata, testAIKey) || strings.Contains(e.Metadata, "sk-new-key") {
			t.Fatalf("audit row holds the key: %+v", e)
		}
	}
}

// An empty list is [] and never null.
func TestListAIAccountsEmpty(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	code, raw := h.doRaw("GET", "/api/v1/ai-accounts", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), `"accounts":[]`) {
		t.Fatalf("empty list = %d %s", code, raw)
	}
}

func TestAIAccountsRequireAuth(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	jar, _ := newJar()
	h.jar = jar

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/ai-accounts"},
		{"POST", "/api/v1/ai-accounts"},
		{"PUT", "/api/v1/ai-accounts/x"},
		{"DELETE", "/api/v1/ai-accounts/x"},
	} {
		code, _ := h.doRaw(c.method, c.path, aiAccountBody("x"))
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d; want 401", c.method, c.path, code)
		}
	}
}

// Accounts belong to one user. Another user's account is invisible on the
// list and answers 404 on edit and delete, the same as a missing id, and the
// attempt audits a failure.
func TestAIAccountOwnershipScoping(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")
	alices := h.createAIAccount(aiAccountBody("Claude"))

	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	code, raw := h.doRaw("GET", "/api/v1/ai-accounts", nil)
	if code != http.StatusOK || strings.Contains(string(raw), alices.ID) || strings.Contains(string(raw), "Claude") {
		t.Fatalf("bob's list shows alice's account: %d %s", code, raw)
	}

	// The same label is free for bob: labels are unique per owner.
	bobs := h.createAIAccount(aiAccountBody("Claude"))

	code, _ = h.doRaw("PUT", "/api/v1/ai-accounts/"+alices.ID, aiAccountBody("mine now"))
	if code != http.StatusNotFound {
		t.Errorf("edit another user's account = %d; want 404", code)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountUpdate, alices.ID, false) {
		t.Error("edit of another user's account did not audit a failure")
	}
	h.elevate("bobpass")
	code, _ = h.doRaw("DELETE", "/api/v1/ai-accounts/"+alices.ID, nil)
	if code != http.StatusNotFound {
		t.Errorf("delete another user's account = %d; want 404", code)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountDelete, alices.ID, false) {
		t.Error("delete of another user's account did not audit a failure")
	}
	if got, err := h.st.GetAIAccount(alices.ID); err != nil || got.Label != "Claude" || got.OwnerUserID == "u_bob" {
		t.Fatalf("another user's account changed: %+v, %v", got, err)
	}

	// A missing id answers the same way.
	code, _ = h.doRaw("PUT", "/api/v1/ai-accounts/nope", aiAccountBody("x"))
	if code != http.StatusNotFound {
		t.Errorf("edit missing = %d; want 404", code)
	}

	// An admin gets no more reach into bob's accounts than he has into hers.
	h.loginAs("alice", "pass1")
	h.elevate("pass1")
	code, _ = h.doRaw("DELETE", "/api/v1/ai-accounts/"+bobs.ID, nil)
	if code != http.StatusNotFound {
		t.Errorf("admin deleting a member's account = %d; want 404", code)
	}
}

// Add and edit need no re-prompt. Delete does.
func TestAIAccountElevationOnlyOnDelete(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")
	a := h.createAIAccount(aiAccountBody("Work"))

	code, raw := h.doRaw("DELETE", "/api/v1/ai-accounts/"+a.ID, nil)
	if code != http.StatusForbidden || !strings.Contains(string(raw), "elevation_required") {
		t.Fatalf("delete without elevation = %d %s; want 403 elevation_required", code, raw)
	}
	if _, err := h.st.GetAIAccount(a.ID); err != nil {
		t.Fatalf("un-elevated delete removed the account: %v", err)
	}

	h.elevate("pass1")
	code, _ = h.doRaw("DELETE", "/api/v1/ai-accounts/"+a.ID, nil)
	if code != http.StatusNoContent {
		t.Fatalf("elevated delete = %d; want 204", code)
	}
	if _, err := h.st.GetAIAccount(a.ID); err == nil {
		t.Fatal("account survived the delete")
	}
	if !h.hasAuditEvent(audit.ActionAIAccountDelete, a.ID, true) {
		t.Error("delete did not audit success")
	}
}

func TestAIAccountDuplicateLabel409(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")
	h.createAIAccount(aiAccountBody("Work"))
	other := h.createAIAccount(aiAccountBody("Home"))

	code, raw := h.doRaw("POST", "/api/v1/ai-accounts", aiAccountBody("Work"))
	if code != http.StatusConflict || !strings.Contains(string(raw), "you already have an account with that name") {
		t.Fatalf("duplicate create = %d %s; want 409", code, raw)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountCreate, "", false) {
		t.Error("duplicate create did not audit a failure")
	}
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/"+other.ID, aiAccountBody("Work"))
	if code != http.StatusConflict || !strings.Contains(string(raw), "you already have an account with that name") {
		t.Fatalf("rename to a taken label = %d %s; want 409", code, raw)
	}
	if !h.hasAuditEvent(audit.ActionAIAccountUpdate, other.ID, false) {
		t.Error("conflicting update did not audit a failure")
	}
}

// Each validation rule answers a plain 422 and stores nothing.
func TestAIAccountValidation422(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")

	long := strings.Repeat("a", 101)
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no provider", map[string]any{"provider_id": "  ", "label": "x", "api_key": "k"}, "provider_id is required"},
		{"unknown provider", map[string]any{"provider_id": "nope", "label": "x", "api_key": "k"}, "unknown AI provider"},
		{"no label", map[string]any{"provider_id": "acme", "label": "   ", "api_key": "k"}, "label is required"},
		{"long label", map[string]any{"provider_id": "acme", "label": long, "api_key": "k"}, "label is too long"},
		{"label line break", map[string]any{"provider_id": "acme", "label": "a\nb", "api_key": "k"}, "label must not contain"},
		{"listed without key", map[string]any{"provider_id": "acme", "label": "x"}, "api_key is required for this provider"},
		{"key only spaces", map[string]any{"provider_id": "acme", "label": "x", "api_key": "   "}, "api_key is required for this provider"},
		{"key line break", map[string]any{"provider_id": "acme", "label": "x", "api_key": "sk-a\nEVIL=1"}, "api_key must not contain"},
		{"key control char", map[string]any{"provider_id": "acme", "label": "x", "api_key": "sk-a\x00b"}, "api_key must not contain"},
		{"key inner space", map[string]any{"provider_id": "acme", "label": "x", "api_key": "sk a"}, "api_key must not contain"},
		{"key too long", map[string]any{"provider_id": "acme", "label": "x", "api_key": strings.Repeat("k", 4097)}, "api_key is too long"},
		{"compatible without url", map[string]any{"provider_id": "openai_compatible", "label": "x"}, "base_url is required for an OpenAI-compatible server"},
		{"url not absolute", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "example.com/v1"}, "base_url must be a full http or https address"},
		{"url bad scheme", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "ftp://example.com"}, "base_url must be a full http or https address"},
		{"url no host", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "https:///v1"}, "base_url must be a full http or https address"},
		{"url userinfo", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "https://u:p@example.com/v1"}, "must not contain a user name or password"},
		{"url query", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "https://example.com/v1?a=1"}, "must not contain a query"},
		{"url empty query", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "https://example.com/v1?"}, "must not contain a query"},
		{"url fragment", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "https://example.com/v1#x"}, "must not contain a query"},
		{"url inner space", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "https://exa mple.com"}, "base_url must not contain"},
		{"url too long", map[string]any{"provider_id": "acme", "label": "x", "api_key": "k", "base_url": "https://example.com/" + strings.Repeat("a", 2048)}, "base_url is too long"},
	}
	for _, c := range cases {
		code, raw := h.doRaw("POST", "/api/v1/ai-accounts", c.body)
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), c.want) {
			t.Errorf("%s: %d %s; want 422 %q", c.name, code, raw, c.want)
		}
	}
	if all, _ := h.st.ListAllAIAccounts(); len(all) != 0 {
		t.Fatalf("a refused create stored an account: %+v", all)
	}
	if h.hasAuditEvent(audit.ActionAIAccountCreate, "", false) {
		t.Error("a validation 422 audited")
	}
}

// Accepted shapes: a listed provider with an http override (a LAN server may
// have no TLS), and an OpenAI-compatible server with or without a key.
func TestAIAccountAcceptedShapes(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")

	a := h.createAIAccount(map[string]any{"provider_id": "plain", "label": "Plain", "api_key": " sk-plain ", "base_url": " http://10.0.0.5:8080/v1 "})
	if a.BaseURL != "http://10.0.0.5:8080/v1" {
		t.Fatalf("base_url not trimmed: %q", a.BaseURL)
	}
	if got, _ := h.st.GetAIAccount(a.ID); got.APIKey != "sk-plain" {
		t.Fatalf("key not trimmed: %q", got.APIKey)
	}

	keyless := h.createAIAccount(map[string]any{"provider_id": "openai_compatible", "label": "Home server", "base_url": "http://192.168.1.20:11434/v1"})
	if keyless.KeySet || keyless.ProviderID != "openai_compatible" {
		t.Fatalf("keyless compatible = %+v", keyless)
	}
	keyed := h.createAIAccount(map[string]any{"provider_id": "openai_compatible", "label": "Proxy", "api_key": "sk-proxy", "base_url": "https://llm.example.com/v1"})
	if !keyed.KeySet {
		t.Fatalf("keyed compatible = %+v", keyed)
	}

	// Moving a keyless compatible account to a listed provider needs a key.
	code, raw := h.doRaw("PUT", "/api/v1/ai-accounts/"+keyless.ID, map[string]any{"provider_id": "acme", "label": "Home server"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "api_key is required for this provider") {
		t.Fatalf("switch to listed without key = %d %s", code, raw)
	}
	// Moving a compatible account to an unknown provider is refused.
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/"+keyed.ID, map[string]any{"provider_id": "nope", "label": "Proxy"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "unknown AI provider") {
		t.Fatalf("switch to unknown provider = %d %s", code, raw)
	}
	// Dropping the base URL of a compatible account is refused.
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/"+keyed.ID, map[string]any{"provider_id": "openai_compatible", "label": "Proxy"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "base_url is required") {
		t.Fatalf("compatible without url on update = %d %s", code, raw)
	}
}

// With no provider data (the catalog has not loaded), a listed provider cannot
// be checked and is refused with its own message. An OpenAI-compatible server
// still works, and an existing account can still be renamed.
func TestAIAccountEmptyProviderData(t *testing.T) {
	h := newHarness(t)
	alice := h.setupAdmin("alice", "pass1")

	code, raw := h.doRaw("POST", "/api/v1/ai-accounts", aiAccountBody("Work"))
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "the list of AI providers is not loaded yet") {
		t.Fatalf("listed provider with no data = %d %s", code, raw)
	}
	a := h.createAIAccount(map[string]any{"provider_id": "openai_compatible", "label": "Home", "base_url": "http://192.168.1.20:11434/v1"})

	// An account made while the data was there can still be renamed.
	stored := store.AIAccount{ID: "ai_old", OwnerUserID: alice.ID, ProviderID: "acme", Label: "Old", APIKey: "sk-old"}
	if err := h.st.CreateAIAccount(stored); err != nil {
		t.Fatal(err)
	}
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/ai_old", map[string]any{"provider_id": "acme", "label": "Old renamed"})
	if code != http.StatusOK {
		t.Fatalf("rename with no provider data = %d %s; want 200", code, raw)
	}
	code, raw = h.doRaw("PUT", "/api/v1/ai-accounts/"+a.ID, map[string]any{"provider_id": "acme", "label": "Home", "api_key": "k"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "not loaded yet") {
		t.Fatalf("switch to listed with no data = %d %s", code, raw)
	}
}

// Deleting a user takes their AI accounts with them. If the host step fails,
// the user row comes back with its AI accounts.
func TestDeleteUserAIAccounts(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", deleteFailUser, "pw-bob")
	h.addMember("u_carol", "carol", "pw-carol")

	h.loginAs(deleteFailUser, "pw-bob")
	bobs := h.createAIAccount(aiAccountBody("bob ai"))
	h.loginAs("carol", "pw-carol")
	h.createAIAccount(aiAccountBody("carol ai"))

	h.loginAs("alice", "pass1")
	h.elevate("pass1")

	code, _ := h.doRaw("DELETE", "/api/v1/users/u_bob", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("delete with host failure = %d; want 502", code)
	}
	got, _ := h.st.ListAIAccounts("u_bob")
	if len(got) != 1 || got[0].ID != bobs.ID || got[0].APIKey != testAIKey {
		t.Fatalf("rollback lost the user's AI accounts: %+v", got)
	}

	code, _ = h.doRaw("DELETE", "/api/v1/users/u_carol", nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete carol = %d; want 204", code)
	}
	if got, _ := h.st.ListAIAccounts("u_carol"); len(got) != 0 {
		t.Fatalf("deleted user's AI accounts survived: %+v", got)
	}
}
