package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The providers in these tests are invented, like the apps in the pinned
// fixture. They cover the shape, not any real provider list.

// aiSnapshot wraps a raw ai_providers value in an otherwise valid payload.
func aiSnapshot(providers string) []byte {
	return []byte(`{"schema_version":` + strconv.Itoa(wireSchemaVersion) +
		`,"version":"v1","apps":[],"ai_providers":` + providers + `}`)
}

// syncedAI serves one payload plus logo bytes from a bucket path and returns a
// remote source that has synced it.
func syncedAI(t *testing.T, body []byte) (*remoteSource, *int) {
	t.Helper()
	var logoHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/catalog":
			w.Write(body)
		case strings.HasPrefix(r.URL.Path, "/bucket/"):
			logoHits++
			fmt.Fprint(w, "SVG-"+r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	r := newRemote(srv.URL, "hosted", t.TempDir())
	if err := r.syncOnce(context.Background()); err != nil {
		t.Fatalf("a payload with ai_providers must sync: %v", err)
	}
	return r, &logoHits
}

func providerIDs(ps []AIProvider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// TestAIProvidersKeepAuthoredOrder: the list is served in file order, which is
// the display order. There is no featured rank.
func TestAIProvidersKeepAuthoredOrder(t *testing.T) {
	r, _ := syncedAI(t, aiSnapshot(`[
		{"id":"zeta","name":"Zeta AI"},
		{"id":"alpha","name":"Alpha AI"},
		{"id":"mid","name":"Mid AI"}
	]`))
	got, err := r.aiProviders()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"zeta", "alpha", "mid"}; !reflect.DeepEqual(providerIDs(got), want) {
		t.Errorf("order = %v, want %v", providerIDs(got), want)
	}
	for _, p := range got {
		if p.Models == nil {
			t.Errorf("provider %q: models is null, want an empty list", p.ID)
		}
	}
}

// TestAIProvidersLenientReading covers every drop rule. What the box does not
// understand goes, and the rest of the provider stays.
func TestAIProvidersLenientReading(t *testing.T) {
	r, _ := syncedAI(t, aiSnapshot(`[
		{"id":"acme","name":"Acme AI",
		 "defaults":{"chat":"acme-chat","embedding":"acme-chat","video":"acme-chat","image":"gone"},
		 "models":[
			{"id":"acme-chat","name":"Acme Chat","types":["chat","video"],"flags":["tools","telepathy"]},
			{"id":"acme-video","name":"Acme Video","types":["video"]},
			{"id":"","name":"No id","types":["chat"]},
			{"id":"acme-chat","name":"Second copy","types":["embedding"]},
			{"id":"acme-bare","types":["rerank"]}
		 ]},
		{"id":"","name":"No id AI"},
		{"id":"noname"},
		{"id":"acme","name":"Acme again"},
		"not an object",
		{"id":"plain","name":"Plain AI"}
	]`))
	got, err := r.aiProviders()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme", "plain"}; !reflect.DeepEqual(providerIDs(got), want) {
		t.Fatalf("providers = %v, want %v", providerIDs(got), want)
	}
	acme := got[0]
	if acme.Name != "Acme AI" {
		t.Errorf("a repeated id must keep the first entry, got name %q", acme.Name)
	}
	want := []AIModel{
		{ID: "acme-chat", Name: "Acme Chat", Types: []string{"chat"}, Flags: []string{"tools"}},
		{ID: "acme-bare", Name: "acme-bare", Types: []string{"rerank"}},
	}
	if !reflect.DeepEqual(acme.Models, want) {
		t.Errorf("models = %+v, want %+v", acme.Models, want)
	}
	// chat names a model that has chat. embedding names a model without that
	// type, video is not a type the box knows, and image names no model.
	if want := map[string]string{"chat": "acme-chat"}; !reflect.DeepEqual(acme.Defaults, want) {
		t.Errorf("defaults = %v, want %v", acme.Defaults, want)
	}
	if got[1].Defaults != nil {
		t.Errorf("a provider with no defaults must omit them, got %v", got[1].Defaults)
	}
}

// TestAIProvidersNeverRefuseTheSnapshot: an ai_providers value the box cannot
// read at all costs the provider list, never the apps.
func TestAIProvidersNeverRefuseTheSnapshot(t *testing.T) {
	for _, raw := range []string{`"a string"`, `{"id":"x"}`, `42`, `null`, `[]`} {
		body := []byte(`{"schema_version":` + strconv.Itoa(wireSchemaVersion) +
			`,"version":"v1","apps":[{"id":"alpha","name":"Alpha","version":"1"}],"ai_providers":` + raw + `}`)
		f, err := parseSnapshot(body)
		if err != nil {
			t.Fatalf("ai_providers=%s refused the snapshot: %v", raw, err)
		}
		snap := newSnapshot(f)
		if len(snap.apps) != 1 {
			t.Errorf("ai_providers=%s: apps = %d, want 1", raw, len(snap.apps))
		}
		if len(snap.providers) != 0 {
			t.Errorf("ai_providers=%s: providers = %+v, want none", raw, snap.providers)
		}
	}
}

// TestAIProvidersAbsentIsEmpty: an older catalog without the field, a box that
// never synced, and the disk source all answer an empty list, not an error.
func TestAIProvidersAbsentIsEmpty(t *testing.T) {
	r, _ := syncedAI(t, []byte(`{"schema_version":`+strconv.Itoa(wireSchemaVersion)+`,"version":"v1","apps":[]}`))
	if got, err := r.aiProviders(); err != nil || len(got) != 0 {
		t.Errorf("payload without ai_providers: %v, %v; want empty", got, err)
	}
	never := newRemote("http://127.0.0.1:1", "hosted", t.TempDir())
	if got, err := never.aiProviders(); err != nil || len(got) != 0 {
		t.Errorf("never synced: %v, %v; want empty", got, err)
	}
	if _, err := never.aiProviderLogoPath("acme", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("never synced logo: %v, want ErrNotFound", err)
	}
	disk := New(t.TempDir())
	if got, err := disk.AIProviders(); err != nil || len(got) != 0 {
		t.Errorf("disk: %v, %v; want empty", got, err)
	}
	if _, err := disk.AIProviderLogoPath("acme", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("disk logo: %v, want ErrNotFound", err)
	}
}

// TestAIProviderLogosAreBoxRoutes: the UI gets box URLs, never the asset
// origin, and a logo the provider does not have is left out.
func TestAIProviderLogosAreBoxRoutes(t *testing.T) {
	r, _ := syncedAI(t, aiSnapshot(`[
		{"id":"acme","name":"Acme AI","logo_url":"https://assets.example.invalid/acme.svg","logo_dark_url":"https://assets.example.invalid/acme-dark.svg"},
		{"id":"light","name":"Light AI","logo_url":"https://assets.example.invalid/light.svg"},
		{"id":"bare","name":"Bare AI"}
	]`))
	got, err := r.aiProviders()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].LogoURL != "/api/v1/ai-providers/acme/logo" || got[0].LogoDarkURL != "/api/v1/ai-providers/acme/logo-dark" {
		t.Errorf("acme logos = %q, %q; want the box routes", got[0].LogoURL, got[0].LogoDarkURL)
	}
	if got[1].LogoURL == "" || got[1].LogoDarkURL != "" {
		t.Errorf("light logos = %q, %q; want the light route only", got[1].LogoURL, got[1].LogoDarkURL)
	}
	if got[2].LogoURL != "" || got[2].LogoDarkURL != "" {
		t.Errorf("bare logos = %q, %q; want none", got[2].LogoURL, got[2].LogoDarkURL)
	}
	b, _ := json.Marshal(got[2])
	if strings.Contains(string(b), "logo") {
		t.Errorf("a provider with no logo must omit the keys: %s", b)
	}
}

// TestAIProviderLogoProxyAndCache: a logo is fetched from where it was
// published, a relative URL resolves against the catalog base, and the second
// request is served from the cache.
func TestAIProviderLogoProxyAndCache(t *testing.T) {
	r, hits := syncedAI(t, aiSnapshot(`[
		{"id":"acme","name":"Acme AI","logo_url":"/bucket/acme/logo.svg","logo_dark_url":"/bucket/acme/logo-dark.svg"},
		{"id":"light","name":"Light AI","logo_url":"/bucket/light/logo.svg"}
	]`))

	for _, dark := range []bool{false, true} {
		p, err := r.aiProviderLogoPath("acme", dark)
		if err != nil {
			t.Fatalf("dark=%v: %v", dark, err)
		}
		b, err := os.ReadFile(p)
		want := "SVG-/bucket/acme/logo.svg"
		if dark {
			want = "SVG-/bucket/acme/logo-dark.svg"
		}
		if err != nil || string(b) != want {
			t.Errorf("dark=%v: cached logo = %q, %v; want %q", dark, b, err, want)
		}
	}
	if _, err := r.aiProviderLogoPath("acme", false); err != nil {
		t.Fatal(err)
	}
	if *hits != 2 {
		t.Errorf("logo fetches = %d, want 2 (the repeat is a cache hit)", *hits)
	}

	if _, err := r.aiProviderLogoPath("light", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing dark logo: %v, want ErrNotFound", err)
	}
	if _, err := r.aiProviderLogoPath("ghost", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown provider: %v, want ErrNotFound", err)
	}
}

// TestAIProvidersFromFixture reads the pinned payload's providers, the same
// way a box reads a published one.
func TestAIProvidersFromFixture(t *testing.T) {
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	got, dropped := readAIProviders(f.AIProviders)
	if len(dropped) != 0 {
		t.Errorf("the fixture must read cleanly, dropped %v", dropped)
	}
	if len(got) < 2 {
		t.Fatalf("fixture carries %d providers, want the full and the minimal one", len(got))
	}
	full := got[0]
	if full.LogoURL == "" || full.LogoDarkURL == "" || full.KeyPrefix == "" || full.NativeProtocol == "" ||
		full.OpenAIBaseURL == "" || full.Defaults["chat"] == "" || len(full.Models) == 0 {
		t.Errorf("the first fixture provider must carry every optional field: %+v", full)
	}
}
