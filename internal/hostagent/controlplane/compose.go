package controlplane

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ComposeFile is the staged control-plane compose's name inside
// MOOSE_CONTROL_PLANE_DIR. Fixed by the image build, which stages it there; the
// brain reconciles the stack from this exact file
// (lifecycle.EnsureControlPlane).
const ComposeFile = "compose.yml"

// UIServiceName is the compose service running the dashboard bundle
// (CONTROL_PLANE.md # the dashboard UI is a brain-launched container).
const UIServiceName = "moose-ui"

// RewriteUIImage points the moose-ui service at ref, in place, and returns the
// ref it replaced so a caller can record it as the previous generation.
//
// This is the # 8.3 handoff in one function: host-agent writes the new ref here
// **before** recreating anything, so when the brain restarts and reconciles the
// control-plane stack it converges to the version already running instead of
// reverting it. Both actors read one declaration; only one writes it.
//
// It edits a single line rather than round-tripping the YAML through a marshal.
// The file is hand-authored and heavily commented — it explains why Caddy is
// interpolated, why the proxy is absent, why the network is external — and a
// marshal would silently delete every one of those comments the first time a
// box updated. A one-line edit also means a bug here can damage exactly one
// line. Any inline comment on the image line itself is dropped, which is
// correct: a comment about the old ref is wrong the moment the ref changes.
func RewriteUIImage(dir, ref string) (old string, err error) {
	if err := validRef(ref); err != nil {
		return "", err
	}
	p := filepath.Join(dir, ComposeFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read control-plane compose: %w", err)
	}

	lines := strings.Split(string(b), "\n")
	idx, old, err := uiImageLine(lines, p)
	if err != nil {
		return "", err
	}
	// A ${VAR} ref is resolved by `docker compose`, not by us. Rewriting one
	// would drop whatever indirection the author put there; the read side
	// (lifecycle.ControlPlaneUIImage) refuses to report one for the same reason.
	if strings.Contains(old, "${") {
		return "", fmt.Errorf("control-plane compose %s pins %q for %q, which this rewriter does not resolve", p, old, UIServiceName)
	}
	indent := lines[idx][:len(lines[idx])-len(strings.TrimLeft(lines[idx], " "))]
	lines[idx] = indent + "image: " + ref

	out := strings.Join(lines, "\n")
	// Read back through a YAML parser before committing the write. The edit is
	// textual, so nothing else would catch a rewrite that produced valid-looking
	// text and invalid YAML — and the reader that would catch it runs in the
	// brain, on the next boot, after the containers have already been recreated.
	if err := verifyUIImage([]byte(out), ref, p); err != nil {
		return "", err
	}
	if err := writeFileAtomic(p, []byte(out), 0o644); err != nil {
		return "", err
	}
	return old, nil
}

// ReadUIImage reports the image ref the staged compose currently pins for the
// dashboard service.
//
// The updater needs this even when the UI is not the thing moving: a brain-only
// update still records the **pair** in the ledger, and half a pair is not
// something a revert can act on.
func ReadUIImage(dir string) (string, error) {
	p := filepath.Join(dir, ComposeFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read control-plane compose: %w", err)
	}
	_, ref, err := uiImageLine(strings.Split(string(b), "\n"), p)
	return ref, err
}

// uiImageLine finds the index of the moose-ui service's `image:` line and the
// ref it currently pins.
//
// The scan is indentation-based because that is what distinguishes the service
// named moose-ui from any other line that happens to contain the string: a
// service key sits at one indent under `services:`, and its `image:` sits at
// the next indent in, before the following key at the service's own level.
func uiImageLine(lines []string, path string) (idx int, ref string, err error) {
	inServices := false
	serviceIndent := -1 // indent of the moose-ui key, once found
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if serviceIndent >= 0 {
			// Inside moose-ui: a key back at or above its own indent ends it.
			if indent <= serviceIndent {
				break
			}
			if v, found := strings.CutPrefix(trimmed, "image:"); found {
				return i, stripInlineComment(v), nil
			}
			continue
		}
		if indent == 0 {
			inServices = trimmed == "services:"
			continue
		}
		if inServices && trimmed == UIServiceName+":" {
			serviceIndent = indent
		}
	}
	if serviceIndent < 0 {
		return 0, "", fmt.Errorf("control-plane compose %s has no %q service", path, UIServiceName)
	}
	return 0, "", fmt.Errorf("control-plane compose %s pins no image for %q", path, UIServiceName)
}

// stripInlineComment removes a trailing YAML comment from a scalar value, so
// `moose-ui:dev # baked at build` reads as the ref and not as the whole line.
//
// This matters more than the tidiness suggests: the ref returned by
// uiImageLine is the one a caller records as the **previous** generation, which
// is what a revert pins. A ref carrying a comment is not an image reference, so
// the revert would try to run one and fail at the moment the box most needs it
// to work. The `${` guard reads the same value, so it too would have been
// testing the wrong string.
//
// YAML starts a comment only at a `#` preceded by whitespace, which is why this
// does not cut at the first `#` it sees.
func stripInlineComment(v string) string {
	v = strings.TrimSpace(v)
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}

// verifyUIImage parses the rewritten compose the same narrow way the brain does
// (lifecycle.ControlPlaneUIImage) and asserts it now pins want.
//
// The parse is duplicated rather than imported: internal/lifecycle is the
// brain's transaction owner, and only cmd/brain and internal/api may import it
// (CLAUDE.md # Layer boundaries). Ten lines of struct is the cheaper side of
// that boundary. The two are held together by a test that parses the committed
// dev/control-plane/compose.yml.
func verifyUIImage(content []byte, want, path string) error {
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return fmt.Errorf("rewritten control-plane compose %s does not parse: %w", path, err)
	}
	if got := strings.TrimSpace(doc.Services[UIServiceName].Image); got != want {
		return fmt.Errorf("rewritten control-plane compose %s pins %q for %q, want %q", path, got, UIServiceName, want)
	}
	return nil
}

// validRef rejects refs that would corrupt the file rather than change it. A
// newline is the one that matters — it would inject arbitrary YAML into the
// service block — but an empty or space-bearing ref is equally not an image
// reference, and failing here beats writing a file no compose can run.
func validRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("image ref is empty")
	}
	if strings.ContainsAny(ref, " \t\r\n#") {
		return fmt.Errorf("image ref %q contains whitespace or a comment character", ref)
	}
	return nil
}
