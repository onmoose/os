// Package manifest parses and validates manifest.yml (APP_MANIFEST.md).
// v1 skeleton: the required fields plus the permission flags the override
// generator currently consumes. The compose file itself is held verbatim and
// validated separately by the admission policy (APP_LIFECYCLE.md).
package manifest

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type Manifest struct {
	ID              string      `yaml:"id"`
	ManifestVersion int         `yaml:"manifest_version"`
	Name            string      `yaml:"name"`
	Version         string      `yaml:"version"`
	Description     Description `yaml:"description,omitempty"`

	// Identity/metadata for the store UI (APP_MANIFEST.md # A). All optional —
	// the brain doesn't act on any of them; they're surfaced verbatim to the
	// store grid and detail page. Icon and Screenshots are package-relative paths
	// (`./icon.png`); the catalog turns them into asset URLs (APP_STORE.md #
	// Catalog schema). Absent ⇒ the store falls back to a generic glyph and an
	// empty gallery.
	Icon string `yaml:"icon,omitempty"`
	// IconGlyph names a Lucide icon (kebab-case, e.g. `notebook-pen`; the set at
	// https://lucide.dev/icons) the store renders as the card/header glyph when no
	// raster Icon is declared. Author-chosen fallback for apps that ship no logo;
	// the brain doesn't act on it and can't verify the name exists (the icon set
	// lives in the UI), so an unknown-but-well-formed name degrades to the generic
	// glyph client-side. Ignored when Icon is set (APP_STORE.md # Catalog schema).
	IconGlyph    string   `yaml:"icon_glyph,omitempty"`
	Screenshots  []string `yaml:"screenshots,omitempty"`
	Categories   []string `yaml:"categories,omitempty"`
	Author       Author   `yaml:"author,omitempty"`
	License      string   `yaml:"license,omitempty"`
	Links        Links    `yaml:"links,omitempty"`
	ChangelogURL string   `yaml:"changelog_url,omitempty"`

	// Listed controls store visibility (APP_MANIFEST.md # A). A *bool so absent is
	// distinguishable from explicit false: nil or true ⇒ the app shows in the store
	// grid/detail page and can be installed; explicit `listed: false` ⇒ the manifest
	// stays in the catalog (parses, lints, serves icons) but is hidden from browse
	// and uninstallable through the store — the way a Blocked/Rejected app is
	// pulled without throwing away its adaptation.
	// Read it through IsListed(), never the raw pointer. Default-true, so every
	// existing manifest stays listed with no change (back-compatible field add).
	Listed *bool `yaml:"listed,omitempty"`

	ComposeFile    string      `yaml:"compose_file"`
	MainService    string      `yaml:"main_service"`
	MainPort       int         `yaml:"main_port"`
	PreferredSlugs []string    `yaml:"preferred_slugs"`
	Permissions    Permissions `yaml:"permissions"`

	// Images is the optional catalog-promised image map (APP_STORE.md # Catalog
	// schema — the `images` object). Keyed by the exact `image:` reference used
	// in the compose (e.g. `traefik/whoami:v1.10.3`); the value carries the
	// pinned `sha256:…` digest plus the display-only download/disk sizes the
	// store renders before install. CI (`moose manifest resolve`) resolves all
	// three from the registry at catalog-build time. Absent ⇒ TOFU at install
	// (Door-2 always, Door-1 until the catalog publishes a digest).
	Images map[string]ImageRef `yaml:"images,omitempty"`

	// Storage holds the author's on-disk hints (APP_MANIFEST.md # Storage). v1
	// reads only estimated_size — hoisted verbatim into Footprint; any other
	// keys (e.g. data_volumes) live in the compose, not parsed here.
	Storage Storage `yaml:"storage,omitempty"`

	// HealthProbe is the optional "up but not responding" probe config
	// (APP_MANIFEST.md # B). nil ⇒ the app is never probed and the
	// app-unresponsive health issue is never raised for it. Door-2 synthetic
	// manifests omit it.
	HealthProbe *HealthProbe `yaml:"health_probe,omitempty"`

	// ServiceUser opts a folderless app into a dedicated, moose-allocated
	// non-root runtime identity instead of the folderless default (the brain's
	// euid — root in production). Boolean intent only: no UID is namable in a
	// manifest (APP_MANIFEST.md # B, APP_ISOLATION.md # Runtime identity & data
	// ownership). Meaningless with a folders grant — admission rejects the
	// combination. Door-2 synthetic manifests never set it.
	ServiceUser bool `yaml:"service_user,omitempty"`

	// Secrets declares per-app random secrets the brain generates once at install
	// and injects as `MOOSE_SECRET_<NAME>` env vars (APP_MANIFEST.md # secrets,
	// SERVICE_PROVISIONING.md # Env-var injection). Each name maps in the compose
	// to whatever the app actually expects (e.g. BETTER_AUTH_SECRET) — same
	// app-defined mapping convention as MOOSE_SERVICE_*. The value is generated
	// from a CSPRNG, persisted, and re-emitted stably across restarts so
	// token-signing secrets don't rotate underneath live sessions.
	Secrets []Secret `yaml:"secrets,omitempty"`

	// Services declares the managed data services the app consumes (APP_MANIFEST.md
	// # D, SERVICE_PROVISIONING.md # Tier 1). The map key is the app's logical name
	// for the dependency (`database`, `cache`); the brain provisions a per-app
	// database+role in the shared instance of that type+version and injects the
	// credentials as `MOOSE_SERVICE_<KEY>_*` (uppercased key). The app's compose
	// maps those to whatever it expects — same app-defined convention as
	// MOOSE_SECRET_*. Absent ⇒ the app brings its own datastore in its compose.
	Services map[string]ServiceDep `yaml:"services,omitempty"`

	// Mail declares the app can send outgoing email through an admin-registered
	// SMTP provider (APP_MANIFEST.md # D, SERVICE_PROVISIONING.md # BYO outgoing
	// mail). Presence of the block makes the install dialog offer the provider
	// picker; when bound, the brain injects MOOSE_MAIL_* into the app's .env and
	// the compose maps those to whatever the app expects — same app-defined
	// convention as MOOSE_SECRET_* / MOOSE_SERVICE_*. Absent ⇒ no picker, never
	// injected. nil-able so "no block" and "block present" are distinguishable.
	Mail *Mail `yaml:"mail,omitempty"`

	// Config declares user-supplied configuration fields (APP_MANIFEST.md # D4):
	// values only the user can provide (a third-party API token, an external
	// connection string, a provider/model selector). Unlike the MOOSE_* injected
	// family, each value lands DIRECTLY under its own AppEnv name in the target
	// service's compose-override environment — no indirection, no mapping line —
	// because it is the app's own native variable, not a moose-owned value the app
	// must adapt to. The brain renders a form at install (required fields gate the
	// install button) and on the app detail page after. Absent ⇒ no form.
	Config []ConfigField `yaml:"config,omitempty"`

	// Requires lists "at least one of" groups over the config fields
	// (INSTALL_SETUP.md # 3, roles.go): the install is gated until each group
	// has a filled member. Read leniently: a group the box cannot use is
	// dropped, never a Parse error. Read it through EffectiveRequires.
	Requires Requirements `yaml:"requires,omitempty"`

	// ExternalCosts names money a THIRD PARTY charges to make the app useful: a
	// model-provider API key the assistant cannot answer without, a mail provider
	// an email app sends through. The brain does not act on it — it is display
	// metadata the store surfaces before install, so the user is not surprised by
	// a bill from someone else. Absent ⇒ the app costs nothing beyond the box.
	//
	// It is deliberately NOT what moose charges for the app. That is a commercial
	// decision that changes without the app changing, so it lives in the curation
	// source next to `listed`/`environments` (store `status.yml` `price:`), not in
	// this schema. Nor is either one a limitation: a limitation is a broken
	// feature, and paying for something is not a defect.
	ExternalCosts []ExternalCost `yaml:"external_costs,omitempty"`

	// Access declares path-scoped exceptions to the box's own login gate
	// (APP_MANIFEST.md # E2, ENVIRONMENT.md # Per-app owner-only access). Absent ⇒
	// the whole app follows the instance's exposure, which is the normal case.
	Access Access `yaml:"access,omitempty"`
}

// Access is the app's declared access shape. Today it holds one field, and it is
// a block rather than a bare list so later access-related declarations have a
// home that does not re-open the top level.
type Access struct {
	// PublicPaths lists request paths an owner-only ("restricted") app still
	// serves to anonymous callers. It exists for the app that pairs a
	// token-authed API with a session-authed UI: without it, letting an external
	// SDK reach the API means making the whole app public, which drops the box
	// login in front of the UI too (#415).
	//
	// It is a narrow exception, not a second exposure switch. The paths are
	// carried by the catalog, so catalog review is the trust boundary; the
	// validation below is a guard against an author mistake, not against a
	// hostile manifest (a hostile manifest already chooses the app's images).
	// The gate is hosted-only: an appliance app is always public, so the field
	// changes nothing there.
	PublicPaths []string `yaml:"public_paths,omitempty" json:"public_paths,omitempty"`
}

// ExternalCost is one third-party charge the app depends on (APP_MANIFEST.md #
// A). Authoring rules — the wording, and how to write an honest Estimate — live
// with the curation source (store `docs/app-description.md` # External costs),
// which enforces the house style this package does not: no em dashes, no vendor
// names, the provider's currency. What is enforced here is the shape every
// consumer depends on.
type ExternalCost struct {
	// ID is a stable kebab-case key, unique within the app, so a surface can key
	// on one cost across versions and translations.
	ID string `yaml:"id" json:"id"`
	// Title is what the money is for, in two to four words ("Sending email",
	// "Model access"). No vendor name: vendors change, the cost does not.
	Title string `yaml:"title" json:"title"`
	// Description says who charges and why the app needs it.
	Description string `yaml:"description" json:"description"`
	// Required marks a cost the app's main job does not work without. It never
	// gates install: an app that will not *boot* without a paid thing is a
	// blocked/blocks-start curation verdict, not an external cost.
	//
	// A *bool with no default, because there is no safe one. Defaulting to false
	// would let a forgotten line render a cost the app cannot work without as
	// "optional" — the exact surprise this block exists to prevent — and
	// defaulting to true would overstate every nice-to-have. The author states
	// it, and validate() rejects the manifest if they did not. The curation
	// source's own validator already demands the key, so anything looser here
	// would mean two gates disagreeing about one field. Read it through
	// IsRequired(), never the raw pointer.
	Required *bool `yaml:"required" json:"required"`
	// Estimate is a short unit rate ("$0.20 per 1000 emails (varies by
	// provider)"), never a monthly total — a total depends on how one person uses
	// the app, which nobody here can know. Empty is always valid and is the right
	// answer when no honest unit rate exists; the Description then carries the
	// shape in prose. Capped at EstimateMaxChars so it stays one glanceable line.
	Estimate string `yaml:"estimate,omitempty" json:"estimate,omitempty"`
	// EstimateChecked (YYYY-MM-DD) is the day the Estimate was last confirmed.
	// Required whenever Estimate is set, because a free-text price is the one
	// thing in this manifest nothing can re-measure: every other number is taken
	// from a real boot (resolved digests, measured storage), while this one is a
	// market observation that goes stale in silence. The date is what lets the
	// curation source flag an estimate nobody has re-checked.
	EstimateChecked string `yaml:"estimate_checked,omitempty" json:"estimate_checked,omitempty"`
}

// EstimateMaxChars caps ExternalCost.Estimate. Counted in runes, not bytes, so a
// currency sign costs one character here and in the curation source's own check
// (store tools/check.py) — two validators that disagree on the limit would let a
// line pass one gate and fail the other.
const EstimateMaxChars = 100

// IsRequired reports whether the app's main job depends on paying this cost.
// validate() rejects a manifest that omits the key, so the nil case here is a
// cost built in code rather than parsed; it reads as not-required, the same
// direction ConfigField.Required defaults.
func (c *ExternalCost) IsRequired() bool { return c.Required != nil && *c.Required }

// Description holds the app's catalog-facing text (APP_MANIFEST.md # A).
// Both fields are optional; the store surfaces Short as the one-liner and Long
// as the markdown body on the app detail page.
type Description struct {
	Short string `yaml:"short,omitempty"`
	// Long is a markdown string rendered on the app-store detail page. Multi-line
	// literal blocks (`|`) are idiomatic in manifests.
	Long string `yaml:"long,omitempty"`
}

// Author is the app's publisher, shown on the detail page (APP_MANIFEST.md # A).
type Author struct {
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	URL  string `yaml:"url,omitempty" json:"url,omitempty"`
}

// Links are the author's outbound links, surfaced in the detail page's info
// panel (APP_MANIFEST.md # A). All optional.
type Links struct {
	Homepage string `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	Source   string `yaml:"source,omitempty" json:"source,omitempty"`
	Support  string `yaml:"support,omitempty" json:"support,omitempty"`
}

// ImageRef is one entry in the catalog's `images` map (APP_STORE.md # Catalog
// schema): the pinned digest plus the display-only on-disk footprint of a
// single `image:tag`. Digest is the binding the brain enforces at install
// (# Trust model); DownloadBytes (sum of the image's compressed layer sizes)
// and DiskBytes (sum of its uncompressed layer sizes, deduped within the app's
// own image set) are advisory and gate nothing — a size that drifts from
// reality is a cosmetic bug, not an integrity failure.
type ImageRef struct {
	Digest        string `yaml:"digest" json:"digest"`
	DownloadBytes int64  `yaml:"download_bytes,omitempty" json:"download_bytes,omitempty"`
	DiskBytes     int64  `yaml:"disk_bytes,omitempty" json:"disk_bytes,omitempty"`
}

// UnmarshalYAML accepts both the object form ({digest, download_bytes,
// disk_bytes}) and the legacy scalar shorthand (`image:tag: sha256:…`, digest
// only) so manifests written before sizes were resolved still parse. Mirrors
// the HealthProbe string-or-mapping pattern.
func (r *ImageRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&r.Digest)
	}
	type raw ImageRef // shed the method set to avoid recursing into this func
	var w raw
	if err := node.Decode(&w); err != nil {
		return fmt.Errorf("images: %w", err)
	}
	*r = ImageRef(w)
	return nil
}

// Storage is the author's on-disk hint block (APP_MANIFEST.md # Storage). v1
// parses only estimated_size — a human-readable size string ("10GB") hoisted
// verbatim into the catalog footprint; the brain does no unit math on it there.
type Storage struct {
	EstimatedSize string `yaml:"estimated_size,omitempty" json:"estimated_size,omitempty"`
}

// EstimatedSizeBytes parses storage.estimated_size into a byte count for the
// box-specific install-plan footprint (BRAIN_UI_PROTOCOL.md # install-plan).
// The catalog grid keeps the verbatim string (Footprint.EstimatedState); this
// is the numeric form the install dialog adds to the image bytes.
//
// The three returns disambiguate cases the caller treats differently:
//   - unset ("")        → (0, false, nil)  — author gave no estimate; omit it
//   - valid ("10GB")    → (n, true,  nil)
//   - malformed ("big") → (0, false, err)  — surfaced, never silently zeroed
//
// Units are binary (GB = 2³⁰, matching the spec example where "10GB" is
// 10737418240), case-insensitive, with an optional fractional mantissa
// ("1.5GB"); the result truncates to whole bytes.
func (s Storage) EstimatedSizeBytes() (int64, bool, error) {
	raw := strings.TrimSpace(s.EstimatedSize)
	if raw == "" {
		return 0, false, nil
	}
	n, err := parseBinarySize(raw)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// sizeUnits maps a case-folded size suffix to its binary multiplier. Empty and
// "b" are bytes; the k/m/g/t families are all powers of 1024 — the bare,
// two-letter, and explicit -ib spellings are accepted as the same value because
// authors write them interchangeably and the figure is advisory either way.
var sizeUnits = map[string]int64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
	"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
}

// sizeRe splits a size string into a numeric mantissa and an optional unit
// suffix, tolerating whitespace between them ("10 GB").
var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$`)

// parseBinarySize parses a human size like "10GB" or "1.5 GiB" into bytes using
// the binary multipliers in sizeUnits. The caller passes a non-empty, trimmed
// string.
func parseBinarySize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("estimated_size %q is not a valid size (e.g. 10GB, 512MB)", s)
	}
	mult, ok := sizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("estimated_size %q has an unknown unit %q", s, m[2])
	}
	mant, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("estimated_size %q: %w", s, err)
	}
	return int64(mant * float64(mult)), nil
}

// Footprint is the per-app on-disk summary the store grid renders without
// fetching the full manifest (APP_STORE.md # Catalog schema — `footprint`). The
// image totals are an upper bound (nothing assumed cached locally); the install
// dialog shows a sharper, box-specific figure (BRAIN_UI_PROTOCOL.md #
// GET /api/v1/catalog/:id/install-plan). EstimatedState is the manifest's
// measured app-state baseline at install, not a usage projection (DECISIONS.md
// 2026-06-09).
type Footprint struct {
	ImageDownloadBytes int64  `json:"image_download_bytes" yaml:"image_download_bytes"`
	ImageDiskBytes     int64  `json:"image_disk_bytes" yaml:"image_disk_bytes"`
	EstimatedState     string `json:"estimated_state,omitempty" yaml:"estimated_state,omitempty"`
}

// Footprint derives the per-app footprint (APP_STORE.md # Catalog schema): it
// sums the resolved Images entries and hoists storage.estimated_size verbatim.
// Derived, never hand-authored. Summing per-image DiskBytes trusts the
// resolver's within-app layer dedup when it filled those numbers.
func (m *Manifest) Footprint() Footprint {
	f := Footprint{EstimatedState: m.Storage.EstimatedSize}
	for _, ref := range m.Images {
		f.ImageDownloadBytes += ref.DownloadBytes
		f.ImageDiskBytes += ref.DiskBytes
	}
	return f
}

// HealthProbe declares the HTTP probe that backs the app-unresponsive detector
// (APP_MANIFEST.md # B, HEALTH.md # app-unresponsive). It is not a Docker
// HEALTHCHECK: the brain executes it through the app's Caddy route on the
// health-poll tick. It accepts a shorthand string (`health_probe: /healthz`,
// expanding to {path: /healthz}) or the full mapping.
type HealthProbe struct {
	// Path is the HTTP path the brain GETs (required; must be absolute).
	Path string
	// HealthyStatus is the set of status codes treated as healthy. Empty ⇒ the
	// default "any status < 500" (the server answered coherently); 401/403/404
	// still count as responding.
	HealthyStatus []int
	// StartPeriod is the grace after the container starts before a failing probe
	// counts, so a warming-up app doesn't flap the banner on install/update.
	// Defaults to DefaultStartPeriod when omitted.
	StartPeriod time.Duration
}

// DefaultStartPeriod is the health_probe.start_period default (APP_MANIFEST.md
// # B): the warm-up grace before a probe failure counts.
const DefaultStartPeriod = 60 * time.Second

// healthProbeWire is the YAML mapping shape. start_period is a Go duration
// string ("60s") on the wire, parsed to a time.Duration in HealthProbe.
type healthProbeWire struct {
	Path          string `yaml:"path"`
	HealthyStatus []int  `yaml:"healthy_status,omitempty"`
	StartPeriod   string `yaml:"start_period,omitempty"`
}

// UnmarshalYAML accepts the shorthand string form or the full mapping
// (APP_MANIFEST.md # B). The string form sets only Path.
func (h *HealthProbe) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*h = HealthProbe{Path: s}
		return nil
	}
	var w healthProbeWire
	if err := node.Decode(&w); err != nil {
		return fmt.Errorf("health_probe: %w", err)
	}
	h.Path = w.Path
	h.HealthyStatus = w.HealthyStatus
	if w.StartPeriod != "" {
		d, err := time.ParseDuration(w.StartPeriod)
		if err != nil {
			return fmt.Errorf("health_probe.start_period %q: %w", w.StartPeriod, err)
		}
		h.StartPeriod = d
	}
	return nil
}

// MarshalYAML emits the mapping form so a parsed manifest round-trips through
// the per-instance manifest.yml the installer writes (start_period back to a
// duration string).
func (h HealthProbe) MarshalYAML() (any, error) {
	w := healthProbeWire{Path: h.Path, HealthyStatus: h.HealthyStatus}
	if h.StartPeriod != 0 {
		w.StartPeriod = h.StartPeriod.String()
	}
	return w, nil
}

type Permissions struct {
	Internet bool `yaml:"internet"`
	LAN      bool `yaml:"lan"`

	// Folders is the app's declared access to use-case content folders
	// (APP_MANIFEST.md # folders). The manifest declares *what* content the app
	// touches (folder + mode + subfolder granularity); it does NOT declare the
	// source. Source — the owner's personal `~/<Folder>/` vs the household
	// `/srv/moose/shared/<Folder>/` — is the installer's per-folder election at
	// install time, because the author can't know whether a given household
	// wants "my own Jellyfin on my movies" or "on the family library"
	// (DECISIONS.md 2026-05-30 — folder source is installer-elected). Supersedes
	// the earlier `user_folders` / `shared_folders` split.
	Folders []Folder `yaml:"folders"`

	// Devices lists explicit /dev/... paths to pass through (Zigbee dongles,
	// webcams). The brain validates each exists before start (APP_ISOLATION.md #
	// Devices). Separate from GPU.
	Devices []string `yaml:"devices"`

	// GPU requests the platform-appropriate GPU runtime (NVIDIA / Intel / AMD),
	// its own field because driver wiring is platform-specific and not a plain
	// device passthrough (APP_MANIFEST.md # gpu). No-GPU box fails at the
	// capacity check.
	GPU bool `yaml:"gpu"`
}

// Folder is one declared use-case content folder. Folder names come from the
// fixed v1 taxonomy; Mode defaults to read; Scope defaults to whole.
// pick-subfolder may carry a Default subpath the user can override at install.
type Folder struct {
	Folder  string `yaml:"folder"`            // photos|documents|movies|music|notes|downloads
	Mode    string `yaml:"mode"`              // read|write (default read)
	Scope   string `yaml:"scope"`             // whole|pick-subfolder (default whole)
	Default string `yaml:"default,omitempty"` // default subpath for pick-subfolder

	// Target is the explicit in-container destination path, set ONLY by Door-2
	// synthetic manifests (DASHBOARD.md # Folder grants carry an explicit
	// destination path). A pasted third-party compose has no author to map
	// MOOSE_FOLDER_<NAME>, so the admin types where the app reads its data and
	// the brain binds the elected source straight there. Store manifests omit it
	// and keep the fixed `/moose/<folder>` + env-var convention.
	Target string `yaml:"target,omitempty"`
}

// Secret is one declared per-app generated secret (APP_MANIFEST.md # secrets).
// Name is lowercase snake_case; the injected env var is `MOOSE_SECRET_` + the
// uppercased name (so `auth` → `MOOSE_SECRET_AUTH`). Bytes is the entropy drawn
// from the CSPRNG before base64url encoding; it defaults to DefaultSecretBytes
// and is floored at MinSecretBytes so a manifest can't request a weak secret.
//
// Show opts the secret into being surfaced to the instance owner (and admins) on
// the app detail page (#152): set it for a per-instance bootstrap credential the
// user must read to finish first sign-in (e.g. a self-auth app's setup token),
// so the manifest never has to ship a published constant. Absent ⇒ internal-only
// (a DB password the app consumes but the user never sees), never revealed.
type Secret struct {
	Name  string `yaml:"name"`
	Bytes int    `yaml:"bytes,omitempty"`
	Show  bool   `yaml:"show,omitempty"`
}

// ServiceDep is one managed-service dependency (APP_MANIFEST.md # D). Type is
// the service kind (`postgres`, `mysql`, `mariadb`, `valkey`, `redis`); Version
// is the version pin the brain runs a shared instance of (a major for
// postgres/valkey, an upstream LTS series for the MySQL family). `redis` is a
// compatibility alias for `valkey` — both run the BSD-3 Valkey engine, never
// upstream Redis (see internal/lifecycle/services.go normalizeEngine). Name is
// the author's advisory
// logical name for the resource; v1 ignores it (the brain generates the real
// database name) — parsed for forward-compat, not used.
type ServiceDep struct {
	Type    string `yaml:"type"`
	Version string `yaml:"version"`
	Name    string `yaml:"name,omitempty"`
}

// Mail is the optional outgoing-email declaration (APP_MANIFEST.md # D,
// SERVICE_PROVISIONING.md # BYO outgoing mail). Optional must be true — v1 has
// no required-mail semantics, because a box may have no provider registered and
// every app must still install and run unbound (validateMail rejects false).
type Mail struct {
	Optional bool `yaml:"optional"`
}

// ConfigField is one user-supplied configuration field (APP_MANIFEST.md # D4).
// AppEnv is the app's own environment-variable name: it is the storage key, the
// form hint shown in monospace, AND the variable the brain stamps verbatim into
// the target service's compose-override environment (so `app_env: OPENAI_API_KEY`
// sets `OPENAI_API_KEY=<the user's answer>`). There is no MOOSE_* indirection —
// see the Manifest.Config doc and DECISIONS.md 2026-06-26.
//
// Because the value lands directly under AppEnv and the override wins over the
// base compose, AppEnv is a security boundary: it is validated to be a bare
// uppercase env-var identifier, never the brain-owned MOOSE_ prefix and never a
// loader/runtime-critical var (# Reserved names; reservedConfigEnv).
type ConfigField struct {
	AppEnv      string   `yaml:"app_env"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Secret      bool     `yaml:"secret,omitempty"`   // masked input; stored + handled like MOOSE_SECRET_*; no default allowed
	Required    bool     `yaml:"required,omitempty"` // gates the install button until provided
	Type        string   `yaml:"type,omitempty"`     // text|enum|bool (default text); normalized in place
	Options     []string `yaml:"options,omitempty"`  // enum only, non-empty
	Default     string   `yaml:"default,omitempty"`  // prefill / value-when-blank; required on bool; forbidden on secret
	Service     string   `yaml:"service,omitempty"`  // target compose service; default main_service

	// Role says what the field means, so the setup page can fill it from a
	// provider account: <kind>.<protocol>.<attribute>, e.g.
	// ai.anthropic.api_key (INSTALL_SETUP.md # 1, roles.go). Optional. The value
	// still lands under AppEnv. Read it through Manifest.FillableRoles: a role
	// the box cannot fill leaves the field a plain field, never a Parse error.
	Role string `yaml:"role,omitempty"`
	// Separator joins a models.<type> list. nil means DefaultSeparator. Read it
	// through EffectiveSeparator.
	Separator *string `yaml:"separator,omitempty"`

	// unreadable names the keys (role, separator) whose value was not a single
	// scalar. The lenient decoder reads them as absent; Lint reports them.
	unreadable []string
}

// serviceVersions is the allowlist of versions per managed-service type
// (SERVICE_PROVISIONING.md # Catalog (v1)). A manifest declaring a type/version
// outside this set is rejected at parse time. New manifests should prefer
// `valkey`; `redis` is kept for ecosystem compatibility and normalizes to the
// Valkey engine at provisioning time (redis 7 → valkey 8, RESP/ACL-compatible).
// The MySQL-family entries are the upstream LTS series; mysql 8.0 is past Oracle
// EOL but kept because Ghost pins it specifically.
//
// Postgres 15 is deprecated for new manifests (still accepted so existing
// installs keep working — there is no cross-version migration path yet, so
// removing it would strand any box already on it). New manifests should target
// 18: apps have started reaching for 18-only built-ins such as uuidv7().
var serviceVersions = map[string]map[string]bool{
	"postgres": {"15": true, "16": true, "17": true, "18": true},
	"valkey":   {"8": true},
	"redis":    {"7": true},
	"mysql":    {"8.0": true, "8.4": true},
	"mariadb":  {"10.11": true, "11.4": true},
}

// DefaultSecretBytes is the entropy drawn for a declared secret when `bytes` is
// omitted: 32 raw bytes → 43 base64url chars, comfortably past the "32+ char"
// floor most token-signing libraries (Better Auth, Rails secret_key_base) want.
const DefaultSecretBytes = 32

// MinSecretBytes is the floor on a declared secret's entropy. 16 bytes (128
// bits) is the minimum we'll generate even if a manifest asks for less.
const MinSecretBytes = 16

// secretName matches lowercase snake_case so the uppercased form is a valid,
// unambiguous environment-variable suffix: `auth`, `session_key` ok; `Auth`,
// `2fa`, `a-b` rejected.
var secretName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// configEnvName matches a bare uppercase env-var identifier for a config field's
// `app_env` (APP_MANIFEST.md # D4). Uppercase-only is enforced (not merely
// conventional) so the MOOSE_-prefix and reservedConfigEnv boundaries can't be
// bypassed by a lowercase variant: `OPENAI_API_KEY` ok; `openai_key`, `2fa`,
// `API-KEY` rejected.
var configEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// MaxPublicPaths caps how many path-scoped exceptions one app may declare
// (APP_MANIFEST.md # E2). The cap is not a security boundary — an author with
// enough entries can still cover an app — it keeps the list reviewable, and a
// manifest that needs more than this is really asking to be a public app.
const MaxPublicPaths = 16

// maxPublicPathLen caps one declared path. Long enough for any real API prefix,
// short enough that a path can be read at a glance in review.
const maxPublicPathLen = 128

// reservedConfigEnv is the loader/runtime denylist a config `app_env` may never
// take (APP_MANIFEST.md # D4 # Reserved names): names that reach process or
// platform internals. The MOOSE_ prefix is rejected separately (the brain's
// injected family). Grows if a new sensitive loader var appears (THREAT_MODEL.md).
var reservedConfigEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "SHELL": true, "HOSTNAME": true,
	"IFS": true, "LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
}

// folderTaxonomy is the fixed v1 use-case folder set (APP_ISOLATION.md # User
// content). User-defined folders are deferred.
var folderTaxonomy = map[string]bool{
	"photos": true, "documents": true, "movies": true,
	"music": true, "notes": true, "downloads": true,
}

// IsListed reports whether the app is visible/installable through the store.
// Absent (`listed:` omitted) defaults to true, so the catalog hides an app only
// when a manifest explicitly sets `listed: false` (APP_MANIFEST.md # A).
func (m *Manifest) IsListed() bool { return m.Listed == nil || *m.Listed }

func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	missing := func(field string) error {
		return fmt.Errorf("manifest missing required field: %s", field)
	}
	switch {
	case m.ID == "":
		return missing("id")
	case m.ManifestVersion == 0:
		return missing("manifest_version")
	case m.Name == "":
		return missing("name")
	case m.Version == "":
		return missing("version")
	case m.ComposeFile == "":
		return missing("compose_file")
	case m.MainService == "":
		return missing("main_service")
	case m.MainPort == 0:
		return missing("main_port")
	}
	if m.ManifestVersion != 1 {
		return fmt.Errorf("unsupported manifest_version %d (this build supports 1)", m.ManifestVersion)
	}
	// Slugs must be strict kebab-case so they stay parseable inside the
	// `<slug>--<user>` personal-instance scheme (DASHBOARD.md # instance
	// naming): single internal hyphens only — no leading/trailing hyphen and no
	// `--` run (which would collide with the owner separator and also covers the
	// reserved `xn--` prefix). The id is the fallback slug when preferred_slugs
	// is empty, so it's checked too.
	for _, slug := range append([]string{m.ID}, m.PreferredSlugs...) {
		if !kebabSlug.MatchString(slug) {
			return fmt.Errorf("slug %q must be kebab-case (lowercase alphanumerics, single internal hyphens)", slug)
		}
	}
	// icon_glyph is a Lucide name; same kebab-case shape as a slug. We can only
	// check the shape (the 1700+ name set lives in the UI), so a typo'd-but-valid
	// name passes here and falls back to the generic glyph client-side.
	if m.IconGlyph != "" && !kebabSlug.MatchString(m.IconGlyph) {
		return fmt.Errorf("icon_glyph %q must be a lucide icon name in kebab-case", m.IconGlyph)
	}
	if err := m.validateHealthProbe(); err != nil {
		return err
	}
	if err := m.validateSecrets(); err != nil {
		return err
	}
	if err := m.validateConfig(); err != nil {
		return err
	}
	if err := m.validateExternalCosts(); err != nil {
		return err
	}
	if err := m.validateServices(); err != nil {
		return err
	}
	if err := m.validateMail(); err != nil {
		return err
	}
	if err := m.validateAccess(); err != nil {
		return err
	}
	return ValidatePermissions(&m.Permissions)
}

// validateAccess checks the declared public paths (APP_MANIFEST.md # E2). Two
// shapes are allowed and nothing else:
//
//	/v1        an exact path
//	/v1/*      that path and everything under it
//
// Everything else is rejected, and each rule pays for itself:
//
//   - A path must start with "/" — a relative pattern matches nothing in Caddy,
//     so it would silently leave the API gated.
//   - "/" and "/*" are rejected outright. Either one exposes the whole app while
//     the dashboard still offers an access toggle for it, so the manifest would
//     be overriding a decision that belongs to the box owner.
//   - The wildcard may only appear as a trailing "/*". The obvious-looking
//     "/v1*" also matches "/v1admin", so an author reaching for "everything
//     under /v1" would open a sibling path they never read.
//   - "%", "?", "#", "\", "//" and ".." are rejected. Caddy matches a cleaned,
//     decoded path while the app sees the original URI, so a declaration
//     containing any of these means two different things on the two sides of the
//     proxy. Authors declare the plain, decoded prefix and let Caddy normalize.
//
// Matching in Caddy is case-insensitive, so "/v1/*" also opens "/V1/x"; entries
// are therefore de-duplicated case-insensitively and the spec says so. Absent ⇒
// no-op.
func (m *Manifest) validateAccess() error {
	paths := m.Access.PublicPaths
	if len(paths) == 0 {
		return nil
	}
	if len(paths) > MaxPublicPaths {
		return fmt.Errorf("access: %d public_paths declared, at most %d allowed", len(paths), MaxPublicPaths)
	}
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		switch {
		case p == "" || !strings.HasPrefix(p, "/"):
			return fmt.Errorf("access: public_paths entry %q must start with %q", p, "/")
		case p == "/" || p == "/*" || p == "/**":
			return fmt.Errorf("access: public_paths entry %q would make the whole app public; use the app's access toggle instead", p)
		case len(p) > maxPublicPathLen:
			return fmt.Errorf("access: public_paths entry %q is longer than %d characters", p, maxPublicPathLen)
		}
		for _, bad := range []string{"%", "?", "#", `\`, "//", ".."} {
			if strings.Contains(p, bad) {
				return fmt.Errorf("access: public_paths entry %q may not contain %q (declare the plain decoded path)", p, bad)
			}
		}
		if strings.ContainsAny(p, " \t") {
			return fmt.Errorf("access: public_paths entry %q may not contain whitespace", p)
		}
		// The only wildcard allowed is a trailing "/*". Trimming it must leave a
		// star-free literal path, which rejects "/v1*", "/*/v1" and "/v1/*/x".
		literal := strings.TrimSuffix(p, "/*")
		if strings.Contains(literal, "*") {
			return fmt.Errorf("access: public_paths entry %q may only use a trailing %q wildcard (%q matches %q too)", p, "/*", "/v1*", "/v1admin")
		}
		if literal == "" {
			return fmt.Errorf("access: public_paths entry %q would make the whole app public; use the app's access toggle instead", p)
		}
		key := strings.ToLower(p)
		if seen[key] {
			return fmt.Errorf("access: duplicate public_paths entry %q (matching is case-insensitive)", p)
		}
		seen[key] = true
	}
	return nil
}

// validateSecrets checks declared secret names and normalizes the byte length
// in place (APP_MANIFEST.md # secrets). Names must be snake_case and unique; a
// requested length below MinSecretBytes is raised to it, and an omitted length
// defaults to DefaultSecretBytes. Absent ⇒ no-op.
func (m *Manifest) validateSecrets() error {
	seen := make(map[string]bool, len(m.Secrets))
	for i := range m.Secrets {
		s := &m.Secrets[i]
		if !secretName.MatchString(s.Name) {
			return fmt.Errorf("secrets: name %q must be snake_case (lowercase, starting with a letter)", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("secrets: duplicate name %q", s.Name)
		}
		seen[s.Name] = true
		if s.Bytes == 0 {
			s.Bytes = DefaultSecretBytes
		} else if s.Bytes < MinSecretBytes {
			s.Bytes = MinSecretBytes
		}
	}
	return nil
}

// validateConfig checks the user-supplied config block and normalizes each
// field's type default in place (APP_MANIFEST.md # D4). Absent ⇒ no-op. The
// `service` field is NOT checked against the compose here (validate() has no
// compose); lifecycle backstops a non-existent service at install time.
func (m *Manifest) validateConfig() error {
	seen := make(map[string]bool, len(m.Config))
	for i := range m.Config {
		c := &m.Config[i]
		if !configEnvName.MatchString(c.AppEnv) {
			return fmt.Errorf("config: app_env %q must be an uppercase env-var name (e.g. OPENAI_API_KEY)", c.AppEnv)
		}
		if strings.HasPrefix(c.AppEnv, "MOOSE_") {
			return fmt.Errorf("config: app_env %q may not use the reserved MOOSE_ prefix", c.AppEnv)
		}
		if reservedConfigEnv[c.AppEnv] {
			return fmt.Errorf("config: app_env %q is a reserved runtime variable and may not be set", c.AppEnv)
		}
		if seen[c.AppEnv] {
			return fmt.Errorf("config: duplicate app_env %q", c.AppEnv)
		}
		seen[c.AppEnv] = true
		if c.Title == "" {
			return fmt.Errorf("config[%s]: title is required", c.AppEnv)
		}
		if c.Description == "" {
			return fmt.Errorf("config[%s]: description is required", c.AppEnv)
		}
		if c.Type == "" {
			c.Type = "text"
		}
		switch c.Type {
		case "text":
		case "enum":
			if len(c.Options) == 0 {
				return fmt.Errorf("config[%s]: type enum requires a non-empty options list", c.AppEnv)
			}
		case "bool":
			// A toggle always carries a concrete value, so an optional-blank bool
			// has no "inject nothing" state. Requiring a default keeps the toggle
			// well-defined and means the author explicitly owns the variable.
			if c.Default == "" {
				return fmt.Errorf("config[%s]: type bool requires a default of \"true\" or \"false\"", c.AppEnv)
			}
			if c.Default != "true" && c.Default != "false" {
				return fmt.Errorf("config[%s]: bool default must be \"true\" or \"false\", got %q", c.AppEnv, c.Default)
			}
		default:
			return fmt.Errorf("config[%s]: unknown type %q (allowed: text, enum, bool)", c.AppEnv, c.Type)
		}
		if c.Type != "enum" && len(c.Options) > 0 {
			return fmt.Errorf("config[%s]: options is only valid with type enum", c.AppEnv)
		}
		if c.Secret && c.Default != "" {
			return fmt.Errorf("config[%s]: a secret field may not carry a default", c.AppEnv)
		}
		if c.Type == "enum" && c.Default != "" && !slices.Contains(c.Options, c.Default) {
			return fmt.Errorf("config[%s]: default %q is not one of the options", c.AppEnv, c.Default)
		}
	}
	return nil
}

// validateExternalCosts checks the third-party-cost block (APP_MANIFEST.md # A).
// Absent ⇒ no-op. The pairing that matters is Estimate ⇄ EstimateChecked: a
// price with no date cannot be re-checked, and a date with no price describes
// nothing.
func (m *Manifest) validateExternalCosts() error {
	seen := make(map[string]bool, len(m.ExternalCosts))
	for i := range m.ExternalCosts {
		c := &m.ExternalCosts[i]
		if !kebabSlug.MatchString(c.ID) {
			return fmt.Errorf("external_costs[%d]: id %q must be kebab-case (e.g. model-access)", i, c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("external_costs: duplicate id %q", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Title) == "" {
			return fmt.Errorf("external_costs[%s]: title is required", c.ID)
		}
		if strings.TrimSpace(c.Description) == "" {
			return fmt.Errorf("external_costs[%s]: description is required", c.ID)
		}
		if c.Required == nil {
			return fmt.Errorf("external_costs[%s]: required must be stated true or false; there is no safe default for whether the app works without paying", c.ID)
		}
		// A blank-but-present estimate is an authoring slip, not the deliberate
		// "no honest rate exists" answer — that one omits the key. Rejected rather
		// than silently treated as absent, because it would render as an empty
		// line on the cost card and the curation source rejects it too.
		if c.Estimate != "" && strings.TrimSpace(c.Estimate) == "" {
			return fmt.Errorf("external_costs[%s]: estimate is blank; omit the key when there is no honest rate to quote", c.ID)
		}
		if c.Estimate == "" {
			if c.EstimateChecked != "" {
				return fmt.Errorf("external_costs[%s]: estimate_checked is set but there is no estimate to date", c.ID)
			}
			continue
		}
		if n := utf8.RuneCountInString(c.Estimate); n > EstimateMaxChars {
			return fmt.Errorf("external_costs[%s]: estimate is %d characters, max %d; quote the rate and move the rest to description", c.ID, n, EstimateMaxChars)
		}
		if c.EstimateChecked == "" {
			return fmt.Errorf("external_costs[%s]: an estimate needs estimate_checked (YYYY-MM-DD) so a stale price can be found", c.ID)
		}
		if _, err := time.Parse(time.DateOnly, c.EstimateChecked); err != nil {
			return fmt.Errorf("external_costs[%s]: estimate_checked %q must be a YYYY-MM-DD date", c.ID, c.EstimateChecked)
		}
	}
	return nil
}

// validateServices checks each managed-service declaration (APP_MANIFEST.md #
// D). The map key must be snake_case (it becomes the uppercased env-var suffix
// `MOOSE_SERVICE_<KEY>_*`, so the same rule as a secret name); the type must be
// a known managed kind and the version one this build runs for that kind. Absent
// ⇒ no-op.
func (m *Manifest) validateServices() error {
	for key, dep := range m.Services {
		if !secretName.MatchString(key) {
			return fmt.Errorf("services: key %q must be snake_case (lowercase, starting with a letter)", key)
		}
		versions, ok := serviceVersions[dep.Type]
		if !ok {
			return fmt.Errorf("services[%s]: unknown type %q (allowed: postgres, valkey, redis, mysql, mariadb)", key, dep.Type)
		}
		if dep.Version == "" {
			return fmt.Errorf("services[%s]: version is required", key)
		}
		if !versions[dep.Version] {
			return fmt.Errorf("services[%s]: unsupported %s version %q", key, dep.Type, dep.Version)
		}
	}
	return nil
}

// validateMail checks the optional outgoing-mail block (APP_MANIFEST.md # D).
// Absent ⇒ no-op. v1 supports only `optional: true`: an app that cannot run
// unbound would have to fail install on every box with no provider registered,
// so required mail is rejected until a real consumer needs it.
func (m *Manifest) validateMail() error {
	if m.Mail == nil {
		return nil
	}
	if !m.Mail.Optional {
		return fmt.Errorf("mail: v1 supports only optional mail (set `optional: true`; apps must run unbound)")
	}
	return nil
}

// validateHealthProbe checks the optional probe block and normalizes its
// start_period default in place (APP_MANIFEST.md # B). Absent ⇒ no-op. The path
// must be a non-empty absolute path (the brain GETs it through the app's Caddy
// route); healthy_status entries must be plausible HTTP codes.
func (m *Manifest) validateHealthProbe() error {
	if m.HealthProbe == nil {
		return nil
	}
	p := m.HealthProbe
	if p.Path == "" || !strings.HasPrefix(p.Path, "/") {
		return fmt.Errorf("health_probe.path must be a non-empty absolute path (e.g. /healthz), got %q", p.Path)
	}
	if p.StartPeriod < 0 {
		return fmt.Errorf("health_probe.start_period must not be negative, got %s", p.StartPeriod)
	}
	if p.StartPeriod == 0 {
		p.StartPeriod = DefaultStartPeriod
	}
	for _, s := range p.HealthyStatus {
		if s < 100 || s > 599 {
			return fmt.Errorf("health_probe.healthy_status: %d is not a valid HTTP status code", s)
		}
	}
	return nil
}

// ValidatePermissions normalizes folder defaults (mode=read, scope=whole) in
// place and rejects unknown folder names, modes, scopes, or malformed Door-2
// targets. It deliberately validates nothing about *source* — personal vs shared
// is resolved by the installer at install time, not declared here (APP_MANIFEST.md
// # folders). Exported because the Door-2 "Edit as YAML" overlay parses an
// admin-authored permissions block through the same gate (DASHBOARD.md # Form is
// a projection of the synthetic manifest).
func ValidatePermissions(p *Permissions) error {
	for i := range p.Folders {
		f := &p.Folders[i]
		if !folderTaxonomy[f.Folder] {
			return fmt.Errorf("permissions.folders: unknown folder %q (allowed: photos, documents, movies, music, notes, downloads)", f.Folder)
		}
		if f.Mode == "" {
			f.Mode = "read"
		}
		if f.Mode != "read" && f.Mode != "write" {
			return fmt.Errorf("permissions.folders[%s]: mode must be read or write, got %q", f.Folder, f.Mode)
		}
		if f.Scope == "" {
			f.Scope = "whole"
		}
		if f.Scope != "whole" && f.Scope != "pick-subfolder" {
			return fmt.Errorf("permissions.folders[%s]: scope must be whole or pick-subfolder, got %q", f.Folder, f.Scope)
		}
		if f.Default != "" {
			if f.Scope != "pick-subfolder" {
				return fmt.Errorf("permissions.folders[%s]: default is only valid with scope: pick-subfolder", f.Folder)
			}
			if strings.HasPrefix(f.Default, "/") || strings.Contains(f.Default, "..") {
				return fmt.Errorf("permissions.folders[%s]: default must be a relative subpath under the folder", f.Folder)
			}
		}
		// Door-2-only: the explicit in-container destination must be an absolute
		// path with no traversal (it's a container path the admin typed, not a
		// host path — host binds are an admission concern). Store grants omit it.
		if f.Target != "" && (!strings.HasPrefix(f.Target, "/") || strings.Contains(f.Target, "..")) {
			return fmt.Errorf("permissions.folders[%s]: target must be an absolute in-container path with no '..', got %q", f.Folder, f.Target)
		}
	}
	return nil
}

// kebabSlug matches lowercase alphanumeric labels joined by single hyphens:
// `home-assistant` ok; `whoami-`, `-x`, `a--b`, `xn--y`, `Foo` rejected.
var kebabSlug = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
