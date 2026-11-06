package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestAutoUpdateMigratesOnForAnOlderDatabase opens an instances table from
// before the update columns, the way a running box has it. Automatic update
// is on for such an instance, and it has no update and no failure (#631).
func TestAutoUpdateMigratesOnForAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := old.Exec(`
		CREATE TABLE instances (
			id          TEXT PRIMARY KEY,
			manifest_id TEXT NOT NULL,
			name        TEXT NOT NULL,
			slug        TEXT NOT NULL UNIQUE,
			version     TEXT NOT NULL,
			state       TEXT NOT NULL,
			mdns_name   TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL
		);
		CREATE TABLE instance_images (
			instance_id TEXT NOT NULL,
			service     TEXT NOT NULL,
			image       TEXT NOT NULL,
			digest      TEXT NOT NULL,
			PRIMARY KEY (instance_id, service)
		);
		INSERT INTO instances (id, manifest_id, name, slug, version, state, created_at)
		VALUES ('i_1','whoami','Whoami','whoami','1','running',1700000000);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store on the old db: %v", err)
	}
	defer s.Close()
	row, err := s.Get("i_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.AutoUpdateOff {
		t.Fatal("migrated row has automatic update off, want on")
	}
	if !row.Update.IsZero() || row.UpdateFailure != (UpdateFailure{}) {
		t.Fatalf("migrated row has update %+v and failure %+v, want none", row.Update, row.UpdateFailure)
	}
	if prev, err := s.GetInstancePrevImages("i_1"); err != nil || len(prev) != 0 {
		t.Fatalf("prev images = %v, %v; want none", prev, err)
	}
}

// TestUpdateColumnsRoundTrip writes each update field and reads it back, and
// checks FinishInstanceUpdate clears the check and the failure.
func TestUpdateColumnsRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if row, _ := s.Get("a"); row.AutoUpdateOff {
		t.Fatal("fresh row has automatic update off")
	}
	if err := s.SetInstanceAutoUpdate("a", false); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Get("a"); !row.AutoUpdateOff {
		t.Fatal("automatic update still on after turning it off")
	}
	check := UpdateCheck{Version: "2.0", Status: UpdateNeedsApproval, Reason: "wants more access"}
	if err := s.SetInstanceUpdateCheck("a", check); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_700_000_500, 0)
	fail := UpdateFailure{Version: "1.9", Kind: UpdateFailedHealth, Message: "did not start", At: at}
	if err := s.RecordInstanceUpdateFailure("a", fail); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Get("a")
	if !row.Update.Equal(check) {
		t.Fatalf("update = %+v, want %+v", row.Update, check)
	}
	if row.UpdateFailure.Version != "1.9" || row.UpdateFailure.Kind != UpdateFailedHealth || !row.UpdateFailure.At.Equal(at) {
		t.Fatalf("failure = %+v", row.UpdateFailure)
	}
	if err := s.FinishInstanceUpdate("a", "2.0", "Whoami 2"); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get("a")
	if row.Version != "2.0" || row.Name != "Whoami 2" || row.State != "running" {
		t.Fatalf("after finish: version %q name %q state %q", row.Version, row.Name, row.State)
	}
	if !row.Update.IsZero() || row.UpdateFailure != (UpdateFailure{}) {
		t.Fatalf("after finish: update %+v failure %+v, want none", row.Update, row.UpdateFailure)
	}
	if row.AutoUpdateOff != true {
		t.Fatal("finish changed the automatic update setting")
	}
	if err := s.SetInstanceAutoUpdate("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: %v, want ErrNotFound", err)
	}
}

// TestPrevImagesCascade keeps a previous generation and checks that deleting
// the instance removes it too, so uninstall leaves no stray rows.
func TestPrevImagesCascade(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatal(err)
	}
	prev := []InstanceImage{{Service: "web", Image: "nginx:1", Digest: "sha256:aa", Ref: "mirror/nginx@sha256:aa"}}
	if err := s.SetInstancePrevImages("a", prev); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInstancePrevImages("a")
	if err != nil || len(got) != 1 || got[0] != prev[0] {
		t.Fatalf("prev = %v, %v", got, err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetInstancePrevImages("a"); len(got) != 0 {
		t.Fatalf("prev pins survived the delete: %v", got)
	}
}

// TestNightlyColumnsMigrateForAnOlderDatabase opens an instances table with
// #631's update columns and none of #632's, the way a box running #631 has
// it. The instance has no try by the window and no failure in a row, so the
// window may try it tonight and it is not paused (#632).
func TestNightlyColumnsMigrateForAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := old.Exec(`
		CREATE TABLE instances (
			id          TEXT PRIMARY KEY,
			manifest_id TEXT NOT NULL,
			name        TEXT NOT NULL,
			slug        TEXT NOT NULL UNIQUE,
			version     TEXT NOT NULL,
			state       TEXT NOT NULL,
			mdns_name   TEXT NOT NULL DEFAULT '',
			auto_update_off INTEGER NOT NULL DEFAULT 0,
			update_version TEXT NOT NULL DEFAULT '',
			update_status TEXT NOT NULL DEFAULT '',
			update_reason TEXT NOT NULL DEFAULT '',
			update_failed_version TEXT NOT NULL DEFAULT '',
			update_failed_kind TEXT NOT NULL DEFAULT '',
			update_failed_message TEXT NOT NULL DEFAULT '',
			update_failed_at INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL
		);
		INSERT INTO instances (id, manifest_id, name, slug, version, state, update_version, update_status,
			update_failed_version, update_failed_kind, update_failed_at, created_at)
		VALUES ('i_1','whoami','Whoami','whoami','1','running','2','available','2','health',1700000100,1700000000);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store on the old db: %v", err)
	}
	defer s.Close()
	row, err := s.Get("i_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.UpdateTry != (UpdateTry{}) || row.UpdateFailure.Count != 0 || row.UpdatePaused() {
		t.Fatalf("migrated row: try %+v, count %d, paused %v; want no try, no count, not paused",
			row.UpdateTry, row.UpdateFailure.Count, row.UpdatePaused())
	}
	// The old failure is kept, and the next failure of the same version is
	// the first one counted.
	if err := s.RecordInstanceUpdateFailure("i_1", UpdateFailure{Version: "2", Kind: UpdateFailedHealth}); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Get("i_1"); row.UpdateFailure.Count != 1 {
		t.Fatalf("count after one failure on an old row = %d, want 1", row.UpdateFailure.Count)
	}
}

// TestUpdateFailCount counts failures in a row on one version: another
// version starts again at 1, a reset and a good update set it to 0, and the
// pause comes at UpdatePauseAfter on the version the box offers.
func TestUpdateFailCount(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstanceUpdateCheck("a", UpdateCheck{Version: "2.0", Status: UpdateAvailable}); err != nil {
		t.Fatal(err)
	}
	fail := func(v string) Instance {
		t.Helper()
		if err := s.RecordInstanceUpdateFailure("a", UpdateFailure{Version: v, Kind: UpdateFailedHealth, Message: "x"}); err != nil {
			t.Fatal(err)
		}
		row, _ := s.Get("a")
		return row
	}
	if row := fail("2.0"); row.UpdateFailure.Count != 1 || row.UpdatePaused() {
		t.Fatalf("one failure: count %d paused %v", row.UpdateFailure.Count, row.UpdatePaused())
	}
	fail("2.0")
	if row := fail("2.0"); row.UpdateFailure.Count != 3 || !row.UpdatePaused() {
		t.Fatalf("three failures: count %d paused %v, want 3 and paused", row.UpdateFailure.Count, row.UpdatePaused())
	}
	// A new catalog version is not paused, and its first failure counts 1.
	if err := s.SetInstanceUpdateCheck("a", UpdateCheck{Version: "2.1", Status: UpdateAvailable}); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.Get("a"); row.UpdatePaused() {
		t.Fatal("a new version is paused")
	}
	if row := fail("2.1"); row.UpdateFailure.Count != 1 {
		t.Fatalf("first failure of a new version: count %d, want 1", row.UpdateFailure.Count)
	}
	fail("2.1")
	if err := s.ResetInstanceUpdateFailCount("a"); err != nil {
		t.Fatal(err)
	}
	if row := fail("2.1"); row.UpdateFailure.Count != 1 {
		t.Fatalf("a failure after a reset: count %d, want 1", row.UpdateFailure.Count)
	}
	if err := s.RecordInstanceUpdateTry("a", "2.1", "2026-11-07"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishInstanceUpdate("a", "2.1", "Alpha"); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Get("a")
	if row.UpdateFailure.Count != 0 || row.UpdateTry != (UpdateTry{Version: "2.1", Night: "2026-11-07"}) {
		t.Fatalf("after a good update: count %d try %+v; want 0 and the try kept", row.UpdateFailure.Count, row.UpdateTry)
	}
	if err := s.RecordInstanceUpdateTry("missing", "1", "2026-11-07"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: %v, want ErrNotFound", err)
	}
}

// TestUpdateApprovalColumnsRoundTrip writes the access a version asks for,
// the owner's approval and decline (#633), reads them back, and checks that a
// good update clears all of them.
func TestUpdateApprovalColumnsRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if row, _ := s.Get("a"); row.UpdateApproval != (UpdateApproval{}) || row.UpdateDeclined != "" || row.Update.Wants != nil {
		t.Fatalf("fresh row: approval %+v declined %q wants %v", row.UpdateApproval, row.UpdateDeclined, row.Update.Wants)
	}
	check := UpdateCheck{Version: "2.0", Status: UpdateNeedsApproval, Reason: "asks for more", Wants: []AccessWant{
		{Kind: WantInternet},
		{Kind: WantFolder, Folder: "movies", Mode: "write", Change: FolderNew, Source: "personal"},
		{Kind: WantPublicPath, Path: "/share/"},
	}}
	if err := s.SetInstanceUpdateCheck("a", check); err != nil {
		t.Fatal(err)
	}
	if err := s.DeclineInstanceUpdate("a", "2.0"); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Get("a")
	if !row.Update.Equal(check) || row.UpdateDeclined != "2.0" {
		t.Fatalf("update %+v declined %q", row.Update, row.UpdateDeclined)
	}
	at := time.Unix(1_700_000_900, 0)
	appr := UpdateApproval{Version: "2.0", Access: `{"permissions":{"internet":true}}`, By: "u_owner", At: at}
	if err := s.ApproveInstanceUpdate("a", appr); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get("a")
	if !row.UpdateApproval.Same(appr) {
		t.Fatalf("approval = %+v, want %+v", row.UpdateApproval, appr)
	}
	if row.UpdateDeclined != "" {
		t.Fatalf("an approval left the decline %q", row.UpdateDeclined)
	}
	if err := s.DeclineInstanceUpdate("a", "2.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishInstanceUpdate("a", "2.0", "Alpha"); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get("a")
	if row.UpdateApproval != (UpdateApproval{}) || row.UpdateDeclined != "" || !row.Update.IsZero() {
		t.Fatalf("after finish: approval %+v declined %q update %+v, want none", row.UpdateApproval, row.UpdateDeclined, row.Update)
	}
	if err := s.ApproveInstanceUpdate("missing", appr); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: %v, want ErrNotFound", err)
	}
}

// TestApprovalColumnsMigrateOnAnOlderDatabase opens an instances and a
// sessions table from before #633: the row has no wants, approval or decline,
// and the session has not closed the prompt.
func TestApprovalColumnsMigrateOnAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := old.Exec(`
		CREATE TABLE instances (
			id          TEXT PRIMARY KEY,
			manifest_id TEXT NOT NULL,
			name        TEXT NOT NULL,
			slug        TEXT NOT NULL UNIQUE,
			version     TEXT NOT NULL,
			state       TEXT NOT NULL,
			mdns_name   TEXT NOT NULL DEFAULT '',
			update_version TEXT NOT NULL DEFAULT '',
			update_status TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL
		);
		CREATE TABLE users (
			id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
			recovery_hash TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
		);
		CREATE TABLE sessions (
			token TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL
		);
		INSERT INTO instances (id, manifest_id, name, slug, version, state, update_version, update_status, created_at)
		VALUES ('i_1','whoami','Whoami','whoami','1','running','2','needs_approval',1700000000);
		INSERT INTO users (id, username, role, created_at) VALUES ('u_1','alex','member',1700000000);
		INSERT INTO sessions (token, user_id, created_at, last_seen_at) VALUES ('tok','u_1',1700000000,1700000000);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store on the old db: %v", err)
	}
	defer s.Close()
	row, err := s.Get("i_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Update.Status != UpdateNeedsApproval || row.Update.Wants != nil || row.UpdateApproval != (UpdateApproval{}) || row.UpdateDeclined != "" {
		t.Fatalf("migrated row: update %+v approval %+v declined %q", row.Update, row.UpdateApproval, row.UpdateDeclined)
	}
	sess, err := s.GetSession("tok")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.UpdatePromptDismissed {
		t.Fatal("a session from before the column has closed the prompt")
	}
}

// TestDismissUpdatePromptIsPerSession: closing the prompt marks one session,
// and a new session (a new login) does not have the mark.
func TestDismissUpdatePromptIsPerSession(t *testing.T) {
	s := open(t)
	if err := s.CreateUser(User{ID: "u_1", Username: "alex", DisplayName: "Alex", Role: RoleMember}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tok := range []string{"first", "second"} {
		if err := s.CreateSession(Session{Token: tok, UserID: "u_1", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DismissUpdatePrompt("first"); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.GetSession("first"); !sess.UpdatePromptDismissed {
		t.Fatal("the session that closed the prompt is not marked")
	}
	if sess, _ := s.GetSession("second"); sess.UpdatePromptDismissed {
		t.Fatal("another session is marked")
	}
	if err := s.DismissUpdatePrompt("gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v, want ErrNotFound", err)
	}
}
