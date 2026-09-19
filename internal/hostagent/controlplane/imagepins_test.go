package controlplane

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dev/control-plane/images.lock pins every third-party control-plane image by
// digest (#432). Nothing in Go reads that file — the Makefile does — so this
// guard lives with the other tests that read the committed dev/control-plane/
// files (see stageRealCompose). It catches the two ways the pin file goes wrong
// silently:
//
//  1. an entry loses its digest and goes back to a mutable tag, and
//  2. a tag is bumped in the pin file but not in the places a box names the
//     image by tag. A box loads the tarball offline and never pulls, so if the
//     saved tag and the named tag drift the control plane cannot start.
const pinFile = "images.lock"

// name:tag@sha256:<64 hex>. The tag half stays readable as a label; the digest
// decides the bytes.
var pinRE = regexp.MustCompile(`^[^\s@]+:[^\s@:]+@sha256:[0-9a-f]{64}$`)

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", ".."}, parts...)...)
}

// readPins parses the lock file the same way `make` and `sh` do: plain
// NAME=value lines, `#` comments, blank lines ignored.
func readPins(t *testing.T) map[string]string {
	t.Helper()
	src := repoPath("dev", "control-plane", pinFile)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	pins := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s:%d: not a NAME=value line: %q", src, i+1, line)
		}
		if name != strings.TrimSpace(name) || value != strings.TrimSpace(value) {
			t.Errorf("%s:%d: spaces around `=` break `make include` (and `sh` sourcing, if a script ever needs one): %q", src, i+1, line)
		}
		pins[name] = value
	}
	return pins
}

func tagOf(ref string) string {
	tag, _, _ := strings.Cut(ref, "@")
	return tag
}

func TestImagePinsAreDigestPinned(t *testing.T) {
	pins := readPins(t)
	want := []string{
		"CADDY_IMAGE", "PROXY_IMAGE",
		"CADDY_ACMEDNS_BUILDER_IMAGE", "CADDY_ACMEDNS_BASE_IMAGE",
		"BRAIN_BUILDER_IMAGE", "BRAIN_RUNTIME_IMAGE", "UI_BUILDER_IMAGE",
	}
	for _, name := range want {
		ref, ok := pins[name]
		if !ok {
			t.Errorf("%s: missing from %s — the Makefile expects it", name, pinFile)
			continue
		}
		if !pinRE.MatchString(ref) {
			t.Errorf("%s = %q, want name:tag@sha256:<64 hex> — a bare tag is not a lookup key (APP_LIFECYCLE.md # Locked: image digest pinning)", name, ref)
		}
	}
}

// The tag a pin is saved under is the tag a box looks the image up by, in files
// the pin file cannot interpolate into.
// The one pin that is not an image: xcaddy fetches this module at build time and
// takes the latest release when given no version, so the plugin could change
// under two frozen base images.
func TestAcmednsModuleIsVersionPinned(t *testing.T) {
	got := readPins(t)["CADDY_ACMEDNS_MODULE"]
	if !regexp.MustCompile(`^[^\s@]+@v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(got) {
		t.Errorf("CADDY_ACMEDNS_MODULE = %q, want module@vX.Y.Z — unversioned, xcaddy takes whatever is latest that day", got)
	}
}

// Every base image the control-plane Dockerfiles build on has to come in as a
// build arg that is declared with no default and actually fed from the pin file.
// A bare tag in a FROM is the regression this whole change removes, and it is
// invisible in a green build: the image just holds different bytes. Checking
// only for "${" would not catch it — `ARG BASE=caddy:2-alpine` interpolates
// exactly the same way, and an arg the Makefile forgot to pass leaves an empty
// base name. So all three properties are asserted together.
func TestDockerfilesTakeTheirBasesAsBuildArgs(t *testing.T) {
	makefile, err := os.ReadFile(repoPath("Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	argRef := regexp.MustCompile(`^\$\{([A-Z0-9_]+)\}$`)
	// An explicit list, not a walk: the tree also holds gitignored build residue
	// with Dockerfiles in it that are none of our business.
	for _, parts := range [][]string{
		{"cmd", "brain", "Dockerfile"},
		{"web-ui", "Dockerfile"},
		{"dev", "control-plane", "caddy-acmedns", "Dockerfile"},
	} {
		src := repoPath(parts...)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if !strings.HasPrefix(line, "FROM ") {
				continue
			}
			base := strings.Fields(line)[1]
			m := argRef.FindStringSubmatch(base)
			if m == nil {
				t.Errorf("%s:%d: FROM %s names a base directly; it must be a build arg fed from %s", src, i+1, base, pinFile)
				continue
			}
			arg := m[1]
			// Declared, and declared without a default — a default is a second
			// copy of the pin, free to drift.
			var declared bool
			for _, l := range lines {
				switch {
				case strings.TrimSpace(l) == "ARG "+arg:
					declared = true
				case strings.HasPrefix(strings.TrimSpace(l), "ARG "+arg+"="):
					t.Errorf("%s: ARG %s has a default; the pin in %s must be the only copy", src, arg, pinFile)
					declared = true
				}
			}
			if !declared {
				t.Errorf("%s:%d: FROM uses ${%s} but no ARG declares it, so the base name is empty", src, i+1, arg)
			}
			// And something has to pass it, or the build fails on an empty base.
			if !strings.Contains(string(makefile), "--build-arg "+arg+"=$(") {
				t.Errorf("%s: no make target passes --build-arg %s=...; it would build with an empty base", src, arg)
			}
		}
	}
}

func TestPinnedTagsMatchTheirConsumers(t *testing.T) {
	pins := readPins(t)
	cases := []struct {
		pin      string
		consumer []string // path parts, relative to the repo root
		want     func(tag string) string
	}{
		{"CADDY_IMAGE", []string{"dev", "control-plane", "compose.yml"},
			func(tag string) string { return "image: ${MOOSE_CADDY_IMAGE:-" + tag + "}" }},
		{"PROXY_IMAGE", []string{"dev", "cloud", "stage-control-plane.sh"},
			func(tag string) string { return "Environment=MOOSE_PROXY_IMAGE=" + tag }},
		{"PROXY_IMAGE", []string{"dev", "test-qemu", "bootstrap.sh"},
			func(tag string) string { return "Environment=MOOSE_PROXY_IMAGE=" + tag }},
		{"PROXY_IMAGE", []string{"cmd", "host-agent-real", "main.go"},
			func(tag string) string { return `env("MOOSE_PROXY_IMAGE", "` + tag + `")` }},
	}
	for _, tc := range cases {
		src := repoPath(tc.consumer...)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		want := tc.want(tagOf(pins[tc.pin]))
		if !strings.Contains(string(b), want) {
			t.Errorf("%s does not contain %q — it names the image by tag, so it must track %s in %s", src, want, tc.pin, pinFile)
		}
	}
}
