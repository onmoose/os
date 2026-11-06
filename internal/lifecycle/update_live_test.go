//go:build dockerlive

package lifecycle

// Real-system proof for the app update (#631, APP_LIFECYCLE.md # update +
// rollback): a real app is installed at an old version, data is written, the
// app is updated, and the data is still there; then an update whose new
// version never becomes healthy, after it has changed the data, ends with the
// old version running on its old data. Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveUpdate -v -timeout 900s
//
// Needs a Docker daemon and network access to pull alpine and postgres images.
// Excluded from the default suite by the build tag.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/store"
)

// liveUpdateEnv is a Manager over real Docker, with a catalog dir the test
// publishes versions into.
type liveUpdateEnv struct {
	m        *Manager
	st       *store.Store
	catDir   string
	stateDir string
}

func newLiveUpdateEnv(t *testing.T) *liveUpdateEnv {
	t.Helper()
	ctx := context.Background()
	stateDir, catDir := t.TempDir(), t.TempDir()
	st, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	docker := NewCLIDocker()
	m := NewManager(st, catalog.New(catDir), newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)
	// A short budget, so the failing version is rolled back in seconds.
	m.SetHealthTiming(30*time.Second, time.Second)
	m.updateMargin, m.defaultStartPeriod = 0, 0
	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s", "alpine", "rm", "-rf", "/s/services", "/s/instances").Run()
	})
	return &liveUpdateEnv{m: m, st: st, catDir: catDir, stateDir: stateDir}
}

func (e *liveUpdateEnv) publish(t *testing.T, id, manifestYAML, compose string) {
	t.Helper()
	writeLiveCatalogApp(t, e.catDir, id, compose, manifestYAML)
}

// liveApp is a shell loop that writes a start mark, runs cmd once, and waits.
// It exits on SIGTERM, so a stop is quick. A healthcheck that always fails
// makes the version never healthy.
func liveAppCompose(image, cmd string, failHealth bool, env string) string {
	hc := ""
	if failHealth {
		hc = `
    healthcheck:
      test: ["CMD", "false"]
      interval: 1s
      retries: 1
      start_period: 0s`
	}
	return fmt.Sprintf(`
services:
  app:
    image: %s
    command: ["sh", "-c", "trap 'exit 0' TERM; %s; while :; do sleep 1; done"]
    volumes:
      - ./data:/data%s%s
`, image, cmd, env, hc)
}

func liveAppManifest(id, version, extra string) string {
	return fmt.Sprintf(`
id: %s
manifest_version: 1
name: Live Update
version: %q
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [%s]
storage:
  data_volumes: [./data]
  cache_volumes: [./data/cache]
permissions:
  internet: false
  lan: false
%s`, id, version, id, extra)
}

// TestLiveUpdateFiles: an app whose data is files.
func TestLiveUpdateFiles(t *testing.T) {
	ctx := context.Background()
	e := newLiveUpdateEnv(t)
	const id = "liveupdate"

	e.publish(t, id, liveAppManifest(id, "1.0", ""), liveAppCompose("alpine:3.20", "echo v1 >> /data/starts", false, ""))
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	data := filepath.Join(e.stateDir, "instances", inst.ID, "data")
	mustWrite(t, filepath.Join(data, "notes.txt"), "written by the user on 1.0")

	// A good update: alpine 3.20 to 3.21, data kept.
	e.publish(t, id, liveAppManifest(id, "2.0", ""), liveAppCompose("alpine:3.21", "echo v2 >> /data/starts", false, ""))
	e.m.CheckUpdates(ctx)
	if row, _ := e.st.Get(inst.ID); row.Update.Status != store.UpdateAvailable {
		t.Fatalf("check = %+v", row.Update)
	}
	if err := e.m.Update(ctx, inst.ID, func(s string) { t.Logf("step %s", s) }); err != nil {
		t.Fatalf("update to 2.0: %v", err)
	}
	if row, _ := e.st.Get(inst.ID); row.Version != "2.0" || row.State != "running" {
		t.Fatalf("after the update: version %q state %q", row.Version, row.State)
	}
	if got := liveRead(t, filepath.Join(data, "notes.txt")); got != "written by the user on 1.0" {
		t.Fatalf("data after the update = %q", got)
	}
	waitFor(t, func() bool { return strings.Contains(liveRead(t, filepath.Join(data, "starts")), "v2") })
	if img := liveContainerImage(t, inst.ID); img != "alpine:3.21" && !strings.HasPrefix(img, "alpine@") {
		t.Logf("running image %s", img)
	}
	t.Log("2.0 runs on the 1.0 data")

	// A bad update: 3.0 rewrites the data, then never becomes healthy.
	e.publish(t, id, liveAppManifest(id, "3.0", ""), liveAppCompose("alpine:3.21", "echo broken > /data/notes.txt; mkdir -p /data/cache; cat /data/notes.txt > /data/cache/v3-saw", true, ""))
	e.m.CheckUpdates(ctx)
	err = e.m.Update(ctx, inst.ID, func(s string) { t.Logf("step %s", s) })
	if err == nil {
		t.Fatal("the update to a version that never becomes healthy went through")
	}
	t.Logf("update to 3.0 failed as it should: %v", err)
	row, _ := e.st.Get(inst.ID)
	if row.State != "running" || row.Version != "2.0" || row.UpdateFailure.Kind != store.UpdateFailedHealth {
		t.Fatalf("after the rollback: state %q version %q failure %+v", row.State, row.Version, row.UpdateFailure)
	}
	// The cache is not restored, so it shows what 3.0 did before it failed.
	if got := liveRead(t, filepath.Join(data, "cache", "v3-saw")); got != "broken" {
		t.Fatalf("3.0 did not change the data before it failed (cache says %q)", got)
	}
	if got := liveRead(t, filepath.Join(data, "notes.txt")); got != "written by the user on 1.0" {
		t.Fatalf("data after the rollback = %q, want the data from before the update", got)
	}
	if running, health, err := e.m.docker.Inspect(ctx, inst.ID, "app"); err != nil || !running || health == "unhealthy" {
		t.Fatalf("the old version is not running: running %v health %q err %v", running, health, err)
	}
	t.Log("2.0 runs again on its old data")
}

// TestLiveUpdatePostgres: an app with a managed Postgres. The bad version
// changes rows and adds a table before it fails; the rollback restores the
// database from the pre-update dump.
func TestLiveUpdatePostgres(t *testing.T) {
	ctx := context.Background()
	e := newLiveUpdateEnv(t)
	const id = "liveupdatepg"
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
	if got := sql("SELECT body FROM notes ORDER BY id"); got != "first\nsecond" {
		t.Fatalf("rows after the update = %q", got)
	}

	migrate := `psql \"$$DATABASE_URL\" -c \"UPDATE notes SET body = 'rewritten'\" -c \"CREATE TABLE v3_only (x INT)\" > /data/cache/migrate.log 2>&1`
	e.publish(t, id, liveAppManifest(id, "3.0", db), liveAppCompose(image, "mkdir -p /data/cache; "+migrate, true, env))
	if err := e.m.Update(ctx, inst.ID, nil); err == nil {
		t.Fatal("the update to a version that never becomes healthy went through")
	}
	// The cache is not restored, so it shows that 3.0's migration ran.
	if log := liveRead(t, filepath.Join(e.stateDir, "instances", inst.ID, "data", "cache", "migrate.log")); !strings.Contains(log, "UPDATE 2") || !strings.Contains(log, "CREATE TABLE") {
		t.Fatalf("3.0's migration did not run before it failed: %q", log)
	} else {
		t.Logf("3.0's migration said: %s", strings.ReplaceAll(log, "\n", " "))
	}
	row, _ := e.st.Get(inst.ID)
	if row.State != "running" || row.Version != "2.0" {
		t.Fatalf("after the rollback: state %q version %q failure %+v", row.State, row.Version, row.UpdateFailure)
	}
	if got := sql("SELECT body FROM notes ORDER BY id"); got != "first\nsecond" {
		t.Fatalf("rows after the rollback = %q, want the rows from before the update", got)
	}
	if got := sql(tableCountQuery("postgres", grants[0].DBName)); got != "1" {
		t.Fatalf("%s tables after the rollback, want 1", got)
	}
	t.Log("2.0 runs again on its old database")
}

func liveRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func liveContainerImage(t *testing.T, id string) string {
	t.Helper()
	out, _ := exec.Command("docker", "inspect", "-f", "{{.Config.Image}}", "moose-"+id+"-app").Output()
	return strings.TrimSpace(string(out))
}
