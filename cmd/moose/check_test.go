package main

import (
	"context"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/admission"
)

// check is exercised with admission.CheckStructure (the daemon-free admission
// path) so these stay hermetic — same reason admission's own tests use it.

// --- check passes on representative catalog samples (testdata/) -----------

func TestCheck_RealSamples(t *testing.T) {
	for _, p := range []string{
		"testdata/whoami/manifest.yml",
		"testdata/files-demo/manifest.yml",
	} {
		if _, err := check(context.Background(), admission.CheckStructure, p, lintOptions{}); err != nil {
			t.Errorf("check(%s): want clean, got %v", p, err)
		}
	}
}

// --- check runs admission, not just schema -------------------------------

func TestCheck_RejectsOnAdmission(t *testing.T) {
	cases := []struct {
		name    string
		compose string
		wantMsg string // substring the admission error must contain
	}{
		{
			name: "host ports",
			compose: `services:
  web:
    image: nginx:1.0
    ports:
      - "8080:80"
`,
			wantMsg: "host ports",
		},
		{
			name: "named volume",
			compose: `services:
  web:
    image: nginx:1.0
    volumes:
      - db_data:/var/lib/data
`,
			wantMsg: "named volume",
		},
		{
			name: "privileged",
			compose: `services:
  web:
    image: nginx:1.0
    privileged: true
`,
			wantMsg: "privileged",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// validManifest/validCompose live in main_test.go; here the manifest is
			// schema-valid so the failure must come from admission, not lint.
			mp := writeApp(t, validManifest, tc.compose)
			_, err := check(context.Background(), admission.CheckStructure, mp, lintOptions{})
			if err == nil {
				t.Fatalf("want an admission error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not name the admission problem (want substring %q)", err, tc.wantMsg)
			}
		})
	}
}

// --- check runs lint first: a schema error fails before admission ---------

func TestCheck_RejectsOnSchema(t *testing.T) {
	bad := strings.Replace(validManifest, "id: test-app", "id: Test_App", 1)
	_, err := check(context.Background(), admission.CheckStructure, writeApp(t, bad, validCompose), lintOptions{})
	if err == nil || !strings.Contains(err.Error(), "kebab-case") {
		t.Fatalf("bad slug: want a schema error naming kebab-case, got %v", err)
	}
}

// --- check rejects an unresolved/placeholder images: entry -----------------
// Regression: an author-written placeholder digest with zero sizes (e.g.
// copy-pasted before running `moose manifest resolve`) is a syntactically
// valid ImageRef and would otherwise sail through both lint and admission
// undetected (nextcloud import, store #33).

func TestCheck_RejectsUnresolvedImages(t *testing.T) {
	withImages := validManifest + `
images:
  nginx:1.0:
    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
    download_bytes: 0
    disk_bytes: 0
`
	_, err := check(context.Background(), admission.CheckStructure, writeApp(t, withImages, validCompose), lintOptions{})
	if err == nil || !strings.Contains(err.Error(), "not resolved") {
		t.Fatalf("want an unresolved-images error, got %v", err)
	}
}

func TestCheck_RejectsMalformedDigest(t *testing.T) {
	withImages := validManifest + `
images:
  nginx:1.0:
    digest: sha256:not-a-real-digest
    download_bytes: 100
    disk_bytes: 200
`
	_, err := check(context.Background(), admission.CheckStructure, writeApp(t, withImages, validCompose), lintOptions{})
	if err == nil || !strings.Contains(err.Error(), "well-formed sha256 digest") {
		t.Fatalf("want a malformed-digest error, got %v", err)
	}
}

func TestCheck_AcceptsResolvedImages(t *testing.T) {
	withImages := validManifest + `
images:
  nginx:1.0:
    digest: sha256:43a68d10b9dfcfc3ffbfe4dd42100dc9aeaf29b3a5636c856337a5940f1b4f1c
    download_bytes: 2850040
    disk_bytes: 6581646
`
	if _, err := check(context.Background(), admission.CheckStructure, writeApp(t, withImages, validCompose), lintOptions{}); err != nil {
		t.Fatalf("want clean, got %v", err)
	}
}
