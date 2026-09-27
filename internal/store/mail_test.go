package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/onmoose/os/internal/mailpreset"
)

// mailOwner is the user every sampleProvider belongs to. openWithOwner
// creates it, since an account's owner must be a real user row.
const mailOwner = "u_owner"

func addUser(t *testing.T, s *Store, id, role string, created int64) {
	t.Helper()
	if err := s.CreateUser(User{ID: id, Username: id, DisplayName: id, Role: role, CreatedAt: time.Unix(created, 0)}); err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

func openWithOwner(t *testing.T) *Store {
	t.Helper()
	s := open(t)
	addUser(t, s, mailOwner, RoleAdmin, 1_600_000_000)
	return s
}

func sampleProvider(id, label string) MailProvider {
	return MailProvider{
		ID: id, OwnerUserID: mailOwner, Label: label, Host: "smtp.example.com", Port: 587,
		Username: "moose@example.com", Password: "hunter2",
		FromAddress: "moose@example.com", Encryption: MailEncryptionSTARTTLS,
		ProviderType: mailpreset.Custom,
		CreatedAt:    time.Unix(1_700_000_000, 0),
	}
}

func TestMailProviderCRUD(t *testing.T) {
	s := openWithOwner(t)
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
	list, err := s.ListMailProviders(mailOwner)
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

	if err := s.DeleteMailProvider("mp_1", mailOwner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMailProvider("mp_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: got %v, want ErrNotFound", err)
	}
	if err := s.DeleteMailProvider("mp_1", mailOwner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: got %v, want ErrNotFound", err)
	}
}

func TestMailProviderRejectsBadEncryption(t *testing.T) {
	s := openWithOwner(t)
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
	s := openWithOwner(t)
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
	if err := s.DeleteMailProvider("mp_2", mailOwner); err != nil {
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
	s := openWithOwner(t)

	// Simulate a pre-preset row: written without the provider_type column,
	// so the column default applies.
	if _, err := s.db.Exec(
		`INSERT INTO mail_providers (id, owner_user_id, label, host, port, username, password, from_address, encryption, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"mp_old", mailOwner, "Old", "smtp.example.com", 587, "u", "p", "u@example.com", MailEncryptionSTARTTLS,
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
	s := openWithOwner(t)
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

// Labels are unique per owner, not per box: two people can each have a
// "Gmail". The same owner still cannot have two, on create or on rename.
// Update and delete only touch the owner's own row.
func TestMailProviderLabelUniquePerOwner(t *testing.T) {
	s := openWithOwner(t)
	addUser(t, s, "u_other", RoleMember, 1_600_000_100)

	if err := s.CreateMailProvider(sampleProvider("mp_1", "Gmail")); err != nil {
		t.Fatalf("create: %v", err)
	}
	theirs := sampleProvider("mp_2", "Gmail")
	theirs.OwnerUserID = "u_other"
	if err := s.CreateMailProvider(theirs); err != nil {
		t.Fatalf("same label, other owner: %v", err)
	}
	if err := s.CreateMailProvider(sampleProvider("mp_3", "Gmail")); !errors.Is(err, ErrConflict) {
		t.Fatalf("same label, same owner: got %v, want ErrConflict", err)
	}

	mine, _ := s.ListMailProviders(mailOwner)
	other, _ := s.ListMailProviders("u_other")
	if len(mine) != 1 || mine[0].ID != "mp_1" || len(other) != 1 || other[0].ID != "mp_2" {
		t.Fatalf("lists not scoped to owner: mine=%+v other=%+v", mine, other)
	}

	// An update or delete that names the wrong owner finds nothing.
	stolen := theirs
	stolen.OwnerUserID = mailOwner
	stolen.Host = "evil.example.com"
	if err := s.UpdateMailProvider(stolen); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update other owner's row: got %v, want ErrNotFound", err)
	}
	if err := s.DeleteMailProvider("mp_2", mailOwner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete other owner's row: got %v, want ErrNotFound", err)
	}
	if got, _ := s.GetMailProvider("mp_2"); got.Host != "smtp.example.com" {
		t.Fatalf("other owner's row changed: %+v", got)
	}

	// A provider needs an owner.
	orphan := sampleProvider("mp_4", "Orphan")
	orphan.OwnerUserID = ""
	if err := s.CreateMailProvider(orphan); err == nil {
		t.Fatal("create without an owner: want error")
	}
}

// Deleting a user deletes their accounts, and the apps bound to them fall back
// to unbound. The apps themselves survive.
func TestDeleteUserCascadesMailProviders(t *testing.T) {
	s := openWithOwner(t)
	addUser(t, s, "u_other", RoleMember, 1_600_000_100)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	theirs := sampleProvider("mp_2", "Theirs")
	theirs.OwnerUserID = "u_other"
	if err := s.CreateMailProvider(theirs); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateMailProvider(sampleProvider("mp_1", "Mine")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetInstanceMailBinding("a", "mp_2"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	binds, err := s.ListMailBindingsForOwner("u_other")
	if err != nil || len(binds) != 1 || binds[0] != (MailBinding{InstanceID: "a", ProviderID: "mp_2"}) {
		t.Fatalf("bindings for owner = %+v, %v", binds, err)
	}

	if err := s.DeleteUser("u_other"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.GetMailProvider("mp_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user's account survived: %v", err)
	}
	if _, err := s.GetInstanceMailProvider("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("app still bound to a deleted user's account: %v", err)
	}
	if _, err := s.Get("a"); err != nil {
		t.Fatalf("instance must survive: %v", err)
	}
	if _, err := s.GetMailProvider("mp_1"); err != nil {
		t.Fatalf("another user's account went with it: %v", err)
	}
}

// A box from before owners has a box-wide mail_providers table with a
// box-wide UNIQUE label, and possibly no provider_type column either. Opening
// it moves every account to the founding admin, keeps every app binding, and
// leaves labels unique per owner only.
func TestMailProviderOwnerMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Two admins and a member. The member is the oldest user, to prove the
	// backfill picks the oldest ADMIN, not the oldest user.
	addUser(t, s, "u_member", RoleMember, 1_500_000_000)
	addUser(t, s, "u_founder", RoleAdmin, 1_600_000_000)
	addUser(t, s, "u_later", RoleAdmin, 1_700_000_000)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	// Put the pre-owner table back, the way the oldest brains created it.
	for _, stmt := range []string{
		`PRAGMA foreign_keys=OFF`,
		`DROP TABLE mail_providers`,
		`CREATE TABLE mail_providers (
			id           TEXT    PRIMARY KEY,
			label        TEXT    NOT NULL UNIQUE,
			host         TEXT    NOT NULL,
			port         INTEGER NOT NULL,
			username     TEXT    NOT NULL,
			password     TEXT    NOT NULL,
			from_address TEXT    NOT NULL,
			encryption   TEXT    NOT NULL CHECK (encryption IN ('none','starttls','tls')),
			created_at   INTEGER NOT NULL
		)`,
		`INSERT INTO mail_providers VALUES ('mp_1','Fastmail','smtp.fastmail.com',465,'u','p','u@example.com','tls',1700000000)`,
		`INSERT INTO mail_providers VALUES ('mp_2','SES','email-smtp.example.com',587,'u','p','u@example.com','starttls',1700000001)`,
		`INSERT INTO instance_mail_bindings (instance_id, provider_id) VALUES ('a','mp_2')`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("stage legacy schema: %s: %v", stmt, err)
		}
	}
	s.Close()

	// Open twice: the second open must find nothing to do.
	for i := 0; i < 2; i++ {
		s, err = Open(path)
		if err != nil {
			t.Fatalf("open %d after legacy schema: %v", i, err)
		}
		if i == 0 {
			s.Close()
		}
	}
	defer s.Close()

	all, err := s.ListAllMailProviders()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("migration lost rows: %+v", all)
	}
	for _, p := range all {
		if p.OwnerUserID != "u_founder" {
			t.Errorf("%s owner = %q, want the founding admin u_founder", p.ID, p.OwnerUserID)
		}
		if p.ProviderType != mailpreset.Custom {
			t.Errorf("%s provider_type = %q, want custom", p.ID, p.ProviderType)
		}
	}
	if got, _ := s.GetMailProvider("mp_1"); got.Host != "smtp.fastmail.com" || got.Port != 465 || got.Encryption != MailEncryptionTLS || got.Password != "p" {
		t.Errorf("row values changed in the rebuild: %+v", got)
	}
	// The binding survived: the rebuild must not cascade it away.
	if got, err := s.GetInstanceMailProvider("a"); err != nil || got.ID != "mp_2" {
		t.Fatalf("binding after migration = %+v, %v; want mp_2", got, err)
	}
	// The box-wide UNIQUE is gone: another user may reuse a label.
	theirs := sampleProvider("mp_3", "Fastmail")
	theirs.OwnerUserID = "u_later"
	if err := s.CreateMailProvider(theirs); err != nil {
		t.Fatalf("same label for another owner after migration: %v", err)
	}
	dup := sampleProvider("mp_4", "Fastmail")
	dup.OwnerUserID = "u_founder"
	if err := s.CreateMailProvider(dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("same label for the same owner after migration: got %v, want ErrConflict", err)
	}
	// Foreign keys are back on after the rebuild: deleting the owner cascades.
	if err := s.DeleteUser("u_later"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.GetMailProvider("mp_3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign keys off after migration: account outlived its owner (%v)", err)
	}
}

// With no user at all there is nobody to own the rows and nobody who could
// sign in to use them. The migration drops them rather than failing the start.
func TestMailProviderOwnerMigrationWithoutUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE mail_providers`,
		`CREATE TABLE mail_providers (id TEXT PRIMARY KEY, label TEXT NOT NULL UNIQUE, host TEXT NOT NULL, port INTEGER NOT NULL,
			username TEXT NOT NULL, password TEXT NOT NULL, from_address TEXT NOT NULL, encryption TEXT NOT NULL,
			provider_type TEXT NOT NULL DEFAULT 'custom', created_at INTEGER NOT NULL)`,
		`INSERT INTO mail_providers VALUES ('mp_1','Fastmail','h',465,'u','p','u@example.com','tls','custom',1)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("open after legacy schema with no users: %v", err)
	}
	defer s.Close()
	if all, _ := s.ListAllMailProviders(); len(all) != 0 {
		t.Fatalf("ownerless rows kept: %+v", all)
	}
	if has, _ := s.hasColumn("mail_providers", "owner_user_id"); !has {
		t.Fatal("table not rebuilt")
	}
}
