// Package catalog is the box's read model of the app store. It exposes a fixed
// six-method surface — List, Entry, Detail, IconPath, ScreenshotPath, Load — that
// internal/api (the box UI's catalog routes) and internal/lifecycle (install) both
// consume, and hides behind it whether the catalog is synced from the catalog
// service or read from a local directory tree.
//
// In production every box — appliance and hosted alike — uses the remote client
// (NewRemote): a thin HTTP consumer of the catalog service's API. Browse data
// (GET /catalog) is held in memory and never written to disk; an app's install
// payload is fetched per app, at install time (remote.go). No catalog is baked
// into the image (#62). The original disk reader (New) is
// retained only as the constructor internal/api and internal/lifecycle tests build
// a controlled catalog with; it implements the same private source interface, so
// the rest of the brain holds a *Catalog and is agnostic to which is wired.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/onmoose/os/internal/manifest"
)

// ErrNotFound is returned by the lookup methods when no app exists for the id (on
// disk: the directory or manifest.yml is absent; remote: no such app in the synced
// snapshot). It is deliberately distinct from a manifest that exists but fails to
// parse or is missing its compose file — those are integrity errors a curated
// catalog should never ship — so the API maps ErrNotFound to 404 and every other
// error to 500. Follows the "typed errors at boundaries" rule (CLAUDE.md).
var ErrNotFound = errors.New("catalog: manifest not found")

// source is the read model behind Catalog: the six methods the brain consumes,
// implemented once per backing (disk / remote). Private because the brain always
// talks to *Catalog; the facade lets the remote client back production while the
// disk reader stays available to tests, with no change to internal/api or
// internal/lifecycle.
type source interface {
	List() ([]Entry, error)
	Entry(id string) (Entry, error)
	Detail(id string) (Detail, error)
	IconPath(id string) (string, error)
	ScreenshotPath(id string, i int) (string, error)
	// Load returns the app's install payload: the parsed manifest and the
	// verbatim compose bytes. It takes a context because the remote source
	// fetches the two documents over the network (#434) — it is an install-path
	// call only. For an app that is ALREADY installed, read the manifest the
	// installer persisted next to it (lifecycle InstanceManifest) instead, so
	// routine box operation never depends on the catalog service.
	Load(ctx context.Context, id string) (*manifest.Manifest, []byte, error)
	// featured returns the curated "top apps" for this box's surface, in the order
	// they should render (ascending rank). It is the only segmentation input the
	// facade can't derive from List: the remote source reads it from the synced
	// snapshot's Featured/Rank; the disk source has no curation and returns nil.
	featured() ([]Entry, error)
	// categories returns the authored category vocabulary (id + display label) in
	// authored order, or nil when the source has none — the disk source has no
	// curation, and a never-synced remote has no snapshot. A nil vocabulary is not
	// an error: the facade falls back to a readable form of each id.
	categories() ([]Category, error)
	// home returns the authored landing page's spotlight app (nil when unset or
	// not carried on this surface) and its category groups (a group left with no
	// resolvable apps is dropped). Like featured, this can't be derived from
	// List: the remote source reads it from the synced browse payload's home
	// block; the disk source has no curation and returns nothing.
	home() (*Entry, []HomeGroupView, error)
	// aiProviders returns the AI provider data in authored order, or nil when
	// the source has none (the disk source, a never-synced remote, or a catalog
	// without the field). aiProviderLogoPath resolves one provider's logo, or
	// its dark variant, to a local file, the way IconPath does for an app.
	aiProviders() ([]AIProvider, error)
	aiProviderLogoPath(id string, dark bool) (string, error)
	// sections returns the authored landing sections, resolved against the
	// apps and packs the source holds, in authored order (APP_STORE.md #
	// Landing page). packs returns every pack the source can show, in
	// authored order. Group labels are left empty: the facade fills them from
	// the vocabulary. Both are nil for a source with no curation.
	sections() ([]HomeSection, error)
	packs() ([]Pack, error)
	// illustrationPath resolves a slide or pack illustration, by the key in its
	// brain URL (illustrationURL), to a local file, the way IconPath does for
	// an app icon.
	illustrationPath(key string) (string, error)
}

// Catalog is the brain-facing catalog handle. It is a thin facade over a source;
// New builds the disk-backed one, NewRemote the catalog-service client.
type Catalog struct{ src source }

// New builds a disk-backed catalog rooted at a directory tree of
// <root>/<manifest_id>/{manifest.yml, <compose_file>}. Production no longer uses
// it (every box is a thin client of the catalog service, NewRemote); it is
// retained as the
// constructor internal/api and internal/lifecycle tests build a controlled catalog
// with, off a temp directory.
func New(root string) *Catalog { return &Catalog{src: newDiskSource(root)} }

func (c *Catalog) List() ([]Entry, error)             { return c.src.List() }
func (c *Catalog) Entry(id string) (Entry, error)     { return c.src.Entry(id) }
func (c *Catalog) Detail(id string) (Detail, error)   { return c.src.Detail(id) }
func (c *Catalog) IconPath(id string) (string, error) { return c.src.IconPath(id) }
func (c *Catalog) ScreenshotPath(id string, i int) (string, error) {
	return c.src.ScreenshotPath(id, i)
}
func (c *Catalog) Load(ctx context.Context, id string) (*manifest.Manifest, []byte, error) {
	return c.src.Load(ctx, id)
}

// Home / Category / Search are the segmented store views the box UI browses
// through, mirroring the catalog service's public API so the box never pulls the
// whole catalog up front. They
// are derived from the same source the flat List reads — the box already holds the
// whole snapshot in memory, so these stay same-origin and need no round trip,
// rather than re-proxying each hit to the catalog service. They are computed on the facade (not per-source) because every input but
// featured is a projection of List; only featured differs by backing.

// Home is the store landing payload: the categories present on this box's
// surface, the authored recommended-apps page (a spotlight app plus category
// groups), and the flat curated top apps for a consumer that just wants the
// top-apps row. No per-app grid — the UI drills into a category or search for
// that. Mirror of the catalog service's Home.
type Home struct {
	// Categories are the categories present on this surface, each with its
	// authored display label, in the vocabulary's authored order. Carrying the
	// label is what stops the UI deriving display text from the id.
	Categories []Category `json:"categories"`
	// Spotlight is the banner app, nil when unset or not advertised on this
	// surface.
	Spotlight *Entry `json:"spotlight,omitempty"`
	// Groups are the authored category rows, in authored order. A group whose
	// apps are all hidden on this surface is dropped rather than rendered empty.
	Groups   []HomeGroupView `json:"groups,omitempty"`
	Featured []Entry         `json:"featured,omitempty"`
	// Sections is the authored landing page, in the order it renders: search,
	// discover, intents, packs and categories. When it is empty the UI draws
	// the older landing from Spotlight, Groups and Featured.
	Sections []HomeSection `json:"sections,omitempty"`
}

// The landing section types the box knows. A section of any other type is
// dropped when the snapshot is resolved, so the store can publish a new one
// before the box can draw it.
const (
	SectionSearch     = "search"
	SectionDiscover   = "discover"
	SectionIntents    = "intents"
	SectionPacks      = "packs"
	SectionCategories = "categories"
)

// HomeSection is one resolved landing section. Which fields it uses depends on
// Type: search has Suggestions, discover has Slides, intents and packs have
// Packs, and categories has Groups. Every app is a full Entry and every pack a
// full Pack, so the UI needs no second request to draw the page.
type HomeSection struct {
	Type        string          `json:"type"`
	Title       string          `json:"title,omitempty"`
	Suggestions []string        `json:"suggestions,omitempty"`
	Slides      []Slide         `json:"slides,omitempty"`
	Packs       []Pack          `json:"packs,omitempty"`
	Groups      []HomeGroupView `json:"groups,omitempty"`
}

// Slide is one discover slide. Size is "hero" or "side". Headline and Blurb
// are the authored text, empty when the slide uses the app's own name and
// tagline. IllustrationURL is the brain's own route for the art, empty when
// the slide has none.
type Slide struct {
	App             Entry  `json:"app"`
	Size            string `json:"size"`
	Headline        string `json:"headline,omitempty"`
	Blurb           string `json:"blurb,omitempty"`
	IllustrationURL string `json:"illustration_url,omitempty"`
}

// Pack is a named set of apps that meet one need. A pack never installs in one
// step: its page lists the apps, and the user installs each one on its own.
// Keywords are what search matches the pack on, next to its title.
// IllustrationURL is the brain's own route for the art, empty when the pack
// has none.
type Pack struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Description     string   `json:"description,omitempty"`
	IllustrationURL string   `json:"illustration_url,omitempty"`
	Apps            []Entry  `json:"apps"`
	Keywords        []string `json:"keywords,omitempty"`
}

// HomeGroupView is one rendered category row of the landing page. Label is the
// row heading — the authored one, so the heading and the pill for the same id
// always read identically. Mirror of the catalog service's own HomeGroupView shape.
type HomeGroupView struct {
	Category string  `json:"category"`
	Label    string  `json:"label"`
	Apps     []Entry `json:"apps"`
}

// Category is one entry of the authored category vocabulary: the id apps tag
// themselves with, and the display label the UI renders. Authored in the store
// curation source and carried on the snapshot, never derived here. Mirror of the
// catalog service's own Category shape.
type Category struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// CategoryPage is one category's apps, its authored display label, plus the
// curated top apps. Named CategoryPage, not Category, because Category is the
// authored vocabulary entry above — this is the rendered page, that is the
// datum. Featured travels on the payload for parity with the catalog service's
// own Category shape (which this mirrors), but the box UI's category view does
// not currently render it — only the landing does, and only as its
// no-authored-home fallback (docs/specs/APP_STORE.md # Landing page); a
// category view is a filtered view, and the curated row is a landing-only
// concept, matching how the website's store pages render a category.
type CategoryPage struct {
	Category string  `json:"category"`
	Label    string  `json:"label"`
	Apps     []Entry `json:"apps"`
	Featured []Entry `json:"featured,omitempty"`
}

// Home returns the landing payload: the sorted union of every browsable app's
// categories, plus the featured row.
func (c *Catalog) Home() (Home, error) {
	apps, err := c.src.List()
	if err != nil {
		return Home{}, err
	}
	seen := map[string]bool{}
	for _, a := range apps {
		for _, cat := range a.Categories {
			seen[cat] = true
		}
	}
	vocab, err := c.src.categories()
	if err != nil {
		return Home{}, err
	}
	cats := presentCategories(vocab, seen)
	feat, err := c.src.featured()
	if err != nil {
		return Home{}, err
	}
	spotlight, groups, err := c.src.home()
	if err != nil {
		return Home{}, err
	}
	for i := range groups {
		groups[i].Label = labelFor(vocab, groups[i].Category)
	}
	sections, err := c.src.sections()
	if err != nil {
		return Home{}, err
	}
	for i := range sections {
		for j := range sections[i].Groups {
			sections[i].Groups[j].Label = labelFor(vocab, sections[i].Groups[j].Category)
		}
	}
	return Home{Categories: cats, Spotlight: spotlight, Groups: groups, Featured: feat, Sections: sections}, nil
}

// Pack returns one pack by id. ErrNotFound when this box cannot show it: the
// catalog does not carry it, or one of its apps is not on this box.
func (c *Catalog) Pack(id string) (Pack, error) {
	packs, err := c.src.packs()
	if err != nil {
		return Pack{}, err
	}
	for _, p := range packs {
		if p.ID == id {
			return p, nil
		}
	}
	return Pack{}, fmt.Errorf("%w: pack %q", ErrNotFound, id)
}

// IllustrationPath returns a local file path to a slide or pack illustration,
// by the key in its brain URL. ErrNotFound when the snapshot names no such art.
func (c *Catalog) IllustrationPath(key string) (string, error) {
	return c.src.illustrationPath(key)
}

// presentCategories returns the vocabulary entries for the categories actually
// present on this surface, in authored order. A category the vocabulary doesn't
// carry still gets a pill — dropping it would hide a browsable app — with a
// readable form of its id, appended after the authored ones in sorted order so
// the result is deterministic.
func presentCategories(vocab []Category, present map[string]bool) []Category {
	out := make([]Category, 0, len(present))
	known := make(map[string]bool, len(vocab))
	for _, c := range vocab {
		known[c.ID] = true
		if present[c.ID] {
			out = append(out, c)
		}
	}
	var extra []string
	for id := range present {
		if !known[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		out = append(out, Category{ID: id, Label: readableID(id)})
	}
	return out
}

// labelFor resolves a category id to its authored display label, falling back to a
// readable form of the id. The fallback should be unreachable for a synced box —
// the publish flow rejects a category with no label — but it also covers the disk
// source and a never-synced box, where there is no vocabulary at all.
func labelFor(vocab []Category, id string) string {
	for _, c := range vocab {
		if c.ID == id {
			return c.Label
		}
	}
	return readableID(id)
}

// readableID makes a category id presentable ("developer-tools" -> "Developer
// tools"). Only the label fallbacks use it. The other store surfaces hold the
// same fallback so they agree even when neither has a label.
func readableID(id string) string {
	s := strings.ReplaceAll(id, "-", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Category returns the apps tagged with cat (case-insensitive), plus the featured
// row. ErrNotFound when no browsable app carries the category, so an unknown or
// emptied category is a clean 404 rather than an empty page.
func (c *Catalog) Category(cat string) (CategoryPage, error) {
	apps, err := c.src.List()
	if err != nil {
		return CategoryPage{}, err
	}
	var out []Entry
	for _, a := range apps {
		if containsFold(a.Categories, cat) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return CategoryPage{}, fmt.Errorf("%w: category %q", ErrNotFound, cat)
	}
	feat, err := c.src.featured()
	if err != nil {
		return CategoryPage{}, err
	}
	vocab, err := c.src.categories()
	if err != nil {
		return CategoryPage{}, err
	}
	return CategoryPage{Category: cat, Label: labelFor(vocab, cat), Apps: out, Featured: feat}, nil
}

// SearchResult is what a store search finds: the matching apps, then the
// matching packs. The UI shows them in that order.
type SearchResult struct {
	Apps  []Entry `json:"apps"`
	Packs []Pack  `json:"packs"`
}

// Search returns the packs whose title or keywords match q (matchesPack) and
// the apps that match: first the apps of the matching packs, because a pack is
// the authored answer to a described need ("back up my photos"), then the
// browsable apps whose name, short description, or categories contain q
// (case-insensitive substring). Each app appears once. A blank query returns
// nothing rather than the whole catalog: search narrows, browse is for
// everything.
func (c *Catalog) Search(q string) (SearchResult, error) {
	q = strings.TrimSpace(strings.ToLower(q))
	if q == "" {
		return SearchResult{}, nil
	}
	apps, err := c.src.List()
	if err != nil {
		return SearchResult{}, err
	}
	packs, err := c.src.packs()
	if err != nil {
		return SearchResult{}, err
	}
	var out SearchResult
	seen := map[string]bool{}
	for _, p := range packs {
		if !matchesPack(p, q) {
			continue
		}
		out.Packs = append(out.Packs, p)
		for _, a := range p.Apps {
			if !seen[a.ID] {
				seen[a.ID] = true
				out.Apps = append(out.Apps, a)
			}
		}
	}
	for _, a := range apps {
		if seen[a.ID] {
			continue
		}
		hay := strings.ToLower(strings.Join(append([]string{a.Name, a.ShortDescription}, a.Categories...), "\n"))
		if strings.Contains(hay, q) {
			seen[a.ID] = true
			out.Apps = append(out.Apps, a)
		}
	}
	return out, nil
}

// matchesPack reports whether a query matches a pack's title or one of its
// keywords, as phrases: the query starts a word of the phrase ("phot" matches
// "Back up photos"), or the query holds the whole phrase ("back up my photos"
// matches the keyword "photos"). Both sides are normalized first, so case and
// punctuation do not matter. A query shorter than two characters matches no
// pack, so the first keystroke does not list every pack. The other store
// surface matches packs the same way.
func matchesPack(p Pack, q string) bool {
	words := normalizePhrase(q)
	if len([]rune(words)) < 2 {
		return false
	}
	for _, phrase := range append([]string{p.Title}, p.Keywords...) {
		phrase = normalizePhrase(phrase)
		if phrase == "" {
			continue
		}
		if strings.Contains(" "+phrase, " "+words) || strings.Contains(" "+words+" ", " "+phrase+" ") {
			return true
		}
	}
	return false
}

// normalizePhrase lowercases s and turns every run of characters that are not
// letters or digits into one space, so "Back up my photos!" and "back up my
// photos" are the same phrase.
func normalizePhrase(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
			continue
		}
		space = true
	}
	return b.String()
}

// containsFold reports whether needle equals any of haystack, case-insensitively —
// the category match, so "Media" and "media" are the same store category.
func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

// StartRefresh starts the background sync loop of a remote catalog, bound to ctx
// (it stops when ctx is cancelled). It is a no-op for a disk-backed catalog, so
// cmd/brain can call it unconditionally. The first sync also runs immediately so a
// freshly provisioned box populates its store without waiting a full interval.
func (c *Catalog) StartRefresh(ctx context.Context) {
	if r, ok := c.src.(*remoteSource); ok {
		r.startRefresh(ctx)
	}
}

// Entry is the store-facing summary of one available app. It carries exactly what
// the browse grid needs to render a card without a second fetch (APP_STORE.md #
// Catalog schema): the identity, the one-liner, the categories, and an icon URL.
// The detail page fetches Detail for the rest.
type Entry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`

	// ShortDescription is the one-line tagline (manifest description.short).
	ShortDescription string `json:"short_description,omitempty"`
	// Categories group the app in the store (manifest categories).
	Categories []string `json:"categories,omitempty"`
	// IconURL points at the brain's icon asset route for this app, set only when
	// the app declares an icon. Empty ⇒ the store falls back to a glyph. The route
	// is always the brain's own /api/v1/catalog/{id}/icon — never the control
	// plane directly — so the box UI stays on the box origin (AUTH_AND_ACCESS.md;
	// the brain proxies the asset for a remote catalog).
	IconURL string `json:"icon_url,omitempty"`
	// IconGlyph is the manifest's Lucide-icon fallback name (kebab-case) the store
	// renders when IconURL is empty. Empty (and no IconURL) ⇒ the generic glyph.
	IconGlyph string `json:"icon_glyph,omitempty"`

	// Footprint is the on-disk summary the store grid renders without a second
	// fetch (APP_STORE.md # Catalog schema). The image totals are an upper bound
	// — nothing is assumed cached locally, so the install dialog shows a sharper,
	// box-specific figure (BRAIN_UI_PROTOCOL.md # install-plan). The app-state
	// figure (estimated_size) is the manifest's measured baseline at install, not
	// a usage projection (APP_MANIFEST.md # Storage, DECISIONS.md 2026-06-09).
	Footprint manifest.Footprint `json:"footprint"`

	// needsRemap is true when the app sets root_setup or image_user, so it can
	// only run on a Docker daemon with the userns-remap. It is not on the API:
	// its one use is WithoutRemapApps, which leaves such an app out of the
	// store lists on a box with no remap.
	needsRemap bool
}

// Detail is the full store detail-page view of one app (APP_STORE.md # Catalog
// schema): everything in Entry plus the long markdown body, screenshots, and the
// author/license/links metadata. Rendered by the app detail page; the brain acts
// on none of it.
type Detail struct {
	Entry

	// LongDescription is the markdown body shown on the detail page
	// (manifest description.long).
	LongDescription string `json:"long_description,omitempty"`
	// ScreenshotURLs point at the brain's screenshot asset route, in manifest
	// order. Empty ⇒ no gallery.
	ScreenshotURLs []string `json:"screenshot_urls,omitempty"`

	// Author and Links are pointers so a manifest that declares neither omits the
	// keys entirely rather than serializing `{}` — `omitempty` is a no-op on a
	// struct value (only pointers/slices/maps/scalars count as empty), which
	// would otherwise hand the UI an empty-but-present block to render.
	Author       *manifest.Author `json:"author,omitempty"`
	License      string           `json:"license,omitempty"`
	Links        *manifest.Links  `json:"links,omitempty"`
	ChangelogURL string           `json:"changelog_url,omitempty"`

	// ExternalCosts is what a third party charges to make the app useful, shown
	// before install so a bill from someone else is never a surprise. Detail
	// only, not Entry: it is a paragraph of reading, so it belongs on the page
	// where someone decides to install, not on a grid card.
	ExternalCosts []ExternalCost `json:"external_costs,omitempty"`
}

// iconURL / screenshotURL are the brain-served asset routes the store loads
// directly in <img> tags (APP_STORE.md # Catalog schema). Both catalog sources
// project these same brain-origin URLs (the remote source proxies the underlying
// published asset behind them), so the UI's hard-coded route shapes never
// change. Kept here so the URL shape lives next to the types that carry it.
func iconURL(id string) string { return "/api/v1/catalog/" + id + "/icon" }

// illustrationURL is the brain-served route for a slide or pack illustration.
// The art has no id of its own, so the route takes a key made from its
// published URL (illustrationKey). The key is a query value, not a path
// segment, because /api/v1/catalog/<x>/<y> paths would clash with the
// /api/v1/catalog/{id}/icon and /install-plan routes in net/http's mux.
func illustrationURL(key string) string { return "/api/v1/catalog/illustration?key=" + key }

// illustrationKey names an illustration by a short hash of its published URL.
// The same URL always gets the same key, and new art at a new URL gets a new
// key, so a browser cache never shows old art under a new name.
func illustrationKey(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:8])
}
func screenshotURL(id string, i int) string {
	return fmt.Sprintf("/api/v1/catalog/%s/screenshots/%d", id, i)
}
