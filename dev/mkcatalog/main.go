// Command mkcatalog generates a control-plane catalog snapshot (the GET /catalog
// browse wire format, with each app's manifest and compose inlined for the seed
// seam) from one or more on-disk app packages, optionally with a curated
// landing page and a list of AI providers. The brain reads that snapshot once
// at boot (MOOSE_CATALOG_FILE, internal/catalog/remote.go # loadSnapshotFile)
// and installs an app from it, so this exercises the real remote read path
// (verify → project → Load) with no catalog/ directory in the image and no
// control plane to reach. The file is an input for dev and test lanes only: a
// box keeps no catalog on disk and a real one never sets MOOSE_CATALOG_FILE.
//
// Two callers:
//
//   - `make dev-app APP=<id>` (single app) / `make seed-catalog APPS="<id> ..."
//     [HOMEFILE=<path/to/home.yml>]` (several apps, plus an optional curated
//     landing): the native dev inner loop for authoring/curating store apps
//     against the brain as a thin client of the control plane. It reads
//     apps/<id>/manifest.yml + compose straight from a store curation checkout,
//     so it needs no verdict on the app (unlike the control plane's own publish
//     tool, which serves only listed: true records) — you boot the app to
//     *decide* its verdict. The Makefile points MOOSE_CATALOG_URL at an inert
//     address so the background sync can't replace the seed with the real
//     published catalog.
//
//   - dev/test-qemu/bootstrap.sh: the air-gapped QEMU full-stack lane, which
//     can't reach a control plane (restrict=on), seeds a whoami snapshot at
//     image-build time (single -pkg, no -home — unchanged).
//
// Display-only fidelity: it fills the inline card fields (name, descriptions,
// author, license, links, categories, footprint) but not icon_file/screenshots —
// those are proxied from the control plane, and the seed path has no asset server,
// so an authored icon renders as its glyph fallback. For full visual QA run a
// local control plane instead.
//
// The apps + optional home block are built as internal/catalog's own exported
// SnapshotApp / SnapshotHome types (aliases of its wire shape) and handed to
// catalog.BuildSnapshot, which stamps the schema version and the version token
// and marshals — so this tool never re-declares the wire shape. Keeping the
// shape in exactly one place (internal/catalog/wire.go) is what keeps a seed
// file readable by the brain that parses it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/manifest"
	"gopkg.in/yaml.v3"
)

// homeYAML is the shape of the store curation source's home.yml: a spotlight app
// id plus ordered category groups. Parsed straight into catalog.SnapshotHome via
// the same field names — home.yml IS the wire shape here (the control plane's
// sync tool carries it verbatim too).
type homeYAML struct {
	Spotlight string `yaml:"spotlight"`
	Groups    []struct {
		Category string   `yaml:"category"`
		Apps     []string `yaml:"apps"`
	} `yaml:"groups"`
}

// categoriesYAML is the shape of the store curation source's categories.yml: the
// closed category vocabulary, each id with the display label the store surfaces
// render. Parsed straight into catalog.SnapshotCategory via the same field names,
// for the same reason homeYAML is — the file IS the wire shape here.
type categoriesYAML struct {
	Categories []struct {
		ID    string `yaml:"id"`
		Label string `yaml:"label"`
	} `yaml:"categories"`
}

// pkgList collects one or more -pkg flags, in the order given — flag.Value's Set
// is called once per occurrence, so a single -pkg (every call site today) behaves
// exactly as before, and a curated multi-app seed just passes it more than once.
type pkgList []string

func (p *pkgList) String() string { return strings.Join(*p, ",") }
func (p *pkgList) Set(v string) error {
	*p = append(*p, v)
	return nil
}

func main() {
	var (
		pkgs    pkgList
		out     = flag.String("out", "", "output snapshot path (default: stdout)")
		homeArg = flag.String("home", "", "path to a store home.yml to carry as the snapshot's curated landing page (optional)")
		catsArg = flag.String("categories", "", "path to a store categories.yml to carry as the snapshot's category vocabulary (optional)")
		aiArg   = flag.String("ai-providers", "", "path to a JSON list of AI providers, in the published ai_providers shape, to carry on the snapshot (optional)")
	)
	flag.Var(&pkgs, "pkg", "app package directory (contains manifest.yml + compose file); repeatable")
	flag.Parse()
	if len(pkgs) == 0 {
		fatal("mkcatalog: at least one -pkg is required")
	}

	apps := make([]catalog.SnapshotApp, 0, len(pkgs))
	for _, pkgDir := range pkgs {
		apps = append(apps, loadApp(pkgDir))
	}

	var home catalog.SnapshotHome
	if *homeArg != "" {
		home = loadHome(*homeArg, apps)
	}
	var cats []catalog.SnapshotCategory
	if *catsArg != "" {
		cats = loadCategories(*catsArg)
	}

	var providers []catalog.SnapshotAIProvider
	if *aiArg != "" {
		providers = loadAIProviders(*aiArg)
	}

	b, err := catalog.BuildSnapshot(apps, home, cats, providers, "")
	if err != nil {
		fatal("build snapshot: %v", err)
	}
	if *out == "" || *out == "-" {
		os.Stdout.Write(b)
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fatal("write %q: %v", *out, err)
	}
}

// loadApp parses one package directory's manifest + compose into the wire app
// shape, carrying the author/links display metadata too, so the store card an
// author eyeballs during a curation boot is the real one. Assets (icon_url /
// screenshot_urls) are deliberately omitted: the box proxies those from the
// control plane, and the seed path has no asset server behind the inert URL, so
// a URL here would only 404 (docs/dev/authoring-apps-with-an-agent.md).
//
// The manifest and compose are inlined on the record. A published record carries
// manifest_url / compose_url instead and the box fetches them at install time
// (#434), but a staged seed file has no control plane behind it to serve those
// routes, so the inline fields are the seed seam (internal/catalog/wire.go).
func loadApp(pkgDir string) catalog.SnapshotApp {
	manBytes, err := os.ReadFile(filepath.Join(pkgDir, "manifest.yml"))
	if err != nil {
		fatal("read manifest: %v", err)
	}
	man, err := manifest.Parse(manBytes)
	if err != nil {
		fatal("parse manifest: %v", err)
	}
	composeBytes, err := os.ReadFile(filepath.Join(pkgDir, man.ComposeFile))
	if err != nil {
		fatal("read compose %q: %v", man.ComposeFile, err)
	}

	a := catalog.SnapshotApp{
		ID:               man.ID,
		Name:             man.Name,
		Version:          man.Version,
		ShortDescription: man.Description.Short,
		LongDescription:  man.Description.Long,
		Categories:       man.Categories,
		IconGlyph:        man.IconGlyph,
		License:          man.License,
		ChangelogURL:     man.ChangelogURL,
		Footprint:        man.Footprint(),
		Manifest:         string(manBytes),
		Compose:          string(composeBytes),
	}
	if man.Author != (manifest.Author{}) {
		a.Author = &man.Author
	}
	if man.Links != (manifest.Links{}) {
		a.Links = &man.Links
	}
	return a
}

// loadHome parses a store home.yml and validates it against the seeded apps: any
// spotlight or group app id that isn't among the packages just seeded is a fatal
// error, not a silently-dropped slot — the whole point of exercising this locally
// is to catch that before a real publish does (the publish flow's own admission
// check catches it there; this is the equivalent local guard).
func loadHome(path string, apps []catalog.SnapshotApp) catalog.SnapshotHome {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read home: %v", err)
	}
	var y homeYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		fatal("parse home %q: %v", path, err)
	}

	seeded := make(map[string]bool, len(apps))
	for _, a := range apps {
		seeded[a.ID] = true
	}
	requireSeeded := func(id string) {
		if !seeded[id] {
			fatal("home %q names app %q, which was not seeded with -pkg (seed it too, or drop it from home.yml)", path, id)
		}
	}

	home := catalog.SnapshotHome{Spotlight: y.Spotlight}
	if y.Spotlight != "" {
		requireSeeded(y.Spotlight)
	}
	for _, g := range y.Groups {
		for _, id := range g.Apps {
			requireSeeded(id)
		}
		home.Groups = append(home.Groups, catalog.SnapshotHomeGroup{Category: g.Category, Apps: g.Apps})
	}
	return home
}

// loadCategories parses a store categories.yml into the snapshot's vocabulary. It
// requires a label per entry for the same reason the publish flow does: a category
// with no label is a pill the store UI renders blank, and the point of seeding
// locally is to hit that here rather than in a browser.
func loadCategories(path string) []catalog.SnapshotCategory {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read categories: %v", err)
	}
	var y categoriesYAML
	if err := yaml.Unmarshal(data, &y); err != nil {
		fatal("parse categories %q: %v", path, err)
	}
	out := make([]catalog.SnapshotCategory, 0, len(y.Categories))
	for _, c := range y.Categories {
		if c.ID == "" || c.Label == "" {
			fatal("categories %q: every entry needs an id and a label (got id=%q label=%q)", path, c.ID, c.Label)
		}
		out = append(out, catalog.SnapshotCategory{ID: c.ID, Label: c.Label})
	}
	return out
}

// loadAIProviders reads a JSON list of AI providers in the same shape the
// catalog service publishes as ai_providers, so the setup page's provider
// tiles can be tried against a seeded store. It is read as given: the brain
// applies its own lenient reading when it loads the seed, so what it drops
// here shows in the brain's log, as it would for a published catalog.
func loadAIProviders(path string) []catalog.SnapshotAIProvider {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read ai providers: %v", err)
	}
	var out []catalog.SnapshotAIProvider
	if err := json.Unmarshal(data, &out); err != nil {
		fatal("parse ai providers %q: %v", path, err)
	}
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
