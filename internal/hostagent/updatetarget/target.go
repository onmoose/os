// Package updatetarget answers one question for host-agent: **which control
// plane should this box be running?**
//
// It is the cheap half of the update design. The expensive half — pull by
// digest, snapshot the brain's SQLite, write the staged declaration, recreate
// only what moved, health-check, revert both on failure — is
// internal/hostagent/cpupdate, and it is trigger-free on purpose (UPDATES.md
// # 8: "the expensive half of the update machinery is shared, and we only build
// it once"). This package picks the target and hands it over. It never applies
// anything itself.
//
// There is **one seam** (Source) with an implementation per environment profile
// (ENVIRONMENT.md):
//
//   - hosted — reads the target from an update-target URL over the box's
//     existing outbound path (http.go). No inbound port, no listener, nothing
//     new in the firewall: the box asks, the cloud never connects in
//     (UPDATES.md # 8.1).
//   - appliance — reads it from the signed release manifest (manifest.go,
//     RELEASE_MANIFEST.md), unchanged in how that file is fetched and verified.
//
// Both answer the same shape, so the comparison, the window gate, the apply and
// the failure handling are written once in loop.go. A second copy of any of
// that, per profile, is the outcome this package exists to avoid.
//
// # The answer is digests, never tags
//
// An image can be named two ways. A tag (`ghcr.io/onmoose/brain:v0.6.0`) is a
// label, and a label can be moved to point at different bytes. A digest
// (`ghcr.io/onmoose/brain@sha256:670b…`) IS the bytes. cpupdate pulls by digest
// on purpose (BUILD.md # 6), so something has to turn "v0.6.0" into that hash.
//
// **The source does it, and the box consumes what it is given.** A box that
// resolved a tag itself would be asking a registry at update time and trusting
// a movable label, and two boxes told "v0.6.0" a week apart could end up running
// different bytes. Resolving once, at the sender, is what makes a fleet
// provably uniform.
//
// That is a hard requirement **on the box**, not only on the sender: the hosted
// update-target read is unauthenticated today, so its answer is untrusted input.
// Validate is the boundary check, and it runs before anything is pulled.
package updatetarget

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Target is what a source answers: the control-plane pair this box should be
// running, named by pinned reference.
//
// The pinned reference and the bare digest are both carried because the wire
// carries both. The reference is what goes to `docker pull` verbatim; the digest
// is the same value split out, for logging "now running sha256:…" without
// parsing. They cannot disagree — Validate refuses an answer where they do.
type Target struct {
	// Version is for display, comparison in logs, and nothing else. **Nothing
	// pulls by it.**
	Version string
	// BrainImage / UIImage are full pinned references: repository@sha256:…
	BrainImage string
	UIImage    string
	// BrainDigest / UIDigest are the same digests split out.
	BrainDigest string
	UIDigest    string
	// PublishedAt is when the release was published. Informational.
	PublishedAt time.Time
	// Window is when this box may apply, as "HH:MM-HH:MM" local, or empty when
	// the source has no opinion.
	//
	// **Empty is not "use the default".** It means the box keeps the window it
	// is configured with. Reading it as the default would let a source that
	// says nothing outrank an operator's own setting (UPDATES.md # 8.4).
	//
	// It is carried raw, not parsed, because a source is untrusted input and a
	// window it cannot state properly must not stop the box updating. The loop
	// parses it, warns, and falls back.
	Window string
}

// Source is the seam: one call, one answer.
//
// The three outcomes are deliberately distinct, and loop.go treats them
// differently:
//
//   - a Target and no error — this is what the box should run,
//   - ErrNoTarget — the source is fine and legitimately has nothing to offer
//     (nothing published yet). "Keep running what you are running", never
//     "there is nothing to run",
//   - any other error — the source could not be reached or answered something
//     unusable. Also a no-op, but a loud one.
//
// A box must never degrade, refuse to serve, or roll anything back because it
// could not ask.
type Source interface {
	// Target reports the target for this box, or ErrNoTarget when the source
	// has none.
	Target(ctx context.Context) (Target, error)
}

// ErrNoTarget means the source answered, and its answer is "nothing published".
// It is not a failure: a channel with no release on it is a normal state,
// especially before the first ship.
var ErrNoTarget = errors.New("updatetarget: the source has no target")

// ErrNotPinned means the source named an image by something other than a
// digest — a tag, a truncated digest, an empty string. The box refuses to apply
// it rather than pulling it (see the package comment).
var ErrNotPinned = errors.New("updatetarget: image reference is not pinned to a digest")

// ErrWrongRepository means an image reference is pinned, but points at a
// repository this box does not expect. It is the check that stops a compromised
// or misconfigured source redirecting a box to pull arbitrary images: a digest
// is only as trustworthy as the repository it is fetched from.
var ErrWrongRepository = errors.New("updatetarget: image reference is for an unexpected repository")

// Repositories is the expected home of each control-plane image. Configuration,
// not a constant: the CI boot proof serves both images from a registry inside
// the guest, and a test box may be pointed somewhere else entirely.
type Repositories struct {
	Brain string
	UI    string
}

// DefaultRepositories is where the published control-plane images live
// (BUILD.md # 6 — built from the public repo, pulled by digest).
var DefaultRepositories = Repositories{
	Brain: "ghcr.io/onmoose/brain",
	UI:    "ghcr.io/onmoose/ui",
}

// pinnedRef matches a reference pinned to a sha256 digest, capturing the
// repository. Lowercase hex only, exactly 64 of them: a shorter string is a
// truncated digest, and an uppercase one is not a digest Docker will accept.
var pinnedRef = regexp.MustCompile(`^([^@\s]+)@(sha256:[0-9a-f]{64})$`)

// Validate is the boundary check on an untrusted answer. It runs **before
// anything is pulled**, and a failure leaves the box on its current version.
//
// It refuses:
//
//   - an answer missing either image. A half-answer must never produce a
//     half-apply: the brain and the UI move together in one transaction
//     (UPDATES.md # 8.3), so a target naming only one of them is not
//     applicable — not "an update to one component",
//   - a reference that is not pinned to a digest (the whole point, see the
//     package comment),
//   - a reference pointing at an unexpected repository,
//   - a carried digest that disagrees with the digest inside its own reference.
//     The two halves come from one value at the sender, so a disagreement means
//     the answer was assembled by something that does not know that, and no
//     reading of it is safe.
func (t Target) Validate(repos Repositories) error {
	brainDigest, err := checkRef("brain", t.BrainImage, repos.Brain)
	if err != nil {
		return err
	}
	uiDigest, err := checkRef("ui", t.UIImage, repos.UI)
	if err != nil {
		return err
	}
	if t.BrainDigest != "" && t.BrainDigest != brainDigest {
		return fmt.Errorf("updatetarget: brain_digest %q does not match the digest in brain_image", t.BrainDigest)
	}
	if t.UIDigest != "" && t.UIDigest != uiDigest {
		return fmt.Errorf("updatetarget: ui_digest %q does not match the digest in ui_image", t.UIDigest)
	}
	return nil
}

// checkRef validates one reference and returns the digest inside it.
func checkRef(name, ref, wantRepo string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("updatetarget: no %s image in the answer", name)
	}
	m := pinnedRef.FindStringSubmatch(ref)
	if m == nil {
		return "", fmt.Errorf("%w: %s image %q", ErrNotPinned, name, ref)
	}
	if wantRepo != "" && m[1] != wantRepo {
		return "", fmt.Errorf("%w: %s image %q is not %s", ErrWrongRepository, name, ref, wantRepo)
	}
	return m[2], nil
}
