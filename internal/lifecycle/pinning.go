package lifecycle

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
	"gopkg.in/yaml.v3"
)

// servicePin is the resolved digest pin for one compose service
// (APP_LIFECYCLE.md # image digest pinning).
type servicePin struct {
	Service string
	Image   string // original `image:` ref from the author's compose
	Digest  string // `sha256:…`
	// ref is the image reference written into the compose override. Normally the
	// `name@sha256:…` digest form (byte-deterministic). In offline mode, when the
	// image was resolved from a docker-loaded local image, it is the original tag
	// instead: a `docker save`/`load` image carries no RepoDigest, so a digest ref
	// isn't locally resolvable and `docker compose up` would try to pull it (and
	// fail, air-gapped). The tag IS present locally; Digest still records the
	// trusted bytes in SQLite. See resolveImages.
	ref string
}

// PinnedRef returns the image reference to write into the compose override.
func (p servicePin) PinnedRef() string {
	return p.ref
}

// resolveImages resolves each service's image to the bytes it will run and
// returns the per-service pin in stable order.
//
// For a Door-1 (catalog) install the manifest already carries the promised
// digest, so that digest IS the address: the image is pulled as
// `name@sha256:…` and the tag is never consulted (APP_STORE.md # Trust model —
// "the box pulls by digest, so the upstream's new bytes don't affect it").
// Upstream rebuilding a tag is therefore a non-event, and two boxes installing
// the same catalog version always run identical bytes. The tag survives only as
// the human-readable label in the manifest.
//
// Door-2 callers pass a manifest with an empty Images map: there is no promise,
// so the tag is pulled and whatever it resolves to is trusted on first use
// (TOFU).
//
// Failures here happen before any compose up — they roll back the partial
// install cleanly.
//
// When offline is set (a baked, air-gapped box — there is no registry to pull
// from; CONTROL_PLANE.md # First-boot brain bootstrap, APP_LIFECYCLE.md # image
// digest pinning), a pull failure is not fatal: if the image is already present
// locally (the offline bundle docker-loaded it) and the catalog promised a
// digest, that promise is trusted as the pin — the bundle is the trust anchor
// in place of the absent registry.
func resolveImages(ctx context.Context, docker DockerDriver, man *manifest.Manifest, composeBytes []byte, offline bool) ([]servicePin, error) {
	svcImages, err := serviceImages(composeBytes)
	if err != nil {
		return nil, err
	}

	// Pull each unique image once.
	seen := map[string]string{} // image → digest
	local := map[string]bool{}  // image → resolved from a local-only (offline) image
	for _, img := range svcImages {
		if _, done := seen[img]; done {
			continue
		}
		var promised string
		if p, ok := man.Images[img]; ok {
			promised = p.Digest
		}
		digest, fromLocal, err := pullAndResolve(ctx, docker, img, promised, offline)
		if err != nil {
			return nil, err
		}
		seen[img] = digest
		local[img] = fromLocal
	}

	pins := make([]servicePin, 0, len(svcImages))
	for svc, img := range svcImages {
		// Digest ref normally; the original tag when the image was resolved from a
		// docker-loaded local image (offline) — a loaded image has no RepoDigest,
		// so a digest ref isn't locally resolvable (see servicePin.ref).
		ref := repoOf(img) + "@" + seen[img]
		if local[img] {
			ref = img
		}
		pins = append(pins, servicePin{Service: svc, Image: img, Digest: seen[img], ref: ref})
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].Service < pins[j].Service })
	return pins, nil
}

// pullAndResolve pulls the image and returns the content digest (`sha256:…`)
// plus whether it was resolved from a local-only image (the offline fallback —
// the caller then references it by tag, not digest, in the override).
//
// promised is the catalog-promised digest for this image ("" for a Door-2 /
// TOFU install). When we hold a digest — the catalog's promise, or an author who
// pinned `name@sha256:…` in the compose directly — it is the address we pull:
// the registry cannot serve anything else for it, so those exact bytes arrive or
// the pull fails. Only a Door-2 install consults the tag, and then whatever it
// resolves to now is what gets pinned (TOFU).
//
// In offline mode a pull failure falls back to the locally-present image — see
// resolveOffline.
func pullAndResolve(ctx context.Context, docker DockerDriver, image, promised string, offline bool) (string, bool, error) {
	if inRef, ok := digestOf(image); ok {
		// A compose pinned by digest AND a catalog promise for it: if they disagree
		// the catalog contradicts itself. Unlike an upstream tag rebuild (routine,
		// and no longer our problem), this is a curation bug with no safe pick.
		if promised != "" && promised != inRef {
			return "", false, fmt.Errorf("catalog contradicts itself for %s: compose pins %s, catalog promises %s",
				image, inRef, promised)
		}
		promised = inRef
	}
	if promised != "" {
		ref := repoOf(image) + "@" + promised
		if err := docker.Pull(ctx, ref); err != nil {
			if offline {
				return resolveOffline(ctx, docker, image, promised, err)
			}
			return "", false, err
		}
		return promised, false, nil
	}
	if err := docker.Pull(ctx, image); err != nil {
		if offline {
			return resolveOffline(ctx, docker, image, "", err)
		}
		return "", false, err
	}
	repoDigests, err := docker.ImageInspect(ctx, image)
	if err != nil {
		return "", false, err
	}
	repo := repoOf(image)
	for _, rd := range repoDigests {
		name, digest, ok := strings.Cut(rd, "@")
		if !ok {
			continue
		}
		if name == repo {
			return digest, false, nil
		}
	}
	return "", false, fmt.Errorf("no RepoDigest for %s matched repo %s (got %v) — image may be local-only",
		image, repo, repoDigests)
}

// resolveOffline is the air-gapped fallback when a pull fails: there is no
// registry, so the digest cannot be resolved from one. If the image is present
// locally (the offline bundle docker-loaded it — a `docker save`/`load` image
// carries no RepoDigest, so the normal online path can't pin it) and we hold a
// trusted digest (the catalog promise, or an explicit `@sha256:` ref), that
// digest is the pin. The bundle stands in for the registry as the trust anchor.
//
// Two failures stay fatal, distinguished from a transient pull error: no
// trusted digest to fall back on (a Door-2 install can't be pinned offline), or
// the image is genuinely absent (the bundle is incomplete — this is the
// hard-fail the air-gapped lane exists to catch). ImageInspect succeeds with an
// empty RepoDigest list for a loaded image and errors only when it is absent,
// so it is the presence probe.
func resolveOffline(ctx context.Context, docker DockerDriver, image, trusted string, pullErr error) (string, bool, error) {
	if trusted == "" {
		return "", false, fmt.Errorf("offline install: image %s is not pullable and has no catalog-promised digest to trust: %w", image, pullErr)
	}
	if _, err := docker.ImageInspect(ctx, image); err != nil {
		// Surface BOTH the pull failure and the inspect failure: inspect erroring
		// usually means the image is genuinely absent (incomplete bundle), but it
		// could also be the daemon being down or a corrupt image store — wrapping
		// only pullErr ("registry unreachable") would mask that real cause.
		return "", false, fmt.Errorf("offline install: image %s is not present locally and not pullable (offline bundle incomplete?): pull: %v; inspect: %w", image, pullErr, err)
	}
	return trusted, true, nil
}

// serviceImages returns service → `image:` reference for every service in the
// compose. A service without an `image:` is an error here: admission already
// rejects `build:`, so by this point every service must declare an image.
func serviceImages(composeBytes []byte) (map[string]string, error) {
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse compose for images: %w", err)
	}
	out := make(map[string]string, len(doc.Services))
	for name, svc := range doc.Services {
		if strings.TrimSpace(svc.Image) == "" {
			return nil, fmt.Errorf("service %q has no image", name)
		}
		out[name] = svc.Image
	}
	return out, nil
}

// repoOf returns the registry repo portion of an image reference, stripping
// both `:tag` and `@sha256:…` suffixes. A ref may carry both (`name:tag@sha256:…`),
// so the digest goes first and the tag is stripped from what remains — the pin
// written into the override must be the canonical `name@sha256:…`
// (APP_LIFECYCLE.md # image digest pinning), carrying no tag. The tag colon is
// distinguished from a port colon by checking whether a `/` follows it.
func repoOf(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if colon := strings.LastIndex(image, ":"); colon > 0 && !strings.Contains(image[colon:], "/") {
		return image[:colon]
	}
	return image
}

// digestOf returns the `sha256:…` portion if the image is already pinned by
// digest (`name@sha256:…`), otherwise ("", false).
func digestOf(image string) (string, bool) {
	if at := strings.Index(image, "@"); at >= 0 {
		return image[at+1:], true
	}
	return "", false
}

// toInstanceImages converts pins into the row form persisted in SQLite.
func toInstanceImages(pins []servicePin) []store.InstanceImage {
	out := make([]store.InstanceImage, len(pins))
	for i, p := range pins {
		out[i] = store.InstanceImage{Service: p.Service, Image: p.Image, Digest: p.Digest}
	}
	return out
}
