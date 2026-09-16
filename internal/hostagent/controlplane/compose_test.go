package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageRealCompose copies the **committed** dev/control-plane/compose.yml into a
// temp dir and returns it. That file is the source stage-control-plane.sh puts
// on a real box, so testing against it means a service rename, a switch to
// interpolation, or a reindent breaks this suite instead of breaking an update
// on a booted box. (A fixture of our own would test the rewriter against a file
// nothing ships.)
func stageRealCompose(t *testing.T) (dir string, original string) {
	t.Helper()
	src := filepath.Join("..", "..", "..", "dev", "control-plane", "compose.yml")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read committed compose %s: %v", src, err)
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ComposeFile), b, 0o644); err != nil {
		t.Fatalf("stage compose: %v", err)
	}
	return dir, string(b)
}

// The whole point of the rewriter: change one line, leave the hand-authored
// file otherwise byte-identical. The comments in that file explain why Caddy is
// interpolated, why the socket-proxy is absent, and why the network is external
// — a marshal-based rewrite would delete all of it on the first update.
func TestRewriteUIImageChangesExactlyOneLine(t *testing.T) {
	dir, original := stageRealCompose(t)

	old, err := RewriteUIImage(dir, "ghcr.io/onmoose/moose-ui@sha256:abc123")
	if err != nil {
		t.Fatalf("RewriteUIImage: %v", err)
	}
	if old != "moose-ui:dev" {
		t.Errorf("old ref = %q, want moose-ui:dev (the committed file's pin)", old)
	}

	b, err := os.ReadFile(filepath.Join(dir, ComposeFile))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	before, after := strings.Split(original, "\n"), strings.Split(string(b), "\n")
	if len(before) != len(after) {
		t.Fatalf("line count changed: %d → %d", len(before), len(after))
	}
	var changed []int
	for i := range before {
		if before[i] != after[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("changed %d lines (%v), want exactly 1", len(changed), changed)
	}
	got := after[changed[0]]
	if got != "    image: ghcr.io/onmoose/moose-ui@sha256:abc123" {
		t.Errorf("rewritten line = %q, want the same indent and key with the new ref", got)
	}
	// The caddy service's interpolated image is the neighbour most at risk from
	// a scan that keys on "image:" rather than on the service block.
	if !strings.Contains(string(b), "image: ${MOOSE_CADDY_IMAGE:-caddy:2-alpine}") {
		t.Error("caddy's interpolated image line did not survive the rewrite")
	}
}

// A rewrite is only the handoff (UPDATES.md # 8.3) if the brain can read what
// was written. This asserts the written file still parses and pins the new ref
// — the same read the brain performs on its next boot.
func TestRewriteUIImageIsReadableAsYAML(t *testing.T) {
	dir, _ := stageRealCompose(t)
	const ref = "ghcr.io/onmoose/moose-ui@sha256:deadbeef"
	if _, err := RewriteUIImage(dir, ref); err != nil {
		t.Fatalf("RewriteUIImage: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ComposeFile))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := verifyUIImage(b, ref, "test"); err != nil {
		t.Errorf("rewritten compose does not read back as the new ref: %v", err)
	}
}

// Every refusal path must leave the file exactly as it was. A rewriter that
// half-applies a bad input is worse than one that refuses it, because the box
// reconciles to whatever is on disk.
func TestRewriteUIImageRefusalsLeaveTheFileUntouched(t *testing.T) {
	cases := []struct {
		name    string
		compose string // "" ⇒ use the committed file
		ref     string
		wantErr string
	}{
		{
			name:    "empty ref",
			ref:     "  ",
			wantErr: "empty",
		},
		{
			name:    "ref with a newline would inject YAML",
			ref:     "moose-ui:v1\n    command: [\"sh\"]",
			wantErr: "whitespace",
		},
		{
			name:    "interpolated existing ref is not resolved",
			compose: "services:\n  moose-ui:\n    image: ${MOOSE_UI_IMAGE:-moose-ui:dev}\n",
			ref:     "moose-ui:v2",
			wantErr: "does not resolve",
		},
		{
			name:    "no moose-ui service",
			compose: "services:\n  caddy:\n    image: caddy:2-alpine\n",
			ref:     "moose-ui:v2",
			wantErr: "no \"moose-ui\" service",
		},
		{
			name:    "moose-ui service pins no image",
			compose: "services:\n  moose-ui:\n    restart: unless-stopped\n  caddy:\n    image: caddy:2-alpine\n",
			ref:     "moose-ui:v2",
			wantErr: "pins no image",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dir, original string
			if tc.compose == "" {
				dir, original = stageRealCompose(t)
			} else {
				dir, original = t.TempDir(), tc.compose
				if err := os.WriteFile(filepath.Join(dir, ComposeFile), []byte(tc.compose), 0o644); err != nil {
					t.Fatalf("stage compose: %v", err)
				}
			}

			_, err := RewriteUIImage(dir, tc.ref)
			if err == nil {
				t.Fatalf("RewriteUIImage succeeded, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			b, readErr := os.ReadFile(filepath.Join(dir, ComposeFile))
			if readErr != nil {
				t.Fatalf("read back: %v", readErr)
			}
			if string(b) != original {
				t.Error("the compose was modified by a refused rewrite")
			}
		})
	}
}

// The ref this returns is recorded as the *previous* generation, which is what
// a revert pins — so a trailing comment must not ride along with it. A revert
// that tries to run `moose-ui:dev # baked at build` as an image fails at the
// one moment the box most needs it to work. The rewrite also drops the comment
// from the line, which is correct: a comment about the old ref is wrong the
// moment the ref changes.
func TestRewriteUIImageIgnoresAnInlineComment(t *testing.T) {
	dir := t.TempDir()
	const compose = `services:
  moose-ui:
    image: moose-ui:dev # baked into the disk image at build time
`
	if err := os.WriteFile(filepath.Join(dir, ComposeFile), []byte(compose), 0o644); err != nil {
		t.Fatalf("stage compose: %v", err)
	}
	old, err := RewriteUIImage(dir, "moose-ui:v2")
	if err != nil {
		t.Fatalf("RewriteUIImage: %v", err)
	}
	if old != "moose-ui:dev" {
		t.Errorf("old ref = %q, want moose-ui:dev with the comment stripped — this string is what a revert pins", old)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ComposeFile))
	if got := strings.TrimSpace(string(b)); !strings.HasSuffix(got, "image: moose-ui:v2") {
		t.Errorf("rewritten file ends %q, want the image line with no stale comment", got)
	}
}

// The ${VAR} guard reads the same value the previous-ref does, so a commented
// interpolation must still be refused rather than half-read.
func TestRewriteUIImageRefusesCommentedInterpolation(t *testing.T) {
	dir := t.TempDir()
	const compose = "services:\n  moose-ui:\n    image: ${MOOSE_UI_IMAGE:-moose-ui:dev} # overridable\n"
	if err := os.WriteFile(filepath.Join(dir, ComposeFile), []byte(compose), 0o644); err != nil {
		t.Fatalf("stage compose: %v", err)
	}
	if _, err := RewriteUIImage(dir, "moose-ui:v2"); err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("err = %v, want a refusal to resolve the interpolated ref", err)
	}
}

// A ref that is not a service key must not be mistaken for one. `image:` under
// caddy comes first in the real file, so a scan that ignores service boundaries
// would rewrite Caddy's image and leave the UI alone — the box would then serve
// the dashboard bundle as its reverse proxy.
func TestRewriteUIImageIgnoresOtherServices(t *testing.T) {
	dir := t.TempDir()
	const compose = `services:
  caddy:
    image: caddy:2-alpine
    container_name: moose-caddy
  moose-ui:
    image: moose-ui:dev
    read_only: true
`
	if err := os.WriteFile(filepath.Join(dir, ComposeFile), []byte(compose), 0o644); err != nil {
		t.Fatalf("stage compose: %v", err)
	}
	old, err := RewriteUIImage(dir, "moose-ui:v2")
	if err != nil {
		t.Fatalf("RewriteUIImage: %v", err)
	}
	if old != "moose-ui:dev" {
		t.Errorf("old ref = %q, want moose-ui:dev", old)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ComposeFile))
	if !strings.Contains(string(b), "image: caddy:2-alpine") {
		t.Error("caddy's image was rewritten; the scan is not service-scoped")
	}
	if !strings.Contains(string(b), "image: moose-ui:v2") {
		t.Error("the UI image was not rewritten")
	}
}
