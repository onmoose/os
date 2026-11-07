package lifecycle

// The pre-update snapshot (UPDATES.md # Pre-update snapshot, APP_LIFECYCLE.md #
// update + rollback). Before an app update the brain keeps a copy of the app's
// own data, so a failed update can put the old version back on its old data and
// not only on its old image.
//
// A snapshot is a set of files in the instance's snapshots/ dir, all named
// after the version the instance ran when it was taken:
//
//	pre-update-<version>.tar              the data volumes, minus the cache volumes
//	pre-update-<version>.<service>.pgdump a Postgres database (pg_dump custom format)
//	pre-update-<version>.<service>.sql    a MySQL or MariaDB database (mysqldump)
//	pre-update-<version>.json             what the set holds; written last
//
// The .json file is the mark that the set is complete. Restore refuses a set
// without it, and a set that failed half way is removed before the error is
// returned.
//
// The dumps run through the same throwaway client container as provisioning
// (services.go), so the brain never joins a service network. The dump streams
// out of the container's stdout into the file, and back into its stdin on
// restore, so no bind mount is needed and the client's remapped user never has
// to read or write the brain's files.
//
// Valkey has one keyspace shared by every app and no per-app database, so
// there is nothing per-app to dump. Its grants are listed as skipped.
//
// The caller holds the instance's lock and has stopped the app's containers:
// a tar of files that are still being written is not a copy anyone can trust.
// Snapshot and restore take no lock themselves, so the update transaction
// (#631) can call them inside its own. PruneSnapshots takes each instance's
// lock only when it is free.

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// SnapshotKeep is how long a snapshot is kept: the 7 days the rollback button
// works for (UPDATES.md # 4).
const SnapshotKeep = 7 * 24 * time.Hour

// snapshotMaxUsedPercent is how full the drive may be once the snapshot is written.
// It is the disk-full threshold (HEALTH.md # Detector catalog), so a snapshot
// never pushes the box into disk-full by itself.
const snapshotMaxUsedPercent = 95

// tarEntryOverhead is the bytes a tar entry adds on top of its content: a
// 512-byte header, up to 511 bytes of padding, and room for a PAX record.
const tarEntryOverhead = 1536

// ErrSnapshotNoSpace is returned when the drive has no room for the snapshot.
// The update must not go on (UPDATES.md # Pre-update snapshot: the update is
// deferred and the user is told).
var ErrSnapshotNoSpace = errors.New("there is not enough free space on the drive to keep a copy of this app's data before the update")

// ErrNoSnapshot is returned when there is no complete snapshot to restore.
var ErrNoSnapshot = errors.New("there is no saved copy of this app's data to restore")

// Snapshot describes one complete pre-update snapshot. It is the content of
// the set's .json file.
type Snapshot struct {
	// Version is the version the instance ran when the snapshot was taken.
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	// Data is the data volumes, as clean paths relative to the instance dir.
	// Absent is the ones of them that did not exist then: a restore removes
	// them.
	Data   []string `json:"data"`
	Absent []string `json:"absent,omitempty"`
	// Cache is the cache volumes left out of the tar. A restore leaves them as
	// they are.
	Cache []string `json:"cache,omitempty"`
	// Fallback is true when the manifest declares no data_volumes, so the
	// snapshot kept all of data/ and every other bind dir of the compose
	// (APP_MANIFEST.md # C, the Door-2 best effort).
	Fallback bool `json:"fallback,omitempty"`
	// Tar is the file name of the data tar in the snapshots dir.
	Tar   string         `json:"tar"`
	Dumps []SnapshotDump `json:"dumps,omitempty"`
	// Skipped is the managed services with no per-app dump (Valkey).
	Skipped []string `json:"skipped,omitempty"`
}

// SnapshotDump is one managed-service database dump in a snapshot.
type SnapshotDump struct {
	Service string `json:"service"` // the manifest's logical name
	Kind    string `json:"kind"`
	Version string `json:"version"`
	DBName  string `json:"db_name"`
	File    string `json:"file"`
}

func (m *Manager) snapshotsDir(id string) string {
	return filepath.Join(m.instanceDir(id), "snapshots")
}

// snapshotBase is the shared file-name stem of a snapshot set. The version is
// author text, so anything that is not a safe file-name character becomes _.
func snapshotBase(version string) string {
	var b strings.Builder
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "pre-update-" + b.String()
}

// TakeSnapshot keeps a copy of an instance's data before an update: a tar of
// its data volumes minus its cache volumes, and a dump of each managed-service
// database it has a grant for. It replaces any earlier snapshot of the same
// version. The caller holds the instance's lock and has stopped the app.
//
// Every failure is returned, and nothing of a half-written set is left behind:
// no snapshot, no update (UPDATES.md # Pre-update snapshot). ErrSnapshotNoSpace
// says the drive is too full.
func (m *Manager) TakeSnapshot(ctx context.Context, id string) (Snapshot, error) {
	inst, err := m.store.Get(id)
	if err != nil {
		return Snapshot{}, err
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read the app's manifest: %w", err)
	}
	data, cache, err := admission.StoragePaths(man.Storage)
	if err != nil {
		return Snapshot{}, fmt.Errorf("the app's storage list cannot be used for a snapshot: %w", err)
	}
	fallback := len(data) == 0
	if fallback {
		if data, err = m.fallbackDataPaths(id); err != nil {
			return Snapshot{}, err
		}
	}
	grants, err := m.store.GetServiceGrants(id)
	if err != nil {
		return Snapshot{}, err
	}

	dir := m.snapshotsDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create the snapshots folder: %w", err)
	}
	base := snapshotBase(inst.Version)
	// Drop an older set of the same version first, its .json first, so a crash
	// from here on never leaves a set that looks complete and is not.
	if err := removeSnapshotSet(dir, base); err != nil {
		return Snapshot{}, err
	}

	snap := Snapshot{
		Version: inst.Version, CreatedAt: time.Now().UTC(),
		Cache: cache, Fallback: fallback, Tar: base + ".tar",
	}
	ok := false
	defer func() {
		if ok {
			return
		}
		if err := removeSnapshotSet(dir, base); err != nil {
			slog.Warn("snapshot: could not remove a half-written snapshot", "instance_id", id, "snapshot", base, "dir", dir, "err", err)
		}
	}()

	instDir := m.instanceDir(id)
	size, absent, err := measureData(instDir, data, cache)
	if err != nil {
		return Snapshot{}, err
	}
	snap.Data, snap.Absent = data, absent
	// The dumps count too, measured ahead from each database's size, so a large
	// database cannot push the drive past the threshold half way through.
	for _, g := range grants {
		if g.Kind == "valkey" {
			continue
		}
		est, err := m.dumpEstimate(ctx, g)
		if err != nil {
			return Snapshot{}, fmt.Errorf("measure the %s database: %w", g.LogicalName, err)
		}
		size += est
	}
	if err := m.checkSnapshotSpace(dir, size); err != nil {
		return Snapshot{}, err
	}
	if err := writeFileSynced(filepath.Join(dir, snap.Tar), func(w io.Writer) error {
		return writeDataTar(ctx, w, instDir, data, cache)
	}); err != nil {
		return Snapshot{}, m.noSpaceOr(dir, fmt.Errorf("write the data snapshot: %w", err))
	}

	for _, g := range grants {
		if g.Kind == "valkey" {
			snap.Skipped = append(snap.Skipped, g.LogicalName)
			slog.Info("snapshot: no per-app dump for a shared keyspace", "instance_id", id, "service", g.LogicalName)
			continue
		}
		d := SnapshotDump{Service: g.LogicalName, Kind: g.Kind, Version: g.Version, DBName: g.DBName,
			File: base + "." + g.LogicalName + dumpExt(g.Kind)}
		if err := writeFileSynced(filepath.Join(dir, d.File), func(w io.Writer) error {
			return m.dumpDatabase(ctx, g, w)
		}); err != nil {
			return Snapshot{}, m.noSpaceOr(dir, fmt.Errorf("dump the %s database: %w", g.LogicalName, err))
		}
		// The estimate can be low (a MySQL dump is text). Stop as soon as the
		// drive is past the threshold, and say it is full.
		if err := m.checkSnapshotSpace(dir, 0); err != nil {
			return Snapshot{}, err
		}
		snap.Dumps = append(snap.Dumps, d)
	}

	meta, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return Snapshot{}, err
	}
	if err := writeFileSynced(filepath.Join(dir, base+".json"), func(w io.Writer) error {
		_, err := w.Write(meta)
		return err
	}); err != nil {
		return Snapshot{}, fmt.Errorf("write the snapshot record: %w", err)
	}
	ok = true
	slog.Info("snapshot taken", "instance_id", id, "snapshot", base, "dir", dir)
	return snap, nil
}

// RestoreSnapshot puts an instance's data back as the snapshot of version
// found it: every data volume holds exactly what the tar holds, so files added
// since are removed, owners, modes and times are as they were, and the cache
// volumes are left alone. Each dumped database is dropped, made again and
// filled from its dump. The caller holds the instance's lock and has stopped
// the app.
//
// It is safe to run again after a failure: it clears and refills the same
// paths from the same files, which it never changes.
func (m *Manager) RestoreSnapshot(ctx context.Context, id, version string) error {
	dir := m.snapshotsDir(id)
	snap, err := readSnapshot(dir, snapshotBase(version))
	if err != nil {
		return err
	}
	grants, err := m.store.GetServiceGrants(id)
	if err != nil {
		return err
	}
	byName := map[string]store.ServiceGrant{}
	for _, g := range grants {
		byName[g.LogicalName] = g
	}
	// Check every file and grant before anything is changed.
	if _, err := os.Stat(filepath.Join(dir, snap.Tar)); err != nil {
		return fmt.Errorf("the saved copy of the app's data is missing: %w", err)
	}
	for _, d := range snap.Dumps {
		if _, err := os.Stat(filepath.Join(dir, d.File)); err != nil {
			return fmt.Errorf("the saved copy of the %s database is missing: %w", d.Service, err)
		}
		g, ok := byName[d.Service]
		if !ok || g.Kind != d.Kind || g.Version != d.Version || g.DBName != d.DBName {
			return fmt.Errorf("the %s database is not the one the snapshot was taken of", d.Service)
		}
	}

	// Read the whole tar once before the data is cleared. A tar that is
	// damaged on disk then fails here, while the app's data is still there.
	if err := checkDataTar(ctx, filepath.Join(dir, snap.Tar), snap); err != nil {
		return fmt.Errorf("the saved copy of the app's data cannot be read: %w", err)
	}

	instDir := m.instanceDir(id)
	for _, p := range snap.Data {
		if err := clearDataPath(instDir, p, cachesUnder(p, snap.Cache), slices.Contains(snap.Absent, p)); err != nil {
			return fmt.Errorf("clear %s before the restore: %w", p, err)
		}
	}
	if err := m.extractDataTar(ctx, filepath.Join(dir, snap.Tar), instDir, snap); err != nil {
		return fmt.Errorf("restore the app's data: %w", err)
	}

	var errs []error
	for _, d := range snap.Dumps {
		if err := m.restoreDatabase(ctx, byName[d.Service], filepath.Join(dir, d.File)); err != nil {
			errs = append(errs, fmt.Errorf("restore the %s database: %w", d.Service, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	slog.Info("snapshot restored", "instance_id", id, "snapshot", snapshotBase(version))
	return nil
}

// PruneSnapshots removes every snapshot older than SnapshotKeep, and any loose
// snapshot file that old that no complete set names (a set a crash left half
// written). It also removes the previous generation an update kept, and its
// images, once it can no longer be rolled back to (pruneRollbackPoint). It skips an instance whose lock is held, so it never removes a
// snapshot an update or a rollback is using; the next pass gets it. It returns
// how many files it removed. Errors are logged, never returned: the next pass
// tries again.
func (m *Manager) PruneSnapshots(now time.Time) int {
	insts, err := m.store.List()
	if err != nil {
		slog.Warn("snapshot prune: list instances", "err", err)
		return 0
	}
	removed := 0
	for _, inst := range insts {
		unlock, ok := m.tryLockInstance(inst.ID)
		if !ok {
			continue
		}
		removed += pruneSnapshotDir(m.snapshotsDir(inst.ID), now)
		// The previous generation and its images go with the snapshot, so
		// the rollback button goes when what it needs goes (#634).
		m.pruneRollbackPoint(inst, now)
		unlock()
	}
	return removed
}

func pruneSnapshotDir(dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("snapshot prune: read dir", "dir", dir, "err", err)
		}
		return 0
	}
	removed := 0
	remove := func(name string) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("snapshot prune: remove", "src", filepath.Join(dir, name), "err", err)
			return
		}
		removed++
	}
	// Complete sets first, by the time in their record.
	kept := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "pre-update-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		snap, err := readSnapshot(dir, strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue // unreadable record: its files go by age below
		}
		if now.Sub(snap.CreatedAt) < SnapshotKeep {
			kept[name], kept[snap.Tar] = true, true
			for _, d := range snap.Dumps {
				kept[d.File] = true
			}
			continue
		}
		remove(name) // the record first, so the set is no longer complete
		remove(snap.Tar)
		for _, d := range snap.Dumps {
			remove(d.File)
		}
	}
	// Then anything left that no kept set names, by its own age.
	entries, err = os.ReadDir(dir)
	if err != nil {
		return removed
	}
	for _, e := range entries {
		if kept[e.Name()] || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) >= SnapshotKeep {
			remove(e.Name())
		}
	}
	return removed
}

// fallbackDataPaths is what a snapshot keeps when the manifest declares no
// data_volumes: all of data/, and every other relative bind dir of the compose,
// since an author can bind ./config next to ./data (APP_MANIFEST.md # C: the
// best effort over everything the app stores). A bind dir that names a file the
// brain keeps is left out, and one inside another is folded into it.
func (m *Manager) fallbackDataPaths(id string) ([]string, error) {
	composeBytes, err := os.ReadFile(m.composeFile(id))
	if err != nil {
		return nil, fmt.Errorf("read the app's compose file: %w", err)
	}
	dirs, err := relativeBindDirs(composeBytes)
	if err != nil {
		return nil, fmt.Errorf("read the app's compose file: %w", err)
	}
	cands := []string{"data"}
	for _, d := range dirs {
		if p, err := manifest.CleanStoragePath(d); err == nil {
			cands = append(cands, p)
		}
	}
	slices.Sort(cands)
	var out []string
	for _, c := range cands {
		if len(out) > 0 && manifest.PathWithin(c, out[len(out)-1]) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// cachesUnder lists the cache volumes inside data path p.
func cachesUnder(p string, cache []string) []string {
	var out []string
	for _, c := range cache {
		if manifest.PathWithin(c, p) && c != p {
			out = append(out, c)
		}
	}
	return out
}

func inAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if manifest.PathWithin(p, d) {
			return true
		}
	}
	return false
}

// checkParents refuses a path whose parent folders, between the instance dir
// and the path, hold a symlink: following it would read or write outside the
// app's folder. A missing parent is fine: the path is then missing too.
func checkParents(instDir, rel string) error {
	parts := strings.Split(rel, "/")
	cur := instDir
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s goes through %s, which is not a folder", rel, strings.TrimPrefix(cur, instDir+string(filepath.Separator)))
		}
	}
	return nil
}

// measureData sums the bytes the tar will hold and lists the data paths that
// do not exist. It walks the same tree writeDataTar does.
func measureData(instDir string, data, cache []string) (int64, []string, error) {
	var size int64
	var absent []string
	for _, p := range data {
		if err := checkParents(instDir, p); err != nil {
			return 0, nil, err
		}
		root := filepath.Join(instDir, filepath.FromSlash(p))
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			absent = append(absent, p)
			continue
		}
		err := filepath.WalkDir(root, func(abs string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(abs, instDir+string(filepath.Separator)))
			if inAny(rel, cache) {
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			size += tarEntryOverhead
			if e.Type().IsRegular() {
				info, err := e.Info()
				if err != nil {
					return err
				}
				size += info.Size()
			}
			return nil
		})
		if err != nil {
			return 0, nil, fmt.Errorf("read %s: %w", p, err)
		}
	}
	return size, absent, nil
}

// checkSnapshotSpace refuses a snapshot that would leave the drive past the
// disk-full threshold. When the space cannot be read it lets the snapshot go
// on: a write to a full disk still fails, and the set is then removed.
func (m *Manager) checkSnapshotSpace(dir string, need int64) error {
	free, total, err := m.diskSpace(dir)
	if err != nil || total == 0 {
		slog.Warn("snapshot: free space unknown; going on", "dir", dir, "err", err)
		return nil
	}
	used := total - free
	if uint64(need) > free || (used+uint64(need))*100 > total*snapshotMaxUsedPercent {
		return fmt.Errorf("%w (it needs about %d MB)", ErrSnapshotNoSpace, need>>20+1)
	}
	return nil
}

// noSpaceOr reports a failed snapshot write as ErrSnapshotNoSpace when the
// drive is now past the threshold. A full drive does not always come back as
// ENOSPC: a dump fails inside the client container's stream, and the error
// that reaches the brain is the container's. So the drive is asked instead.
func (m *Manager) noSpaceOr(dir string, err error) error {
	if errors.Is(err, ErrSnapshotNoSpace) {
		return err
	}
	if full := m.checkSnapshotSpace(dir, 0); full != nil {
		return fmt.Errorf("%w: %w", full, err)
	}
	return err
}

// dumpTextFactor is how much bigger than its database a MySQL-family dump may
// be: it is SQL text, and --hex-blob writes each binary byte as two.
const dumpTextFactor = 2

// dumpEstimate is the bytes a database's dump may take: the database's own
// size for Postgres (the custom format is compressed, so it is smaller), and
// dumpTextFactor times the size of its tables and indexes for the MySQL family.
func (m *Manager) dumpEstimate(ctx context.Context, g store.ServiceGrant) (int64, error) {
	var args []string
	factor := int64(1)
	switch {
	case g.Kind == "postgres":
		args = []string{"-tA", "-c", fmt.Sprintf("SELECT pg_database_size('%s')", g.DBName)}
	case mysqlTools[g.Kind].dump != "":
		args = []string{"-N", "-B", "-e", fmt.Sprintf(
			"SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.tables WHERE table_schema = '%s'", g.DBName)}
		factor = dumpTextFactor
	default:
		return 0, fmt.Errorf("no dump for managed service kind %q", g.Kind)
	}
	out, err := m.runServiceClient(ctx, g.Kind, g.Version, args...)
	if err != nil {
		return 0, fmt.Errorf("%w\n%s", err, strings.TrimSpace(out))
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("unexpected size %q", strings.TrimSpace(out))
	}
	return n * factor, nil
}

// writeDataTar writes the data paths, minus the cache paths, as a tar. Entry
// names are slash paths relative to the instance dir. It keeps each entry's
// numeric owner, mode and modification time. It never follows a symlink: a
// symlink is kept as a symlink. Regular files, folders and symlinks are kept;
// anything else (a socket, a device, a pipe) is left out and logged. A hard
// link is kept as a second copy of the file.
func writeDataTar(ctx context.Context, w io.Writer, instDir string, data, cache []string) error {
	tw := tar.NewWriter(w)
	for _, p := range data {
		root := filepath.Join(instDir, filepath.FromSlash(p))
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(root, func(abs string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(abs, instDir+string(filepath.Separator)))
			if inAny(rel, cache) {
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			mode := info.Mode()
			link := ""
			switch {
			case mode.IsRegular(), mode.IsDir():
			case mode&fs.ModeSymlink != 0:
				if link, err = os.Readlink(abs); err != nil {
					return err
				}
			default:
				slog.Warn("snapshot: left out a special file", "src", abs)
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = rel
			if mode.IsDir() {
				hdr.Name += "/"
			}
			hdr.Uname, hdr.Gname = "", "" // numeric owners only
			hdr.Format = tar.FormatPAX
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !mode.IsRegular() {
				return nil
			}
			f, err := os.Open(abs)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// clearDataPath empties data path p before the tar is unpacked over it. The
// cache paths inside it, and the folders on the way to them, stay; everything
// else goes. It never follows a symlink. When removeRoot is set (the path did
// not exist when the snapshot was taken) the path itself goes too, unless a
// cache path sits inside it.
func clearDataPath(instDir, p string, keep []string, removeRoot bool) error {
	if err := checkParents(instDir, p); err != nil {
		return err
	}
	root := filepath.Join(instDir, filepath.FromSlash(p))
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return os.Remove(root)
	}
	if err := clearDir(root, p, keep); err != nil {
		return err
	}
	if removeRoot && len(keep) == 0 {
		return os.Remove(root)
	}
	return nil
}

func clearDir(abs, rel string, keep []string) error {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	for _, e := range entries {
		childRel := rel + "/" + e.Name()
		child := filepath.Join(abs, e.Name())
		if slices.Contains(keep, childRel) {
			continue
		}
		holdsKeep := false
		for _, k := range keep {
			if manifest.PathWithin(k, childRel) {
				holdsKeep = true
				break
			}
		}
		// e.IsDir is from lstat, so a symlink to a folder is not followed.
		if holdsKeep && e.IsDir() {
			if err := clearDir(child, childRel, keep); err != nil {
				return err
			}
			continue
		}
		if err := os.RemoveAll(child); err != nil {
			return err
		}
	}
	return nil
}

// checkDataTar reads a snapshot tar to the end without writing anything. It
// fails on a damaged or cut-off tar, and on an entry outside the snapshot's
// data paths, the same names extractDataTar refuses.
func checkDataTar(ctx context.Context, tarPath string, snap Snapshot) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(bufio.NewReader(f))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if _, err := manifest.CleanStoragePath(rel); err != nil || !inAny(rel, snap.Data) || inAny(rel, snap.Cache) {
			return fmt.Errorf("the snapshot holds %q, which is not one of the app's data folders", hdr.Name)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
}

// extractDataTar unpacks a snapshot tar into the instance dir, after
// clearDataPath. Each entry must sit inside one of the snapshot's data paths
// and outside its cache paths, and no folder on its way may be a symlink. The
// owner of every entry is set from the tar; folders get their mode and time
// last, so unpacking their content does not change them again.
func (m *Manager) extractDataTar(ctx context.Context, tarPath, instDir string, snap Snapshot) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(bufio.NewReader(f))
	type dirMeta struct {
		abs string
		hdr *tar.Header
	}
	var dirs []dirMeta
	warned := false
	chown := func(abs string, hdr *tar.Header) error {
		err := m.chown(abs, hdr.Uid, hdr.Gid)
		if err == nil {
			return nil
		}
		// The unprivileged dev brain cannot give a file to another uid, the
		// same limit as the bind dirs. Say so once and go on.
		if os.Geteuid() != 0 {
			if !warned {
				slog.Warn("snapshot restore: owners not set under an unprivileged brain", "src", abs, "uid", hdr.Uid, "gid", hdr.Gid, "err", err)
				warned = true
			}
			return nil
		}
		return err
	}
	setMeta := func(abs string, hdr *tar.Header) error {
		if err := chown(abs, hdr); err != nil {
			return err
		}
		// chmod after chown: a chown clears the setuid and setgid bits.
		mode := hdr.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		if err := os.Chmod(abs, mode); err != nil {
			return err
		}
		at := hdr.AccessTime
		if at.IsZero() {
			at = hdr.ModTime
		}
		return os.Chtimes(abs, at, hdr.ModTime)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if _, err := manifest.CleanStoragePath(rel); err != nil || !inAny(rel, snap.Data) || inAny(rel, snap.Cache) {
			return fmt.Errorf("the snapshot holds %q, which is not one of the app's data folders", hdr.Name)
		}
		if err := checkParents(instDir, rel); err != nil {
			return err
		}
		abs := filepath.Join(instDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			fi, err := os.Lstat(abs)
			switch {
			case err == nil && fi.IsDir():
			case err == nil:
				if err := os.Remove(abs); err != nil {
					return err
				}
				fallthrough
			default:
				if err := os.Mkdir(abs, 0o700); err != nil {
					return err
				}
			}
			dirs = append(dirs, dirMeta{abs, hdr})
		case tar.TypeReg:
			out, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			if err := setMeta(abs, hdr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, abs); err != nil {
				return err
			}
			if err := chown(abs, hdr); err != nil {
				return err
			}
		default:
			slog.Warn("snapshot restore: left out an entry of an unknown kind", "src", hdr.Name)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := setMeta(dirs[i].abs, dirs[i].hdr); err != nil {
			return err
		}
	}
	return nil
}

func dumpExt(kind string) string {
	if kind == "postgres" {
		return ".pgdump"
	}
	return ".sql"
}

// dumpDatabase streams a logical dump of one per-app database to w, through
// the one-shot client container. Postgres uses pg_dump's custom format, the
// format SERVICE_PROVISIONING.md # At backup names; the MySQL family uses
// mysqldump (mariadb-dump) in one transaction, with routines, triggers and
// events. Neither dump creates the database itself, so a restore makes it.
func (m *Manager) dumpDatabase(ctx context.Context, g store.ServiceGrant, w io.Writer) error {
	var tool string
	var args []string
	switch {
	case g.Kind == "postgres":
		tool, args = "pg_dump", []string{"--format=custom", "--no-password", g.DBName}
	case mysqlTools[g.Kind].dump != "":
		tool, args = mysqlTools[g.Kind].dump, []string{"--single-transaction", "--routines", "--triggers", "--events", "--no-tablespaces", "--hex-blob", g.DBName}
	default:
		return fmt.Errorf("no dump for managed service kind %q", g.Kind)
	}
	out, err := m.runServiceTool(ctx, g.Kind, g.Version, tool, args, nil, w)
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", tool, err, strings.TrimSpace(out))
	}
	return nil
}

// restoreDatabase drops one per-app database, makes it again with the same
// owner, and fills it from its dump, so tables and rows added since the dump
// are gone. The database-level grants of the MySQL family are kept by name, so
// they hold for the new database; Postgres makes the role its owner again.
func (m *Manager) restoreDatabase(ctx context.Context, g store.ServiceGrant, dumpPath string) error {
	var tool string
	var args []string
	switch {
	case g.Kind == "postgres":
		if out, err := m.runServiceClient(ctx, "postgres", g.Version,
			"-c", fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, g.DBName),
			"-c", fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, g.DBName, g.RoleName),
		); err != nil {
			return fmt.Errorf("psql: %w\n%s", err, strings.TrimSpace(out))
		}
		tool, args = "pg_restore", []string{"--exit-on-error", "--no-password", "-d", g.DBName}
	case mysqlTools[g.Kind].dump != "":
		t := mysqlTools[g.Kind]
		sql := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; CREATE DATABASE `%s`", g.DBName, g.DBName)
		if out, err := m.runServiceClient(ctx, g.Kind, g.Version, "-e", sql); err != nil {
			return fmt.Errorf("%s: %w\n%s", t.client, err, strings.TrimSpace(out))
		}
		tool, args = t.client, []string{g.DBName}
	default:
		return fmt.Errorf("no restore for managed service kind %q", g.Kind)
	}
	f, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := m.runServiceTool(ctx, g.Kind, g.Version, tool, args, bufio.NewReader(f), io.Discard)
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", tool, err, strings.TrimSpace(out))
	}
	return nil
}

// runServiceTool runs one of the engine's own tools in the one-shot client
// container, with stdin and stdout streamed (toolWrapper, RunOneOffStream).
func (m *Manager) runServiceTool(ctx context.Context, kind, version, tool string, args []string, stdin io.Reader, stdout io.Writer) (string, error) {
	image := serviceImageRepo[kind] + ":" + version
	net := serviceNetworkName(kind, version)
	envFile := filepath.Join(m.serviceDir(kind, version), ".env")
	full := append([]string{"sh", "-c", toolWrapper(kind, version, tool), "sh"}, args...)
	return m.docker.RunOneOffStream(ctx, image, net, envFile, full, stdin, stdout)
}

// writeFileSynced writes a snapshot file with mode 0600 and syncs it to disk
// before it returns.
func writeFileSynced(p string, write func(io.Writer) error) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	if err := write(bw); err != nil {
		f.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readSnapshot reads a set's record. A missing record is ErrNoSnapshot.
func readSnapshot(dir, base string) (Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(dir, base+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{}, ErrNoSnapshot
	}
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, fmt.Errorf("read the snapshot record: %w", err)
	}
	return s, nil
}

// isDumpOf reports whether file name n is a dump of the set named base:
// base.<service>.pgdump or base.<service>.sql. A service name has no dot (it
// is snake_case), so the dumps of version 1.0.1 are not taken for those of 1.0.
func isDumpOf(n, base string) bool {
	rest, ok := strings.CutPrefix(n, base+".")
	if !ok {
		return false
	}
	for _, ext := range []string{".pgdump", ".sql"} {
		if svc, ok := strings.CutSuffix(rest, ext); ok && svc != "" && !strings.Contains(svc, ".") {
			return true
		}
	}
	return false
}

// removeSnapshotSet removes every file of the set named base: the record
// first, then the tar and the dumps.
func removeSnapshotSet(dir, base string) error {
	if err := os.Remove(filepath.Join(dir, base+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if n := e.Name(); n == base+".tar" || isDumpOf(n, base) {
			if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
