package lifecycle

import (
	"context"
	"fmt"
	"testing"

	"github.com/onmoose/moose/internal/manifest"
	"github.com/onmoose/moose/internal/protocol"
)

// twoImageManifest is the fixture for the footprint cases: two images with
// distinct sizes plus a parseable estimated_size.
func twoImageManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ID: "sizer",
		Images: map[string]manifest.ImageRef{
			"app/one:1": {Digest: "sha256:a", DownloadBytes: 100, DiskBytes: 400},
			"app/two:2": {Digest: "sha256:b", DownloadBytes: 30, DiskBytes: 70},
		},
		Storage: manifest.Storage{EstimatedSize: "10GB"},
	}
}

// present marks an image as already pulled in the fake Docker store by scripting
// its digest-pinned ref so ImageInspect succeeds (no error = present).
func present(d *fakeDocker, imageRef, digest string) {
	d.digests[repoOf(imageRef)+"@"+digest] = digest
}

// TestInstallFootprintSubtractsPresentImages is the core incremental case: one
// image already pulled, one not — only the not-present image's bytes count, and
// the host's free figure flows through.
func TestInstallFootprintSubtractsPresentImages(t *testing.T) {
	docker := newFakeDocker()
	present(docker, "app/one:1", "sha256:a") // app/one is cached; app/two is not
	host := newFakeHost()
	host.systemStatus = protocol.SystemStatus{DataDiskFreeBytes: 500 << 30}
	m := &Manager{docker: docker, host: host}

	fp := m.InstallFootprint(context.Background(), twoImageManifest())

	// Only app/two (30 / 70) remains to download.
	if fp.DownloadBytes != 30 || fp.ImageDiskBytes != 70 {
		t.Fatalf("want 30/70 for the not-present image, got %d/%d", fp.DownloadBytes, fp.ImageDiskBytes)
	}
	if !fp.HasEstimate || fp.EstimatedStateBytes != 10<<30 {
		t.Fatalf("estimate wrong: has=%v bytes=%d", fp.HasEstimate, fp.EstimatedStateBytes)
	}
	if fp.FreeBytes != 500<<30 {
		t.Fatalf("free bytes not carried from host: %d", fp.FreeBytes)
	}
}

// TestInstallFootprintAllImagesPresent: nothing to download when every image is
// already local.
func TestInstallFootprintAllImagesPresent(t *testing.T) {
	docker := newFakeDocker()
	present(docker, "app/one:1", "sha256:a")
	present(docker, "app/two:2", "sha256:b")
	host := newFakeHost()
	m := &Manager{docker: docker, host: host}

	fp := m.InstallFootprint(context.Background(), twoImageManifest())
	if fp.DownloadBytes != 0 || fp.ImageDiskBytes != 0 {
		t.Fatalf("want zero image bytes when all present, got %d/%d", fp.DownloadBytes, fp.ImageDiskBytes)
	}
}

// TestInstallFootprintNoImagesPresent: none cached ⇒ the full coarse sum, same
// as the catalog upper bound.
func TestInstallFootprintNoImagesPresent(t *testing.T) {
	docker := newFakeDocker() // no digests scripted ⇒ ImageInspect errors ⇒ absent
	host := newFakeHost()
	m := &Manager{docker: docker, host: host}

	fp := m.InstallFootprint(context.Background(), twoImageManifest())
	if fp.DownloadBytes != 130 || fp.ImageDiskBytes != 470 {
		t.Fatalf("want full 130/470, got %d/%d", fp.DownloadBytes, fp.ImageDiskBytes)
	}
}

// TestInstallFootprintBlankDigestCountsAbsent: an unresolved catalog entry (no
// digest) can't be proven present, so it counts as a download — Docker is never
// even consulted for it.
func TestInstallFootprintBlankDigestCountsAbsent(t *testing.T) {
	docker := newFakeDocker()
	host := newFakeHost()
	m := &Manager{docker: docker, host: host}
	man := &manifest.Manifest{
		ID: "sizer",
		Images: map[string]manifest.ImageRef{
			"app/one:1": {Digest: "", DownloadBytes: 100, DiskBytes: 400},
		},
	}

	fp := m.InstallFootprint(context.Background(), man)
	if fp.DownloadBytes != 100 || fp.ImageDiskBytes != 400 {
		t.Fatalf("blank-digest image must count as download, got %d/%d", fp.DownloadBytes, fp.ImageDiskBytes)
	}
	if docker.called("ImageInspect") {
		t.Fatalf("ImageInspect must be skipped for a blank-digest image")
	}
}

// TestInstallFootprintUnsetEstimate: no storage block ⇒ HasEstimate false so the
// install plan omits estimated_state_bytes rather than reporting zero.
func TestInstallFootprintUnsetEstimate(t *testing.T) {
	docker := newFakeDocker()
	host := newFakeHost()
	m := &Manager{docker: docker, host: host}
	man := &manifest.Manifest{ID: "bare"} // no images, no storage

	fp := m.InstallFootprint(context.Background(), man)
	if fp.HasEstimate || fp.EstimatedStateBytes != 0 {
		t.Fatalf("want no estimate, got has=%v bytes=%d", fp.HasEstimate, fp.EstimatedStateBytes)
	}
}

// TestInstallFootprintMalformedEstimateDegrades: a bad estimated_size is treated
// as absent (logged, not surfaced) — the plan still renders.
func TestInstallFootprintMalformedEstimateDegrades(t *testing.T) {
	docker := newFakeDocker()
	host := newFakeHost()
	m := &Manager{docker: docker, host: host}
	man := &manifest.Manifest{ID: "bad", Storage: manifest.Storage{EstimatedSize: "huge"}}

	fp := m.InstallFootprint(context.Background(), man)
	if fp.HasEstimate || fp.EstimatedStateBytes != 0 {
		t.Fatalf("malformed estimate must degrade to absent, got has=%v bytes=%d", fp.HasEstimate, fp.EstimatedStateBytes)
	}
}

// TestInstallFootprintHostErrorDegrades: when the host can't report free space,
// FreeBytes is 0 and the call still returns the image/estimate figures.
func TestInstallFootprintHostErrorDegrades(t *testing.T) {
	docker := newFakeDocker()
	host := newFakeHost()
	host.statusErr = fmt.Errorf("host-agent unreachable")
	m := &Manager{docker: docker, host: host}

	fp := m.InstallFootprint(context.Background(), twoImageManifest())
	if fp.FreeBytes != 0 {
		t.Fatalf("want FreeBytes 0 on host error, got %d", fp.FreeBytes)
	}
	if fp.DownloadBytes != 130 {
		t.Fatalf("image figures must survive host error, got %d", fp.DownloadBytes)
	}
}
