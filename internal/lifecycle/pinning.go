package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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
	type resolved struct {
		digest string
		ref    string // the reference the bytes were pulled under
		local  bool   // resolved from a local-only (offline) image
	}
	seen := map[string]resolved{}
	for _, img := range svcImages {
		if _, done := seen[img]; done {
			continue
		}
		var promised string
		var sources []manifest.ImageSource
		if p, ok := man.Images[img]; ok {
			promised = p.Digest
			sources = p.Sources
		}
		digest, ref, fromLocal, err := pullAndResolve(ctx, docker, img, promised, sources, offline)
		if err != nil {
			return nil, err
		}
		seen[img] = resolved{digest: digest, ref: ref, local: fromLocal}
	}

	pins := make([]servicePin, 0, len(svcImages))
	for svc, img := range svcImages {
		r := seen[img]
		// The reference that was pulled: `name@sha256:…` from upstream, or
		// `<source>@sha256:…` from a backup source (#588). Docker refuses to tag
		// a digest reference under another name, so a source pull exists only
		// under the source's name. The original tag when the image was resolved
		// from a docker-loaded local image (offline): a loaded image has no
		// RepoDigest, so a digest ref isn't locally resolvable (see servicePin.ref).
		ref := r.ref
		if r.local {
			ref = img
		}
		pins = append(pins, servicePin{Service: svc, Image: img, Digest: r.digest, ref: ref})
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].Service < pins[j].Service })
	return pins, nil
}

// pullAndResolve pulls the image and returns the content digest (`sha256:…`),
// the reference the bytes were pulled under, and whether it was resolved from a
// local-only image (the offline fallback: the caller then references it by tag,
// not digest, in the override).
//
// promised is the catalog-promised digest for this image ("" for a Door-2 /
// TOFU install). When we hold a digest (the catalog's promise, or an author who
// pinned `name@sha256:…` in the compose directly) it is the address we pull:
// the registry cannot serve anything else for it, so those exact bytes arrive or
// the pull fails. Only a Door-2 install consults the tag, and then whatever it
// resolves to now is what gets pinned (TOFU).
//
// sources are the catalog's backup places for this image (#588). They are used
// only with a digest, and never in offline mode, see pullImage.
//
// In offline mode a pull failure falls back to the locally-present image, see
// resolveOffline.
func pullAndResolve(ctx context.Context, docker DockerDriver, image, promised string, sources []manifest.ImageSource, offline bool) (string, string, bool, error) {
	if inRef, ok := digestOf(image); ok {
		// A compose pinned by digest AND a catalog promise for it: if they disagree
		// the catalog contradicts itself. Unlike an upstream tag rebuild (routine,
		// and no longer our problem), this is a curation bug with no safe pick.
		if promised != "" && promised != inRef {
			return "", "", false, fmt.Errorf("catalog contradicts itself for %s: compose pins %s, catalog promises %s",
				image, inRef, promised)
		}
		promised = inRef
	}
	if promised != "" {
		var backups []string
		if !offline {
			backups = sourceRefs(image, sources, promised)
		}
		ref, err := pullImage(ctx, docker, repoOf(image)+"@"+promised, backups)
		if err != nil {
			if offline {
				digest, local, oerr := resolveOffline(ctx, docker, image, promised, err)
				return digest, image, local, oerr
			}
			return "", "", false, err
		}
		return promised, ref, false, nil
	}
	if err := pullWithRetry(ctx, docker, image); err != nil {
		if offline {
			digest, local, oerr := resolveOffline(ctx, docker, image, "", err)
			return digest, image, local, oerr
		}
		return "", "", false, err
	}
	repoDigests, err := docker.ImageInspect(ctx, image)
	if err != nil {
		return "", "", false, err
	}
	repo := repoOf(image)
	for _, rd := range repoDigests {
		name, digest, ok := strings.Cut(rd, "@")
		if !ok {
			continue
		}
		if name == repo {
			return digest, repo + "@" + digest, false, nil
		}
	}
	return "", "", false, fmt.Errorf("no RepoDigest for %s matched repo %s (got %v): image may be local-only",
		image, repo, repoDigests)
}

// pullRetryDelays is how long a rate-limited upstream pull waits before each
// retry: four retries over about 30 seconds. A var so tests can shorten it.
var pullRetryDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// pullWithRetry pulls ref with the rate-limit backoff of pullImage and no
// backup source. Door-2 (TOFU) pulls by tag, and a source is only ever used
// with a digest, so this is its whole pull step.
func pullWithRetry(ctx context.Context, docker DockerDriver, ref string) error {
	_, err := pullImage(ctx, docker, ref, nil)
	return err
}

// pullImage pulls upstream and, when upstream fails, the same digest from each
// backup source in order (#588). It returns the reference that was pulled.
//
// The order:
//
//   - Upstream first.
//   - On a registry rate limit (#586), back off and retry upstream on the
//     pullRetryDelays ladder. A box pulls without logging in, so ghcr and Docker
//     Hub count its requests against a source IP that other traffic may share,
//     and a short spike answers 429 to one pull of a working install. If
//     upstream still answers 429 after the first wait, try the sources. If every
//     source fails too, finish the ladder on upstream.
//   - Any other error from the registry or the network (unreachable, timeout,
//     5xx, `manifest unknown`, 401, 403) tries the sources at once.
//   - A local error (see isLocalPullError) and a cancelled install never move
//     to a source: a source cannot fix them. The caller passes no sources in
//     offline mode.
//
// When every source fails, the error is upstream's: that is the one a person
// can act on. Each source failure is logged. If the context ends during a wait,
// the error wraps ctx.Err() so a cancelled install does not read as a rate limit.
func pullImage(ctx context.Context, docker DockerDriver, upstream string, sources []string) (string, error) {
	return pullImageWaits(ctx, docker, upstream, sources, pullRetryDelays)
}

// pullImageWaits is pullImage with its own backoff ladder. With no waits, a
// rate limit tries the sources at once and then fails: the reconcile pass at
// boot uses that, because it shares one short budget across every app.
func pullImageWaits(ctx context.Context, docker DockerDriver, upstream string, sources []string, waits []time.Duration) (string, error) {
	err := docker.Pull(ctx, upstream)
	if err == nil {
		return upstream, nil
	}
	triedSources := len(sources) == 0
	step := 0 // the next wait on the ladder
	for {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: %w", ctx.Err(), err)
		}
		limited := isRateLimited(err, upstream)
		if !limited && isLocalPullError(err) {
			return "", err
		}
		// On a rate limit, sources come after the first wait; on any other
		// registry or network error, at once.
		if !triedSources && (!limited || step >= 1 || len(waits) == 0) {
			triedSources = true
			if ref, ok := pullFromSources(ctx, docker, upstream, sources, err); ok {
				return ref, nil
			}
			continue // re-check the context, then finish the ladder
		}
		if !limited || step >= len(waits) {
			return "", err
		}
		delay := waits[step]
		step++
		slog.Warn("image pull rate-limited, retrying", "image", upstream, "delay", delay, "err", err)
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %w", ctx.Err(), err)
		case <-time.After(delay):
		}
		if err = docker.Pull(ctx, upstream); err == nil {
			return upstream, nil
		}
	}
}

// pullFromSources tries each source once, in order, and reports the first
// that pulled. upErr is upstream's error, logged so the move can be traced.
func pullFromSources(ctx context.Context, docker DockerDriver, upstream string, sources []string, upErr error) (string, bool) {
	slog.Warn("image pull failed upstream, trying backup sources", "image", upstream, "count", len(sources), "err", upErr)
	for _, ref := range sources {
		if ctx.Err() != nil {
			return "", false
		}
		if err := docker.Pull(ctx, ref); err != nil {
			slog.Warn("image pull from backup source failed", "image", upstream, "source", ref, "err", err)
			continue
		}
		slog.Info("image pulled from backup source", "image", upstream, "source", ref)
		return ref, true
	}
	return "", false
}

// localPullError matches `docker pull` failures that happen on this box, not at
// the registry: the daemon (or the socket proxy in front of it) is not
// reachable, the disk is full, or the image store cannot be written. Pulling
// the same bytes from another registry cannot fix these, so they never move to
// a backup source. Every other failure is treated as the registry's or the
// network's, so an unknown error tries the sources: the cost of a wrong guess is
// one failed pull, and the cost of the other guess is a failed install.
var localPullError = regexp.MustCompile(`(?i)no space left on device|read-only file system|cannot connect to the docker daemon|error during connect|executable file not found`)

// isLocalPullError reports whether a pull error happened on this box.
func isLocalPullError(err error) bool {
	return localPullError.MatchString(err.Error())
}

// sourceRefs turns an image's backup sources into the digest references to
// pull, in order. A source this box cannot use is skipped and logged: one that
// sets auth (no login is known yet, #588), and one whose ref is not a plain
// repository (it carries a tag or a digest, or is empty).
func sourceRefs(image string, sources []manifest.ImageSource, digest string) []string {
	var out []string
	for _, s := range sources {
		switch {
		case s.Auth != nil:
			slog.Warn("backup source needs a login this box does not know, skipping", "image", image, "source", s.Ref)
		case s.Ref == "" || strings.ContainsAny(s.Ref, "@ \t") || repoOf(s.Ref) != s.Ref:
			slog.Warn("backup source is not a plain repository, skipping", "image", image, "source", s.Ref)
		default:
			out = append(out, s.Ref+"@"+digest)
		}
	}
	return out
}

// rateLimitError matches a registry rate limit in `docker pull` output: the OCI
// error code `toomanyrequests` (ghcr and Docker Hub both send it, as
// "toomanyrequests: <detail>") or the bare HTTP status some registries print.
// An image reference has no spaces and no capitals, so neither form can match
// inside one.
var rateLimitError = regexp.MustCompile(`(^|\s)toomanyrequests: |429 Too Many Requests`)

// isRateLimited reports whether a pull error is a registry rate limit. The CLI
// driver starts its error with "pull <ref>: ", which is cut first so that an
// untagged image called `toomanyrequests` is not read as a rate limit.
func isRateLimited(err error, ref string) bool {
	return rateLimitError.MatchString(strings.Replace(err.Error(), "pull "+ref+": ", "", 1))
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
// Ref is kept only when the image came from a backup source (#588), so every
// other row reads as before: `name@sha256:…`, see storedRef.
func toInstanceImages(pins []servicePin) []store.InstanceImage {
	out := make([]store.InstanceImage, len(pins))
	for i, p := range pins {
		out[i] = store.InstanceImage{Service: p.Service, Image: p.Image, Digest: p.Digest, Ref: pinRef(p.Image, p.Digest, p.ref)}
	}
	return out
}

// pinRef is the Ref to store for an image pulled under ref: ref itself when it
// is a digest reference other than `name@sha256:…`, else "".
func pinRef(image, digest, ref string) string {
	if strings.Contains(ref, "@") && ref != repoOf(image)+"@"+digest {
		return ref
	}
	return ""
}

// storedRef is the local reference of a stored pin: the backup source it was
// pulled from, or else `name@sha256:…`.
func storedRef(img store.InstanceImage) string {
	if img.Ref != "" {
		return img.Ref
	}
	return repoOf(img.Image) + "@" + img.Digest
}

// composeUpInstance is `compose up -d` for an installed instance, after the
// brain has made sure every image is here (ensureImages). Every `up` after
// install goes through it: the override sets `pull_policy: never`, so compose
// no longer pulls a missing image by itself (#588).
//
// waits is the rate-limit backoff for a missing image: pullRetryDelays for a
// start or a recreate, nil for the reconcile pass at boot, which shares one
// short budget across every app and must not spend it waiting on one registry.
func (m *Manager) composeUpInstance(ctx context.Context, id string, waits []time.Duration) (string, error) {
	if err := m.ensureImages(ctx, id, waits); err != nil {
		return "", err
	}
	return m.docker.ComposeUp(ctx, m.instanceDir(id), "moose-"+id)
}

// ensureImages pulls any image an instance's override names that is not in the
// local image store: it was removed, or the instance's state was moved to a new
// server. The pull takes the same path as at install, upstream first and then
// the backup sources (pullImage), never the override's reference alone. If the
// bytes now come from somewhere else, the override and the stored pin are
// rewritten to the reference that was pulled. A present image costs one
// `docker image inspect`.
//
// An override without an `image:` for a service, or a pin with no digest, is
// left for compose to report, as before.
func (m *Manager) ensureImages(ctx context.Context, id string, waits []time.Duration) error {
	pins, err := m.store.GetInstanceImages(id)
	if err != nil {
		return fmt.Errorf("read image pins: %w", err)
	}
	if len(pins) == 0 {
		return nil
	}
	ovPath := filepath.Join(m.instanceDir(id), "compose.override.yml")
	raw, err := os.ReadFile(ovPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // compose up reports the missing file
		}
		return fmt.Errorf("read override: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse override: %w", err)
	}
	services, _ := doc["services"].(map[string]any)

	// The backup sources come from the instance's own manifest, read only when
	// a pull is needed. Without it the pull still goes to upstream.
	var sources map[string]manifest.ImageRef
	loaded := false
	pulled := map[string]string{} // override ref → the ref it is now
	changed, pinsChanged := false, false
	for i, pin := range pins {
		svc, _ := services[pin.Service].(map[string]any)
		ref, _ := svc["image"].(string)
		if ref == "" {
			continue
		}
		now, done := pulled[ref]
		if !done {
			now = ref
			if _, err := m.docker.ImageInspect(ctx, ref); err != nil && pin.Digest != "" {
				if !loaded {
					loaded = true
					if man, merr := m.loadInstanceManifest(id); merr != nil {
						slog.Warn("image missing and manifest unreadable, pulling from upstream only", "instance_id", id, "err", merr)
					} else {
						sources = man.Images
					}
				}
				var backups []string
				if !m.offlineInstall {
					backups = sourceRefs(pin.Image, sources[pin.Image].Sources, pin.Digest)
				}
				slog.Info("image missing, pulling", "instance_id", id, "service", pin.Service, "image", pin.Image)
				if now, err = pullImageWaits(ctx, m.docker, repoOf(pin.Image)+"@"+pin.Digest, backups, waits); err != nil {
					return fmt.Errorf("pull image for service %q: %w", pin.Service, err)
				}
			}
			pulled[ref] = now
		}
		if now != ref {
			svc["image"] = now
			changed = true
		}
		// The stored pin follows the override, even when nothing was pulled: a
		// save that failed on an earlier start is repaired here.
		if want := pinRef(pin.Image, pin.Digest, now); want != pin.Ref {
			pins[i].Ref = want
			pinsChanged = true
		}
	}
	if changed {
		out, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		if err := os.WriteFile(ovPath, out, 0o644); err != nil {
			return fmt.Errorf("write override: %w", err)
		}
	}
	if !pinsChanged {
		return nil
	}
	if err := m.store.SetInstanceImages(id, pins); err != nil {
		return fmt.Errorf("persist image pins: %w", err)
	}
	return nil
}
