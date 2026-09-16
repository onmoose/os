package lifecycle

import (
	"context"
	"log/slog"

	"github.com/onmoose/moose/internal/manifest"
)

// InstallFootprint is the box-specific install estimate behind the install-plan
// footprint block (BRAIN_UI_PROTOCOL.md # install-plan). Where the catalog
// Entry's Footprint carries static image totals (an upper bound — nothing assumed
// cached) plus the manifest's measured app-state baseline, this sharpens the image
// side for THIS box: it subtracts images already pulled locally and adds the live
// free-space reading, so the install dialog shows what the install will actually
// cost here. The app-state figure (estimated_size) is the same measured baseline
// either way (APP_MANIFEST.md # Storage, DECISIONS.md 2026-06-09).
type InstallFootprint struct {
	// DownloadBytes is the compressed bytes still to pull — the sum of
	// download_bytes over images NOT already present locally. 0 when every image
	// is cached.
	DownloadBytes int64
	// ImageDiskBytes is the uncompressed on-disk bytes the not-yet-present images
	// add once pulled — the same not-present subset as DownloadBytes.
	ImageDiskBytes int64
	// EstimatedStateBytes is the parsed storage.estimated_size in bytes; only
	// meaningful when HasEstimate is true (the author may omit the hint).
	EstimatedStateBytes int64
	HasEstimate         bool
	// FreeBytes is the data drive's available space (host statfs snapshot), or 0
	// when the host couldn't measure it (no reporter wired, or the host call
	// failed). The UI shows no free figure on 0 rather than a misleading zero.
	FreeBytes int64
}

// InstallFootprint computes the box-specific install estimate for a manifest
// (BRAIN_UI_PROTOCOL.md # install-plan). It is read-only and never fails: each
// sub-step degrades to a zero/partial figure rather than blocking the plan, so
// the install dialog always has something to show. Specifically:
//
//   - Image bytes are summed over images not already present locally. Presence
//     is an image-level check — the catalog carries per-image sizes, not
//     per-layer, so shared layers between a cached image and a new one can't be
//     credited; the estimate is a safe upper bound, never an under-count.
//   - A malformed estimated_size is logged and treated as absent (HasEstimate
//     false), never surfaced as an install-blocking error.
//   - A host that can't report free space (no reporter, or an unreachable
//     socket) leaves FreeBytes 0; the UI omits the free figure rather than fail.
func (m *Manager) InstallFootprint(ctx context.Context, man *manifest.Manifest) InstallFootprint {
	var fp InstallFootprint
	for ref, img := range man.Images {
		if m.imagePresent(ctx, ref, img.Digest) {
			continue
		}
		fp.DownloadBytes += img.DownloadBytes
		fp.ImageDiskBytes += img.DiskBytes
	}
	if n, ok, err := man.Storage.EstimatedSizeBytes(); err != nil {
		slog.Warn("install footprint: unparseable estimated_size", "manifest_id", man.ID, "err", err)
	} else if ok {
		fp.EstimatedStateBytes, fp.HasEstimate = n, true
	}
	if status, err := m.host.SystemStatus(ctx); err != nil {
		slog.Warn("install footprint: host status unavailable", "manifest_id", man.ID, "err", err)
	} else {
		fp.FreeBytes = status.DataDiskFreeBytes
	}
	return fp
}

// imagePresent reports whether the digest-pinned image is already in the local
// Docker store — a successful `docker image inspect` on `repo@digest` means the
// bytes are here and won't be re-downloaded. A blank digest (an unresolved
// catalog entry) counts as absent: without the digest we can't prove presence,
// so we assume a download is needed — the safe over-estimate.
func (m *Manager) imagePresent(ctx context.Context, imageRef, digest string) bool {
	if digest == "" || m.docker == nil {
		return false
	}
	pinned := repoOf(imageRef) + "@" + digest
	_, err := m.docker.ImageInspect(ctx, pinned)
	return err == nil
}
