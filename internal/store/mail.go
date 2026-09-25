package store

// BYO outgoing mail (SERVICE_PROVISIONING.md # BYO outgoing mail). A user
// adds SMTP accounts in Settings or on the install setup page; each account
// belongs to the user who added it. A mail-capable app binds to at most one,
// and the lifecycle injects the bound account as MOOSE_MAIL_* at .env write
// time. No moose-run relay exists: unbound apps get nothing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/onmoose/os/internal/mailpreset"
)

// Mail provider encryption modes. tls is implicit TLS (smtps, usually port
// 465); starttls upgrades a plaintext connection (usually port 587).
const (
	MailEncryptionNone     = "none"
	MailEncryptionSTARTTLS = "starttls"
	MailEncryptionTLS      = "tls"
)

// MailProvider is one outgoing SMTP account. It belongs to OwnerUserID, the
// user who added it, and only that user can see it or bind an app to it.
// Label is the human name shown in pickers, unique per owner. Password is
// plaintext at rest, same trust model as instance_secrets (NEXT.md #
// App-secret injection hardening).
type MailProvider struct {
	ID          string
	OwnerUserID string
	Label       string
	Host        string
	Port        int
	Username    string
	Password    string
	FromAddress string
	Encryption  string // none | starttls | tls
	// ProviderType names the built-in preset the user picked, or "custom"
	// for hand-typed values (internal/mailpreset). It records the choice, not
	// a guarantee the other fields still match the preset: the advanced form
	// lets the user override host, port and encryption.
	ProviderType string
	CreatedAt    time.Time
}

func validMailEncryption(enc string) bool {
	return enc == MailEncryptionNone || enc == MailEncryptionSTARTTLS || enc == MailEncryptionTLS
}

// mailProvidersDDL is the one CREATE TABLE for mail_providers. Both the fresh
// schema and the owner migration's table rebuild use it, so the two cannot
// drift. name is the table name, with "IF NOT EXISTS " in front for the fresh
// path.
//
// The brain holds the credential and injects it into bound apps as
// MOOSE_MAIL_*; there is no moose-run relay. password is plaintext at rest
// (same trust model as instance_secrets; hardening deferred, NEXT.md #
// App-secret injection hardening).
//
// owner_user_id: the user who added the account. Only that user can use it.
// Deleting the user deletes their accounts, and instance_mail_bindings
// cascades from there, so an app bound to one falls back to unbound, the same
// as when the account itself is deleted.
//
// Labels are unique per owner, not per box: two people can each call their
// account "Gmail".
//
// provider_type: which built-in preset the user picked, or 'custom' for
// hand-typed values (internal/mailpreset). No CHECK, for the reason given
// when it first rode an ALTER: validated in Go instead, like scope and
// exposure.
func mailProvidersDDL(name string) string {
	return `CREATE TABLE ` + name + ` (
		id            TEXT    PRIMARY KEY,
		owner_user_id TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		label         TEXT    NOT NULL,
		host          TEXT    NOT NULL,
		port          INTEGER NOT NULL,
		username      TEXT    NOT NULL,
		password      TEXT    NOT NULL,
		from_address  TEXT    NOT NULL,
		encryption    TEXT    NOT NULL CHECK (encryption IN ('none','starttls','tls')),
		provider_type TEXT    NOT NULL DEFAULT 'custom',
		created_at    INTEGER NOT NULL,
		UNIQUE (owner_user_id, label)
	)`
}

// migrateMailProviderOwners moves a box from box-wide email accounts to
// per-user ones. Before, the table had no owner and a box-wide UNIQUE on
// label. SQLite cannot drop a UNIQUE constraint, so the table is rebuilt
// with the documented copy, drop and rename steps
// (https://www.sqlite.org/lang_altertable.html#otheralter).
//
// Every existing account goes to the founding admin (the oldest admin), the
// same rule the instances owner backfill uses. Only an admin could add an
// account before this change, and the last admin cannot be deleted, so an
// admin always exists when rows do. If none does anyway, the oldest user of
// any role gets them. With no user at all nobody could ever sign in to use
// the rows, so they are dropped with a warning rather than failing the start.
//
// Foreign keys are off for the rebuild. With them on, dropping the old table
// would cascade instance_mail_bindings away and unbind every app. The
// bindings keep pointing at the name mail_providers, which the rebuilt table
// takes over. foreign_keys cannot change inside a transaction, so the pragma
// is set on one pinned connection around it.
//
// Idempotent: a table that already has owner_user_id is left alone.
func (s *Store) migrateMailProviderOwners() error {
	has, err := s.hasColumn("mail_providers", "owner_user_id")
	if err != nil || has {
		return err
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
			slog.Error("re-enable foreign keys after the mail_providers rebuild failed", "err", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owner sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(
		(SELECT id FROM users WHERE role='admin' ORDER BY created_at, id LIMIT 1),
		(SELECT id FROM users ORDER BY created_at, id LIMIT 1))`).Scan(&owner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, mailProvidersDDL("mail_providers_new")); err != nil {
		return err
	}
	if owner.Valid {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mail_providers_new (id, owner_user_id, label, host, port, username, password, from_address, encryption, provider_type, created_at)
			 SELECT id, ?, label, host, port, username, password, from_address, encryption, provider_type, created_at FROM mail_providers`,
			owner.String); err != nil {
			return err
		}
	} else {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_providers`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			slog.Warn("email accounts dropped: the box has no user to own them")
			if _, err := tx.ExecContext(ctx, `DELETE FROM instance_mail_bindings`); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE mail_providers`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE mail_providers_new RENAME TO mail_providers`); err != nil {
		return err
	}
	// The rebuild ran with foreign keys off, so check by hand that it left
	// no binding pointing at nothing and no account without its owner. Only
	// these two tables: a problem elsewhere is not this migration's to fail on.
	for _, table := range []string{"mail_providers", "instance_mail_bindings"} {
		rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check(`+table+`)`)
		if err != nil {
			return err
		}
		bad := rows.Next()
		rows.Close()
		if bad {
			return fmt.Errorf("%s has a broken foreign key after the mail_providers rebuild", table)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if owner.Valid {
		slog.Info("email accounts moved to the founding admin", "user_id", owner.String)
	}
	return nil
}

// normalizeMailProvider applies the same defense-in-depth as Create's scope
// and exposure checks: the ALTER-migration path carries no CHECK on
// provider_type, so the invariant is enforced here, on both write paths.
func normalizeMailProvider(p *MailProvider) error {
	if !validMailEncryption(p.Encryption) {
		return fmt.Errorf("invalid encryption %q", p.Encryption)
	}
	if p.ProviderType == "" {
		p.ProviderType = mailpreset.Custom
	}
	if !mailpreset.Valid(p.ProviderType) {
		return fmt.Errorf("invalid provider type %q", p.ProviderType)
	}
	return nil
}

// CreateMailProvider inserts a provider. Caller generates the ID and sets the
// owner. Returns ErrConflict on a duplicate id, or on a label the same owner
// already uses.
func (s *Store) CreateMailProvider(p MailProvider) error {
	if err := normalizeMailProvider(&p); err != nil {
		return err
	}
	if p.OwnerUserID == "" {
		return errors.New("mail provider has no owner")
	}
	_, err := s.db.Exec(
		`INSERT INTO mail_providers (id, owner_user_id, label, host, port, username, password, from_address, encryption, provider_type, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.OwnerUserID, p.Label, p.Host, p.Port, p.Username, p.Password, p.FromAddress, p.Encryption, p.ProviderType, p.CreatedAt.Unix())
	if err != nil && isUniqueErr(err) {
		return ErrConflict
	}
	return err
}

const mailProviderCols = `id, owner_user_id, label, host, port, username, password, from_address, encryption, provider_type, created_at`

// GetMailProvider returns one provider by ID, or ErrNotFound. It does not
// check the owner; callers that act for a user compare OwnerUserID.
func (s *Store) GetMailProvider(id string) (MailProvider, error) {
	return scanMailProvider(s.db.QueryRow(
		`SELECT `+mailProviderCols+` FROM mail_providers WHERE id=?`, id))
}

// ListMailProviders returns the providers ownerID added, ordered by label.
func (s *Store) ListMailProviders(ownerID string) ([]MailProvider, error) {
	return s.listMailProviders(`WHERE owner_user_id=?`, ownerID)
}

// ListAllMailProviders returns every provider on the box, whoever owns it.
// For tests and whole-box checks; nothing that acts for one user may use it.
func (s *Store) ListAllMailProviders() ([]MailProvider, error) {
	return s.listMailProviders(``)
}

func (s *Store) listMailProviders(where string, args ...any) ([]MailProvider, error) {
	rows, err := s.db.Query(
		`SELECT `+mailProviderCols+` FROM mail_providers `+where+` ORDER BY label, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailProvider
	for rows.Next() {
		p, err := scanMailProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateMailProvider replaces every mutable field of the provider identified
// by p.ID and owned by p.OwnerUserID. The owner never changes. Returns
// ErrNotFound when that owner has no such provider and ErrConflict when the
// new label collides with another of the owner's providers.
func (s *Store) UpdateMailProvider(p MailProvider) error {
	if err := normalizeMailProvider(&p); err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE mail_providers SET label=?, host=?, port=?, username=?, password=?, from_address=?, encryption=?, provider_type=?
		 WHERE id=? AND owner_user_id=?`,
		p.Label, p.Host, p.Port, p.Username, p.Password, p.FromAddress, p.Encryption, p.ProviderType, p.ID, p.OwnerUserID)
	if err != nil {
		if isUniqueErr(err) {
			return ErrConflict
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMailProvider removes a provider ownerID owns; bindings cascade away
// so bound apps fall back to unbound (their next .env write injects nothing).
// Returns ErrNotFound when that owner has no such provider.
func (s *Store) DeleteMailProvider(id, ownerID string) error {
	res, err := s.db.Exec(`DELETE FROM mail_providers WHERE id=? AND owner_user_id=?`, id, ownerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInstanceMailBinding binds an instance to a provider, replacing any
// existing binding (an instance sends through at most one provider).
func (s *Store) SetInstanceMailBinding(instanceID, providerID string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO instance_mail_bindings (instance_id, provider_id) VALUES (?,?)`,
		instanceID, providerID)
	return err
}

// DeleteInstanceMailBinding unbinds an instance. Idempotent: unbinding an
// unbound instance is a no-op, not an error.
func (s *Store) DeleteInstanceMailBinding(instanceID string) error {
	_, err := s.db.Exec(`DELETE FROM instance_mail_bindings WHERE instance_id=?`, instanceID)
	return err
}

// MailBinding is one app bound to one provider.
type MailBinding struct {
	InstanceID string
	ProviderID string
}

// ListMailBindingsForOwner returns the bindings to every provider ownerID
// owns. deleteUser reads them before the user row goes, because the delete
// cascades them away and a failed host step has to put them back.
func (s *Store) ListMailBindingsForOwner(ownerID string) ([]MailBinding, error) {
	rows, err := s.db.Query(
		`SELECT b.instance_id, b.provider_id FROM instance_mail_bindings b
		 JOIN mail_providers p ON p.id = b.provider_id
		 WHERE p.owner_user_id=? ORDER BY b.instance_id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailBinding
	for rows.Next() {
		var b MailBinding
		if err := rows.Scan(&b.InstanceID, &b.ProviderID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetInstanceMailProvider returns the provider an instance is bound to, or
// ErrNotFound when unbound (writeEnv's signal to inject nothing).
func (s *Store) GetInstanceMailProvider(instanceID string) (MailProvider, error) {
	return scanMailProvider(s.db.QueryRow(
		`SELECT p.id, p.owner_user_id, p.label, p.host, p.port, p.username, p.password, p.from_address, p.encryption, p.provider_type, p.created_at
		 FROM instance_mail_bindings b JOIN mail_providers p ON p.id = b.provider_id
		 WHERE b.instance_id=?`, instanceID))
}

func scanMailProvider(row scanner) (MailProvider, error) {
	var p MailProvider
	var created int64
	err := row.Scan(&p.ID, &p.OwnerUserID, &p.Label, &p.Host, &p.Port, &p.Username, &p.Password, &p.FromAddress, &p.Encryption, &p.ProviderType, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return MailProvider{}, ErrNotFound
	}
	if err != nil {
		return MailProvider{}, fmt.Errorf("scan mail_provider: %w", err)
	}
	p.CreatedAt = time.Unix(created, 0)
	return p, nil
}
