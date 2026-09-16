package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/onmoose/moose/internal/manifest"
)

// fakeSizer stands in for the Docker daemon: canned refs by image, or a forced
// error to exercise the fail-loud path.
type fakeSizer struct {
	refs map[string]manifest.ImageRef
	err  error
}

func (f fakeSizer) Size(_ context.Context, image string) (manifest.ImageRef, error) {
	if f.err != nil {
		return manifest.ImageRef{}, f.err
	}
	ref, ok := f.refs[image]
	if !ok {
		return manifest.ImageRef{}, fmt.Errorf("fake: unknown image %s", image)
	}
	return ref, nil
}

// manifestWithImages carries a legacy scalar images block under an explanatory
// comment — resolve must replace the block while keeping the comment.
const manifestWithImages = validManifest + `
# keep me: explains the images
images:
  nginx:1.0: sha256:old
`

func TestResolve_AppendsObjectFormAndFootprint(t *testing.T) {
	mp := writeApp(t, validManifest+"storage:\n  estimated_size: 5GB\n", validCompose)
	sizer := fakeSizer{refs: map[string]manifest.ImageRef{
		"nginx:1.0": {Digest: "sha256:zzz", DownloadBytes: 100, DiskBytes: 250},
	}}
	if err := resolve(context.Background(), sizer, mp); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(out)
	if err != nil {
		t.Fatalf("regenerated manifest does not parse: %v\n%s", err, out)
	}
	ref := m.Images["nginx:1.0"]
	if ref.Digest != "sha256:zzz" || ref.DownloadBytes != 100 || ref.DiskBytes != 250 {
		t.Fatalf("image not written in object form: %+v", ref)
	}
	f := m.Footprint()
	if f.ImageDownloadBytes != 100 || f.ImageDiskBytes != 250 || f.EstimatedState != "5GB" {
		t.Fatalf("footprint wrong: %+v", f)
	}
}

func TestResolve_ReplacesBlockKeepingComment(t *testing.T) {
	mp := writeApp(t, manifestWithImages, validCompose)
	sizer := fakeSizer{refs: map[string]manifest.ImageRef{
		"nginx:1.0": {Digest: "sha256:new", DownloadBytes: 7, DiskBytes: 9},
	}}
	if err := resolve(context.Background(), sizer, mp); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(mp)
	s := string(out)
	if !strings.Contains(s, "# keep me: explains the images") {
		t.Fatalf("comment above images block was lost:\n%s", s)
	}
	if !strings.Contains(s, "digest: sha256:new") {
		t.Fatalf("new digest not written:\n%s", s)
	}
	if strings.Contains(s, "sha256:old") {
		t.Fatalf("old digest survived:\n%s", s)
	}
}

func TestResolve_FailsLoudLeavingFileUntouched(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizer fakeSizer
	}{
		{"sizer error", fakeSizer{err: errors.New("no network")}},
		{"empty digest", fakeSizer{refs: map[string]manifest.ImageRef{"nginx:1.0": {DownloadBytes: 1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mp := writeApp(t, manifestWithImages, validCompose)
			before, _ := os.ReadFile(mp)
			if err := resolve(context.Background(), tc.sizer, mp); err == nil {
				t.Fatal("want a loud error, got nil")
			}
			after, _ := os.ReadFile(mp)
			if string(before) != string(after) {
				t.Fatalf("file was modified on failure:\n%s", after)
			}
		})
	}
}

// --- replaceImagesBlock: pure write-back, no docker -----------------------

func TestReplaceImagesBlock_PreservesTrailingNewline(t *testing.T) {
	src := "id: x\nimages:\n  a:1: sha256:old\n"
	got := replaceImagesBlock(src, map[string]manifest.ImageRef{"a:1": {Digest: "sha256:n"}})
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Fatalf("want exactly one trailing newline, got %q", got)
	}
}

func TestReplaceImagesBlock_AppendsWhenAbsent(t *testing.T) {
	src := "id: x\nname: X\n"
	got := replaceImagesBlock(src, map[string]manifest.ImageRef{"a:1": {Digest: "sha256:n", DownloadBytes: 2, DiskBytes: 3}})
	if !strings.Contains(got, "id: x") || !strings.Contains(got, "name: X") {
		t.Fatalf("existing keys dropped: %q", got)
	}
	if !strings.Contains(got, "images:") || !strings.Contains(got, "disk_bytes: 3") {
		t.Fatalf("images block not appended: %q", got)
	}
}

func TestReplaceImagesBlock_Idempotent(t *testing.T) {
	src := "id: x\nimages:\n  a:1: sha256:old\n"
	refs := map[string]manifest.ImageRef{"a:1": {Digest: "sha256:n", DownloadBytes: 2, DiskBytes: 3}}
	once := replaceImagesBlock(src, refs)
	twice := replaceImagesBlock(once, refs)
	if once != twice {
		t.Fatalf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// renderImagesBlock sorts images for a stable diff.
func TestRenderImagesBlock_Sorted(t *testing.T) {
	block := renderImagesBlock(map[string]manifest.ImageRef{
		"z/img:1": {Digest: "sha256:z"},
		"a/img:1": {Digest: "sha256:a"},
	})
	if strings.Index(block, "a/img:1") > strings.Index(block, "z/img:1") {
		t.Fatalf("images not sorted:\n%s", block)
	}
}

func TestRun_ResolveUsage(t *testing.T) {
	good := writeApp(t, validManifest, validCompose)
	cases := []struct {
		name string
		args []string
	}{
		{"resolve without path", []string{"manifest", "resolve"}},
		{"unknown manifest subcommand", []string{"manifest", "frobnicate", good}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.args); !errors.Is(err, errUsage) {
				t.Fatalf("args %v: want errUsage, got %v", tc.args, err)
			}
		})
	}
}

func TestRepoOf(t *testing.T) {
	cases := map[string]string{
		"traefik/whoami:v1.10.3":         "traefik/whoami",
		"nginx":                          "nginx",
		"registry:5000/app:tag":          "registry:5000/app",
		"repo@sha256:abc":                "repo",
		"ghcr.io/owner/app:1.2@sha256:d": "ghcr.io/owner/app:1.2",
	}
	for in, want := range cases {
		if got := repoOf(in); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", in, got, want)
		}
	}
}
