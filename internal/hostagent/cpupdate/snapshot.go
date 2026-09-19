package cpupdate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// brainDBFile is the brain's SQLite database inside its state directory
// (cmd/brain: store.Open(filepath.Join(stateDir, "moose.db"))).
const brainDBFile = "moose.db"

// sqliteSidecars are SQLite's write-ahead-log companions. The brain opens its
// database with `PRAGMA journal_mode=WAL` (internal/store), which means recent
// commits can live in `-wal` and not yet be in the main file. Copying only
// `moose.db` would therefore snapshot a database missing its newest writes —
// and would look completely fine, because the file it produced is a valid older
// database. They are copied when present and skipped when absent.
var sqliteSidecars = []string{"-wal", "-shm"}

// snapshotBrainDB copies the brain's SQLite database (and its WAL sidecars)
// into dstDir.
//
// **The caller must have stopped the brain first.** A copy taken while a writer
// is live can catch a torn page or a WAL mid-append, and the result is a backup
// that restores into a corrupt database — the failure surfaces only when it is
// used, which is during a failed update, which is the worst moment for the
// safety net to be the thing that breaks. Stopping first is why this is a plain
// file copy rather than an online-backup dance.
func snapshotBrainDB(stateDir, dstDir string) error {
	src := filepath.Join(stateDir, brainDBFile)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		// A box that has never run a brain has no database to protect. Not an
		// error: there is nothing a failed update could lose.
		return nil
	} else if err != nil {
		return fmt.Errorf("stat brain database: %w", err)
	}
	// Clear first. The dir is named after the brain ref, so the same one is
	// reused whenever the same pre-update ref comes round again — a retried
	// update after a failed one, most obviously. Writing into it would leave a
	// `-wal` from the earlier attempt beside a database that has since been
	// checkpointed and no longer has one, and the restore would then put back a
	// merge of two generations, which is exactly what restoreBrainDB exists to
	// prevent.
	if err := os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("clear snapshot dir: %w", err)
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	for _, name := range append([]string{brainDBFile}, sidecarNames()...) {
		from := filepath.Join(stateDir, name)
		if _, err := os.Stat(from); os.IsNotExist(err) {
			continue
		}
		if err := copyFile(from, filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return syncDir(dstDir)
}

// restoreBrainDB copies a snapshot back over the brain's state directory. Like
// the snapshot, it must run with the brain stopped.
//
// Sidecars absent from the snapshot are **removed** from the state directory
// rather than left behind: a stale `-wal` next to a restored database describes
// writes that database never saw, and SQLite would replay it on the next open.
// The restore has to leave the exact set of files it captured, not a merge of
// two generations.
func restoreBrainDB(srcDir, stateDir string) error {
	src := filepath.Join(srcDir, brainDBFile)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		// Nothing was captured (a box with no database when the update began),
		// so there is nothing to put back.
		return nil
	} else if err != nil {
		return fmt.Errorf("stat snapshot: %w", err)
	}
	if err := copyFile(src, filepath.Join(stateDir, brainDBFile)); err != nil {
		return err
	}
	for _, name := range sidecarNames() {
		from, to := filepath.Join(srcDir, name), filepath.Join(stateDir, name)
		if _, err := os.Stat(from); os.IsNotExist(err) {
			if err := os.Remove(to); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove stale %s: %w", to, err)
			}
			continue
		}
		if err := copyFile(from, to); err != nil {
			return err
		}
	}
	return syncDir(stateDir)
}

// syncDir fsyncs a directory so the entries created in it survive a power cut.
// copyFile syncs each file's contents, but a directory entry is its own write —
// without this a snapshot can come back empty, and restoreBrainDB reads a
// missing moose.db as "nothing to put back" rather than as a lost backup. The
// same reasoning as controlplane.writeFileAtomic.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s to sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

func sidecarNames() []string {
	names := make([]string, 0, len(sqliteSidecars))
	for _, s := range sqliteSidecars {
		names = append(names, brainDBFile+s)
	}
	return names
}

// copyFile writes src to dst and fsyncs it. The fsync is not ceremony: the next
// thing that happens after a snapshot is a container recreate, and the thing
// after a restore is a brain start — a copy still sitting in the page cache
// when the box loses power is not a backup.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	return out.Close()
}

// snapshotDirFor names the snapshot directory for the generation identified by
// ref. Image refs carry `/`, `:` and `@`, none of which can appear in a path
// component, so they are flattened. The name is a label for humans reading
// `ls`; the ledger is what actually maps a generation to its files.
func snapshotDirFor(root, ref string) string {
	safe := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(ref)
	return filepath.Join(root, safe)
}

// removeSnapshotDir deletes a retained generation's snapshot. A directory that
// was never created (the brain did not move in that generation) is not an
// error — there is nothing to collect.
func removeSnapshotDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove snapshot dir %s: %w", dir, err)
	}
	return nil
}
