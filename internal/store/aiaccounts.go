package store

// AI provider accounts (INSTALL_SETUP.md # 5, SERVICE_PROVISIONING.md # AI
// provider accounts). A user saves a key for one AI provider once, and later
// picks the account when an app needs a provider. Each account belongs to the
// user who added it, the same model as email accounts (mail.go). Nothing binds
// an app to an account yet: that comes with slot filling.

import (
	"database/sql"
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
func (s *Store) UpdateAIAccount(a AIAccount) error {
	if a.ProviderID == "" {
		return errors.New("ai account has no provider")
	}
	res, err := s.db.Exec(
		`UPDATE ai_accounts SET provider_id=?, label=?, api_key=?, base_url=?, updated_at=?
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
