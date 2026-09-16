package lifecycle

// The shape of the digest pin written into compose.override.yml
// (APP_LIFECYCLE.md # image digest pinning: every image pins as
// `name@sha256:…`). The offline exception to that shape lives in
// pinning_offline_test.go.

import (
	"context"
	"testing"

	"github.com/onmoose/os/internal/store"
)

// An author who writes BOTH a tag and a digest (`name:tag@sha256:…` — a
// hand-authored Door-2 compose; catalog-built manifests don't emit this shape)
// still gets the canonical tag-free pin. The digest is the address, and the tag
// is a label we don't restate in the override.
func TestInstallTagAndDigestPinsCanonicalRef(t *testing.T) {
	e := newTestEnv(t)
	const compose = `
services:
  whoami:
    image: traefik/whoami:v1.10.3@` + testDigest + `
`
	e.writeCatalogApp(t, "whoami", compose, whoamiManifest("")) // no promise: the compose's own digest is the trust anchor

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopePersonal, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got, want := overridePin(t, e.stateDir, inst.ID, "whoami"), "traefik/whoami@"+testDigest; got != want {
		t.Fatalf("override pin = %q, want %q (canonical: no tag)", got, want)
	}
	imgs, err := e.store.GetInstanceImages(inst.ID)
	if err != nil {
		t.Fatalf("InstanceImages: %v", err)
	}
	if len(imgs) != 1 || imgs[0].Digest != testDigest {
		t.Fatalf("stored images = %+v, want one with digest %s", imgs, testDigest)
	}
}
