//go:build dockerlive

package lifecycle

// Real-system proof for moving an app's managed database to a new major
// version during an update (#635, SERVICE_PROVISIONING.md # At app update,
// different major version). For each engine, on real Docker:
//
//  1. 1.0 runs on the old major, with rows, a view and a trigger (MySQL
//     family) or a serial and a trusted extension (Postgres).
//  2. 2.0 asks for the new major and never becomes healthy: the app stays on
//     1.0 and the old major with its rows, and nothing is left on the new one.
//  3. 2.1 asks for the new major and is good: the app runs on the new major
//     with all its rows, and the old database is still there.
//  4. The rollback by hand puts 1.0 back on the old major, with the rows from
//     before the update, and drops the database on the new major.
//  5. 2.1 again, then the prune 7 days later drops the old database.
//
// Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveMove -v -timeout 1800s
//
// Needs a Docker daemon and the postgres 15 and 18, mysql 8.0 and 8.4, and
// mariadb 10.11 and 11.4 images (pulled when missing). Excluded from the
// default suite by the build tag.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

func TestLiveMovePostgres(t *testing.T) { liveMove(t, "postgres", "15", "18") }
func TestLiveMoveMySQL(t *testing.T)    { liveMove(t, "mysql", "8.0", "8.4") }
func TestLiveMoveMariaDB(t *testing.T)  { liveMove(t, "mariadb", "10.11", "11.4") }

func liveMove(t *testing.T, kind, from, to string) {
	ctx := context.Background()
	e := newLiveUpdateEnv(t)
	id := "livemove" + strings.ReplaceAll(kind, "sql", "")
	db := func(v string) string {
		return "services:\n  database:\n    type: " + kind + "\n    version: \"" + v + "\"\n"
	}
	t.Cleanup(func() {
		for _, v := range []string{from, to} {
			_ = exec.Command("docker", "rm", "-f", serviceContainerName(kind, v)).Run()
			_ = exec.Command("docker", "network", "rm", serviceNetworkName(kind, v)).Run()
		}
	})
	env := "\n    environment:\n      DATABASE_URL: ${MOOSE_SERVICE_DATABASE_DSN}"
	const image = "alpine:3.21"

	e.publish(t, id, liveAppManifest(id, "1.0", db(from)), liveAppCompose(image, "echo v1 >> /data/starts", false, env))
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	grants, _ := e.st.GetServiceGrants(inst.ID)
	if len(grants) != 1 || grants[0].Version != from {
		t.Fatalf("grants = %+v", grants)
	}
	g := grants[0]
	sqlOn := func(version, q string) string {
		t.Helper()
		out, err := liveSQL(kind, version, g, q)
		if err != nil {
			t.Fatalf("on %s %s: %s: %v", kind, version, q, err)
		}
		return out
	}
	// What 1.0 keeps: rows, and the objects a dump must carry across.
	if kind == "postgres" {
		sqlOn(from, "CREATE EXTENSION IF NOT EXISTS pgcrypto")
		sqlOn(from, "CREATE TABLE notes (id SERIAL PRIMARY KEY, body TEXT, h TEXT)")
		sqlOn(from, "INSERT INTO notes (body, h) VALUES ('first', encode(digest('first', 'sha256'), 'hex')), ('second', NULL)")
	} else {
		sqlOn(from, "CREATE TABLE notes (id INT AUTO_INCREMENT PRIMARY KEY, body TEXT, n INT DEFAULT 0)")
		// MySQL with binary logging does not let the app's own user make a
		// trigger, so root makes it, with the app's user as its DEFINER: the
		// dump names that user, and the restore must carry it across.
		trig := "CREATE DEFINER='" + g.RoleName + "'@'%' TRIGGER notes_n BEFORE INSERT ON notes FOR EACH ROW SET NEW.n = 7"
		tools := mysqlTools[kind]
		if out, err := exec.Command("docker", "exec", serviceContainerName(kind, from), "sh", "-c",
			`MYSQL_PWD="$`+tools.rootPWVar+`" exec `+tools.client+` -uroot -D "$0" -e "$1"`, g.DBName, trig).CombinedOutput(); err != nil {
			t.Fatalf("make the trigger: %v: %s", err, out)
		}
		sqlOn(from, "CREATE VIEW note_bodies AS SELECT body FROM notes")
		sqlOn(from, "INSERT INTO notes (body) VALUES ('first'), ('second')")
	}
	rows := func(version string) string {
		t.Helper()
		return sqlOn(version, "SELECT body FROM notes ORDER BY id")
	}
	if got := rows(from); got != "first\nsecond" {
		t.Fatalf("rows on 1.0 = %q", got)
	}

	// 2. A new major, and a version that never becomes healthy.
	e.publish(t, id, liveAppManifest(id, "2.0", db(to)), liveAppCompose(image, "echo v2 >> /data/starts", true, env))
	e.m.CheckUpdates(ctx)
	if row, _ := e.st.Get(inst.ID); row.Update.Status != store.UpdateAvailable {
		t.Fatalf("check = %+v, want available", row.Update)
	}
	if err := e.m.Update(ctx, inst.ID, func(s string) { t.Logf("step %s", s) }); err == nil {
		t.Fatal("the update to a version that never becomes healthy went through")
	}
	row, _ := e.st.Get(inst.ID)
	if row.State != "running" || row.Version != "1.0" || row.UpdateFailure.Kind != store.UpdateFailedHealth {
		t.Fatalf("after the failed 2.0: state %q version %q failure %+v", row.State, row.Version, row.UpdateFailure)
	}
	if grants, _ := e.st.GetServiceGrants(inst.ID); grants[0].Version != from {
		t.Fatalf("grant after the failed 2.0 = %+v, want %s", grants, from)
	}
	if got := rows(from); got != "first\nsecond" {
		t.Fatalf("rows on %s after the failed 2.0 = %q", from, got)
	}
	if _, err := liveSQL(kind, to, g, "SELECT 1"); err == nil {
		t.Fatalf("the app's database is still on %s %s after the failed update", kind, to)
	}
	t.Logf("2.0 failed; 1.0 runs on %s %s with its rows, and %s %s holds nothing of it", kind, from, kind, to)

	// 3. A good version on the new major.
	e.publish(t, id, liveAppManifest(id, "2.1", db(to)), liveAppCompose(image, "echo v2.1 >> /data/starts", false, env))
	if err := e.m.Update(ctx, inst.ID, func(s string) { t.Logf("step %s", s) }); err != nil {
		t.Fatalf("update to 2.1: %v", err)
	}
	row, _ = e.st.Get(inst.ID)
	if row.State != "running" || row.Version != "2.1" {
		t.Fatalf("after 2.1: state %q version %q", row.State, row.Version)
	}
	if grants, _ := e.st.GetServiceGrants(inst.ID); grants[0].Version != to {
		t.Fatalf("grant after 2.1 = %+v, want %s", grants, to)
	}
	if got := rows(to); got != "first\nsecond" {
		t.Fatalf("rows on %s = %q", to, got)
	}
	if kind == "postgres" {
		if got := sqlOn(to, "SELECT h = encode(digest('first', 'sha256'), 'hex') FROM notes WHERE id = 1"); got != "t" {
			t.Fatalf("pgcrypto on %s: %q", to, got)
		}
		sqlOn(to, "INSERT INTO notes (body) VALUES ('third')") // the serial goes on
		if got := sqlOn(to, "SELECT id FROM notes WHERE body = 'third'"); got != "3" {
			t.Fatalf("serial on %s gave %q, want 3", to, got)
		}
	} else {
		sqlOn(to, "INSERT INTO notes (body) VALUES ('third')")
		if got := sqlOn(to, "SELECT n FROM notes WHERE body = 'third'"); got != "7" {
			t.Fatalf("the trigger did not run on %s: n = %q", to, got)
		}
		if got := sqlOn(to, "SELECT count(*) FROM note_bodies"); got != "3" {
			t.Fatalf("the view on %s counts %q, want 3", to, got)
		}
	}
	if version := sqlOn(to, "SELECT version()"); !strings.Contains(version, to) {
		t.Fatalf("the app's database runs %q, want %s", version, to)
	}
	if got := rows(from); got != "first\nsecond" {
		t.Fatalf("the old database on %s is not kept: %q", from, got)
	}
	retired, _ := e.st.GetRetiredServiceGrants(inst.ID)
	if len(retired) != 1 || retired[0].Version != from {
		t.Fatalf("retired = %+v", retired)
	}
	t.Logf("2.1 runs on %s %s with every row; the %s database is kept", kind, to, from)

	// 4. The rollback by hand goes back to the old major.
	if err := e.m.Rollback(ctx, inst.ID, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	row, _ = e.st.Get(inst.ID)
	if row.State != "running" || row.Version != "1.0" {
		t.Fatalf("after the rollback: state %q version %q", row.State, row.Version)
	}
	if grants, _ := e.st.GetServiceGrants(inst.ID); grants[0].Version != from {
		t.Fatalf("grant after the rollback = %+v, want %s", grants, from)
	}
	if got := rows(from); got != "first\nsecond" {
		t.Fatalf("rows after the rollback = %q, want the rows from before the update", got)
	}
	if _, err := liveSQL(kind, to, g, "SELECT 1"); err == nil {
		t.Fatalf("the database on %s %s is still there after the rollback", kind, to)
	}
	t.Logf("the rollback put 1.0 back on %s %s", kind, from)

	// 5. 2.1 again, and the prune after 7 days.
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update to 2.1 again: %v", err)
	}
	e.m.PruneSnapshots(time.Now().Add(SnapshotKeep + time.Minute))
	if _, err := liveSQL(kind, from, g, "SELECT 1"); err == nil {
		t.Fatalf("the old database on %s %s is still there after 7 days", kind, from)
	}
	if retired, _ := e.st.GetRetiredServiceGrants(inst.ID); len(retired) != 0 {
		t.Fatalf("retired after 7 days = %+v", retired)
	}
	if got := rows(to); got != "first\nsecond" {
		t.Fatalf("rows on %s after the prune = %q", to, got)
	}
	t.Logf("after 7 days the %s database is dropped, and 2.1 still has its rows on %s", from, to)
}
