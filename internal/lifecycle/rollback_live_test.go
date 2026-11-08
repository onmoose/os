//go:build dockerlive

package lifecycle

// Real-system proof for the rollback by hand (#634, UPDATES.md # 4 As built
// (#634)): a real app is updated, the user writes data on the new version,
// and the rollback puts the old version back on the data from before the
// update. Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveRollback -v -timeout 900s
//
// Needs a Docker daemon and network access to pull alpine and postgres images.
// Excluded from the default suite by the build tag.

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"
)

// TestLiveRollbackFiles: 1.0 (alpine 3.20) to 2.0 (alpine 3.21) goes well,
// the user writes on 2.0, then 3.0 fails and is rolled back. The rollback by
// hand still goes back to 1.0, on 1.0's data, on 1.0's image.
func TestLiveRollbackFiles(t *testing.T) {
	ctx := context.Background()
	e := newLiveUpdateEnv(t)
	const id = "liverollback"

	e.publish(t, id, liveAppManifest(id, "1.0", ""), liveAppCompose("alpine:3.20", "echo v1 >> /data/starts", false, ""))
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	data := filepath.Join(e.stateDir, "instances", inst.ID, "data")
	mustWrite(t, filepath.Join(data, "notes.txt"), "written by the user on 1.0")

	e.publish(t, id, liveAppManifest(id, "2.0", ""), liveAppCompose("alpine:3.21", "echo v2 >> /data/starts", false, ""))
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update to 2.0: %v", err)
	}
	mustWrite(t, filepath.Join(data, "notes.txt"), "written by the user on 2.0")
	mustWrite(t, filepath.Join(data, "new-on-2.txt"), "only on 2.0")

	e.publish(t, id, liveAppManifest(id, "3.0", ""), liveAppCompose("alpine:3.21", "echo v3 >> /data/starts", true, ""))
	if err := e.m.Update(ctx, inst.ID, nil); err == nil {
		t.Fatal("the update to a version that never becomes healthy went through")
	}
	row, _ := e.st.Get(inst.ID)
	if row.Version != "2.0" || row.LastUpdate.From != "1.0" {
		t.Fatalf("after the failed 3.0: version %q last %+v", row.Version, row.LastUpdate)
	}
	if got := liveRead(t, filepath.Join(data, "notes.txt")); got != "written by the user on 2.0" {
		t.Fatalf("the failed 3.0 did not put 2.0's data back: %q", got)
	}
	t.Log("3.0 failed; 2.0 runs, and the way back to 1.0 is kept")

	if err := e.m.Rollback(ctx, inst.ID, func(s string) { t.Logf("step %s", s) }); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	row, _ = e.st.Get(inst.ID)
	if row.Version != "1.0" || row.State != "running" || !row.AutoUpdateOff {
		t.Fatalf("after the rollback: version %q state %q auto off %v", row.Version, row.State, row.AutoUpdateOff)
	}
	if got := liveRead(t, filepath.Join(data, "notes.txt")); got != "written by the user on 1.0" {
		t.Fatalf("data after the rollback = %q, want 1.0's", got)
	}
	if got := liveRead(t, filepath.Join(data, "new-on-2.txt")); got != "" {
		t.Fatalf("a file written on 2.0 survived the rollback: %q", got)
	}
	if running, health, err := e.m.docker.Inspect(ctx, inst.ID, "app"); err != nil || !running || health == "unhealthy" {
		t.Fatalf("1.0 is not running: running %v health %q err %v", running, health, err)
	}
	out, _ := exec.Command("docker", "inspect", "-f", "{{.Config.Image}}", "moose-"+inst.ID+"-app").Output()
	pins, _ := e.st.GetInstanceImages(inst.ID)
	if len(pins) != 1 || !strings.Contains(string(out), pins[0].Digest) {
		t.Fatalf("the container runs %q, want 1.0's pin %+v", strings.TrimSpace(string(out)), pins)
	}
	waitFor(t, func() bool { return strings.HasSuffix(liveRead(t, filepath.Join(data, "starts")), "v1") })
	t.Logf("1.0 runs again on its own data, on %s", strings.TrimSpace(string(out)))
}

// TestLiveRollbackBrokenComposeFiles: the compose files on disk cannot be read,
// so `compose stop` fails while the app still runs. The rollback finds the
// container by its project label, stops it by id, and then goes back to 1.0.
func TestLiveRollbackBrokenComposeFiles(t *testing.T) {
	ctx := context.Background()
	e := newLiveUpdateEnv(t)
	const id = "livestopfails"

	e.publish(t, id, liveAppManifest(id, "1.0", ""), liveAppCompose("alpine:3.20", "echo v1 >> /data/starts", false, ""))
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	dir := filepath.Join(e.stateDir, "instances", inst.ID)
	data := filepath.Join(dir, "data")
	mustWrite(t, filepath.Join(data, "notes.txt"), "written by the user on 1.0")

	e.publish(t, id, liveAppManifest(id, "2.0", ""), liveAppCompose("alpine:3.21", "echo v2 >> /data/starts", false, ""))
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update to 2.0: %v", err)
	}
	mustWrite(t, filepath.Join(data, "notes.txt"), "written by the user on 2.0")

	mustWrite(t, filepath.Join(dir, "compose.yml"), "services: [this is not a compose file")
	project := "moose-" + inst.ID
	if out, err := e.m.docker.ComposeStop(ctx, dir, project); err == nil {
		t.Fatalf("compose stop worked on broken files: %s", out)
	}
	running, err := e.m.docker.RunningContainers(ctx, project)
	if err != nil || len(running) != 1 {
		t.Fatalf("running before the rollback = %v %v, want the app's one container", running, err)
	}

	if err := e.m.Rollback(ctx, inst.ID, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	row, _ := e.st.Get(inst.ID)
	if row.Version != "1.0" || row.State != "running" {
		t.Fatalf("after the rollback: version %q state %q", row.Version, row.State)
	}
	if got := liveRead(t, filepath.Join(data, "notes.txt")); got != "written by the user on 1.0" {
		t.Fatalf("data after the rollback = %q, want 1.0's", got)
	}
	if running, health, err := e.m.docker.Inspect(ctx, inst.ID, "app"); err != nil || !running || health == "unhealthy" {
		t.Fatalf("1.0 is not running: running %v health %q err %v", running, health, err)
	}
}

// TestLiveRollbackPostgres: rows changed on the new version are put back
// from the snapshot's dump.
func TestLiveRollbackPostgres(t *testing.T) {
	ctx := context.Background()
	e := newLiveUpdateEnv(t)
	const id = "liverollbackpg"
	const db = "services:\n  database:\n    type: postgres\n    version: \"16\"\n"
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", serviceContainerName("postgres", "16")).Run()
		_ = exec.Command("docker", "network", "rm", serviceNetworkName("postgres", "16")).Run()
	})
	env := "\n    environment:\n      DATABASE_URL: ${MOOSE_SERVICE_DATABASE_DSN}"
	const image = "postgres:16-alpine"

	e.publish(t, id, liveAppManifest(id, "1.0", db), liveAppCompose(image, "echo v1 >> /data/starts", false, env))
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	grants, _ := e.st.GetServiceGrants(inst.ID)
	if len(grants) != 1 {
		t.Fatalf("grants = %v", grants)
	}
	sql := func(q string) string {
		t.Helper()
		out, err := liveSQL("postgres", "16", grants[0], q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return out
	}
	sql("CREATE TABLE notes (id INT PRIMARY KEY, body TEXT)")
	sql("INSERT INTO notes VALUES (1, 'first'), (2, 'second')")

	e.publish(t, id, liveAppManifest(id, "2.0", db), liveAppCompose(image, "echo v2 >> /data/starts", false, env))
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update to 2.0: %v", err)
	}
	// What 2.0 and its user did the next day.
	sql("UPDATE notes SET body = 'changed on 2.0'")
	sql("CREATE TABLE v2_only (x INT)")

	if err := e.m.Rollback(ctx, inst.ID, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if row, _ := e.st.Get(inst.ID); row.Version != "1.0" || row.State != "running" {
		t.Fatalf("after the rollback: version %q state %q", row.Version, row.State)
	}
	if got := sql("SELECT body FROM notes ORDER BY id"); got != "first\nsecond" {
		t.Fatalf("rows after the rollback = %q, want the rows from before the update", got)
	}
	if got := sql(tableCountQuery("postgres", grants[0].DBName)); got != "1" {
		t.Fatalf("%s tables after the rollback, want 1", got)
	}
	t.Log("1.0 runs again on its database from before the update")
}
