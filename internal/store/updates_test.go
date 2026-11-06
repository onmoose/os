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
	if row.Update != (UpdateCheck{}) || row.UpdateFailure != (UpdateFailure{}) {
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
	if row.Update != check {
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
	if row.Update != (UpdateCheck{}) || row.UpdateFailure != (UpdateFailure{}) {
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
