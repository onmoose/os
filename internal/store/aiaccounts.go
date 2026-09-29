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
	// Models are the model ids an OpenAI-compatible account (a server of the
	// user's own, with no model list) serves, by model type ("chat" to
	// ["llama3"]). A binding that names no model for a type takes them from
	// here. Empty for a listed provider.
	Models    map[string][]string
	CreatedAt time.Time
	UpdatedAt time.Time
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
//
// models is a JSON object from model type to model ids, never NULL: '{}' when
// the account has none (every listed-provider account).
const aiAccountsDDL = `CREATE TABLE IF NOT EXISTS ai_accounts (
	id            TEXT    PRIMARY KEY,
	owner_user_id TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	provider_id   TEXT    NOT NULL,
	label         TEXT    NOT NULL,
	api_key       TEXT    NOT NULL DEFAULT '',
	base_url      TEXT    NOT NULL DEFAULT '',
	models        TEXT    NOT NULL DEFAULT '{}',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	UNIQUE (owner_user_id, label)
)`

const aiAccountCols = `id, owner_user_id, provider_id, label, api_key, base_url, models, created_at, updated_at`

// encodeAccountModels is the stored form of an account's models.
func encodeAccountModels(m map[string][]string) (string, error) {
	if m == nil {
		m = map[string][]string{}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode ai account models: %w", err)
	}
	return string(raw), nil
}

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
	models, err := encodeAccountModels(a.Models)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO ai_accounts (`+aiAccountCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerUserID, a.ProviderID, a.Label, a.APIKey, a.BaseURL, models, a.CreatedAt.Unix(), a.UpdatedAt.Unix())
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

// UpdateAIAccount replaces the provider, label, key, base URL, models and updated_at
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
	models, err := encodeAccountModels(a.Models)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE ai_accounts SET provider_id=?, label=?, api_key=COALESCE(NULLIF(?, ''), api_key), base_url=?, models=?, updated_at=?
		 WHERE id=? AND owner_user_id=?`,
		a.ProviderID, a.Label, a.APIKey, a.BaseURL, models, a.UpdatedAt.Unix(), a.ID, a.OwnerUserID)
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
	var models string
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.ProviderID, &a.Label, &a.APIKey, &a.BaseURL, &models, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return AIAccount{}, ErrNotFound
	}
	if err != nil {
		return AIAccount{}, fmt.Errorf("scan ai_account: %w", err)
	}
	if models != "" && models != "{}" {
		if err := json.Unmarshal([]byte(models), &a.Models); err != nil {
			return AIAccount{}, fmt.Errorf("decode ai account models: %w", err)
		}
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
	// Envs are the app_env names the binding fills: every field of its slot.
	// An account delete clears exactly these, so it never needs the app's
	// manifest. nil means not recorded (a row from before the column); a
	// delete then falls back to the manifest.
	Envs []string
}

// aiBindingsDDL creates the instance_ai_bindings table. One row per instance
// and slot. It cascades with the instance, so an uninstall removes it, and
// with the account, so deleting an account removes its bindings. The app keeps
// its stored config values until its next write; what it shows then is piece 4.
// models is a JSON object, never NULL: '{}' when the slot takes no model.
// envs is a JSON list of the app_env names the binding fills, or ” when not
// recorded (rows from before the column, which migrate adds with ALTER).
const aiBindingsDDL = `CREATE TABLE IF NOT EXISTS instance_ai_bindings (
	instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
	slot        TEXT NOT NULL,
	account_id  TEXT NOT NULL REFERENCES ai_accounts(id) ON DELETE CASCADE,
	models      TEXT NOT NULL DEFAULT '{}',
	envs        TEXT NOT NULL DEFAULT '',
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
	_, err := writeAIBinding(db, "INSERT OR REPLACE", b)
	return err
}

// writeAIBinding writes one binding row with the given insert verb. It
// returns whether a row was written, which is false when INSERT OR IGNORE
// found the slot taken.
func writeAIBinding(db execer, verb string, b AIBinding) (bool, error) {
	models := b.Models
	if models == nil {
		models = map[string][]string{}
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return false, fmt.Errorf("encode ai binding models: %w", err)
	}
	envs, err := encodeEnvs(b.Envs)
	if err != nil {
		return false, err
	}
	res, err := db.Exec(
		verb+` INTO instance_ai_bindings (instance_id, slot, account_id, models, envs) VALUES (?,?,?,?,?)`,
		b.InstanceID, b.Slot, b.AccountID, string(raw), envs)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// encodeEnvs stores nil as ” (not recorded) and a list as JSON.
func encodeEnvs(envs []string) (string, error) {
	if envs == nil {
		return "", nil
	}
	raw, err := json.Marshal(envs)
	if err != nil {
		return "", fmt.Errorf("encode ai binding envs: %w", err)
	}
	return string(raw), nil
}

// decodeEnvs is the reverse of encodeEnvs.
func decodeEnvs(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	envs := []string{}
	if err := json.Unmarshal([]byte(raw), &envs); err != nil {
		return nil, fmt.Errorf("decode ai binding envs: %w", err)
	}
	return envs, nil
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
	return scanAIBindings(s.db, `SELECT instance_id, slot, account_id, models, envs FROM instance_ai_bindings `+where, args...)
}

// querier is the part of *sql.DB and *sql.Tx that scanAIBindings needs.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func scanAIBindings(db querier, query string, args ...any) ([]AIBinding, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIBinding
	for rows.Next() {
		var (
			b         AIBinding
			raw, envs string
		)
		if err := rows.Scan(&b.InstanceID, &b.Slot, &b.AccountID, &raw, &envs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &b.Models); err != nil {
			return nil, fmt.Errorf("decode ai binding models: %w", err)
		}
		if b.Envs, err = decodeEnvs(envs); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ErrBindingGone means a binding a write relied on no longer exists, because
// its account was deleted (or the slot rebound) since the caller read it. The
// write is not done. Lifecycle turns it into a plain job error.
var ErrBindingGone = errors.New("an ai binding changed since it was read")

// SetInstanceConfigAndAIBindings writes an instance's config values and the
// AI bindings of some of its slots in one transaction, so the values an app
// gets and the bindings that explain them never disagree (INSTALL_SETUP.md #
// 5, piece 4). cfg replaces every config value, as SetInstanceConfig does.
// Each slot in slots loses its binding, and then bindings are written. A slot
// not in slots keeps its binding.
//
// keep are the bindings of the other slots as the caller read them, the ones
// cfg's values for those slots came from. Each must still exist, same slot
// and same account, when the transaction runs; otherwise an account delete
// ran in between, cfg would bring its values back, and the write is refused
// with ErrBindingGone. A new binding whose account no longer exists is
// refused the same way. Nothing is written in either case. The store has one
// connection, so the check and the write cannot interleave with a delete.
func (s *Store) SetInstanceConfigAndAIBindings(instanceID string, cfg []InstanceConfig, slots []string, bindings []AIBinding, keep []AIBinding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keep {
		ok, err := bindingExists(tx, instanceID, k.Slot, k.AccountID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrBindingGone
		}
	}
	for _, b := range bindings {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ai_accounts WHERE id=?`, b.AccountID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrBindingGone
		}
	}
	if err := replaceInstanceConfig(tx, instanceID, cfg); err != nil {
		return err
	}
	for _, slot := range slots {
		if _, err := tx.Exec(`DELETE FROM instance_ai_bindings WHERE instance_id=? AND slot=?`, instanceID, slot); err != nil {
			return err
		}
	}
	for _, b := range bindings {
		b.InstanceID = instanceID
		if err := putAIBinding(tx, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// rowQuerier is the part of *sql.DB and *sql.Tx that bindingExists needs.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func bindingExists(db rowQuerier, instanceID, slot, accountID string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM instance_ai_bindings WHERE instance_id=? AND slot=? AND account_id=?`,
		instanceID, slot, accountID).Scan(&n)
	return n > 0, err
}

// SlotWrite is the new values of one AI slot of an app. Envs are all the
// fields of the slot: they are cleared first, then Values are written, and
// Envs is recorded on the binding.
type SlotWrite struct {
	Slot   string
	Envs   []string
	Values []InstanceConfig
}

// ApplyAISlotValues writes new values into the slots of an instance that are
// bound to accountID, in one transaction, after an account edit. A slot whose
// binding no longer names accountID (rebound, or the account was deleted
// since the caller read it) is skipped and nothing is written for it, so a
// re-stamp can never bring back a deleted key. The other config values are
// not touched. It returns how many slots were written.
func (s *Store) ApplyAISlotValues(instanceID, accountID string, writes []SlotWrite) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	applied := 0
	for _, w := range writes {
		ok, err := bindingExists(tx, instanceID, w.Slot, accountID)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		for _, env := range w.Envs {
			if _, err := tx.Exec(`DELETE FROM instance_config WHERE instance_id=? AND app_env=?`, instanceID, env); err != nil {
				return 0, err
			}
		}
		for _, c := range w.Values {
			if _, err := tx.Exec(
				`INSERT INTO instance_config (instance_id, app_env, value, secret) VALUES (?,?,?,?)`,
				instanceID, c.AppEnv, c.Value, c.Secret); err != nil {
				return 0, err
			}
		}
		envs, err := encodeEnvs(w.Envs)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE instance_ai_bindings SET envs=? WHERE instance_id=? AND slot=?`, envs, instanceID, w.Slot); err != nil {
			return 0, err
		}
		applied++
	}
	return applied, tx.Commit()
}

// SlotFieldsError says which app's slot fields could not be named during
// DeleteAIAccountAndValues. The API needs the instance id to tell the user
// which app is in the way.
type SlotFieldsError struct {
	InstanceID string
	Err        error
}

func (e *SlotFieldsError) Error() string {
	return fmt.Sprintf("fields of instance %s: %v", e.InstanceID, e.Err)
}

func (e *SlotFieldsError) Unwrap() error { return e.Err }

// DeleteAIAccountAndValues removes an account ownerID owns and, in the same
// transaction, the config values its bindings gave each app. The bindings are
// read inside the transaction, so a slot rebound to another account a moment
// before is not touched: only slots still bound to this account lose their
// values. A binding clears the app_env names it recorded (Envs). Only for a
// row with none recorded does slotFields name the fields of the slot (from
// the app's manifest copy). It must not use the store: the store has one
// connection, and the transaction holds it. If slotFields fails, nothing is
// deleted and the error is a *SlotFieldsError, because a deleted key must
// never stay in an app. The binding rows go with the account by cascade.
//
// It returns the ids of the apps whose values were cleared, each once, for
// the job that rewrites them. ErrNotFound when that owner has no such
// account; nothing is removed then.
func (s *Store) DeleteAIAccountAndValues(id, ownerID string, slotFields func(instanceID, slot string) ([]string, error)) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var found int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM ai_accounts WHERE id=? AND owner_user_id=?`, id, ownerID).Scan(&found); err != nil {
		return nil, err
	}
	if found == 0 {
		return nil, ErrNotFound
	}
	bs, err := scanAIBindings(tx, `SELECT instance_id, slot, account_id, models, envs FROM instance_ai_bindings WHERE account_id=? ORDER BY instance_id, slot`, id)
	if err != nil {
		return nil, err
	}
	ids, _, err := clearBindingValues(tx, bs, slotFields)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM ai_accounts WHERE id=? AND owner_user_id=?`, id, ownerID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ClearedConfig is one config value a delete took from an app. A user delete
// keeps them so it can put them back if the host step fails.
type ClearedConfig struct {
	InstanceID string
	// Slot is the AI slot whose binding gave the value.
	Slot string
	InstanceConfig
}

// DeletedUser is what DeleteUserAndAccountValues took from the apps. It is
// read inside the delete transaction, so a binding made a moment before the
// delete is in it.
type DeletedUser struct {
	// AIBindings and MailBindings are the app bindings to the user's AI and
	// email accounts. They went with the accounts by cascade.
	AIBindings   []AIBinding
	MailBindings []MailBinding
	// AIInstanceIDs are the apps whose AI values were cleared, each once.
	AIInstanceIDs []string
	// Cleared are the values themselves.
	Cleared []ClearedConfig
}

// DeleteUserAndAccountValues removes a user and, in the same transaction,
// the config values that the user's AI accounts gave each app. It is the
// user delete's version of DeleteAIAccountAndValues, with the same rules: a
// binding clears the app_env names it recorded, and slotFields names them
// only for a row with none recorded. If slotFields fails, nothing is deleted
// and the error is a *SlotFieldsError. The accounts, their bindings, and
// everything else the user owns go with the user row by cascade.
//
// It returns what it took, for the job that rewrites the apps and for
// RestoreDeletedUser. ErrNotFound when there is no such user; nothing is
// removed then.
func (s *Store) DeleteUserAndAccountValues(userID string, slotFields func(instanceID, slot string) ([]string, error)) (DeletedUser, error) {
	var d DeletedUser
	tx, err := s.db.Begin()
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if d.AIBindings, err = scanAIBindings(tx, `SELECT b.instance_id, b.slot, b.account_id, b.models, b.envs
		FROM instance_ai_bindings b JOIN ai_accounts a ON a.id = b.account_id
		WHERE a.owner_user_id=? ORDER BY b.instance_id, b.slot`, userID); err != nil {
		return d, err
	}
	if d.MailBindings, err = listMailBindingsForOwner(tx, userID); err != nil {
		return d, err
	}
	if d.AIInstanceIDs, d.Cleared, err = clearBindingValues(tx, d.AIBindings, slotFields); err != nil {
		return DeletedUser{}, err
	}
	res, err := tx.Exec(`DELETE FROM users WHERE id=?`, userID)
	if err != nil {
		return DeletedUser{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return DeletedUser{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return DeletedUser{}, err
	}
	return d, nil
}

// clearBindingValues deletes, inside tx, the config values each binding
// gave its app. It returns the ids of the apps it touched, each once, and
// the values it deleted.
func clearBindingValues(tx *sql.Tx, bs []AIBinding, slotFields func(instanceID, slot string) ([]string, error)) ([]string, []ClearedConfig, error) {
	var (
		ids     []string
		cleared []ClearedConfig
		seen    = map[string]bool{}
	)
	for _, b := range bs {
		envs := b.Envs
		if envs == nil {
			var err error
			if envs, err = slotFields(b.InstanceID, b.Slot); err != nil {
				return nil, nil, &SlotFieldsError{InstanceID: b.InstanceID, Err: err}
			}
		}
		for _, env := range envs {
			c := ClearedConfig{InstanceID: b.InstanceID, Slot: b.Slot, InstanceConfig: InstanceConfig{AppEnv: env}}
			err := tx.QueryRow(`SELECT value, secret FROM instance_config WHERE instance_id=? AND app_env=?`,
				b.InstanceID, env).Scan(&c.Value, &c.Secret)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if _, err := tx.Exec(`DELETE FROM instance_config WHERE instance_id=? AND app_env=?`, b.InstanceID, env); err != nil {
				return nil, nil, err
			}
			cleared = append(cleared, c)
		}
		if !seen[b.InstanceID] {
			seen[b.InstanceID] = true
			ids = append(ids, b.InstanceID)
		}
	}
	return ids, cleared, nil
}

// RestoreDeletedUser puts back the bindings and values a failed user delete
// took, after the caller has put the user row and the accounts back. A row
// written since is kept. A slot someone rebound while the delete ran keeps
// its new binding, and none of the old values come back to it, so a slot
// never holds values from two accounts. A value is also kept when one was
// written since. An app uninstalled since is skipped by its foreign key.
// Each row is its own write, so one that fails does not stop the rest; the
// errors are joined.
func (s *Store) RestoreDeletedUser(d DeletedUser) error {
	var errs []error
	type slotKey struct{ instance, slot string }
	restored := map[slotKey]bool{}
	for _, b := range d.AIBindings {
		// Only a binding this insert wrote gets its values back. A slot
		// someone bound in the meantime, even a moment before this insert,
		// is ignored and keeps its own values.
		wrote, err := writeAIBinding(s.db, "INSERT OR IGNORE", b)
		if err != nil {
			errs = append(errs, fmt.Errorf("ai binding %s %s: %w", b.InstanceID, b.Slot, err))
			continue
		}
		if wrote {
			restored[slotKey{b.InstanceID, b.Slot}] = true
		}
	}
	for _, b := range d.MailBindings {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO instance_mail_bindings (instance_id, provider_id) VALUES (?,?)`,
			b.InstanceID, b.ProviderID); err != nil {
			errs = append(errs, fmt.Errorf("mail binding %s: %w", b.InstanceID, err))
		}
	}
	for _, c := range d.Cleared {
		if !restored[slotKey{c.InstanceID, c.Slot}] {
			continue
		}
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO instance_config (instance_id, app_env, value, secret) VALUES (?,?,?,?)`,
			c.InstanceID, c.AppEnv, c.Value, c.Secret); err != nil {
			errs = append(errs, fmt.Errorf("value %s %s: %w", c.InstanceID, c.AppEnv, err))
		}
	}
	return errors.Join(errs...)
}

// AppUse names one app that uses an account: its instance id and its name.
type AppUse struct {
	InstanceID string
	Name       string
}

// AIAccountUsage maps each of ownerID's AI accounts to the apps bound to it,
// each app once, ordered by name. An account no app uses is absent.
func (s *Store) AIAccountUsage(ownerID string) (map[string][]AppUse, error) {
	return s.accountUsage(
		`SELECT DISTINCT b.account_id, i.id, i.name FROM instance_ai_bindings b
		 JOIN ai_accounts a ON a.id = b.account_id
		 JOIN instances i ON i.id = b.instance_id
		 WHERE a.owner_user_id=? ORDER BY i.name, i.id`, ownerID)
}

// MailProviderUsage maps each of ownerID's email accounts to the apps bound to
// it, ordered by name. An account no app uses is absent.
func (s *Store) MailProviderUsage(ownerID string) (map[string][]AppUse, error) {
	return s.accountUsage(
		`SELECT b.provider_id, i.id, i.name FROM instance_mail_bindings b
		 JOIN mail_providers p ON p.id = b.provider_id
		 JOIN instances i ON i.id = b.instance_id
		 WHERE p.owner_user_id=? ORDER BY i.name, i.id`, ownerID)
}

func (s *Store) accountUsage(query string, args ...any) (map[string][]AppUse, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]AppUse{}
	for rows.Next() {
		var account string
		var u AppUse
		if err := rows.Scan(&account, &u.InstanceID, &u.Name); err != nil {
			return nil, err
		}
		out[account] = append(out[account], u)
	}
	return out, rows.Err()
}
