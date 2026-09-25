package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/manifest"
)

// fixturePath is the pinned browse payload this file's fixture-reading tests use.
var fixturePath = filepath.Join("testdata", "snapshot.json")

// TestParseFixtureSnapshot reads the pinned browse payload the way a box reads
// one served by the catalog service, and checks the fields the box projects from.
//
// testdata/snapshot.json is a SYNTHETIC payload: fake apps, written here, in the
// published wire shape. It is not a copy of any catalog the service serves
// — app manifests and compose files are authored in the store, and this repo
// holds none of them (CLAUDE.md # Catalog apps). What the fixture pins is the
// SHAPE, which is all these tests need.
func TestParseFixtureSnapshot(t *testing.T) {
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseSnapshot(data)
	if err != nil {
		t.Fatalf("fixture snapshot must parse: %v", err)
	}
	if len(f.Apps) == 0 {
		t.Fatal("fixture snapshot carries no apps")
	}
	if f.Version == "" {
		t.Error("fixture snapshot carries no version token; it is the ETag the box sends back")
	}
	a := f.Apps[0]
	if a.ManifestURL == "" || a.ComposeURL == "" {
		t.Errorf("app %q carries no install-document URLs: %+v", a.ID, a)
	}
	if a.IconURL == "" || len(a.ScreenshotURLs) != 2 {
		t.Errorf("app %q asset URLs not carried: icon=%q screenshots=%v", a.ID, a.IconURL, a.ScreenshotURLs)
	}
	// The browse payload must carry no install payload: that is the whole point
	// of the split (#434). A published record leaves these empty; only a staged
	// local seed file inlines them.
	if a.Manifest != "" || a.Compose != "" {
		t.Errorf("app %q inlines an install payload; the published browse record must not", a.ID)
	}
}

// TestVerifyRejectsSchemaVersion covers the one refusal left on the browse
// payload: a schema version the box cannot project. The index digest that used to
// sit beside it is gone — it made field order load-bearing and turned any new
// published field into a flag day (#434) — so a payload the box can read is
// accepted on its own bytes.
func TestVerifyRejectsSchemaVersion(t *testing.T) {
	base := catalogFile{
		SchemaVersion: wireSchemaVersion,
		Version:       "v1",
		Apps:          []wireApp{{ID: "a", Name: "A", Version: "1"}},
	}
	if err := base.verify(); err != nil {
		t.Fatalf("well-formed payload must verify: %v", err)
	}

	bad := base
	bad.SchemaVersion = wireSchemaVersion + 1
	if err := bad.verify(); err == nil {
		t.Fatal("want error for unreadable schema version")
	}

	b, _ := json.Marshal(base)
	if _, err := parseSnapshot(b[:len(b)/2]); err == nil {
		t.Fatal("want error for truncated payload body")
	}
}

// TestUnknownFieldsAreIgnored is the behaviour change #434 bought. An unknown
// key — top-level OR inside an app — is now dropped the way encoding/json drops
// one everywhere else, so the catalog service can add a display field and publish
// it before the fleet has updated.
//
// It used to be asymmetric: a per-app unknown key rejected the whole payload,
// because verify() recomputed the index digest by re-marshalling what it parsed
// and a dropped key could not reproduce it. That made every published field a
// coordinated release, and a box that met one showed an empty store.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	raw := []byte(`{"schema_version":` + strconv.Itoa(wireSchemaVersion) +
		`,"version":"v9","not_modelled_here":{"any":"shape"},"apps":[{` +
		`"id":"alpha","name":"Alpha","version":"1.0",` +
		`"footprint":{"image_download_bytes":0,"image_disk_bytes":0},` +
		`"manifest_url":"/catalog/apps/alpha/manifest",` +
		`"compose_url":"/catalog/apps/alpha/compose",` +
		`"not_modelled_here":{"any":"shape"},"also_new":42}]}`)
	f, err := parseSnapshot(raw)
	if err != nil {
		t.Fatalf("unknown keys must be dropped, not rejected: %v", err)
	}
	if len(f.Apps) != 1 || f.Apps[0].ID != "alpha" {
		t.Fatalf("apps = %+v, want the one app with its modelled fields intact", f.Apps)
	}
	if f.Version != "v9" {
		t.Errorf("version = %q, want v9", f.Version)
	}
}

// TestVersionIsOpaque pins that the box treats the version token as bytes. It
// stores whatever the payload carries and hands it straight back as the
// If-None-Match validator, with no recomputation and no shape requirement — the
// catalog service may mint it any way it likes.
func TestVersionIsOpaque(t *testing.T) {
	raw := []byte(`{"schema_version":` + strconv.Itoa(wireSchemaVersion) +
		`,"version":"2026-09-04T10:00:00Z/17","apps":[]}`)
	f, err := parseSnapshot(raw)
	if err != nil {
		t.Fatalf("a non-hex version token must parse: %v", err)
	}
	if got := quoteETag(f.Version); got != `"2026-09-04T10:00:00Z/17"` {
		t.Errorf("ETag = %s, want the token quoted verbatim", got)
	}
}

// jsonKeys returns the set of JSON object keys a struct type will unmarshal
// into, derived from its own `json:"..."` tags rather than hand-listed — a
// hand-list is one more place for the wire shape to drift out of sync with
// the actual struct. Fields tagged "-" are skipped.
func jsonKeys(t reflect.Type) map[string]bool {
	keys := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		if idx := strings.IndexByte(tag, ','); idx >= 0 {
			name = tag[:idx]
		}
		if name != "" {
			keys[name] = true
		}
	}
	return keys
}

// ignoredTopLevelKeys are published-catalog top-level fields the box
// deliberately does not model. Each entry records why, so widening this list
// is a decision on record, not a silent bypass of TestNoUnmodeledFields.
var ignoredTopLevelKeys = map[string]string{
	// os_capabilities_version is a publish-side provenance stamp: which
	// capability set the publisher admitted apps against when it built this
	// payload. It has no box-side meaning — the box enforces admission against
	// its own manifest.Version each time it parses one, not against a
	// catalog-wide stamp — so there is nothing for the box to do with it.
	"os_capabilities_version": "publish-side provenance stamp, no box-side meaning",
}

// TestNoUnmodeledFields asserts that the wire shape written down in the fixture
// and the wire shape the box's types model are the same set of keys. It parses
// the fixture into a generic map[string]any and fails on any top-level or
// per-app key that catalogFile / wireApp (plus the nested footprint/author/links
// shapes) do not declare a json tag for.
//
// This is no longer a correctness gate — an unknown key is dropped harmlessly
// now (TestUnknownFieldsAreIgnored), so a published field the box does not model
// costs a feature, not a working store. What it still buys is that the shape the
// box believes in stays reviewable as one readable file, and that a json tag
// renamed in wire.go without the fixture following is caught here.
//
// **Never regenerate the fixture from the Go types in THIS package.** A fixture
// generated from the very structs it is checked against agrees with them always,
// and this test becomes a test of nothing. That rule is why the fixture was
// hand-authored for a while.
//
// It is now written by the publisher that builds the published catalog, from the
// publisher's OWN wire types. That is not circular: those types are the other
// side of this contract, so the comparison this test makes is exactly the one it
// is for — their shape against the shape modelled here. A json tag renamed on
// either side and not followed on the other still fails here.
//
// The apps in it stay SYNTHETIC — three invented records covering the shape, not
// the real catalog. This repo is public and the catalog is private, so a fixture
// carrying real records would publish it. That is not hypothetical: it is what
// the older copies of this file did.
func TestNoUnmodeledFields(t *testing.T) {
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	knownTop := jsonKeys(reflect.TypeOf(catalogFile{}))
	for key := range raw {
		if knownTop[key] || ignoredTopLevelKeys[key] != "" {
			continue
		}
		t.Errorf("the pinned fixture has top-level key %q that catalogFile (internal/catalog/wire.go) does not model: "+
			"either add a field for it there (and in dev/mkcatalog if mkcatalog should emit it too), or add it to "+
			"ignoredTopLevelKeys with a comment explaining why the box doesn't need it", key)
	}

	appsRaw, ok := raw["apps"].([]any)
	if !ok {
		t.Fatal("the pinned fixture: \"apps\" is missing or not an array")
	}
	if len(appsRaw) == 0 {
		t.Fatal("the pinned fixture: \"apps\" is empty, guard has nothing to check")
	}

	knownApp := jsonKeys(reflect.TypeOf(wireApp{}))
	knownFootprint := jsonKeys(reflect.TypeOf(manifest.Footprint{}))
	knownAuthor := jsonKeys(reflect.TypeOf(manifest.Author{}))
	knownLinks := jsonKeys(reflect.TypeOf(manifest.Links{}))

	checkNested := func(id, label string, obj map[string]any, known map[string]bool) {
		for key := range obj {
			if known[key] {
				continue
			}
			t.Errorf("app %q: %s has key %q that its wire type (internal/catalog/wire.go / internal/manifest) does not "+
				"model: add the field, or record why it's ignored", id, label, key)
		}
	}

	for _, a := range appsRaw {
		app, ok := a.(map[string]any)
		if !ok {
			t.Fatal("the pinned fixture: an \"apps\" entry is not an object")
		}
		id, _ := app["id"].(string)
		for key := range app {
			if knownApp[key] {
				continue
			}
			t.Errorf("app %q has key %q that wireApp (internal/catalog/wire.go) does not model: "+
				"either add a field for it there (and in dev/mkcatalog if mkcatalog should emit it too), or record it as a "+
				"deliberate omission with a comment", id, key)
		}
		if fp, ok := app["footprint"].(map[string]any); ok {
			checkNested(id, "footprint", fp, knownFootprint)
		}
		if au, ok := app["author"].(map[string]any); ok {
			checkNested(id, "author", au, knownAuthor)
		}
		if li, ok := app["links"].(map[string]any); ok {
			checkNested(id, "links", li, knownLinks)
		}
	}

	// ai_providers is a list of objects of its own, so its keys are checked
	// the same way as an app's. defaults is keyed by model type, which is data,
	// not shape, so its keys are not checked.
	providersRaw, _ := raw["ai_providers"].([]any)
	knownProvider := jsonKeys(reflect.TypeOf(wireAIProvider{}))
	knownModel := jsonKeys(reflect.TypeOf(wireAIModel{}))
	for _, p := range providersRaw {
		prov, ok := p.(map[string]any)
		if !ok {
			t.Fatal("the pinned fixture: an \"ai_providers\" entry is not an object")
		}
		id, _ := prov["id"].(string)
		checkKeys := func(label string, obj map[string]any, known map[string]bool) {
			for key := range obj {
				if !known[key] {
					t.Errorf("ai provider %q: %s has key %q that its wire type (internal/catalog/wire.go) does not "+
						"model: add the field, or record why it's ignored", id, label, key)
				}
			}
		}
		checkKeys("the provider", prov, knownProvider)
		models, _ := prov["models"].([]any)
		for _, m := range models {
			if mo, ok := m.(map[string]any); ok {
				checkKeys("a model", mo, knownModel)
			}
		}
	}
}

// TestExternalCostsProjectOntoDetail checks the newest display field survives the
// round trip from published bytes to the detail page, and stays off the grid card
// — a cost is a paragraph of reading, not something a card can hold.
func TestExternalCostsProjectOntoDetail(t *testing.T) {
	apps := []wireApp{{
		ID: "openclaw", Name: "OpenClaw", Version: "1.0",
		ManifestURL: "/catalog/apps/openclaw/manifest",
		ComposeURL:  "/catalog/apps/openclaw/compose",
		ExternalCosts: []ExternalCost{{
			ID:              "model-access",
			Title:           "Model access",
			Description:     "You bring your own provider key and the provider bills you.",
			Required:        true,
			Estimate:        "$3 per million tokens (long agent runs use many times more)",
			EstimateChecked: "2026-08-10",
		}},
	}}
	raw, err := json.Marshal(catalogFile{
		SchemaVersion: wireSchemaVersion,
		Version:       "v1",
		Apps:          apps,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseSnapshot(raw)
	if err != nil {
		t.Fatalf("payload carrying external_costs must parse: %v", err)
	}
	got := detailOfApp(&f.Apps[0])
	if len(got.ExternalCosts) != 1 {
		t.Fatalf("Detail.ExternalCosts = %+v, want the one declared cost", got.ExternalCosts)
	}
	c := got.ExternalCosts[0]
	if c.ID != "model-access" || !c.Required || c.EstimateChecked != "2026-08-10" {
		t.Errorf("Detail.ExternalCosts[0] = %+v", c)
	}
	if c.Estimate != "$3 per million tokens (long agent runs use many times more)" {
		t.Errorf("estimate not carried verbatim: %q", c.Estimate)
	}
	entry, err := json.Marshal(entryOfApp(&f.Apps[0]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(entry), "external_costs") {
		t.Errorf("Entry carries external_costs: %s", entry)
	}
}

// TestBuildSnapshotRoundTrips covers the seed-building seam (dev/mkcatalog): what
// BuildSnapshot writes, parseSnapshot must read back, including the inlined
// install payload a staged file carries because it has no catalog behind it.
func TestBuildSnapshotRoundTrips(t *testing.T) {
	apps := []SnapshotApp{{
		ID: "alpha", Name: "Alpha", Version: "1.0",
		Manifest: "id: alpha\n", Compose: "services: {}\n",
	}}
	providers := []SnapshotAIProvider{{
		ID: "acme-ai", Name: "Acme AI",
		Models: []SnapshotAIModel{{ID: "acme-1", Name: "Acme 1", Types: []string{"chat"}}},
	}}
	b, err := BuildSnapshot(apps, SnapshotHome{Spotlight: "alpha"},
		[]SnapshotCategory{{ID: "tools", Label: "Tools"}}, providers, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseSnapshot(b)
	if err != nil {
		t.Fatalf("a built snapshot must parse: %v", err)
	}
	if f.Version == "" {
		t.Error("BuildSnapshot must stamp a version token")
	}
	if len(f.Apps) != 1 || f.Apps[0].Manifest != "id: alpha\n" || f.Apps[0].Compose != "services: {}\n" {
		t.Fatalf("inline install payload not round-tripped: %+v", f.Apps)
	}
	if f.StoreRef != "abc123" || f.Home.Spotlight != "alpha" || len(f.Categories) != 1 {
		t.Errorf("built snapshot lost its curation: %+v", f)
	}
	if got, dropped := readAIProviders(f.AIProviders); len(got) != 1 || got[0].ID != "acme-ai" || len(dropped) != 0 {
		t.Errorf("built snapshot lost its AI providers: %+v (dropped %v)", got, dropped)
	}

	// The token is content-derived on the build side, so an unchanged catalog
	// stamps an unchanged version and a seeded box reads a rebuild as a no-op.
	again, err := BuildSnapshot(apps, SnapshotHome{Spotlight: "alpha"},
		[]SnapshotCategory{{ID: "tools", Label: "Tools"}}, providers, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	f2, err := parseSnapshot(again)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Version != f.Version {
		t.Errorf("version = %q then %q; the same apps must stamp the same token", f.Version, f2.Version)
	}
}
