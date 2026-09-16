package store

import (
	"errors"
	"testing"
	"time"

	"github.com/onmoose/os/internal/mailpreset"
)

func sampleProvider(id, label string) MailProvider {
	return MailProvider{
		ID: id, Label: label, Host: "smtp.example.com", Port: 587,
		Username: "moose@example.com", Password: "hunter2",
		FromAddress: "moose@example.com", Encryption: MailEncryptionSTARTTLS,
		ProviderType: mailpreset.Custom,
		CreatedAt:    time.Unix(1_700_000_000, 0),
	}
}

func TestMailProviderCRUD(t *testing.T) {
	s := open(t)
	p := sampleProvider("mp_1", "Fastmail")
	if err := s.CreateMailProvider(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetMailProvider("mp_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != p {
		t.Fatalf("roundtrip: got %+v, want %+v", got, p)
	}

	// Duplicate id and duplicate label both conflict.
	if err := s.CreateMailProvider(sampleProvider("mp_1", "Other")); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup id: got %v, want ErrConflict", err)
	}
	if err := s.CreateMailProvider(sampleProvider("mp_2", "Fastmail")); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup label: got %v, want ErrConflict", err)
	}

	// List is ordered by label.
	if err := s.CreateMailProvider(sampleProvider("mp_2", "Amazon SES")); err != nil {
		t.Fatalf("create second: %v", err)
	}
	list, err := s.ListMailProviders()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Label != "Amazon SES" || list[1].Label != "Fastmail" {
		t.Fatalf("list order: got %+v", list)
	}

	// Update mutates in place; updating to a taken label conflicts.
	p.Host, p.Port, p.Encryption = "mail.example.org", 465, MailEncryptionTLS
	if err := s.UpdateMailProvider(p); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ := s.GetMailProvider("mp_1"); got.Host != "mail.example.org" || got.Port != 465 || got.Encryption != MailEncryptionTLS {
		t.Fatalf("update roundtrip: got %+v", got)
	}
	taken := p
	taken.Label = "Amazon SES"
	if err := s.UpdateMailProvider(taken); !errors.Is(err, ErrConflict) {
		t.Fatalf("update to taken label: got %v, want ErrConflict", err)
	}
	missing := sampleProvider("mp_missing", "Ghost")
	if err := s.UpdateMailProvider(missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: got %v, want ErrNotFound", err)
	}

	if err := s.DeleteMailProvider("mp_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMailProvider("mp_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: got %v, want ErrNotFound", err)
	}
	if err := s.DeleteMailProvider("mp_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: got %v, want ErrNotFound", err)
	}
}

func TestMailProviderRejectsBadEncryption(t *testing.T) {
	s := open(t)
	p := sampleProvider("mp_1", "Fastmail")
	p.Encryption = "ssl"
	if err := s.CreateMailProvider(p); err == nil {
		t.Fatal("create with bad encryption: want error")
	}
	good := sampleProvider("mp_1", "Fastmail")
	if err := s.CreateMailProvider(good); err != nil {
		t.Fatalf("create: %v", err)
	}
	good.Encryption = "ssl"
	if err := s.UpdateMailProvider(good); err == nil {
		t.Fatal("update with bad encryption: want error")
	}
}

func TestInstanceMailBinding(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := s.CreateMailProvider(sampleProvider("mp_1", "Fastmail")); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := s.CreateMailProvider(sampleProvider("mp_2", "Amazon SES")); err != nil {
		t.Fatalf("create provider 2: %v", err)
	}

	// Unbound instance resolves to ErrNotFound (writeEnv injects nothing).
	if _, err := s.GetInstanceMailProvider("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound: got %v, want ErrNotFound", err)
	}

	if err := s.SetInstanceMailBinding("a", "mp_1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	got, err := s.GetInstanceMailProvider("a")
	if err != nil {
		t.Fatalf("get bound: %v", err)
	}
	if got.ID != "mp_1" {
		t.Fatalf("bound provider: got %q, want mp_1", got.ID)
	}

	// Rebinding replaces the existing binding.
	if err := s.SetInstanceMailBinding("a", "mp_2"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if got, _ := s.GetInstanceMailProvider("a"); got.ID != "mp_2" {
		t.Fatalf("rebound provider: got %q, want mp_2", got.ID)
	}

	// Deleting the provider unbinds the app; the instance itself survives.
	if err := s.DeleteMailProvider("mp_2"); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if _, err := s.GetInstanceMailProvider("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after provider delete: got %v, want ErrNotFound", err)
	}
	if _, err := s.Get("a"); err != nil {
		t.Fatalf("instance must survive provider delete: %v", err)
	}

	// Unbind is idempotent; deleting the instance cascades the binding away.
	if err := s.SetInstanceMailBinding("a", "mp_1"); err != nil {
		t.Fatalf("bind again: %v", err)
	}
	if err := s.DeleteInstanceMailBinding("a"); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if err := s.DeleteInstanceMailBinding("a"); err != nil {
		t.Fatalf("unbind unbound: %v", err)
	}
	if err := s.SetInstanceMailBinding("a", "mp_1"); err != nil {
		t.Fatalf("bind for cascade: %v", err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatalf("delete instance: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM instance_mail_bindings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("binding must cascade with instance: n=%d err=%v", n, err)
	}
}

// A provider registered before presets shipped has no provider_type. The
// column migrates in with DEFAULT 'custom', and an empty value on a write
// normalizes to the same thing, so an old row keeps working and opens the
// advanced (custom) form on edit.
func TestMailProviderTypeDefaultsToCustom(t *testing.T) {
	s := open(t)

	// Simulate a pre-migration row: written through the raw SQL an older
	// brain used, without the provider_type column.
	if _, err := s.db.Exec(
		`INSERT INTO mail_providers (id, label, host, port, username, password, from_address, encryption, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		"mp_old", "Old", "smtp.example.com", 587, "u", "p", "u@example.com", MailEncryptionSTARTTLS,
		time.Unix(1_700_000_000, 0).Unix()); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	got, err := s.GetMailProvider("mp_old")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProviderType != mailpreset.Custom {
		t.Fatalf("legacy row provider_type = %q, want custom", got.ProviderType)
	}

	// An empty ProviderType on a write normalizes rather than persisting "".
	p := sampleProvider("mp_blank", "Blank")
	p.ProviderType = ""
	if err := s.CreateMailProvider(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := s.GetMailProvider("mp_blank"); got.ProviderType != mailpreset.Custom {
		t.Fatalf("blank provider_type = %q, want custom", got.ProviderType)
	}
}

func TestMailProviderTypeRejectsUnknown(t *testing.T) {
	s := open(t)
	p := sampleProvider("mp_bad", "Bad")
	p.ProviderType = "not-a-provider"
	if err := s.CreateMailProvider(p); err == nil {
		t.Fatal("create accepted an unknown provider_type")
	}

	good := sampleProvider("mp_good", "Good")
	good.ProviderType = "ses"
	if err := s.CreateMailProvider(good); err != nil {
		t.Fatalf("create with a known preset: %v", err)
	}
	good.ProviderType = "not-a-provider"
	if err := s.UpdateMailProvider(good); err == nil {
		t.Fatal("update accepted an unknown provider_type")
	}
}
