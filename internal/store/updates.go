package store

// App updates (UPDATES.md # 4, APP_LIFECYCLE.md # update + rollback, #631).
// Three things are kept per instance: what the last update check found
// (UpdateCheck), the last update that did not go through (UpdateFailure), and
// the image pins of the previous generation (instance_images_prev), which a
// rollback puts back.

import (
	"encoding/json"
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
	FolderWhole = "whole" // one folder inside it becomes the whole folder
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
type UpdateFailure struct {
	Version string
	Kind    string
	Message string
	At      time.Time
	Count   int
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
		update_failed_count = CASE WHEN update_failed_version=? THEN update_failed_count+1 ELSE 1 END WHERE id=?`,
		f.Version, f.Kind, f.Message, at, f.Version, id)
}

// FinishInstanceUpdate records a good update in one write: the new version
// and name, the state running, no known update and no failure, so no
// failure in a row either. The owner's approval and decline go too: they were
// about the installed version's access, which has just changed (#633).
func (s *Store) FinishInstanceUpdate(id, version, name string) error {
	return s.updateOne(`UPDATE instances SET version=?, name=?, state='running',
		update_version='', update_status='', update_reason='', update_wants='',
		update_failed_version='', update_failed_kind='', update_failed_message='', update_failed_at=0, update_failed_count=0,
		update_approved_version='', update_approved_access='', update_approved_by='', update_approved_at=0, update_declined_version=''
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
