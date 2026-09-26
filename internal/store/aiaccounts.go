package store

// AI provider accounts (INSTALL_SETUP.md # 5, SERVICE_PROVISIONING.md # AI
// provider accounts). A user saves a key for one AI provider once, and later
// picks the account when an app needs a provider. Each account belongs to the
// user who added it, the same model as email accounts (mail.go). An install
// binds an app's AI slot to one of the installer's accounts
// (instance_ai_bindings, below).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AIAccount is one saved AI provider account. It belongs to OwnerUserID, the
// user who added it, and only that user can see or use it. Label is the name
// shown in pickers, unique per owner. ProviderID is an id from the catalog's
// provider data, or "openai_compatible" for a server the user names by its
// address. BaseURL is empty when the account uses the provider's own address.
// APIKey may be empty only for an openai_compatible account; the API checks
// that. The key is plaintext at rest, the same trust model as a mail password
// (NEXT.md # App-secret injection hardening).
type AIAccount struct {
	ID          string
	OwnerUserID string
	ProviderID  string
	Label       string
	APIKey      string
	BaseURL     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// aiAccountsDDL creates the ai_accounts table.
//
// owner_user_id: the user who added the account. Deleting the user deletes
// their accounts.
//
// Labels are unique per owner, not per box, the same as email accounts.
//
// provider_id has no CHECK and no foreign key: the provider list comes from
// the catalog at runtime and can change, so the API checks it on write, and a
// stored id the catalog later drops stays readable.
//
// base_url is the empty string when the account has no address of its own.
const aiAccountsDDL = `CREATE TABLE IF NOT EXISTS ai_accounts (
	id            TEXT    PRIMARY KEY,
	owner_user_id TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	provider_id   TEXT    NOT NULL,
	label         TEXT    NOT NULL,
	api_key       TEXT    NOT NULL DEFAULT '',
	base_url      TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	UNIQUE (owner_user_id, label)
)`

const aiAccountCols = `id, owner_user_id, provider_id, label, api_key, base_url, created_at, updated_at`

// CreateAIAccount inserts an account. The caller generates the ID and sets
// the owner and both times. Returns ErrConflict on a duplicate id, or on a
// label the same owner already uses.
func (s *Store) CreateAIAccount(a AIAccount) error {
	if a.OwnerUserID == "" {
		return errors.New("ai account has no owner")
	}
	if a.ProviderID == "" {
		return errors.New("ai account has no provider")
	}
	_, err := s.db.Exec(
		`INSERT INTO ai_accounts (`+aiAccountCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerUserID, a.ProviderID, a.Label, a.APIKey, a.BaseURL, a.CreatedAt.Unix(), a.UpdatedAt.Unix())
	if err != nil && isUniqueErr(err) {
		return ErrConflict
	}
	return err
}

// GetAIAccount returns one account by ID, or ErrNotFound. It does not check
// the owner; callers that act for a user compare OwnerUserID.
func (s *Store) GetAIAccount(id string) (AIAccount, error) {
	return scanAIAccount(s.db.QueryRow(`SELECT `+aiAccountCols+` FROM ai_accounts WHERE id=?`, id))
}

// ListAIAccounts returns the accounts ownerID added, ordered by label.
func (s *Store) ListAIAccounts(ownerID string) ([]AIAccount, error) {
	return s.listAIAccounts(`WHERE owner_user_id=?`, ownerID)
}

// ListAllAIAccounts returns every account on the box, whoever owns it. For
// tests and whole-box checks; nothing that acts for one user may use it.
func (s *Store) ListAllAIAccounts() ([]AIAccount, error) {
	return s.listAIAccounts(``)
}

func (s *Store) listAIAccounts(where string, args ...any) ([]AIAccount, error) {
	rows, err := s.db.Query(`SELECT `+aiAccountCols+` FROM ai_accounts `+where+` ORDER BY label, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIAccount
	for rows.Next() {
		a, err := scanAIAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAIAccount replaces the provider, label, key, base URL and updated_at
// of the account identified by a.ID and owned by a.OwnerUserID. The owner and
// created_at never change. Returns ErrNotFound when that owner has no such
// account and ErrConflict when the new label collides with another of the
// owner's accounts.
//
// An empty a.APIKey keeps the stored key. The key is kept in the same
// statement, not read and written back, so an edit that sends no key cannot
// undo a key change made by another request at the same time.
func (s *Store) UpdateAIAccount(a AIAccount) error {
	if a.ProviderID == "" {
		return errors.New("ai account has no provider")
	}
	res, err := s.db.Exec(
		`UPDATE ai_accounts SET provider_id=?, label=?, api_key=COALESCE(NULLIF(?, ''), api_key), base_url=?, updated_at=?
		 WHERE id=? AND owner_user_id=?`,
		a.ProviderID, a.Label, a.APIKey, a.BaseURL, a.UpdatedAt.Unix(), a.ID, a.OwnerUserID)
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

// DeleteAIAccount removes an account ownerID owns. Returns ErrNotFound when
// that owner has no such account.
func (s *Store) DeleteAIAccount(id, ownerID string) error {
	res, err := s.db.Exec(`DELETE FROM ai_accounts WHERE id=? AND owner_user_id=?`, id, ownerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanAIAccount(row scanner) (AIAccount, error) {
	var a AIAccount
	var created, updated int64
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.ProviderID, &a.Label, &a.APIKey, &a.BaseURL, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return AIAccount{}, ErrNotFound
	}
	if err != nil {
		return AIAccount{}, fmt.Errorf("scan ai_account: %w", err)
	}
	a.CreatedAt = time.Unix(created, 0)
	a.UpdatedAt = time.Unix(updated, 0)
	return a, nil
}

// AIBinding is one app slot filled from one AI account (INSTALL_SETUP.md # 5).
// Slot is the manifest's kind.protocol, for example ai.anthropic. Models maps a
// model attribute the slot declares (model.chat, models.embedding) to the model
// ids the app was given, after defaults are applied. The values the binding
// resolved to are stored as the instance's config values too, so the compose
// override does not read this table. It is kept so the app's settings screen
// can show the pickers, and so a key change can rewrite the app (piece 4).
type AIBinding struct {
	InstanceID string
	Slot       string
	AccountID  string
	Models     map[string][]string
}

// aiBindingsDDL creates the instance_ai_bindings table. One row per instance
// and slot. It cascades with the instance, so an uninstall removes it, and
// with the account, so deleting an account removes its bindings. The app keeps
// its stored config values until its next write; what it shows then is piece 4.
// models is a JSON object, never NULL: '{}' when the slot takes no model.
const aiBindingsDDL = `CREATE TABLE IF NOT EXISTS instance_ai_bindings (
	instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
	slot        TEXT NOT NULL,
	account_id  TEXT NOT NULL REFERENCES ai_accounts(id) ON DELETE CASCADE,
	models      TEXT NOT NULL DEFAULT '{}',
	PRIMARY KEY (instance_id, slot)
)`

// SetInstanceAIBindings replaces every binding of an instance with bs, in one
// transaction. The InstanceID on each binding is ignored; instanceID is used.
// An empty bs removes them all. A missing account fails the foreign key.
func (s *Store) SetInstanceAIBindings(instanceID string, bs []AIBinding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM instance_ai_bindings WHERE instance_id=?`, instanceID); err != nil {
		return err
	}
	for _, b := range bs {
		b.InstanceID = instanceID
		if err := putAIBinding(tx, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PutAIBinding writes one binding, replacing the row for the same instance and
// slot. deleteUser uses it to put back the bindings a failed delete cascaded
// away, without touching the instance's other slots.
func (s *Store) PutAIBinding(b AIBinding) error {
	return putAIBinding(s.db, b)
}

// execer is the part of *sql.DB and *sql.Tx that putAIBinding needs.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func putAIBinding(db execer, b AIBinding) error {
	models := b.Models
	if models == nil {
		models = map[string][]string{}
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("encode ai binding models: %w", err)
	}
	_, err = db.Exec(
		`INSERT OR REPLACE INTO instance_ai_bindings (instance_id, slot, account_id, models) VALUES (?,?,?,?)`,
		b.InstanceID, b.Slot, b.AccountID, string(raw))
	return err
}

// ListInstanceAIBindings returns an instance's bindings, ordered by slot.
func (s *Store) ListInstanceAIBindings(instanceID string) ([]AIBinding, error) {
	return s.listAIBindings(`WHERE instance_id=? ORDER BY slot`, instanceID)
}

// ListAIBindingsForAccount returns every binding to one account, ordered by
// instance and slot: the apps a key change must rewrite, or a delete touches.
func (s *Store) ListAIBindingsForAccount(accountID string) ([]AIBinding, error) {
	return s.listAIBindings(`WHERE account_id=? ORDER BY instance_id, slot`, accountID)
}

func (s *Store) listAIBindings(where string, args ...any) ([]AIBinding, error) {
	rows, err := s.db.Query(`SELECT instance_id, slot, account_id, models FROM instance_ai_bindings `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIBinding
	for rows.Next() {
		var (
			b   AIBinding
			raw string
		)
		if err := rows.Scan(&b.InstanceID, &b.Slot, &b.AccountID, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &b.Models); err != nil {
			return nil, fmt.Errorf("decode ai binding models: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
