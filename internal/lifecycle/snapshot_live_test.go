//go:build dockerlive

package lifecycle

// Real-system proof for the pre-update snapshot (UPDATES.md # Pre-update
// snapshot, #347): a real app with a real managed database is snapshotted, an
// "update" migrates both its files and its schema, and the restore brings the
// pre-update state back. The dump and the restore go through the real
// one-shot client container (docker run --rm -i) on the service network, the
// same path as provisioning. Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveSnapshot -v -timeout 900s
//
// Needs a Docker daemon and network access to pull the service images and
// traefik/whoami. Excluded from the default suite by the build tag.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/store"
)

func TestLiveSnapshotPostgres(t *testing.T) { liveSnapshot(t, "postgres", "16") }
func TestLiveSnapshotMySQL(t *testing.T)    { liveSnapshot(t, "mysql", "8.0") }
func TestLiveSnapshotMariaDB(t *testing.T)  { liveSnapshot(t, "mariadb", "11.4") }

func liveSnapshot(t *testing.T, kind, version string) {
	ctx := context.Background()
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	docker := NewCLIDocker()
	m := NewManager(s, catalog.New(catDir), newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)
	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}

	id := "livesnap" + strings.ReplaceAll(kind, "sql", "")
	man := fmt.Sprintf(`
id: %s
manifest_version: 1
name: Live Snapshot
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [%s]
storage:
  data_volumes: [./data]
  cache_volumes: [./data/cache]
services:
  database:
    type: %s
    version: "%s"
permissions:
  internet: false
  lan: false
`, id, id, kind, version)
	compose := `
services:
  app:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data:/data
    environment:
      DATABASE_URL: ${MOOSE_SERVICE_DATABASE_DSN}
`
	writeLiveCatalogApp(t, catDir, id, compose, man)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", serviceContainerName(kind, version)).Run()
		_ = exec.Command("docker", "network", "rm", serviceNetworkName(kind, version)).Run()
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s", "alpine", "rm", "-rf", "/s/services", "/s/instances").Run()
	})

	inst, err := m.Install(ctx, mustLoadApp(t, m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = m.Uninstall(context.Background(), inst.ID) })
	grants, err := s.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	g := grants[0]
	sql := func(q string) string {
		t.Helper()
		out, err := liveSQL(kind, version, g, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}

	// The pre-update state: one table with two rows, and some files.
	sql("CREATE TABLE notes (id INT PRIMARY KEY, body TEXT)")
	sql("INSERT INTO notes VALUES (1, 'first'), (2, 'second')")
	data := filepath.Join(stateDir, "instances", inst.ID, "data")
	mustWrite(t, filepath.Join(data, "app.db"), "v1 data")
	mustWrite(t, filepath.Join(data, "cache", "c.bin"), "v1 cache")

	if err := m.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	snap, err := m.TakeSnapshot(ctx, inst.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	dump := filepath.Join(m.snapshotsDir(inst.ID), snap.Dumps[0].File)
	fi, err := os.Stat(dump)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("dump file %s: %v (size %d)", dump, err, fi.Size())
	}
	t.Logf("snapshot %s: tar %s, dump %s (%d bytes)", snap.Version, snap.Tar, snap.Dumps[0].File, fi.Size())

	// The "update": a schema migration, new rows, new files, a cache write.
	sql("ALTER TABLE notes ADD COLUMN migrated INT")
	sql("UPDATE notes SET body = 'rewritten by v2'")
	sql("CREATE TABLE v2_only (x INT)")
	mustWrite(t, filepath.Join(data, "app.db"), "v2 data, migrated")
	mustWrite(t, filepath.Join(data, "v2-file"), "new")
	mustWrite(t, filepath.Join(data, "cache", "c.bin"), "v2 cache")

	if err := m.RestoreSnapshot(ctx, inst.ID, "1.0"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := sql("SELECT body FROM notes ORDER BY id"); got != "first\nsecond" {
		t.Errorf("rows after restore = %q", got)
	}
	if got := sql(columnCountQuery(kind, g.DBName, "notes")); got != "2" {
		t.Errorf("notes has %s columns after restore, want 2 (the migration is not undone)", got)
	}
	if got := sql(tableCountQuery(kind, g.DBName)); got != "1" {
		t.Errorf("%s tables after restore, want 1 (v2_only is still there)", got)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "app.db")); string(b) != "v1 data" {
		t.Errorf("app.db after restore = %q", b)
	}
	if _, err := os.Stat(filepath.Join(data, "v2-file")); !os.IsNotExist(err) {
		t.Errorf("v2-file survived the restore (err %v)", err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "cache", "c.bin")); string(b) != "v2 cache" {
		t.Errorf("the restore touched the cache: %q", b)
	}
	// The app's own credential still reaches its database.
	if got := sql("SELECT count(*) FROM notes"); got != "2" {
		t.Errorf("count after restore = %q", got)
	}
	t.Logf("%s %s: rows, schema and files are back to the pre-update state", kind, version)

	if err := m.Start(ctx, inst.ID); err != nil {
		t.Fatalf("start after restore: %v", err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// liveSQL runs one statement as the app's own role, over TCP inside the
// service container, and returns the rows one per line.
func liveSQL(kind, version string, g store.ServiceGrant, q string) (string, error) {
	var cmd *exec.Cmd
	if kind == "postgres" {
		cmd = exec.Command("docker", "exec", "-e", "PGPASSWORD="+g.Password, serviceContainerName(kind, version),
			"psql", "-h", "127.0.0.1", "-U", g.RoleName, "-d", g.DBName, "-v", "ON_ERROR_STOP=1", "-tAc", q)
	} else {
		cmd = exec.Command("docker", "exec", "-e", "MYSQL_PWD="+g.Password, serviceContainerName(kind, version),
			mysqlTools[kind].client, "-h127.0.0.1", "--protocol=TCP", "-u"+g.RoleName, "-D", g.DBName, "-N", "-B", "-e", q)
	}
	// stdout only: the mariadb client warns about TLS on stderr.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s%s", err, out, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func columnCountQuery(kind, db, table string) string {
	schema := "'public'"
	if kind != "postgres" {
		schema = "'" + db + "'"
	}
	return "SELECT count(*) FROM information_schema.columns WHERE table_schema = " + schema + " AND table_name = '" + table + "'"
}

func tableCountQuery(kind, db string) string {
	schema := "'public'"
	if kind != "postgres" {
		schema = "'" + db + "'"
	}
	return "SELECT count(*) FROM information_schema.tables WHERE table_schema = " + schema
}
