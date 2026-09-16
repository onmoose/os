package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/onmoose/moose/internal/manifest"
)

// composeChecker is the admission seam. Production passes admission.Check
// (syntax via `docker compose config -q` + the structural rejection rules);
// tests pass admission.CheckStructure so they stay hermetic — shelling out to
// the Docker daemon would turn unit tests into flaky integration tests. Mirrors
// resolve's imageSizer seam.
type composeChecker func(ctx context.Context, composeBytes []byte) error

// check is the author's one-shot "would this install?" command: it runs the
// schema lint (lint) and then the compose admission policy (admission.Check) on
// the sibling compose, so a single green `manifest check` proves BOTH the
// schema and the structural compose rules — the two checks the admission policy
// (APP_LIFECYCLE.md) and the manifest schema (APP_MANIFEST.md) split between
// them. lint alone is non-strict and does NOT run admission; this closes that
// gap so authors (and the authoring agent) never hand-eyeball admission.go.
func check(ctx context.Context, admit composeChecker, manifestPath string) error {
	if err := lint(manifestPath); err != nil {
		return err // lint errors already name the field/slug/compose problem
	}

	// lint proved the manifest parses and the compose resolves + parses; re-read
	// the verbatim compose bytes and run them through admission.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	man, err := manifest.Parse(data)
	if err != nil {
		return err
	}
	composePath := filepath.Join(filepath.Dir(manifestPath), man.ComposeFile)
	composeData, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("compose_file %q: %w", man.ComposeFile, err)
	}
	if err := admit(ctx, composeData); err != nil {
		return err // admission.Error messages already name the offending service + field
	}
	return checkImagesResolved(man)
}

// imageDigest matches a well-formed `sha256:<64 lowercase hex>` reference —
// the shape `resolve` always writes.
var imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// checkImagesResolved catches a manifest whose `images:` entries were never
// run through `moose manifest resolve` — a hand-written placeholder digest
// (or one left over from a copy-paste) is a syntactically valid ImageRef, so
// nothing else in `check` (schema lint, admission) would ever flag it, and
// `check` deliberately never hits the registry itself (that's `resolve`'s
// job, run separately). A resolved image's compressed/uncompressed layer sum
// is never zero, so requiring both sizes to be positive whenever an entry is
// present is a cheap, fully offline tripwire for "resolve was skipped."
//
// This is deliberately stricter than `internal/manifest.Parse`, which still
// accepts the legacy digest-only scalar form (`image:tag: sha256:…`, no
// sizes) as a valid pre-resolve authoring draft. `check` is the author's
// last gate before opening a PR, and `docs/curation-workflow.md` always runs
// `resolve` before `check` — by the time `check` runs, every entry should
// carry real sizes, scalar shorthand included.
func checkImagesResolved(man *manifest.Manifest) error {
	for ref, img := range man.Images {
		if !imageDigest.MatchString(img.Digest) {
			return fmt.Errorf("images %q: digest %q is not a well-formed sha256 digest — run `moose manifest resolve`", ref, img.Digest)
		}
		if img.DownloadBytes <= 0 || img.DiskBytes <= 0 {
			return fmt.Errorf("images %q: download_bytes/disk_bytes are not resolved (zero or unset) — run `moose manifest resolve`", ref)
		}
	}
	return nil
}
