package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- EnsureControlPlane: the startup wiring. Predates the image reader below
// and is unrelated to it; both live here because both are the control plane.

func TestEnsureControlPlaneUpsTheStack(t *testing.T) {
	docker := newFakeDocker()
	m := &Manager{docker: docker}

	if err := m.EnsureControlPlane(context.Background(), "/var/lib/moose/control-plane"); err != nil {
		t.Fatalf("EnsureControlPlane: %v", err)
	}
	if !docker.called("ControlPlaneUp") {
		t.Fatalf("ControlPlaneUp not invoked: %v", docker.methods())
	}
	// The fixed project name is what makes the stack idempotent across reboots.
	c := docker.Calls()[0]
	if c.args[0] != "/var/lib/moose/control-plane" || c.args[1] != controlPlaneProject {
		t.Errorf("ControlPlaneUp args = %v, want [dir %s]", c.args, controlPlaneProject)
	}
}

func TestEnsureControlPlaneErrorPropagates(t *testing.T) {
	docker := newFakeDocker()
	docker.controlPlaneUpErr = errors.New("compose boom")
	m := &Manager{docker: docker}

	if err := m.EnsureControlPlane(context.Background(), "/var/lib/moose/control-plane"); err == nil {
		t.Fatal("want error when the control-plane compose up fails")
	}
}

// --- ControlPlaneUIImage: reading the staged compose (#374).

// writeCompose stages a control-plane compose in a temp dir the way the image
// build does, and returns the dir.
func writeCompose(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, controlPlaneComposeFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	return dir
}

// TestControlPlaneUIImage_EmptyDir: dev runs the UI under Vite with no compose
// at all. That is not a failure, so it reads as "unknown" with no error — the
// version endpoint then omits the field instead of 500-ing every dev request.
func TestControlPlaneUIImage_EmptyDir(t *testing.T) {
	img, err := ControlPlaneUIImage("")
	if err != nil {
		t.Fatalf("empty dir: want no error, got %v", err)
	}
	if img != "" {
		t.Fatalf("empty dir: want empty image, got %q", img)
	}
}

// TestControlPlaneUIImage_ReadsPinnedImage: the happy path against the shape the
// real staged compose uses — several services, the UI one among them.
func TestControlPlaneUIImage_ReadsPinnedImage(t *testing.T) {
	dir := writeCompose(t, `
services:
  caddy:
    image: caddy:2-alpine
  moose-ui:
    image: ghcr.io/onmoose/ui:v0.6.0
    read_only: true
networks:
  moose:
    external: true
`)
	img, err := ControlPlaneUIImage(dir)
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	if img != "ghcr.io/onmoose/ui:v0.6.0" {
		t.Fatalf("got %q", img)
	}
}

// TestControlPlaneUIImage_RealStagedCompose guards against this reader drifting
// from the compose the image actually ships. It parses the committed source
// file (dev/control-plane/compose.yml — stage-control-plane.sh copies this
// verbatim into the gitignored dev/cloud/mkosi.extra.wiring/ tree that a real
// box gets, so this is the same content, just at the path git actually tracks)
// rather than a hand-written fixture, so renaming the service or switching it
// to an interpolated ref fails here in CI instead of on a box. The previous
// version of this test pointed at the gitignored staged copy, which does not
// exist in a fresh checkout, so it always skipped in CI — the "fails in CI"
// claim was untested.
func TestControlPlaneUIImage_RealStagedCompose(t *testing.T) {
	const staged = "../../dev/control-plane"
	// Fatal, not Skip: this file is tracked by git, so it is always present in
	// any checkout. Absent means it was deleted or moved, which is precisely the
	// drift this test exists to catch — skipping there would repeat the bug that
	// made the earlier version of this test a no-op.
	if _, err := os.Stat(filepath.Join(staged, controlPlaneComposeFile)); err != nil {
		t.Fatalf("committed control-plane compose missing (moved or deleted?): %v", err)
	}
	img, err := ControlPlaneUIImage(staged)
	if err != nil {
		t.Fatalf("real staged compose: %v", err)
	}
	// Asserting the exact tag would break on every legitimate image bump. What
	// must hold is that a UI image is found and named.
	if img == "" || !strings.Contains(img, "ui") {
		t.Fatalf("real staged compose: got %q, want a moose-ui image ref", img)
	}
}

// TestControlPlaneUIImage_MissingFile: dir set but nothing staged is a real
// misconfiguration, not the dev case, so it must surface.
func TestControlPlaneUIImage_MissingFile(t *testing.T) {
	if _, err := ControlPlaneUIImage(t.TempDir()); err == nil {
		t.Fatal("missing compose: want error, got nil")
	}
}

// TestControlPlaneUIImage_Rejects covers the ways a present file can still fail
// to answer the question. Each must error rather than return a plausible-looking
// empty or unresolved string, since the caller reports this as fact.
func TestControlPlaneUIImage_Rejects(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no ui service", "services:\n  caddy:\n    image: caddy:2-alpine\n"},
		{"no image key", "services:\n  moose-ui:\n    read_only: true\n"},
		{"blank image", "services:\n  moose-ui:\n    image: \"   \"\n"},
		{"interpolated image", "services:\n  moose-ui:\n    image: ${MOOSE_UI_IMAGE:-moose-ui:dev}\n"},
		{"not yaml", "services: [this: is: not: valid\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, err := ControlPlaneUIImage(writeCompose(t, tc.body))
			if err == nil {
				t.Fatalf("want error, got image %q", img)
			}
			if img != "" {
				t.Fatalf("on error want empty image, got %q", img)
			}
		})
	}
}
