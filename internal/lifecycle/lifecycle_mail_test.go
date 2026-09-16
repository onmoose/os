package lifecycle

// BYO outgoing mail injection (SERVICE_PROVISIONING.md # BYO outgoing mail):
// an install electing a registered provider binds it and writeEnv re-emits the
// credentials as MOOSE_MAIL_*; an unbound install injects nothing.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

const mailManifest = `
id: mailapp
manifest_version: 1
name: Mail App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [mailapp]
permissions:
  internet: true
  lan: false
mail:
  optional: true
`

const mailCompose = `
services:
  app:
    image: traefik/whoami:v1.10.3
`

func testProvider() store.MailProvider {
	return store.MailProvider{
		ID: "mp_test", Label: "Fastmail", Host: "smtp.fastmail.com", Port: 465,
		Username: "box@example.com", Password: "p@ss:word/2",
		FromAddress: "box@example.com", Encryption: store.MailEncryptionTLS,
		CreatedAt: time.Unix(1_700_000_000, 0),
	}
}

func installMailApp(t *testing.T, e *testEnv, providerID string) (store.Instance, string) {
	t.Helper()
	e.writeCatalogApp(t, "mailapp", mailCompose, mailManifest)
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "mailapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, providerID, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	env, err := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	return inst, string(env)
}

func TestInstallBoundInjectsMailVars(t *testing.T) {
	e := newTestEnv(t)
	if err := e.store.CreateMailProvider(testProvider()); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	inst, env := installMailApp(t, e, "mp_test")

	want := map[string]string{
		"MOOSE_MAIL_HOST":       "smtp.fastmail.com",
		"MOOSE_MAIL_PORT":       "465",
		"MOOSE_MAIL_USER":       "box@example.com",
		"MOOSE_MAIL_PASSWORD":   "p@ss:word/2",
		"MOOSE_MAIL_FROM":       "box@example.com",
		"MOOSE_MAIL_ENCRYPTION": "tls",
		// tls ⇒ implicit-SSL flag set, STARTTLS flag clear.
		"MOOSE_MAIL_USE_TLS": "false",
		"MOOSE_MAIL_USE_SSL": "true",
		// tls ⇒ smtps scheme; credentials URL-escaped so @ : / survive.
		"MOOSE_MAIL_DSN": "smtps://box%40example.com:p%40ss%3Aword%2F2@smtp.fastmail.com:465",
	}
	for k, v := range want {
		if got := envValue(env, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// The binding is persisted, so later .env rewrites stay stable.
	if mp, err := e.store.GetInstanceMailProvider(inst.ID); err != nil || mp.ID != "mp_test" {
		t.Fatalf("binding not persisted: %v %v", mp, err)
	}
}

func TestInstallUnboundInjectsNothing(t *testing.T) {
	e := newTestEnv(t)
	_, env := installMailApp(t, e, "")
	if strings.Contains(env, "MOOSE_MAIL_") {
		t.Fatalf("unbound install must inject no MOOSE_MAIL_ vars, got:\n%s", env)
	}
}

func TestInstallMailElectionOnNonMailApp(t *testing.T) {
	e := newTestEnv(t)
	if err := e.store.CreateMailProvider(testProvider()); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	e.writeCatalogApp(t, "whoami", mailCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "mp_test", nil, nil)
	if err == nil {
		t.Fatal("want error binding a provider to an app without a mail block")
	}
	// Rejected before any state was written — no instance row left behind.
	if list, _ := e.store.List(); len(list) != 0 {
		t.Fatalf("rejection must write no state, got instances: %v", list)
	}
}

func TestInstallMissingProviderRollsBack(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "mailapp", mailCompose, mailManifest)
	e.docker.digests[testImage] = testDigest
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "mailapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "mp_ghost", nil, nil)
	if err == nil {
		t.Fatal("want error binding a nonexistent provider")
	}
	if list, _ := e.store.List(); len(list) != 0 {
		t.Fatalf("failed install must roll back the instance row, got: %v", list)
	}
}

func TestRebindMail(t *testing.T) {
	e := newTestEnv(t)
	if err := e.store.CreateMailProvider(testProvider()); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	second := testProvider()
	second.ID, second.Label, second.Host = "mp_two", "SES", "email-smtp.example.com"
	if err := e.store.CreateMailProvider(second); err != nil {
		t.Fatalf("create provider 2: %v", err)
	}
	inst, _ := installMailApp(t, e, "mp_test")
	envPath := filepath.Join(e.stateDir, "instances", inst.ID, ".env")

	// Rebind a running app: binding + .env updated, containers recreated.
	upsBefore := countCalls(e.docker.Calls(), "ComposeUp")
	if err := e.m.RebindMail(context.Background(), inst.ID, "mp_two"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	raw, _ := os.ReadFile(envPath)
	if got := envValue(string(raw), "MOOSE_MAIL_HOST"); got != "email-smtp.example.com" {
		t.Fatalf("MOOSE_MAIL_HOST = %q, want email-smtp.example.com", got)
	}
	// Non-mail lines survive the surgical rewrite.
	if got := envValue(string(raw), "MOOSE_INSTANCE_ID"); got != inst.ID {
		t.Fatalf("MOOSE_INSTANCE_ID lost in rewrite, got %q", got)
	}
	if ups := countCalls(e.docker.Calls(), "ComposeUp"); ups != upsBefore+1 {
		t.Fatalf("running rebind must recreate containers: ComposeUp %d -> %d", upsBefore, ups)
	}

	// Rebind to None strips every MOOSE_MAIL_* line.
	if err := e.m.RebindMail(context.Background(), inst.ID, ""); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	raw, _ = os.ReadFile(envPath)
	if strings.Contains(string(raw), "MOOSE_MAIL_") {
		t.Fatalf("unbind must strip MOOSE_MAIL_ vars, got:\n%s", raw)
	}

	// A non-mail app rejects a binding.
	e.writeCatalogApp(t, "whoami", mailCompose, whoamiManifest(testDigest))
	plain, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install plain app: %v", err)
	}
	if err := e.m.RebindMail(context.Background(), plain.ID, "mp_test"); err == nil {
		t.Fatal("want error rebinding an app without a mail block")
	}
}

func countCalls(calls []call, method string) int {
	n := 0
	for _, c := range calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func TestMailDSN(t *testing.T) {
	p := testProvider()

	p.Encryption = store.MailEncryptionSTARTTLS
	p.Port = 587
	if got, want := mailDSN(p), "smtp://box%40example.com:p%40ss%3Aword%2F2@smtp.fastmail.com:587"; got != want {
		t.Errorf("starttls: got %q, want %q", got, want)
	}

	p.Encryption = store.MailEncryptionNone
	p.Username, p.Password = "", ""
	if got, want := mailDSN(p), "smtp://smtp.fastmail.com:587"; got != want {
		t.Errorf("no credentials: got %q, want %q", got, want)
	}

	// A bare IPv6 host must be bracketed so the DSN stays a parseable URL
	// (::1:587 would otherwise read as host ":" with port "1:587").
	p.Host = "::1"
	if got, want := mailDSN(p), "smtp://[::1]:587"; got != want {
		t.Errorf("ipv6 host: got %q, want %q", got, want)
	}
}
