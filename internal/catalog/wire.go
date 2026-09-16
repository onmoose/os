package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/onmoose/moose/internal/manifest"
)

// wire.go models the control plane's published catalog API as the box consumes
// it (cloud specs/CATALOG.md # Consume). The box fetches browse data and install
// payloads on two different routes:
//
//	GET /catalog?env=<environment>   browse records, home, categories, version
//	GET /catalog/apps/{id}/manifest  verbatim manifest.yml, application/yaml
//	GET /catalog/apps/{id}/compose   verbatim compose.yml, application/yaml
//
// so a box downloads ~100KB of browse data plus the two documents of each app it
// actually installs, instead of every app's install payload up front (#434).
//
// The shapes below are NOT a byte-faithful mirror of the control plane's own
// types any more, and they no longer need to be. The published record used to
// carry an index digest the box recomputed by re-marshalling what it parsed,
// which made field order load-bearing and turned ANY new published field into a
// flag day: the recomputed digest stopped matching, the snapshot was refused,
// and the store went empty. That digest was doing cache work, not security work
// — TLS authenticates the origin and HTTP framing catches truncation — so it is
// gone. What replaces it is `version`, an opaque token the box uses as an ETag
// and as a change signal and never recomputes. Unknown keys are now ignored the
// way encoding/json ignores them everywhere else, so the control plane can add a
// display field without waiting for the fleet.
//
// The schema_version refusal stays: a snapshot stamped with a format this box
// cannot project is still refused rather than half-read.

// wireSchemaVersion is the published-catalog wire format this box can read. It
// tracks the cloud's catalog.SchemaVersion; a snapshot stamped with anything else
// is refused at verify (a format the box can't project), the same staleness guard
// the cloud designed the version stamp for.
const wireSchemaVersion = 2

// catalogFile is the browse payload served by GET /catalog?env=<environment>:
// the app records this box's surface may show, the curated landing page, and the
// category vocabulary. It carries no install payloads — those are two separate
// routes per app (see wireApp.ManifestURL / ComposeURL).
type catalogFile struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	StoreRef      string    `json:"store_ref,omitempty"`
	// Version is the catalog's opaque change token, served as the response's
	// ETag and honoured on If-None-Match. The box treats it as bytes: it stores
	// it, sends it back, and compares it for equality. It NEVER recomputes it —
	// that is the whole point of the field (see the file comment).
	Version string    `json:"version"`
	Apps    []wireApp `json:"apps"`
	// Home is the authored recommended-apps page (a curated home.yml): a
	// spotlight app plus ordered category groups. Carried verbatim, not derived —
	// the landing page's shape is a curation decision the store curation source
	// owns. Mirror of the control plane's own CatalogFile.Home.
	Home wireHomePage `json:"home"`
	// Categories is the authored category vocabulary: every category id an app may
	// claim, with the display label the store surfaces render and the order they
	// are authored in. Carried, never derived — before it was on the wire the box
	// invented display text from the id ("developer-tools" -> "developer tools",
	// "ai" -> "ai") and disagreed with the other store surface doing the same.
	Categories []wireCategory `json:"categories"`
}

// wireCategory is one entry of the authored category vocabulary. Mirror of the
// control plane's own Category shape.
type wireCategory struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// wireHomePage / wireHomeGroup mirror the control plane's own HomePage / HomeGroup
// shapes, so the box re-parses exactly what the sync tool published.
type wireHomePage struct {
	Spotlight string          `json:"spotlight"`
	Groups    []wireHomeGroup `json:"groups"`
}

// wireHomeGroup is one category's row on the landing page, in authored order.
// Category is a catalog category id; Apps are app ids resolving within Apps.
type wireHomeGroup struct {
	Category string   `json:"category"`
	Apps     []string `json:"apps"`
}

// wireApp is one published app's BROWSE record: the display metadata the box's
// store surfaces render, plus the URLs to follow for everything that is not
// display — the icon, the screenshots, and the two install documents.
//
// Every URL on this record is opaque. The box follows what it is given (resolved
// against the catalog base URL when relative) and never assembles a path of its
// own, so the control plane can move assets to object storage at an absolute URL
// without a box-side change.
//
// Featured/Rank drive the curated rows; the box's Entry/Detail do not surface
// them. The record carries no environments list: GET /catalog is filtered by the
// ?env= the box sends, so everything it receives is already showable here.
type wireApp struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Version          string             `json:"version"`
	ShortDescription string             `json:"short_description,omitempty"`
	LongDescription  string             `json:"long_description,omitempty"`
	Categories       []string           `json:"categories,omitempty"`
	IconGlyph        string             `json:"icon_glyph,omitempty"`
	Author           *manifest.Author   `json:"author,omitempty"`
	License          string             `json:"license,omitempty"`
	Links            *manifest.Links    `json:"links,omitempty"`
	ChangelogURL     string             `json:"changelog_url,omitempty"`
	Footprint        manifest.Footprint `json:"footprint"`

	// ExternalCosts is money a THIRD PARTY charges to make the app useful (a
	// model-provider API key, a mail provider). It is NOT what moose charges for
	// the app — that is authored in the curation source next to
	// listed/environments and is not on this wire.
	ExternalCosts []ExternalCost `json:"external_costs,omitempty"`

	// IconURL / ScreenshotURLs are where the artwork lives on the control plane.
	// The box proxies and caches them behind its OWN /api/v1/catalog asset routes,
	// so the UI never leaves the box origin (AUTH_AND_ACCESS.md). Empty icon ⇒ the
	// store falls back to the glyph.
	IconURL        string   `json:"icon_url,omitempty"`
	ScreenshotURLs []string `json:"screenshot_urls,omitempty"`

	// ManifestURL / ComposeURL are the app's two install documents, served as
	// application/yaml with the verbatim file as the body. Fetched only when the
	// box actually installs this app (remoteSource.Load), which is what keeps the
	// browse payload small.
	ManifestURL string `json:"manifest_url,omitempty"`
	ComposeURL  string `json:"compose_url,omitempty"`

	Featured bool `json:"featured,omitempty"`
	Rank     *int `json:"rank,omitempty"`

	// Manifest / Compose are the DEV AND TEST SEED SEAM ONLY, and the published
	// catalog never carries them. A staged snapshot file (MOOSE_CATALOG_FILE —
	// dev/mkcatalog, dev/test-qemu, dev/cloud/test) has no control plane behind
	// it to serve the two document routes, so it inlines the verbatim bytes here
	// and Load reads them instead of fetching. A record from a real control plane
	// leaves them empty and Load follows ManifestURL / ComposeURL.
	Manifest string `json:"manifest,omitempty"`
	Compose  string `json:"compose,omitempty"`
}

// ExternalCost is one third-party charge an app depends on: what someone OTHER
// than moose bills the user to make the app useful (a model-provider API key, a
// mail provider). Mirror of cloud catalog.ExternalCost, so it is both a wire
// shape and the shape Detail exposes.
//
// Required is a plain bool, matching the control plane's, NOT the *bool of
// manifest.ExternalCost. The pointer exists in the manifest to reject an author
// who never states the field; by the time a record is published it has been
// stated, so keeping the pointer here would put a nullable boolean on the box's
// public API where the control plane's identical endpoint returns a plain one —
// two store surfaces disagreeing about one field.
type ExternalCost struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	Required        bool   `json:"required"`
	Estimate        string `json:"estimate,omitempty"`
	EstimateChecked string `json:"estimate_checked,omitempty"`
}

// externalCostsOf converts the manifest's authored costs to the published shape,
// collapsing the manifest's tri-state Required to the plain bool every consumer
// sees. Used by the disk source, which reads manifests directly.
func externalCostsOf(costs []manifest.ExternalCost) []ExternalCost {
	if len(costs) == 0 {
		return nil
	}
	out := make([]ExternalCost, 0, len(costs))
	for i := range costs {
		c := &costs[i]
		out = append(out, ExternalCost{
			ID:              c.ID,
			Title:           c.Title,
			Description:     c.Description,
			Required:        c.IsRequired(),
			Estimate:        c.Estimate,
			EstimateChecked: c.EstimateChecked,
		})
	}
	return out
}

// verify refuses a snapshot the box can't project: a schema version it can't
// read. There is no digest check — see the file comment on why the index digest
// is gone. Integrity of the bytes is HTTP's job (framing catches a truncated
// body) and authenticity is TLS's (cloud #62).
func (f catalogFile) verify() error {
	if f.SchemaVersion != wireSchemaVersion {
		return fmt.Errorf("catalog schema version %d, want %d", f.SchemaVersion, wireSchemaVersion)
	}
	return nil
}

// parseSnapshot unmarshals raw GET /catalog bytes and verifies them in one step —
// the only way a browse payload enters the box, whether fetched from the control
// plane or read from a staged local file (the dev/test seam, MOOSE_CATALOG_FILE).
func parseSnapshot(data []byte) (catalogFile, error) {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return catalogFile{}, fmt.Errorf("parse catalog snapshot: %w", err)
	}
	if err := f.verify(); err != nil {
		return catalogFile{}, err
	}
	return f, nil
}

// SnapshotApp, SnapshotHome and SnapshotHomeGroup are exported aliases of this
// file's own wire types, for tooling that builds a snapshot from on-disk app
// packages (dev/mkcatalog) rather than parsing one. They are aliases, not
// copies, so there is exactly one App/Home shape in this box, visible under a
// second name to a second audience — a snapshot builder can never drift from
// the parser the way a hand-copied struct in another package did before.
type (
	SnapshotApp       = wireApp
	SnapshotHome      = wireHomePage
	SnapshotHomeGroup = wireHomeGroup
	SnapshotCategory  = wireCategory
)

// BuildSnapshot assembles a GET /catalog-shaped browse payload from already-built
// apps (and an optional curated landing page) and marshals it to the exact bytes a
// box can parse: it stamps SchemaVersion, GeneratedAt, StoreRef and Version, and
// marshals the whole thing. This is the one seam a snapshot-building tool needs —
// it builds SnapshotApp values from its own source (a manifest+compose pair, a
// home.yml) and calls this once, instead of re-declaring the wire shape.
//
// A tool building a snapshot for the local seed seam (dev/mkcatalog) inlines each
// app's Manifest/Compose, because there is no control plane behind a staged file
// to serve the two document routes.
func BuildSnapshot(apps []SnapshotApp, home SnapshotHome, cats []SnapshotCategory, storeRef string) ([]byte, error) {
	version, err := contentToken(apps)
	if err != nil {
		return nil, err
	}
	f := catalogFile{
		SchemaVersion: wireSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		StoreRef:      storeRef,
		Version:       version,
		Apps:          apps,
		Home:          home,
		Categories:    cats,
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("marshal catalog snapshot: %w", err)
	}
	return b, nil
}

// contentToken derives an opaque change token for a built snapshot, so two builds
// of the same apps stamp the same Version and a box seeded from one recognises the
// other as unchanged. It is a BUILD-side convenience only: no reader recomputes
// it, and the control plane is free to mint its token any other way.
func contentToken(apps []SnapshotApp) (string, error) {
	b, err := json.Marshal(apps)
	if err != nil {
		return "", fmt.Errorf("marshal catalog index: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
