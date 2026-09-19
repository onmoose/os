package store

// BYO outgoing mail (SERVICE_PROVISIONING.md # BYO outgoing mail). An admin
// registers SMTP provider(s) in Settings; a mail-capable app binds to at most
// one, and the lifecycle injects the bound provider as MOOSE_MAIL_* at .env
// write time. No moose-run relay exists — unbound apps get nothing.

import (
	"database/sql"
	"errors"
	"fmt"
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

// MailProvider is one admin-registered outgoing SMTP account. Label is the
// human name shown in pickers (unique). Password is plaintext at rest, same
// trust model as instance_secrets (NEXT.md # App-secret injection hardening).
type MailProvider struct {
	ID          string
	Label       string
	Host        string
	Port        int
	Username    string
	Password    string
	FromAddress string
	Encryption  string // none | starttls | tls
	// ProviderType names the built-in preset the admin picked, or "custom"
	// for hand-typed values (internal/mailpreset). It records the choice, not
	// a guarantee the other fields still match the preset — the advanced form
	// lets an admin override host, port and encryption.
	ProviderType string
	CreatedAt    time.Time
}

func validMailEncryption(enc string) bool {
	return enc == MailEncryptionNone || enc == MailEncryptionSTARTTLS || enc == MailEncryptionTLS
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

// CreateMailProvider inserts a provider. Caller generates the ID. Returns
// ErrConflict on a duplicate id or label.
func (s *Store) CreateMailProvider(p MailProvider) error {
	if err := normalizeMailProvider(&p); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO mail_providers (id, label, host, port, username, password, from_address, encryption, provider_type, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Label, p.Host, p.Port, p.Username, p.Password, p.FromAddress, p.Encryption, p.ProviderType, p.CreatedAt.Unix())
	if err != nil && isUniqueErr(err) {
		return ErrConflict
	}
	return err
}

// GetMailProvider returns one provider by ID, or ErrNotFound.
func (s *Store) GetMailProvider(id string) (MailProvider, error) {
	return scanMailProvider(s.db.QueryRow(
		`SELECT id, label, host, port, username, password, from_address, encryption, provider_type, created_at
		 FROM mail_providers WHERE id=?`, id))
}

// ListMailProviders returns every registered provider, ordered by label.
func (s *Store) ListMailProviders() ([]MailProvider, error) {
	rows, err := s.db.Query(
		`SELECT id, label, host, port, username, password, from_address, encryption, provider_type, created_at
		 FROM mail_providers ORDER BY label`)
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
// by p.ID. Returns ErrNotFound when no such provider exists and ErrConflict
// when the new label collides with another provider's.
func (s *Store) UpdateMailProvider(p MailProvider) error {
	if err := normalizeMailProvider(&p); err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE mail_providers SET label=?, host=?, port=?, username=?, password=?, from_address=?, encryption=?, provider_type=?
		 WHERE id=?`,
		p.Label, p.Host, p.Port, p.Username, p.Password, p.FromAddress, p.Encryption, p.ProviderType, p.ID)
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

// DeleteMailProvider removes a provider; bindings cascade away so bound apps
// fall back to unbound (their next .env write injects nothing). Returns
// ErrNotFound when no such provider exists.
func (s *Store) DeleteMailProvider(id string) error {
	res, err := s.db.Exec(`DELETE FROM mail_providers WHERE id=?`, id)
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

// GetInstanceMailProvider returns the provider an instance is bound to, or
// ErrNotFound when unbound (writeEnv's signal to inject nothing).
func (s *Store) GetInstanceMailProvider(instanceID string) (MailProvider, error) {
	return scanMailProvider(s.db.QueryRow(
		`SELECT p.id, p.label, p.host, p.port, p.username, p.password, p.from_address, p.encryption, p.provider_type, p.created_at
		 FROM instance_mail_bindings b JOIN mail_providers p ON p.id = b.provider_id
		 WHERE b.instance_id=?`, instanceID))
}

func scanMailProvider(row scanner) (MailProvider, error) {
	var p MailProvider
	var created int64
	err := row.Scan(&p.ID, &p.Label, &p.Host, &p.Port, &p.Username, &p.Password, &p.FromAddress, &p.Encryption, &p.ProviderType, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return MailProvider{}, ErrNotFound
	}
	if err != nil {
		return MailProvider{}, fmt.Errorf("scan mail_provider: %w", err)
	}
	p.CreatedAt = time.Unix(created, 0)
	return p, nil
}
