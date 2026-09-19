package cpupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// The WAL sidecar is the whole reason this is not a one-file copy: the brain
// opens SQLite with journal_mode=WAL, so recent commits can live in -wal and
// not yet be in moose.db. A snapshot that took only the main file would restore
// a database missing its newest writes — and would look fine, because what it
// produced is a valid older database.
func TestSnapshotAndRestoreCarryTheWAL(t *testing.T) {
	stateDir, snapDir := t.TempDir(), filepath.Join(t.TempDir(), "snap")
	write(t, stateDir, "moose.db", "DB-v1")
	write(t, stateDir, "moose.db-wal", "WAL-v1")

	if err := snapshotBrainDB(stateDir, snapDir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// The new brain migrates and checkpoints: the main file changes and the WAL
	// is replaced.
	write(t, stateDir, "moose.db", "DB-v2-migrated")
	write(t, stateDir, "moose.db-wal", "WAL-v2")

	if err := restoreBrainDB(snapDir, stateDir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := read(t, stateDir, "moose.db"); got != "DB-v1" {
		t.Errorf("database = %q, want DB-v1", got)
	}
	if got := read(t, stateDir, "moose.db-wal"); got != "WAL-v1" {
		t.Errorf("wal = %q, want WAL-v1", got)
	}
}

// A -wal the snapshot never captured must be deleted, not left behind: SQLite
// replays it on the next open, so a stale one describes writes the restored
// database never saw.
func TestRestoreRemovesASidecarTheSnapshotDidNotHave(t *testing.T) {
	stateDir, snapDir := t.TempDir(), filepath.Join(t.TempDir(), "snap")
	write(t, stateDir, "moose.db", "DB-v1")

	if err := snapshotBrainDB(stateDir, snapDir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	write(t, stateDir, "moose.db-wal", "WAL-WRITTEN-BY-THE-NEW-BRAIN")

	if err := restoreBrainDB(snapDir, stateDir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "moose.db-wal")); !os.IsNotExist(err) {
		t.Error("a WAL the snapshot never captured survived the restore")
	}
}

// A box that has never run a brain has no database. Neither direction is an
// error: there is nothing a failed update could lose.
func TestSnapshotAndRestoreWithNoDatabase(t *testing.T) {
	stateDir, snapDir := t.TempDir(), filepath.Join(t.TempDir(), "snap")
	if err := snapshotBrainDB(stateDir, snapDir); err != nil {
		t.Errorf("snapshot with no database: %v", err)
	}
	if err := restoreBrainDB(snapDir, stateDir); err != nil {
		t.Errorf("restore with no snapshot: %v", err)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// The snapshot dir is named after the brain ref, so a retried update reuses it.
// A `-wal` left by an earlier attempt must not survive into the new snapshot:
// the database has since been checkpointed and no longer has one, so restoring
// the pair would put back a merge of two generations — the exact thing
// restoreBrainDB refuses to do.
func TestSnapshotClearsAnEarlierAttemptsFiles(t *testing.T) {
	stateDir, snapDir := t.TempDir(), filepath.Join(t.TempDir(), "snap")
	write(t, stateDir, "moose.db", "DB-v1")
	write(t, stateDir, "moose.db-wal", "WAL-FROM-THE-FIRST-ATTEMPT")
	if err := snapshotBrainDB(stateDir, snapDir); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}

	// The update failed and reverted; the brain ran on, checkpointed, and no
	// longer has a WAL. The admin retries.
	if err := os.Remove(filepath.Join(stateDir, "moose.db-wal")); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	write(t, stateDir, "moose.db", "DB-v1-plus-more-writes")
	if err := snapshotBrainDB(stateDir, snapDir); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}

	if _, err := os.Stat(filepath.Join(snapDir, "moose.db-wal")); !os.IsNotExist(err) {
		t.Error("a WAL from the earlier attempt survived into the new snapshot")
	}
	if got := read(t, snapDir, "moose.db"); got != "DB-v1-plus-more-writes" {
		t.Errorf("snapshot database = %q, want the current one", got)
	}
}
