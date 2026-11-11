package store

// App updates (UPDATES.md # 4, APP_LIFECYCLE.md # update + rollback, #631).
// Three things are kept per instance: what the last update check found
// (UpdateCheck), the last update that did not go through (UpdateFailure), and
// the image pins of the previous generation (instance_images_prev), which a
// rollback puts back.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Update check statuses. Empty means no update is known.
const (
	// UpdateAvailable is an update the box can apply: on its own in the
	// window (#632), or when a person asks.
	UpdateAvailable = "available"
	// UpdateNeedsApproval widens permissions or public paths; the owner must
	// approve it first (#633). An approved update is UpdateAvailable.
	UpdateNeedsApproval = "needs_approval"
	// UpdateServiceMajor was the class of an update that moves a managed
	// database to a new major version, before the box could move the data
	// (#635). No brain writes it now. A row that still holds it is classified
	// again by the next check.
	UpdateServiceMajor = "service_major"
	// UpdateNeedsNewerMoose is a manifest_version this brain cannot read.
	UpdateNeedsNewerMoose = "needs_newer_moose"
	// UpdateBlocked is an update the box refuses for another reason (Reason
	// says which): it would change how the app is isolated, or it needs a
	// setting the box cannot fill in.
	UpdateBlocked = "blocked"
)

// UpdateCheck is what the last check found: the catalog version that differs
// from the installed one, how it was classified, and, for a status other than
// UpdateAvailable, a plain sentence that says why it is not applied. The zero
// value means no update.
//
// Wants is the access the new version asks for that the installed one does
// not have (#633). It is filled in for every class, so a service-major update
// that also asks for more access says so (#635). Empty means the same or
// less access.
type UpdateCheck struct {
	Version string
	Status  string
	Reason  string
	Wants   []AccessWant
}

// Equal reports whether two checks say the same thing.
func (c UpdateCheck) Equal(o UpdateCheck) bool {
	return c.Version == o.Version && c.Status == o.Status && c.Reason == o.Reason && slices.Equal(c.Wants, o.Wants)
}

// IsZero reports whether the check found no update.
func (c UpdateCheck) IsZero() bool { return c.Equal(UpdateCheck{}) }

// Access want kinds: what kind of access a new version asks for.
const (
	WantInternet   = "internet"
	WantLAN        = "lan"
	WantGPU        = "gpu"
	WantDevice     = "device"
	WantFolder     = "folder"
	WantPublicPath = "public_path"
)

// Folder changes a WantFolder names.
const (
	FolderNew   = "new"   // a folder the installed version has no access to
	FolderWrite = "write" // read access becomes write access
	FolderWhole = "whole" // one folder inside it becomes the whole folder; not made since #657, kept so an older stored check still reads
)

// AccessWant is one piece of access a new version of an app asks for that the
// installed version does not have (UPDATES.md # Auto-apply unless permissions
// expand). The dashboard says it in the install screen's words, so the brain
// sends it as data, not as a sentence.
//
// Device is set for WantDevice. Folder, Mode (read or write, the new
// version's), Change (FolderNew, FolderWrite or FolderWhole) and Source
// (personal or shared, the folder the update binds) are set for WantFolder.
// Path is set for WantPublicPath.
type AccessWant struct {
	Kind   string `json:"kind"`
	Device string `json:"device,omitempty"`
	Folder string `json:"folder,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Change string `json:"change,omitempty"`
	Source string `json:"source,omitempty"`
	Path   string `json:"path,omitempty"`
}

// UpdateApproval is the owner's yes to an update that asks for more access
// (#633): the version they saw, the access they accepted (the new manifest's
// permissions and public paths, as JSON in lifecycle's shape), who said yes
// and when. An update is covered when it asks for no more than Access. A good
// update clears it.
type UpdateApproval struct {
	Version string
	Access  string
	By      string
	At      time.Time
}

// Same reports whether two approvals are the same yes.
func (a UpdateApproval) Same(o UpdateApproval) bool {
	return a.Version == o.Version && a.Access == o.Access && a.By == o.By && a.At.Unix() == o.At.Unix()
}

// Update failure kinds: the step that broke.
const (
	UpdateFailedPull     = "pull"     // the new images could not be downloaded
	UpdateFailedSnapshot = "snapshot" // the copy of the data could not be taken
	UpdateFailedStart    = "start"    // the new version did not start
	UpdateFailedHealth   = "health"   // the new version did not become healthy
	UpdateFailedRefused  = "refused"  // a check that needs the new images refused it
	UpdateFailedRestore  = "snapshot_restore"
	// UpdateFailedInterrupted is an update the brain did not finish: it
	// stopped (a restart, a crash) in the middle, and the startup pass put
	// the old version back.
	UpdateFailedInterrupted = "interrupted"
)

// UpdateFailure is the last update of an instance that did not go through:
// the version it tried, the step that broke, a short message and when. It
// stays until an update to any version succeeds.
//
// Count is how many updates to Version failed in a row (#632). A failure of
// another version starts it again at 1, a good update sets it to 0, and so
// does a person's Update before it runs. At UpdatePauseAfter the nightly
// window stops trying that version (Instance.UpdatePaused).
//
// ByWindow is true when the nightly window made the update that failed, so
// the tile can say "last night" (#634). Acked is true once a person closed the
// tile's banner about it. A new failure sets both back.
type UpdateFailure struct {
	Version  string
	Kind     string
	Message  string
	At       time.Time
	Count    int
	ByWindow bool
	Acked    bool
}

// UpdatePauseAfter is how many failures in a row on one version pause the
// nightly window's updates of an app (UPDATES.md # 4 Update window, # 6
// Failure signaling). A person can still update it.
const UpdatePauseAfter = 3

// UpdatePaused reports whether the nightly window has stopped trying the
// update the box knows of: the last UpdatePauseAfter tries of that version
// all failed. A new catalog version is not paused.
func (i Instance) UpdatePaused() bool {
	return i.Update.Version != "" && i.UpdateFailure.Version == i.Update.Version && i.UpdateFailure.Count >= UpdatePauseAfter
}

// UpdateTry is the last try of the nightly window (#632): the version it
// tried and the night it tried it in, the date the window opened on, as
// YYYY-MM-DD. It is written before the update runs, so a brain that stops in
// the middle does not try the same version again that night.
type UpdateTry struct {
	Version string
	Night   string
}

// LastUpdate is the last good update of an instance, while a person can roll
// it back (#634, UPDATES.md # 4): the version the instance ran before it, when
// the copy of its data was taken (the start of the 7 days), and whether the
// nightly window made it. The version it moved to is the instance's Version.
// The zero value means there is nothing to roll back to.
type LastUpdate struct {
	From     string
	At       time.Time
	ByWindow bool
}

// RecordInstanceUpdateTry stores the nightly window's try of version on night.
func (s *Store) RecordInstanceUpdateTry(id, version, night string) error {
	return s.updateOne(`UPDATE instances SET update_tried_version=?, update_tried_night=? WHERE id=?`, version, night, id)
}

// ResetInstanceUpdateFailCount sets the count of failures in a row to 0. A
// person's Update calls it before it runs, so a failure then counts 1 and
// the window tries again (#632).
func (s *Store) ResetInstanceUpdateFailCount(id string) error {
	return s.updateOne(`UPDATE instances SET update_failed_count=0 WHERE id=?`, id)
}

// SetInstanceAutoUpdate turns the automatic update of an app on or off
// (#631). The nightly window reads it (#632).
func (s *Store) SetInstanceAutoUpdate(id string, on bool) error {
	off := 1
	if on {
		off = 0
	}
	return s.updateOne(`UPDATE instances SET auto_update_off=? WHERE id=?`, off, id)
}

// SetInstanceUpdateCheck stores what an update check found. The zero
// UpdateCheck clears it.
func (s *Store) SetInstanceUpdateCheck(id string, c UpdateCheck) error {
	wants, err := encodeWants(c.Wants)
	if err != nil {
		return err
	}
	return s.updateOne(`UPDATE instances SET update_version=?, update_status=?, update_reason=?, update_wants=? WHERE id=?`,
		c.Version, c.Status, c.Reason, wants, id)
}

// ApproveInstanceUpdate stores the owner's yes to an update that asks for
// more access, and clears a decline: the yes replaces it (#633).
func (s *Store) ApproveInstanceUpdate(id string, a UpdateApproval) error {
	at := int64(0)
	if !a.At.IsZero() {
		at = a.At.Unix()
	}
	return s.updateOne(`UPDATE instances SET update_approved_version=?, update_approved_access=?, update_approved_by=?, update_approved_at=?,
		update_declined_version='' WHERE id=?`, a.Version, a.Access, a.By, at, id)
}

// DeclineInstanceUpdate stores the owner's "Keep current version" for one
// version (#633). The prompt does not ask about that version again; the app's
// settings page still offers it.
func (s *Store) DeclineInstanceUpdate(id, version string) error {
	return s.updateOne(`UPDATE instances SET update_declined_version=? WHERE id=?`, version, id)
}

// encodeWants is the update_wants column: ” for none, else JSON.
func encodeWants(w []AccessWant) (string, error) {
	if len(w) == 0 {
		return "", nil
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("encode update wants: %w", err)
	}
	return string(b), nil
}

func decodeWants(s string) ([]AccessWant, error) {
	if s == "" {
		return nil, nil
	}
	var w []AccessWant
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return nil, fmt.Errorf("decode update wants: %w", err)
	}
	return w, nil
}

// RecordInstanceUpdateFailure stores the last update that did not go through
// and counts it: one more failure in a row when the last one was the same
// version, else the first. f.Count is not read.
func (s *Store) RecordInstanceUpdateFailure(id string, f UpdateFailure) error {
	at := int64(0)
	if !f.At.IsZero() {
		at = f.At.Unix()
	}
	// SQLite reads every column in SET from the row as it was, so the CASE
	// compares with the version of the last failure, not the new one.
	return s.updateOne(`UPDATE instances SET update_failed_version=?, update_failed_kind=?, update_failed_message=?, update_failed_at=?,
		update_failed_count = CASE WHEN update_failed_version=? THEN update_failed_count+1 ELSE 1 END,
		update_failed_auto=0, update_failed_acked=0 WHERE id=?`,
		f.Version, f.Kind, f.Message, at, f.Version, id)
}

// FinishInstanceUpdate records a good update in one write: the new version
// and name, the state running, no known update and no failure, so no
// failure in a row either. The owner's approval and decline go too: they were
// about the installed version's access, which has just changed (#633). last
// is what a person can roll back to (#634).
func (s *Store) FinishInstanceUpdate(id, version, name string, last LastUpdate) error {
	return s.updateOne(`UPDATE instances SET version=?, name=?, state='running',
		update_version='', update_status='', update_reason='', update_wants='',
		update_failed_version='', update_failed_kind='', update_failed_message='', update_failed_at=0, update_failed_count=0,
		update_failed_auto=0, update_failed_acked=0,
		update_approved_version='', update_approved_access='', update_approved_by='', update_approved_at=0, update_declined_version='',
		update_last_from=?, update_last_at=?, update_last_auto=?
		WHERE id=?`, version, name, last.From, unixOrZero(last.At), boolInt(last.ByWindow), id)
}

// FinishInstanceRollback records a rollback by hand in one write (#634): the
// version and name the instance runs again, nothing left to roll back to,
// and its automatic update off, so the nightly window does not apply the
// version again. The version it left is stored as declined, so the update
// prompt does not ask about it again. The update check is cleared; the next
// check finds the catalog's version again.
func (s *Store) FinishInstanceRollback(id, version, name, left string) error {
	return s.updateOne(`UPDATE instances SET version=?, name=?, state='running', auto_update_off=1,
		update_version='', update_status='', update_reason='', update_wants='',
		update_approved_version='', update_approved_access='', update_approved_by='', update_approved_at=0, update_declined_version=?,
		update_last_from='', update_last_at=0, update_last_auto=0
		WHERE id=?`, version, name, left, id)
}

// ClearInstanceLastUpdate forgets the last good update: its 7 days are over,
// or what a rollback needs is gone (#634).
func (s *Store) ClearInstanceLastUpdate(id string) error {
	return s.updateOne(`UPDATE instances SET update_last_from='', update_last_at=0, update_last_auto=0 WHERE id=?`, id)
}

// MarkInstanceUpdateByWindow says the nightly window made the instance's last
// update: the good one when failed is false, else the failure on the row
// (#634). Called right after the window's update ends.
func (s *Store) MarkInstanceUpdateByWindow(id string, failed bool) error {
	if failed {
		return s.updateOne(`UPDATE instances SET update_failed_auto=1 WHERE id=? AND update_failed_version!=''`, id)
	}
	return s.updateOne(`UPDATE instances SET update_last_auto=1 WHERE id=? AND update_last_from!=''`, id)
}

// AckInstanceUpdateFailure closes the tile's banner about the last failed
// update (#634). The failure stays on the row and on the app's page.
func (s *Store) AckInstanceUpdateFailure(id string) error {
	return s.updateOne(`UPDATE instances SET update_failed_acked=1 WHERE id=?`, id)
}

// UpdatesSeen is when a person last saw the dashboard's toast about the apps
// updated overnight (#634). Zero when they never did.
func (s *Store) UpdatesSeen(userID string) (time.Time, error) {
	var at int64
	err := s.db.QueryRow(`SELECT updates_seen_at FROM users WHERE id=?`, userID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil || at == 0 {
		return time.Time{}, err
	}
	return time.Unix(at, 0), nil
}

// SetUpdatesSeen stores when a person saw the toast, so it shows once.
func (s *Store) SetUpdatesSeen(userID string, at time.Time) error {
	return s.updateOne(`UPDATE users SET updates_seen_at=? WHERE id=?`, unixOrZero(at), userID)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) updateOne(q string, args ...any) error {
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInstancePrevImages replaces the previous generation's image pins of an
// instance (APP_LIFECYCLE.md # update + rollback). nil or empty clears them.
func (s *Store) SetInstancePrevImages(instanceID string, images []InstanceImage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM instance_images_prev WHERE instance_id=?`, instanceID); err != nil {
		return err
	}
	for _, img := range images {
		if _, err := tx.Exec(
			`INSERT INTO instance_images_prev (instance_id, service, image, digest, ref) VALUES (?,?,?,?,?)`,
			instanceID, img.Service, img.Image, img.Digest, img.Ref); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetInstancePrevImages returns the previous generation's image pins, ordered
// by service name. Empty when the instance keeps no previous generation.
func (s *Store) GetInstancePrevImages(instanceID string) ([]InstanceImage, error) {
	rows, err := s.db.Query(
		`SELECT service, image, digest, ref FROM instance_images_prev
		 WHERE instance_id=? ORDER BY service`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstanceImage
	for rows.Next() {
		var i InstanceImage
		if err := rows.Scan(&i.Service, &i.Image, &i.Digest, &i.Ref); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RetiredServiceGrant is a per-app database an update moved off to a new
// major version (#635, SERVICE_PROVISIONING.md # At app update, different
// major version). It stays on the old major for the 7 days of the rollback by
// hand: a rollback points the app at it again. Then the prune drops it.
type RetiredServiceGrant struct {
	ServiceGrant
	RetiredAt time.Time
}

// MoveServiceGrant points an instance's grant at another version of its
// service, and keeps the grant as it was as a retired grant, in one
// transaction. The names and the password do not change: the database on the
// new version was made with them. ErrNotFound when there is no such grant.
func (s *Store) MoveServiceGrant(instanceID, logicalName, toVersion string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var g ServiceGrant
	err = tx.QueryRow(`SELECT kind, version, db_name, role_name, password FROM service_grants
		WHERE instance_id=? AND logical_name=?`, instanceID, logicalName).
		Scan(&g.Kind, &g.Version, &g.DBName, &g.RoleName, &g.Password)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if g.Version == toVersion {
		return tx.Commit()
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO service_grants_retired
		(instance_id, logical_name, kind, version, db_name, role_name, password, retired_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		instanceID, logicalName, g.Kind, g.Version, g.DBName, g.RoleName, g.Password, at.Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE service_grants SET version=? WHERE instance_id=? AND logical_name=?`,
		toVersion, instanceID, logicalName); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveServiceGrantBack points an instance's grant at fromVersion again, and
// removes the retired grant of that version, in one transaction. It can run
// again: a grant already on fromVersion stays there.
func (s *Store) MoveServiceGrantBack(instanceID, logicalName, fromVersion string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE service_grants SET version=? WHERE instance_id=? AND logical_name=?`,
		fromVersion, instanceID, logicalName); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM service_grants_retired WHERE instance_id=? AND logical_name=? AND version=?`,
		instanceID, logicalName, fromVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// GetRetiredServiceGrants returns an instance's retired grants, by logical
// name and version.
func (s *Store) GetRetiredServiceGrants(instanceID string) ([]RetiredServiceGrant, error) {
	rows, err := s.db.Query(`SELECT logical_name, kind, version, db_name, role_name, password, retired_at
		FROM service_grants_retired WHERE instance_id=? ORDER BY logical_name, version`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetiredServiceGrant
	for rows.Next() {
		var g RetiredServiceGrant
		var at int64
		if err := rows.Scan(&g.LogicalName, &g.Kind, &g.Version, &g.DBName, &g.RoleName, &g.Password, &at); err != nil {
			return nil, err
		}
		g.RetiredAt = time.Unix(at, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteRetiredServiceGrant removes one retired grant, once its database is
// dropped.
func (s *Store) DeleteRetiredServiceGrant(instanceID, logicalName, version string) error {
	_, err := s.db.Exec(`DELETE FROM service_grants_retired WHERE instance_id=? AND logical_name=? AND version=?`,
		instanceID, logicalName, version)
	return err
}

// ServiceNamesTaken reports whether any grant, current or retired, of any
// instance holds dbName or roleName on that kind and version of a service. A
// database move keeps its names, and must never drop or reuse a database
// another grant holds (#635).
func (s *Store) ServiceNamesTaken(kind, version, dbName, roleName string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM service_grants WHERE kind=? AND version=? AND (db_name=? OR role_name=?)) +
		(SELECT COUNT(*) FROM service_grants_retired WHERE kind=? AND version=? AND (db_name=? OR role_name=?))`,
		kind, version, dbName, roleName, kind, version, dbName, roleName).Scan(&n)
	return n > 0, err
}
