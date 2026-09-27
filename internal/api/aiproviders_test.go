package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/catalog"
)

// aiHarness is the normal harness with a remote catalog seeded from a local
// snapshot file that carries two invented AI providers. Their logos are
// relative URLs, so they resolve against a fake asset server standing in for
// the catalog base.
func aiHarness(t *testing.T) (*harness, *int) {
	t.Helper()
	var logoHits int
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logoHits++
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, "<svg>"+r.URL.Path+"</svg>")
	}))
	t.Cleanup(assets.Close)

	seed := filepath.Join(t.TempDir(), "seed.json")
	snap := `{"schema_version":2,"version":"v1","apps":[],"ai_providers":[
		{"id":"acme","name":"Acme AI","logo_url":"/logos/acme.svg","logo_dark_url":"/logos/acme-dark.svg",
		 "key_url":"https://example.invalid/keys","key_prefix":"sk-acme-","native_protocol":"acme",
		 "defaults":{"chat":"acme-1"},
		 "models":[{"id":"acme-1","name":"Acme 1","types":["chat"],"flags":["tools"]}]},
		{"id":"plain","name":"Plain AI","openai_base_url":"https://api.example.invalid/v1"}
	]}`
	if err := os.WriteFile(seed, []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := catalog.NewRemote(catalog.RemoteOptions{
		BaseURL: assets.URL, Environment: "hosted",
		AssetCacheDir: t.TempDir(), SnapshotFile: seed,
	})
	h := newHarness(t, func(s *Server) { s.catalog = cat })
	return h, &logoHits
}

// TestListAIProviders: the list comes back in authored order, with logo URLs
// on the box's own routes and none for a provider without a logo.
func TestListAIProviders(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/ai-providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	out := decodeJSON[struct {
		Providers []catalog.AIProvider `json:"providers"`
	}](t, resp)
	if len(out.Providers) != 2 || out.Providers[0].ID != "acme" || out.Providers[1].ID != "plain" {
		t.Fatalf("providers = %+v, want acme then plain", out.Providers)
	}
	acme := out.Providers[0]
	if acme.LogoURL != "/api/v1/ai-providers/acme/logo" || acme.LogoDarkURL != "/api/v1/ai-providers/acme/logo-dark" {
		t.Errorf("acme logos = %q, %q; want box routes", acme.LogoURL, acme.LogoDarkURL)
	}
	if acme.Defaults["chat"] != "acme-1" || len(acme.Models) != 1 || acme.KeyPrefix != "sk-acme-" {
		t.Errorf("acme = %+v", acme)
	}
	if out.Providers[1].LogoURL != "" {
		t.Errorf("plain has no logo, got %q", out.Providers[1].LogoURL)
	}
}

// TestListAIProviders_NoCatalog: no data is a 200 with an empty list, never
// null, so the setup page can fall back to plain fields.
func TestListAIProviders_NoCatalog(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/ai-providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"providers":[]`) {
		t.Errorf("body = %s, want an empty providers list", body)
	}
}

// TestAIProviderLogo: both variants are proxied from where they were
// published, and cached after the first request.
func TestAIProviderLogo(t *testing.T) {
	h, hits := aiHarness(t)
	h.setupAdmin("alice", "pass1")

	for _, c := range []struct{ path, want string }{
		{"/api/v1/ai-providers/acme/logo", "<svg>/logos/acme.svg</svg>"},
		{"/api/v1/ai-providers/acme/logo-dark", "<svg>/logos/acme-dark.svg</svg>"},
		{"/api/v1/ai-providers/acme/logo", "<svg>/logos/acme.svg</svg>"},
	} {
		resp := h.do("GET", c.path, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != c.want {
			t.Errorf("%s: %d %q, want 200 %q", c.path, resp.StatusCode, body, c.want)
		}
	}
	if *hits != 2 {
		t.Errorf("asset fetches = %d, want 2 (the repeat is a cache hit)", *hits)
	}
}

// TestAIProviderLogo_NotFound: an unknown provider, or a logo the provider
// does not have, is a 404.
func TestAIProviderLogo_NotFound(t *testing.T) {
	h, _ := aiHarness(t)
	h.setupAdmin("alice", "pass1")

	for _, path := range []string{
		"/api/v1/ai-providers/ghost/logo",
		"/api/v1/ai-providers/plain/logo",
		"/api/v1/ai-providers/plain/logo-dark",
	} {
		resp := h.do("GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", path, resp.StatusCode)
		}
	}
}

// TestAIProviders_RequireAuth: the list and the logos sit behind the session
// check, like the catalog browse routes.
func TestAIProviders_RequireAuth(t *testing.T) {
	h, _ := aiHarness(t)
	for _, path := range []string{
		"/api/v1/ai-providers",
		"/api/v1/ai-providers/acme/logo",
		"/api/v1/ai-providers/acme/logo-dark",
	} {
		resp := h.do("GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", path, resp.StatusCode)
		}
	}
}
