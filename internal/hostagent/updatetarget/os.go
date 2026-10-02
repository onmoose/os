package updatetarget

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

// This file is stream A's half of the answer (UPDATES.md # 1, #563): the OS
// releases the box may install, and the rules for which one it picks.
//
// The answer names OS releases as a list, not one release: the newest patch of
// each minor, from the oldest one still supported up to the target, which is
// the last entry. A box never skips a minor, because only a minor release may
// change an on-disk format and the release in the other slot must always be
// able to read what this one wrote.
//
// **The OS part is checked on its own.** A bad OS part refuses stream A only;
// the control-plane pair in the same answer still applies. The two streams are
// separate transactions with separate rollbacks, so a mistake in one must not
// hold the other back.

// OSRelease is one OS release in the answer.
type OSRelease struct {
	// Version is the moose (OS) release, X.Y.Z. It is what the box compares
	// with its own version, so unlike the control-plane version it is used.
	Version string
	// BundleURL is where the box downloads the RAUC bundle. It must start with
	// the expected prefix (DefaultOSURLPrefix, or MOOSE_UPDATE_OS_URL_PREFIX).
	BundleURL string
	// BundleSHA256 pins the bundle's bytes. The box installs only a bundle
	// whose sha256 is this one, on top of RAUC's signature check, so a leaked
	// signer alone cannot make a box install anything (DECISIONS.md 2026-10-02).
	BundleSHA256 string
}

// DefaultOSURLPrefix is where published OS bundles live: the GitHub Release
// of each OS release (BUILD.md # 6). The digest pins the bytes; the prefix is
// the check that stops a well-formed answer from making the box fetch from
// anywhere at all, the same job the expected repositories do for the brain.
const DefaultOSURLPrefix = "https://github.com/onmoose/os/releases/download/"

// ErrOSRefused wraps every reason the OS part of an answer is refused.
var ErrOSRefused = errors.New("updatetarget: OS part refused")

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// canonical turns "1.2.3" or "v1.2.3" into "v1.2.3", or "" when it is not a
// plain X.Y.Z. Pre-release and build suffixes are refused: an OS release is
// always a plain release (BUILD.md # Versioning).
func canonical(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) || semver.Canonical(v) != v || semver.Prerelease(v) != "" || semver.Build(v) != "" {
		return ""
	}
	return v
}

// ValidateOS is the boundary check on the OS part. It runs before anything is
// downloaded. prefix is the expected start of every bundle URL; empty means
// any http or https URL.
func ValidateOS(list []OSRelease, prefix string) error {
	prev := ""
	for i, r := range list {
		v := canonical(r.Version)
		if v == "" {
			return fmt.Errorf("%w: entry %d: %q is not an X.Y.Z version", ErrOSRefused, i, r.Version)
		}
		if prev != "" && semver.Compare(prev, v) >= 0 {
			return fmt.Errorf("%w: the releases are not in ascending order (%s after %s)", ErrOSRefused, r.Version, strings.TrimPrefix(prev, "v"))
		}
		prev = v
		if !sha256Hex.MatchString(r.BundleSHA256) {
			return fmt.Errorf("%w: %s: the bundle is not pinned to a sha256 digest", ErrOSRefused, r.Version)
		}
		u, err := url.Parse(r.BundleURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: %s: the bundle URL is not an absolute http or https URL", ErrOSRefused, r.Version)
		}
		if prefix != "" && !strings.HasPrefix(r.BundleURL, prefix) {
			return fmt.Errorf("%w: %s: the bundle URL %s does not start with %s", ErrOSRefused, r.Version, RedactURL(r.BundleURL), prefix)
		}
	}
	return nil
}

// line is a version's MAJOR.MINOR, as two numbers.
type line struct{ major, minor int }

func lineOf(v string) line {
	var l line
	var patch int
	_, _ = fmt.Sscanf(strings.TrimPrefix(v, "v"), "%d.%d.%d", &l.major, &l.minor, &patch)
	return l
}

func (a line) less(b line) bool {
	return a.major < b.major || (a.major == b.major && a.minor < b.minor)
}

// PickOS chooses what to install from a validated list (UPDATES.md # 1):
//
//   - the target is the last entry;
//   - target on a later minor: the first entry on a minor above the box's own
//     (the next step; the rest waits for a later window);
//   - target on the box's own minor, or one minor back: the target itself;
//   - target further back, or below the running control plane's floor: refused.
//
// running is this box's OS version and floor the running control plane's
// minimum_host_agent ("" when the box cannot read it, and then every move
// to an older release is refused). current is true when the box already runs the target.
func PickOS(running, floor string, list []OSRelease) (rel OSRelease, current bool, err error) {
	if len(list) == 0 {
		return OSRelease{}, false, fmt.Errorf("%w: the answer names no OS release", ErrOSRefused)
	}
	run := canonical(running)
	if run == "" {
		return OSRelease{}, false, fmt.Errorf("%w: this box's own version %q is not an X.Y.Z version", ErrOSRefused, running)
	}
	target := list[len(list)-1]
	tv := canonical(target.Version)
	if semver.Compare(tv, run) == 0 {
		return target, true, nil
	}
	rl, tl := lineOf(run), lineOf(tv)
	switch {
	case rl.less(tl):
		for _, r := range list {
			if rl.less(lineOf(canonical(r.Version))) {
				rel = r
				break
			}
		}
		// The step must be the line right after the box's own: a list that
		// leaves a minor out would make the box skip it.
		if nl := lineOf(canonical(rel.Version)); !(nl == line{rl.major, rl.minor + 1} || nl == line{rl.major + 1, 0}) {
			return OSRelease{}, false, fmt.Errorf("%w: the next step %s is not the minor right after %s; the list leaves a minor out", ErrOSRefused, rel.Version, running)
		}
	case tl == rl:
		rel = target
	case oneLineBack(tl, rl, list):
		rel = target
	default:
		return OSRelease{}, false, fmt.Errorf("%w: the target %s is more than one minor back from %s", ErrOSRefused, target.Version, running)
	}
	// A downgrade needs the floor: without it the box cannot tell whether the
	// older host-agent is one the running brain still works with.
	if canonical(floor) == "" && semver.Compare(canonical(rel.Version), run) < 0 {
		return OSRelease{}, false, fmt.Errorf("%w: %s is older than %s, and this box cannot read the running control plane's floor", ErrOSRefused, rel.Version, running)
	}
	if f := canonical(floor); f != "" && semver.Compare(canonical(rel.Version), f) < 0 {
		return OSRelease{}, false, fmt.Errorf("%w: %s is below the running control plane's floor %s", ErrOSRefused, rel.Version, floor)
	}
	return rel, false, nil
}

// oneLineBack reports whether t is the line right before r. Within a major
// that is minor-1. Across a major (r is X.0) it is a line of major X-1, and the
// box can only check that the list names no later line of that major.
func oneLineBack(t, r line, list []OSRelease) bool {
	if t.major == r.major {
		return t.minor == r.minor-1
	}
	if r.minor != 0 || t.major != r.major-1 {
		return false
	}
	for _, e := range list {
		if l := lineOf(canonical(e.Version)); l.major == t.major && l.minor > t.minor {
			return false
		}
	}
	return true
}
