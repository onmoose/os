package lifecycle

// The pre-update snapshot (UPDATES.md # Pre-update snapshot), against the fake
// docker driver: the tar keeps the data volumes minus the cache volumes, a
// restore puts them back exactly, the dumps of a managed database go out and
// come back through the one-shot client, every failure leaves nothing behind,
// and old snapshots are pruned.

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

const snapManifest = `
id: snapapp
manifest_version: 1
name: Snap App
version: "1.4"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [snapapp]
storage:
  data_volumes: [./data]
  cache_volumes: [./data/cache]
permissions:
  internet: false
  lan: false
`

const snapCompose = `
services:
  app:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data:/data
`

func installSnapApp(t *testing.T, e *testEnv, manifestYAML, compose string) store.Instance {
	t.Helper()
	e.writeCatalogApp(t, "snapapp", compose, manifestYAML)
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "snapapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst
}

func writeSnapFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func readSnapFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func tarNames(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

func snapshotFiles(t *testing.T, e *testEnv, id string) []string {
	t.Helper()
	entries, err := os.ReadDir(e.m.snapshotsDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		out = append(out, en.Name())
	}
	return out
}

// The issue's "done when", at the file level: ./data with ./data/cache is kept
// minus the cache; an update then changes a file, adds one and touches the
// cache; the restore brings back the old file with its mode and time, removes
// the new one, and leaves the cache as the update left it.
func TestSnapshotRestoreDataMinusCache(t *testing.T) {
	e := newTestEnv(t)
	chowns := recordChowns(e)
	inst := installSnapApp(t, e, snapManifest, snapCompose)
	data := filepath.Join(e.stateDir, "instances", inst.ID, "data")

	writeSnapFile(t, filepath.Join(data, "db", "schema.txt"), "v1 schema", 0o640)
	writeSnapFile(t, filepath.Join(data, "config.yml"), "a: 1", 0o600)
	writeSnapFile(t, filepath.Join(data, "cache", "thumb.bin"), "old thumb", 0o644)
	if err := os.Symlink("db/schema.txt", filepath.Join(data, "current")); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(data, "db", "schema.txt"), old, old); err != nil {
		t.Fatal(err)
	}

	snap, err := e.m.TakeSnapshot(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Version != "1.4" || snap.Fallback || !slices.Equal(snap.Data, []string{"data"}) || !slices.Equal(snap.Cache, []string{"data/cache"}) {
		t.Fatalf("snapshot record = %+v", snap)
	}
	names := tarNames(t, filepath.Join(e.m.snapshotsDir(inst.ID), "pre-update-1.4.tar"))
	for _, want := range []string{"data/", "data/db/schema.txt", "data/config.yml", "data/current"} {
		if !slices.Contains(names, want) {
			t.Errorf("tar is missing %s (has %v)", want, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, "data/cache") {
			t.Errorf("tar holds the cache entry %s", n)
		}
	}

	// The "update": a schema migration, a new file, a cache write.
	writeSnapFile(t, filepath.Join(data, "db", "schema.txt"), "v2 schema, migrated", 0o666)
	writeSnapFile(t, filepath.Join(data, "db", "added-by-v2.txt"), "new", 0o644)
	writeSnapFile(t, filepath.Join(data, "cache", "thumb.bin"), "new thumb", 0o644)
	if err := os.Remove(filepath.Join(data, "config.yml")); err != nil {
		t.Fatal(err)
	}

	if err := e.m.RestoreSnapshot(context.Background(), inst.ID, "1.4"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	schema := filepath.Join(data, "db", "schema.txt")
	if got := readSnapFile(t, schema); got != "v1 schema" {
		t.Errorf("schema after restore = %q", got)
	}
	fi, err := os.Stat(schema)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 || !fi.ModTime().Equal(old) {
		t.Errorf("schema mode %v time %v, want 0640 %v", fi.Mode().Perm(), fi.ModTime(), old)
	}
	if got := readSnapFile(t, filepath.Join(data, "config.yml")); got != "a: 1" {
		t.Errorf("config after restore = %q", got)
	}
	if _, err := os.Stat(filepath.Join(data, "db", "added-by-v2.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file the update added survived the restore (err %v)", err)
	}
	if got := readSnapFile(t, filepath.Join(data, "cache", "thumb.bin")); got != "new thumb" {
		t.Errorf("the restore touched the cache: %q", got)
	}
	if link, err := os.Readlink(filepath.Join(data, "current")); err != nil || link != "db/schema.txt" {
		t.Errorf("symlink after restore = %q, %v", link, err)
	}
	// The owner each entry had is given back.
	uid, gid := os.Getuid(), os.Getgid()
	if got, want := chowns.owner(schema), fmt.Sprintf("%d:%d", uid, gid); got != want {
		t.Errorf("schema owner set to %q, want %q", got, want)
	}

	// A second restore from the same files gives the same tree.
	if err := e.m.RestoreSnapshot(context.Background(), inst.ID, "1.4"); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if got := readSnapFile(t, schema); got != "v1 schema" {
		t.Errorf("schema after second restore = %q", got)
	}
}

// A manifest with no data_volumes (Door 2, or a catalog app that does not
// declare them) keeps all of data/ and the compose's other bind dirs.
func TestSnapshotFallbackKeepsAllBindDirs(t *testing.T) {
	e := newTestEnv(t)
	man := strings.Replace(snapManifest, "  data_volumes: [./data]\n  cache_volumes: [./data/cache]\n", "  estimated_size: 1MB\n", 1)
	compose := snapCompose + "      - ./config:/config\n"
	inst := installSnapApp(t, e, man, compose)
	dir := filepath.Join(e.stateDir, "instances", inst.ID)
	writeSnapFile(t, filepath.Join(dir, "data", "a.txt"), "a", 0o644)
	writeSnapFile(t, filepath.Join(dir, "config", "c.txt"), "c", 0o644)

	snap, err := e.m.TakeSnapshot(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Fallback || !slices.Equal(snap.Data, []string{"config", "data"}) {
		t.Fatalf("fallback snapshot = %+v", snap)
	}
	names := tarNames(t, filepath.Join(e.m.snapshotsDir(inst.ID), snap.Tar))
	for _, want := range []string{"data/a.txt", "config/c.txt"} {
		if !slices.Contains(names, want) {
			t.Errorf("tar is missing %s (has %v)", want, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, "snapshots") || n == "compose.yml" || n == ".env" {
			t.Errorf("tar holds the brain's own file %s", n)
		}
	}
}

// A data volume that did not exist at the snapshot is removed by the restore.
func TestSnapshotRestoreRemovesAbsentVolume(t *testing.T) {
	e := newTestEnv(t)
	man := strings.Replace(snapManifest, "data_volumes: [./data]", "data_volumes: [./data, ./later]", 1)
	inst := installSnapApp(t, e, man, snapCompose)
	dir := filepath.Join(e.stateDir, "instances", inst.ID)
	snap, err := e.m.TakeSnapshot(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !slices.Equal(snap.Absent, []string{"later"}) {
		t.Fatalf("absent = %v", snap.Absent)
	}
	writeSnapFile(t, filepath.Join(dir, "later", "x"), "x", 0o644)
	if err := e.m.RestoreSnapshot(context.Background(), inst.ID, "1.4"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "later")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("./later survived the restore (err %v)", err)
	}
}

// A managed Postgres database is dumped through the one-shot client into the
// snapshot dir and fed back on restore, after a drop and a create; a Valkey
// grant has no per-app dump and is listed as skipped.
func TestSnapshotDumpsAndRestoresDatabases(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBApp(t, e, "dbapp")
	grants, err := e.store.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants %v %v", grants, err)
	}
	g := grants[0]
	// A second, Valkey grant, as an app with a cache would hold.
	if err := e.store.SetServiceGrants(inst.ID, append(grants, store.ServiceGrant{
		LogicalName: "cache", Kind: "valkey", Version: "8", RoleName: "dbapp_cache", Password: "pw",
	})); err != nil {
		t.Fatal(err)
	}

	snap, err := e.m.TakeSnapshot(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Dumps) != 1 || snap.Dumps[0].File != "pre-update-1.0.database.pgdump" || !slices.Equal(snap.Skipped, []string{"cache"}) {
		t.Fatalf("snapshot record = %+v", snap)
	}
	dumpPath := filepath.Join(e.m.snapshotsDir(inst.ID), snap.Dumps[0].File)
	if got := readSnapFile(t, dumpPath); got != "dump:"+g.DBName {
		t.Fatalf("dump file = %q", got)
	}
	if fi, _ := os.Stat(dumpPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("dump mode = %v, want 0600", fi.Mode().Perm())
	}
	if !e.docker.hasCall("RunOneOffStream(postgres:15,moose-svc-postgres-15") || !e.docker.hasCall("exec pg_dump -h postgres-15.moose.internal -U postgres") {
		t.Errorf("no pg_dump one-shot on the service network: %v", e.docker.Calls())
	}

	if err := e.m.RestoreSnapshot(context.Background(), inst.ID, "1.0"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !e.docker.hasCall(`DROP DATABASE IF EXISTS "`+g.DBName+`" WITH (FORCE)`) || !e.docker.hasCall(`CREATE DATABASE "`+g.DBName+`" OWNER "`+g.RoleName+`"`) {
		t.Errorf("restore did not drop and create the database: %v", e.docker.Calls())
	}
	if !e.docker.hasCall("exec pg_restore") || !slices.Equal(e.docker.streamed, []string{"dump:" + g.DBName}) {
		t.Errorf("restore did not feed the dump back: streamed %q", e.docker.streamed)
	}
}

// A dump that fails is an error the caller sees, and no part of the set stays.
func TestSnapshotDumpFailureLeavesNothing(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBApp(t, e, "dbapp")
	e.docker.runStream = func(_, _, _ string, _ []string, _ io.Reader, w io.Writer) (string, error) {
		_, _ = io.WriteString(w, "half a dump")
		return "pg_dump: error: connection refused", errors.New("exit status 1")
	}
	_, err := e.m.TakeSnapshot(context.Background(), inst.ID)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("snapshot err = %v, want the dump failure", err)
	}
	if files := snapshotFiles(t, e, inst.ID); len(files) != 0 {
		t.Errorf("a failed snapshot left %v", files)
	}
	if err := e.m.RestoreSnapshot(context.Background(), inst.ID, "1.0"); !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("restore after a failed snapshot = %v, want ErrNoSnapshot", err)
	}
}

// A drive too full to take the tar refuses the snapshot with ErrSnapshotNoSpace.
func TestSnapshotNoSpace(t *testing.T) {
	e := newTestEnv(t)
	inst := installSnapApp(t, e, snapManifest, snapCompose)
	writeSnapFile(t, filepath.Join(e.stateDir, "instances", inst.ID, "data", "big.bin"), strings.Repeat("x", 4096), 0o644)
	// 100 KB drive with 3 KB free: past 95% once the tar is written.
	e.m.diskSpace = func(string) (uint64, uint64, error) { return 3 << 10, 100 << 10, nil }
	if _, err := e.m.TakeSnapshot(context.Background(), inst.ID); !errors.Is(err, ErrSnapshotNoSpace) {
		t.Fatalf("snapshot err = %v, want ErrSnapshotNoSpace", err)
	}
	if files := snapshotFiles(t, e, inst.ID); len(files) != 0 {
		t.Errorf("a refused snapshot left %v", files)
	}
}

// A data volume reached through a symlink in a parent folder is refused, and a
// symlink inside a data volume is kept as a link, never followed.
func TestSnapshotSymlinks(t *testing.T) {
	e := newTestEnv(t)
	man := strings.Replace(snapManifest, "data_volumes: [./data]\n  cache_volumes: [./data/cache]", "data_volumes: [./data/sub/index]", 1)
	inst := installSnapApp(t, e, man, snapCompose)
	data := filepath.Join(e.stateDir, "instances", inst.ID, "data")
	outside := t.TempDir()
	writeSnapFile(t, filepath.Join(outside, "index", "secret"), "outside", 0o644)
	if err := os.Symlink(outside, filepath.Join(data, "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.TakeSnapshot(context.Background(), inst.ID); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("snapshot through a linked parent: err = %v", err)
	}

	// A link inside a data volume is stored as a link.
	if err := os.Remove(filepath.Join(data, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "sub", "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "sub", "index", "out")); err != nil {
		t.Fatal(err)
	}
	snap, err := e.m.TakeSnapshot(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	names := tarNames(t, filepath.Join(e.m.snapshotsDir(inst.ID), snap.Tar))
	if !slices.Contains(names, "data/sub/index/out") || slices.Contains(names, "data/sub/index/out/index/secret") {
		t.Fatalf("tar names = %v", names)
	}
	if err := e.m.RestoreSnapshot(context.Background(), inst.ID, "1.4"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readSnapFile(t, filepath.Join(outside, "index", "secret")); got != "outside" {
		t.Errorf("the restore reached outside the app's folder: %q", got)
	}
}

// A stored manifest whose storage lists break the rules is refused before
// anything is written, so an instance installed before the rule existed is
// checked too.
func TestSnapshotRefusesBadStorageList(t *testing.T) {
	e := newTestEnv(t)
	inst := installSnapApp(t, e, snapManifest, snapCompose)
	manPath := filepath.Join(e.stateDir, "instances", inst.ID, "manifest.yml")
	raw := readSnapFile(t, manPath)
	raw = strings.Replace(raw, "- ./data/cache", "- ../outside", 1)
	writeSnapFile(t, manPath, raw, 0o644)
	if _, err := e.m.TakeSnapshot(context.Background(), inst.ID); err == nil || !strings.Contains(err.Error(), "leaves the app's folder") {
		t.Fatalf("snapshot err = %v", err)
	}
}

// Snapshots past 7 days go, newer ones stay, and an app whose lock is held is
// skipped. A loose file a crash left behind goes by its own age.
func TestPruneSnapshots(t *testing.T) {
	e := newTestEnv(t)
	inst := installSnapApp(t, e, snapManifest, snapCompose)
	if _, err := e.m.TakeSnapshot(context.Background(), inst.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	dir := e.m.snapshotsDir(inst.ID)
	loose := filepath.Join(dir, "pre-update-0.9.tar")
	writeSnapFile(t, loose, "left by a crash", 0o600)
	now := time.Now()
	if err := os.Chtimes(loose, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if n := e.m.PruneSnapshots(now); n != 1 {
		t.Errorf("first prune removed %d files, want the loose one", n)
	}
	if files := snapshotFiles(t, e, inst.ID); !slices.Equal(files, []string{"pre-update-1.4.json", "pre-update-1.4.tar"}) {
		t.Fatalf("after the first prune: %v", files)
	}

	later := now.Add(SnapshotKeep + time.Minute)
	unlock := e.m.lockInstance(inst.ID)
	if n := e.m.PruneSnapshots(later); n != 0 {
		t.Errorf("prune removed %d files of a locked app", n)
	}
	unlock()
	if n := e.m.PruneSnapshots(later); n != 2 {
		t.Errorf("prune past 7 days removed %d files, want 2", n)
	}
	if files := snapshotFiles(t, e, inst.ID); len(files) != 0 {
		t.Errorf("after the prune: %v", files)
	}
}

func TestIsDumpOf(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		want       bool
	}{
		{"pre-update-1.0.database.pgdump", "pre-update-1.0", true},
		{"pre-update-1.0.db.sql", "pre-update-1.0", true},
		{"pre-update-1.0.1.database.pgdump", "pre-update-1.0", false},
		{"pre-update-1.0.tar", "pre-update-1.0", false},
		{"pre-update-1.0..pgdump", "pre-update-1.0", false},
	} {
		if got := isDumpOf(tc.name, tc.base); got != tc.want {
			t.Errorf("isDumpOf(%q, %q) = %v", tc.name, tc.base, got)
		}
	}
	if got := snapshotBase("1.4/beta 2"); got != "pre-update-1.4_beta_2" {
		t.Errorf("snapshotBase = %q", got)
	}
}

// Run as root, the restore gives every entry back the exact numeric owner it
// had, as an app's data on a remapped daemon is owned by uids in the remap
// range (APP_ISOLATION.md # User-namespace tiers). It uses the real chown, so
// it only runs as root; the other tests cover the same path with a recorder.
func TestSnapshotRestoreKeepsOwnersAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give files to other uids")
	}
	e := newTestEnv(t)
	inst := installSnapApp(t, e, snapManifest, snapCompose)
	data := filepath.Join(e.stateDir, "instances", inst.ID, "data")
	file := filepath.Join(data, "db", "rows")
	link := filepath.Join(data, "db", "link")
	writeSnapFile(t, file, "v1", 0o750)
	if err := os.Symlink("rows", link); err != nil {
		t.Fatal(err)
	}
	for p, id := range map[string]int{data: 1000000, filepath.Join(data, "db"): 1000999, file: 1001000, link: 1001001} {
		if err := os.Lchown(p, id, id+1); err != nil {
			t.Fatal(err)
		}
	}
	// setuid after the chown, which clears it; the restore must keep it too.
	if err := os.Chmod(file, os.ModeSetuid|0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.TakeSnapshot(context.Background(), inst.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(data, "db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(data, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RestoreSnapshot(context.Background(), inst.ID, "1.4"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for p, id := range map[string]int{data: 1000000, filepath.Join(data, "db"): 1000999, file: 1001000, link: 1001001} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != id || int(st.Gid) != id+1 {
			t.Errorf("%s owner = %d:%d, want %d:%d", p, st.Uid, st.Gid, id, id+1)
		}
	}
	if fi, _ := os.Stat(file); fi.Mode()&(os.ModeSetuid|os.ModePerm) != os.ModeSetuid|0o750 {
		t.Errorf("mode after restore = %v, want setuid 0750", fi.Mode())
	}
}
