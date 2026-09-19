package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- lint against representative catalog samples (both must exit 0) -------
// testdata/whoami (minimal) + testdata/files-demo (a use-case-folder grant) are
// kept here as local fixtures: the shipping catalog moved to the control plane
// (cloud #62), so the box repo no longer bakes a catalog/ tree to point at.

func TestLint_RealSamples(t *testing.T) {
	for _, p := range []string{
		"testdata/whoami/manifest.yml",
		"testdata/files-demo/manifest.yml",
	} {
		if err := lint(p); err != nil {
			t.Errorf("lint(%s): want clean, got %v", p, err)
		}
	}
}

// --- lint rejects malformed manifests with an actionable message ----------

const validManifest = `id: test-app
manifest_version: 1
name: Test App
version: "1.0"
compose_file: compose.yml
main_service: web
main_port: 8080
`

const validCompose = `services:
  web:
    image: nginx:1.0
`

// writeApp lays out a manifest + (optionally) a sibling compose in a fresh temp
// dir and returns the manifest path. An empty compose string skips the compose
// file, so the relative resolution hits a missing file.
func writeApp(t *testing.T, manifestYAML, composeYAML string) string {
	t.Helper()
	dir := t.TempDir()
	mp := filepath.Join(dir, "manifest.yml")
	if err := os.WriteFile(mp, []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if composeYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(composeYAML), 0o644); err != nil {
			t.Fatalf("write compose: %v", err)
		}
	}
	return mp
}

func TestLint_Rejects(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		compose  string
		wantMsg  string // substring the error must contain
	}{
		{
			name:     "missing required field",
			manifest: strings.Replace(validManifest, "name: Test App\n", "", 1),
			compose:  validCompose,
			wantMsg:  "name",
		},
		{
			name:     "bad slug",
			manifest: strings.Replace(validManifest, "id: test-app", "id: Test_App", 1),
			compose:  validCompose,
			wantMsg:  "kebab-case",
		},
		{
			name:     "unsupported manifest_version",
			manifest: strings.Replace(validManifest, "manifest_version: 1", "manifest_version: 2", 1),
			compose:  validCompose,
			wantMsg:  "manifest_version",
		},
		{
			name:     "missing compose file",
			manifest: validManifest,
			compose:  "", // don't write compose.yml
			wantMsg:  "compose_file",
		},
		{
			name:     "compose declares no services",
			manifest: validManifest,
			compose:  "version: \"3\"\n",
			wantMsg:  "no services",
		},
		{
			name:     "main_service absent from compose",
			manifest: strings.Replace(validManifest, "main_service: web", "main_service: api", 1),
			compose:  validCompose,
			wantMsg:  "main_service",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := lint(writeApp(t, tc.manifest, tc.compose))
			if err == nil {
				t.Fatalf("want a lint error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not name the problem (want substring %q)", err, tc.wantMsg)
			}
		})
	}
}

func TestLint_MissingManifestFile(t *testing.T) {
	err := lint(filepath.Join(t.TempDir(), "nope.yml"))
	if err == nil || !strings.Contains(err.Error(), "read manifest") {
		t.Fatalf("missing manifest: want a read error, got %v", err)
	}
}

// --- argument dispatch ----------------------------------------------------

func TestRun_Dispatch(t *testing.T) {
	good := writeApp(t, validManifest, validCompose)
	cases := []struct {
		name      string
		args      []string
		wantUsage bool // expect errUsage; otherwise expect success (nil)
	}{
		{"valid lint", []string{"manifest", "lint", good}, false},
		{"no args", nil, true},
		{"manifest only", []string{"manifest"}, true},
		{"lint without path", []string{"manifest", "lint"}, true},
		{"check without path", []string{"manifest", "check"}, true},
		{"unknown subcommand", []string{"frobnicate"}, true},
		{"extra args", []string{"manifest", "lint", good, "extra"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			switch {
			case tc.wantUsage && !errors.Is(err, errUsage):
				t.Fatalf("args %v: want errUsage, got %v", tc.args, err)
			case !tc.wantUsage && err != nil:
				t.Fatalf("args %v: want success, got %v", tc.args, err)
			}
		})
	}
}
