package store

// App updates (UPDATES.md # 4, APP_LIFECYCLE.md # update + rollback, #631).
// Three things are kept per instance: what the last update check found
// (UpdateCheck), the last update that did not go through (UpdateFailure), and
// the image pins of the previous generation (instance_images_prev), which a
// rollback puts back.

import (
	"time"
)

// Update check statuses. Empty means no update is known.
const (
	// UpdateAvailable is an update the box can apply: on its own in the
	// window (#632), or when a person asks.
	UpdateAvailable = "available"
	// UpdateNeedsApproval widens permissions or public paths; the owner must
	// approve it first (#633).
	UpdateNeedsApproval = "needs_approval"
	// UpdateServiceMajor moves a managed service to a new major version,
	// which needs a database migration the box does not do yet (#635).
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
type UpdateCheck struct {
	Version string
	Status  string
	Reason  string
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
type UpdateFailure struct {
	Version string
	Kind    string
	Message string
	At      time.Time
}

// SetInstanceAutoUpdate turns the automatic update of an app on or off
// (#631). #632's update window reads it.
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
	return s.updateOne(`UPDATE instances SET update_version=?, update_status=?, update_reason=? WHERE id=?`,
		c.Version, c.Status, c.Reason, id)
}

// RecordInstanceUpdateFailure stores the last update that did not go through.
func (s *Store) RecordInstanceUpdateFailure(id string, f UpdateFailure) error {
	at := int64(0)
	if !f.At.IsZero() {
		at = f.At.Unix()
	}
	return s.updateOne(`UPDATE instances SET update_failed_version=?, update_failed_kind=?, update_failed_message=?, update_failed_at=? WHERE id=?`,
		f.Version, f.Kind, f.Message, at, id)
}

// FinishInstanceUpdate records a good update in one write: the new version
// and name, the state running, no known update and no failure.
func (s *Store) FinishInstanceUpdate(id, version, name string) error {
	return s.updateOne(`UPDATE instances SET version=?, name=?, state='running',
		update_version='', update_status='', update_reason='',
		update_failed_version='', update_failed_kind='', update_failed_message='', update_failed_at=0
		WHERE id=?`, version, name, id)
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
