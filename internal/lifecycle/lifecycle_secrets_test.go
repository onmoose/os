package lifecycle

// Per-app secret generation + injection (SERVICE_PROVISIONING.md # Env-var
// injection): the brain generates a CSPRNG value for each declared secret,
// persists it, and re-emits it as MOOSE_SECRET_<NAME> in the instance .env.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

const secretsManifest = `
id: secretapp
manifest_version: 1
name: Secret App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [secretapp]
permissions:
  internet: false
  lan: false
secrets:
  - name: auth
`

const secretsCompose = `
services:
  app:
    image: traefik/whoami:v1.10.3
`

// installSecretApp installs secretapp and returns the instance plus its .env.
func installSecretApp(t *testing.T, e *testEnv) (store.Instance, string) {
	t.Helper()
	e.writeCatalogApp(t, "secretapp", secretsCompose, secretsManifest)
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "secretapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	env, err := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	return inst, string(env)
}

func TestInstallInjectsGeneratedSecret(t *testing.T) {
	e := newTestEnv(t)
	inst, env := installSecretApp(t, e)

	// The .env carries MOOSE_SECRET_AUTH with a non-empty generated value.
	val := envValue(env, "MOOSE_SECRET_AUTH")
	if val == "" {
		t.Fatalf("env missing non-empty MOOSE_SECRET_AUTH, got:\n%s", env)
	}
	// It matches what was persisted (so re-emit is store-backed, not re-rolled).
	secrets, err := e.store.GetInstanceSecrets(inst.ID)
	if err != nil {
		t.Fatalf("get secrets: %v", err)
	}
	if len(secrets) != 1 || secrets[0].Name != "auth" || secrets[0].Value != val {
		t.Fatalf("persisted secrets %v don't match env value %q", secrets, val)
	}
}

func TestSecretsAreUniquePerInstance(t *testing.T) {
	// Separate envs: instance IDs are second-resolution timestamps, so two
	// installs in one env within the same second would collide on the PK.
	_, env1 := installSecretApp(t, newTestEnv(t))
	_, env2 := installSecretApp(t, newTestEnv(t))
	if v1, v2 := envValue(env1, "MOOSE_SECRET_AUTH"), envValue(env2, "MOOSE_SECRET_AUTH"); v1 == v2 {
		t.Fatalf("two installs reused the same secret %q", v1)
	}
}

func TestGenerateSecretsEntropy(t *testing.T) {
	decls := []manifest.Secret{{Name: "auth", Bytes: 32}, {Name: "k", Bytes: 16}}
	got, err := generateSecrets(decls)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 secrets, got %d", len(got))
	}
	// base64url(no pad) of 32 bytes is 43 chars, of 16 bytes is 22.
	if l := len(got[0].Value); l != 43 {
		t.Errorf("32-byte secret encoded to %d chars, want 43", l)
	}
	if l := len(got[1].Value); l != 22 {
		t.Errorf("16-byte secret encoded to %d chars, want 22", l)
	}
}

// revealSecretsManifest declares one owner-visible secret (`show: true`) and one
// internal one, so RevealSecrets must return only the former (#152).
const revealSecretsManifest = `
id: revealapp
manifest_version: 1
name: Reveal App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [revealapp]
permissions:
  internet: false
  lan: false
secrets:
  - name: setup_token
    show: true
  - name: db
`

func TestRevealSecretsReturnsOnlyOwnerVisible(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "revealapp", secretsCompose, revealSecretsManifest)
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "revealapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	revealed, err := e.m.RevealSecrets(inst.ID)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if len(revealed) != 1 || revealed[0].Name != "setup_token" {
		t.Fatalf("want only setup_token revealed, got %v", revealed)
	}
	// The revealed value is the stored/injected one, never re-rolled.
	want := ""
	all, _ := e.store.GetInstanceSecrets(inst.ID)
	for _, s := range all {
		if s.Name == "setup_token" {
			want = s.Value
		}
	}
	if revealed[0].Value == "" || revealed[0].Value != want {
		t.Fatalf("revealed value %q != stored %q", revealed[0].Value, want)
	}
}

// TestRevealSecretsEmptyWhenNoneVisible: an app whose secrets are all internal
// (no `show`) reveals nothing — the early return before touching the store.
func TestRevealSecretsEmptyWhenNoneVisible(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installSecretApp(t, e) // `auth`, no show flag
	revealed, err := e.m.RevealSecrets(inst.ID)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if len(revealed) != 0 {
		t.Fatalf("want nothing revealable, got %v", revealed)
	}
}

// TestRevealSecretsUnknownInstance: no instance dir ⇒ the manifest load fails,
// surfaced as an error (the API maps it to 500).
func TestRevealSecretsUnknownInstance(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.m.RevealSecrets("inst_missing"); err == nil {
		t.Fatal("want error for unknown instance, got nil")
	}
}

// envValue pulls the value of KEY=... from a .env blob, or "" if absent.
func envValue(env, key string) string {
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	return ""
}
