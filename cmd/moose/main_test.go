package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/admission"
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
		if _, err := lint(p, lintOptions{}); err != nil {
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
			_, err := lint(writeApp(t, tc.manifest, tc.compose), lintOptions{})
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
	_, err := lint(filepath.Join(t.TempDir(), "nope.yml"), lintOptions{})
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

// --- roles and requires: errors, warnings, --ai-providers ----------------

const rolesManifest = validManifest + `config:
  - app_env: ANTHROPIC_API_KEY
    title: Anthropic key
    description: d
    secret: true
    role: ai.anthropic.api_key
  - app_env: GROKK_API_KEY
    title: Grokk key
    description: d
    secret: true
    role: ai.grokk.api_key
  - app_env: CUSTOM_BASE_URL
    title: Custom base URL
    description: d
    role: ai.openai_compatible.base_url
  - app_env: CUSTOM_MODEL
    title: Custom model
    description: d
    role: ai.openai_compatible.model.chat
  - app_env: TOKEN
    title: Token
    description: d
requires:
  - one_of: [ai]
  - one_of: [TOKEN]
`

const providersYAML = `providers:
  - id: anthropic
    name: Anthropic
    native_protocol: anthropic
  - id: other
    name: Other
  - just a string the reader skips
`

func TestLint_RolesWarningsDoNotFail(t *testing.T) {
	mp := writeApp(t, rolesManifest, validCompose)
	warnings, err := lint(mp, lintOptions{})
	if err != nil {
		t.Fatalf("lint: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `requires[1]: the group has only "TOKEN"`) {
		t.Fatalf("warnings = %v; want the single-member warning", warnings)
	}

	pp := filepath.Join(t.TempDir(), "ai_providers.yml")
	if err := os.WriteFile(pp, []byte(providersYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	protocols, err := readNativeProtocols(pp)
	if err != nil {
		t.Fatalf("readNativeProtocols: %v", err)
	}
	if len(protocols) != 1 || !protocols["anthropic"] {
		t.Fatalf("protocols = %v; want only anthropic", protocols)
	}
	warnings, err = lint(mp, lintOptions{nativeProtocols: protocols})
	if err != nil {
		t.Fatalf("lint with providers: %v", err)
	}
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), `no AI provider offers the "grokk" protocol`) {
		t.Fatalf("warnings = %v; want the grokk warning too", warnings)
	}

	// check returns the same warnings.
	warnings, err = check(context.Background(), admission.CheckStructure, mp, lintOptions{nativeProtocols: protocols})
	if err != nil || len(warnings) != 2 {
		t.Fatalf("check = %v, %v; want the two warnings and no error", warnings, err)
	}

	// The flag works in either position.
	for _, args := range [][]string{
		{"manifest", "lint", "--ai-providers", pp, mp},
		{"manifest", "lint", mp, "--ai-providers=" + pp},
	} {
		if err := run(args); err != nil {
			t.Errorf("run(%v) = %v", args, err)
		}
	}
	if err := run([]string{"manifest", "lint", "--ai-providers", filepath.Join(t.TempDir(), "nope.yml"), mp}); err == nil || errors.Is(err, errUsage) {
		t.Errorf("missing providers file: want a read error, got %v", err)
	}

	// The snapshot shape (ai_providers:) reads the same; a file with neither
	// list is an error, not a warning on every native slot.
	sp := filepath.Join(t.TempDir(), "snapshot.yml")
	if err := os.WriteFile(sp, []byte("ai_providers:\n  - {id: x, name: X, native_protocol: grokk}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := readNativeProtocols(sp); err != nil || !p["grokk"] {
		t.Errorf("snapshot shape = %v, %v; want grokk", p, err)
	}
	if err := os.WriteFile(sp, []byte("apps: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeProtocols(sp); err == nil {
		t.Error("a file with no provider list: want an error")
	}
}

func TestLint_RolesErrorsAreAllListed(t *testing.T) {
	bad := strings.Replace(rolesManifest, "role: ai.anthropic.api_key", "role: ai.anthropic.token", 1)
	bad = strings.Replace(bad, "one_of: [ai]", "one_of: [ai, ai.gemini]", 1)
	_, err := lint(writeApp(t, bad, validCompose), lintOptions{})
	if err == nil {
		t.Fatal("want a lint error")
	}
	for _, want := range []string{"2 problem(s)", `config[ANTHROPIC_API_KEY]: role "ai.anthropic.token" has an unknown attribute`, `requires[0]: "ai.gemini" matches no field`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
}

func TestLintArgs(t *testing.T) {
	cases := []struct {
		args       []string
		path, prov string
		ok         bool
	}{
		{[]string{"m.yml"}, "m.yml", "", true},
		{[]string{"--ai-providers", "p.yml", "m.yml"}, "m.yml", "p.yml", true},
		{[]string{"m.yml", "--ai-providers=p.yml"}, "m.yml", "p.yml", true},
		{[]string{"--ai-providers", "p.yml"}, "", "", false},
		{[]string{"m.yml", "--ai-providers"}, "", "", false},
		{[]string{"m.yml", "n.yml"}, "", "", false},
		{[]string{"--nope", "m.yml"}, "", "", false},
		{[]string{"--ai-providers", "a", "--ai-providers", "b", "m.yml"}, "", "", false},
		{[]string{"m.yml", "--ai-providers="}, "", "", false},
		{[]string{"--ai-providers", "", "m.yml"}, "", "", false},
	}
	for _, c := range cases {
		path, prov, ok := lintArgs(c.args)
		if ok != c.ok || (ok && (path != c.path || prov != c.prov)) {
			t.Errorf("lintArgs(%v) = %q, %q, %v; want %q, %q, %v", c.args, path, prov, ok, c.path, c.prov, c.ok)
		}
	}
}
