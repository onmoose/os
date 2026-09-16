package catalog

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/onmoose/os/internal/manifest"
)

// diskSource reads app manifests from a directory tree:
// <root>/<manifest_id>/{manifest.yml, <compose_file>} (APP_STORE.md). Production
// no longer uses it — every box syncs the catalog from the control plane
// (remote.go) and no catalog is baked into the image (cloud #62) — but it is
// retained as the backing internal/api and internal/lifecycle tests construct via
// catalog.New off a temp directory.
type diskSource struct{ root string }

func newDiskSource(root string) *diskSource { return &diskSource{root: root} }

// entryFor builds the grid Entry from a parsed manifest.
func entryFor(man *manifest.Manifest) Entry {
	e := Entry{
		ID:               man.ID,
		Name:             man.Name,
		Version:          man.Version,
		ShortDescription: man.Description.Short,
		Categories:       man.Categories,
		IconGlyph:        man.IconGlyph,
		Footprint:        man.Footprint(),
	}
	if man.Icon != "" {
		e.IconURL = iconURL(man.ID)
	}
	return e
}

// List returns the store browse grid: one Entry per *listed* app, sorted by
// name. Unlisted apps (`listed: false`) are skipped — they exist on disk and
// load fine (Load/Entry still resolve them for an installed instance's card),
// but the store doesn't advertise them. For installed-instance enrichment by a
// known id, use Entry, not List, so an app unlisted after install keeps its
// metadata (APP_STORE.md # Listed apps).
func (d *diskSource) List() ([]Entry, error) {
	dirs, err := os.ReadDir(d.root)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	var out []Entry
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		man, _, err := d.load(dir.Name())
		if err != nil {
			continue // skip malformed entries
		}
		if !man.IsListed() {
			continue // hidden from browse (APP_STORE.md # Listed apps)
		}
		out = append(out, entryFor(man))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// featured returns nothing: featuring is store curation carried on the published
// snapshot (Featured/Rank), which a disk manifest tree has no notion of. The disk
// source backs only tests now, so its Home/Category views render with an empty
// featured row — honest, and enough for the handler tests that exercise it.
func (d *diskSource) featured() ([]Entry, error) { return nil, nil }

// home returns nothing: the recommended-apps page is store curation carried on
// the published snapshot (home.yml via the sync tool), which a disk manifest
// tree has no notion of. The disk source backs only tests now, so its Home view
// renders with no spotlight/groups — honest, and enough for the handler tests
// that exercise it.
func (d *diskSource) home() (*Entry, []HomeGroupView, error) { return nil, nil, nil }

// categories: the disk source is a plain directory of app packages with no
// curation alongside it, so there is no authored vocabulary — the facade falls
// back to a readable form of each id, exactly as it does for a never-synced box.
func (d *diskSource) categories() ([]Category, error) { return nil, nil }

// Entry returns the grid summary for one app by id, honestly — it does *not*
// apply the store-visibility filter, so an unlisted-but-installed app still
// resolves its card metadata. ErrNotFound when the manifest doesn't exist; other
// Load errors are integrity failures. This is the lookup the instance list uses
// to enrich an installed app; List is the store-facing (filtered) browse.
func (d *diskSource) Entry(manifestID string) (Entry, error) {
	man, _, err := d.load(manifestID)
	if err != nil {
		return Entry{}, err
	}
	return entryFor(man), nil
}

// Detail returns the full detail-page view of one app, store-facing: an unlisted
// app (`listed: false`) is reported as ErrNotFound so its detail page is
// unreachable through the store, the same as a missing manifest. ErrNotFound is
// mapped to 404 by the API; other Load errors are integrity failures a curated
// catalog shouldn't ship and map to 500. (Installed-instance enrichment must not
// go through here — use Entry, which doesn't hide unlisted apps.)
func (d *diskSource) Detail(manifestID string) (Detail, error) {
	man, _, err := d.load(manifestID)
	if err != nil {
		return Detail{}, err
	}
	if !man.IsListed() {
		return Detail{}, fmt.Errorf("%w: %q is unlisted", ErrNotFound, manifestID)
	}
	det := Detail{
		Entry:           entryFor(man),
		LongDescription: man.Description.Long,
		License:         man.License,
		ChangelogURL:    man.ChangelogURL,
		ExternalCosts:   externalCostsOf(man.ExternalCosts),
	}
	if man.Author != (manifest.Author{}) {
		det.Author = &man.Author
	}
	if man.Links != (manifest.Links{}) {
		det.Links = &man.Links
	}
	for i := range man.Screenshots {
		det.ScreenshotURLs = append(det.ScreenshotURLs, screenshotURL(man.ID, i))
	}
	return det, nil
}

// IconPath resolves the on-disk path of an app's icon for serving. ErrNotFound
// when the manifest declares no icon (or the app is unknown).
func (d *diskSource) IconPath(manifestID string) (string, error) {
	man, _, err := d.load(manifestID)
	if err != nil {
		return "", err
	}
	if man.Icon == "" {
		return "", fmt.Errorf("%w: %q has no icon", ErrNotFound, manifestID)
	}
	return d.assetPath(manifestID, man.Icon)
}

// ScreenshotPath resolves the on-disk path of the i-th screenshot (manifest
// order, 0-based). ErrNotFound when the index is out of range or the app is
// unknown.
func (d *diskSource) ScreenshotPath(manifestID string, i int) (string, error) {
	man, _, err := d.load(manifestID)
	if err != nil {
		return "", err
	}
	if i < 0 || i >= len(man.Screenshots) {
		return "", fmt.Errorf("%w: %q screenshot %d", ErrNotFound, manifestID, i)
	}
	return d.assetPath(manifestID, man.Screenshots[i])
}

// assetPath resolves a manifest-declared package-relative path (e.g.
// "./icon.png") to an absolute path inside the app's catalog directory,
// rejecting anything that would escape it (path traversal). The asset must
// exist on disk.
func (d *diskSource) assetPath(manifestID, rel string) (string, error) {
	dir := filepath.Join(d.root, manifestID)
	// Treat rel as rooted at the app dir: Clean("/"+rel) collapses any ".."
	// against that root, then Join re-bases it under dir. The prefix check is a
	// belt-and-braces guard against symlink-free escapes.
	full := filepath.Join(dir, filepath.Clean("/"+rel))
	if full != dir && !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q asset escapes catalog dir", ErrNotFound, manifestID)
	}
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %q asset %q", ErrNotFound, manifestID, rel)
		}
		return "", fmt.Errorf("catalog: stat asset %q for %q: %w", rel, manifestID, err)
	}
	// Regular files only: os.Stat succeeds on a directory too, and http.ServeFile
	// would then serve a listing — leaking the catalog dir structure.
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q asset %q is not a regular file", ErrNotFound, manifestID, rel)
	}
	return full, nil
}

// Load returns the parsed manifest and the verbatim compose bytes. The context
// is unused — a disk read needs none — but the method takes one because the
// remote source's Load fetches over the network (source.Load, #434).
func (d *diskSource) Load(_ context.Context, manifestID string) (*manifest.Manifest, []byte, error) {
	return d.load(manifestID)
}

// load is the disk read every other method of this source is built on.
func (d *diskSource) load(manifestID string) (*manifest.Manifest, []byte, error) {
	dir := filepath.Join(d.root, manifestID)
	manBytes, err := os.ReadFile(filepath.Join(dir, "manifest.yml"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %q", ErrNotFound, manifestID)
		}
		return nil, nil, fmt.Errorf("catalog: read manifest for %q: %w", manifestID, err)
	}
	man, err := manifest.Parse(manBytes)
	if err != nil {
		return nil, nil, err
	}
	composeBytes, err := os.ReadFile(filepath.Join(dir, man.ComposeFile))
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: compose %q for %q: %w", man.ComposeFile, manifestID, err)
	}
	return man, composeBytes, nil
}
