package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/onmoose/moose/internal/manifest"
)

// imageSizer resolves the catalog ImageRef (pinned digest + download/disk
// sizes) for one `image:tag`. It is an interface so resolve's parse/write-back
// logic is unit-testable with a fake; the production impl (dockerSizer) drives
// the local Docker daemon.
type imageSizer interface {
	Size(ctx context.Context, image string) (manifest.ImageRef, error)
}

// resolve fills the manifest's `images` block with registry-resolved sizes
// (APP_STORE.md # Catalog schema): for each distinct image in the sibling
// compose it resolves {digest, download_bytes, disk_bytes}, rewrites the
// manifest's images block in place (preserving every other line and comment),
// and prints the per-image sizes plus the derived per-app footprint. A single
// unsizable image aborts the whole run — nothing is written — so the catalog
// never carries a bogus zero (issue #69 "fails loudly").
func resolve(ctx context.Context, sizer imageSizer, manifestPath string) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	man, err := manifest.Parse(data)
	if err != nil {
		return err // Parse errors already name the field/slug at fault
	}

	composePath := filepath.Join(filepath.Dir(manifestPath), man.ComposeFile)
	composeData, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("compose_file %q: %w", man.ComposeFile, err)
	}
	images, err := manifest.ComposeImages(composeData)
	if err != nil {
		return fmt.Errorf("compose_file %q: %w", man.ComposeFile, err)
	}

	resolved := make(map[string]manifest.ImageRef, len(images))
	for _, img := range images {
		ref, err := sizer.Size(ctx, img)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", img, err)
		}
		if ref.Digest == "" {
			return fmt.Errorf("resolve %s: registry returned no digest", img)
		}
		resolved[img] = ref
		fmt.Printf("  %s\n    digest %s\n    download %s  disk %s\n",
			img, ref.Digest, humanBytes(ref.DownloadBytes), humanBytes(ref.DiskBytes))
		if lowExpansion(ref) {
			fmt.Printf("    warning: disk < 1.2× download — unless this image is mostly pre-compressed content, the sizer may be recording compressed sizes (#117)\n")
		}
	}

	newContent := replaceImagesBlock(string(data), resolved)
	// Never write a manifest that wouldn't parse back, and confirm the sizes
	// survived the round-trip before touching the file.
	reparsed, err := manifest.Parse([]byte(newContent))
	if err != nil {
		return fmt.Errorf("internal: regenerated manifest does not parse: %w", err)
	}
	if err := os.WriteFile(manifestPath, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	f := reparsed.Footprint()
	fmt.Printf("footprint: download %s  disk %s", humanBytes(f.ImageDownloadBytes), humanBytes(f.ImageDiskBytes))
	if f.EstimatedState != "" {
		fmt.Printf("  state %s", f.EstimatedState)
	}
	fmt.Println()
	return nil
}

// replaceImagesBlock returns src with its top-level `images:` block rewritten to
// the resolved object form, leaving every other byte — comments, blank lines,
// the explanatory header above `images:` — untouched. Surgical text editing
// rather than a YAML re-encode, because re-encoding collapses the blank lines
// and comment spacing the curated manifests rely on. The block spans the
// `images:` line through every following indented or blank line; an absent
// block is appended at EOF.
func replaceImagesBlock(src string, resolved map[string]manifest.ImageRef) string {
	block := renderImagesBlock(resolved)

	lines := strings.Split(src, "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimRight(ln, " \t") == "images:" {
			start = i
			break
		}
	}
	if start < 0 {
		// No existing block — append after a single trailing blank line.
		trimmed := strings.TrimRight(src, "\n")
		return trimmed + "\n\n" + block + "\n"
	}
	end := start + 1
	for end < len(lines) {
		ln := lines[end]
		if strings.TrimSpace(ln) == "" || strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t") {
			end++
			continue
		}
		break // first non-blank, non-indented line begins the next top-level key
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, strings.Split(block, "\n")...)
	out = append(out, lines[end:]...)
	result := strings.Join(out, "\n")
	// When `images:` was the final block, its trailing newline lived in the last
	// split element we just consumed — restore the single trailing newline.
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

// renderImagesBlock renders the object-form `images:` block, 2-space indented to
// match the curated manifests, images sorted for a stable diff.
func renderImagesBlock(resolved map[string]manifest.ImageRef) string {
	imgs := make([]string, 0, len(resolved))
	for img := range resolved {
		imgs = append(imgs, img)
	}
	sort.Strings(imgs)

	var b strings.Builder
	b.WriteString("images:")
	for _, img := range imgs {
		ref := resolved[img]
		fmt.Fprintf(&b, "\n  %s:\n", img)
		fmt.Fprintf(&b, "    digest: %s\n", ref.Digest)
		fmt.Fprintf(&b, "    download_bytes: %d\n", ref.DownloadBytes)
		fmt.Fprintf(&b, "    disk_bytes: %d", ref.DiskBytes)
	}
	return b.String()
}

// dockerSizer resolves sizes by driving the local Docker daemon: the registry
// manifest for the compressed download size, a `--platform linux/amd64` pull
// then a `docker save` decompress-count for the uncompressed on-disk size and
// the pinned (index) digest. moose is x86-only (CLAUDE.md # Load-bearing
// decisions), so amd64 is the size that matters.
type dockerSizer struct{}

func (dockerSizer) Size(ctx context.Context, image string) (manifest.ImageRef, error) {
	// Pull first so the layer blobs and the index RepoDigest are readable locally.
	if _, err := runDocker(ctx, "pull", "--platform", "linux/amd64", image); err != nil {
		return manifest.ImageRef{}, err
	}
	diskBytes, err := unpackedDiskBytes(ctx, image)
	if err != nil {
		return manifest.ImageRef{}, err
	}
	digest, err := indexDigest(ctx, image)
	if err != nil {
		return manifest.ImageRef{}, err
	}
	download, err := downloadBytes(ctx, image)
	if err != nil {
		return manifest.ImageRef{}, err
	}
	return manifest.ImageRef{Digest: digest, DownloadBytes: download, DiskBytes: diskBytes}, nil
}

// indexDigest returns the `sha256:…` the brain pins — the RepoDigest docker
// recorded for this repo on pull (the manifest-list digest for a multi-arch
// tag). Mirrors internal/lifecycle's resolveImages.
func indexDigest(ctx context.Context, image string) (string, error) {
	out, err := runDocker(ctx, "image", "inspect", image, "--format", "{{json .RepoDigests}}")
	if err != nil {
		return "", err
	}
	var rds []string
	if err := json.Unmarshal([]byte(out), &rds); err != nil {
		return "", fmt.Errorf("parse RepoDigests for %s: %w", image, err)
	}
	repo := repoOf(image)
	for _, rd := range rds {
		if name, digest, ok := strings.Cut(rd, "@"); ok && name == repo {
			return digest, nil
		}
	}
	return "", fmt.Errorf("no RepoDigest for %s matched repo %s (got %v)", image, repo, rds)
}

// downloadBytes returns the compressed download size: the sum of the amd64
// image manifest's layer sizes, read from the registry without keeping the
// blobs. A multi-arch tag is a manifest list — pick its linux/amd64 entry and
// inspect that sub-manifest; a single-arch tag carries its layers inline.
func downloadBytes(ctx context.Context, image string) (int64, error) {
	raw, err := runDocker(ctx, "manifest", "inspect", image)
	if err != nil {
		return 0, err
	}
	var doc struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return 0, fmt.Errorf("parse manifest for %s: %w", image, err)
	}
	if len(doc.Manifests) == 0 { // single-arch image
		if len(doc.Layers) == 0 {
			return 0, fmt.Errorf("manifest for %s has no layers", image)
		}
		return sumLayers(doc.Layers), nil
	}
	var sub string
	for _, m := range doc.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			sub = m.Digest
			break
		}
	}
	if sub == "" {
		return 0, fmt.Errorf("%s has no linux/amd64 manifest", image)
	}
	subRaw, err := runDocker(ctx, "manifest", "inspect", repoOf(image)+"@"+sub)
	if err != nil {
		return 0, err
	}
	var subDoc struct {
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal([]byte(subRaw), &subDoc); err != nil {
		return 0, fmt.Errorf("parse amd64 manifest for %s: %w", image, err)
	}
	if len(subDoc.Layers) == 0 {
		return 0, fmt.Errorf("amd64 manifest for %s has no layers", image)
	}
	return sumLayers(subDoc.Layers), nil
}

func sumLayers(layers []struct {
	Size int64 `json:"size"`
}) int64 {
	var total int64
	for _, l := range layers {
		total += l.Size
	}
	return total
}

// runDocker runs `docker <args>` and returns stdout; on failure the error
// carries the command and docker's stderr so the author sees why a resolve
// aborted (image not found, no network, daemon down).
func runDocker(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// repoOf strips both `:tag` and `@sha256:…` from an image reference. The tag
// colon is distinguished from a registry-port colon by checking for a following
// `/`. Same rule as internal/lifecycle (cmd/ can't reach that unexported helper
// across the layer boundary; the duplication is a few lines).
func repoOf(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		return image[:at]
	}
	if colon := strings.LastIndex(image, ":"); colon > 0 && !strings.Contains(image[colon:], "/") {
		return image[:colon]
	}
	return image
}

// lowExpansion reports a disk size suspiciously close to the compressed
// download size (< 1.2×). Genuine images expand 2–4× when unpacked, so a ≈1×
// ratio is the signature of recording compressed sizes (issue #117) — or,
// rarely, an image whose content is itself pre-compressed. Advisory only;
// drives a warning print, never a failure.
func lowExpansion(ref manifest.ImageRef) bool {
	return ref.DiskBytes*10 < ref.DownloadBytes*12
}

// humanBytes renders a byte count in decimal units (MB = 1e6) to match the
// plain-English sizes the store shows ("~1.5 GB"). Author-facing display only.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
