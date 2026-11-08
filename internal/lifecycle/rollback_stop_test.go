package lifecycle

// A rollback whose `compose stop` fails (UPDATES.md # As built (#631)): the
// undo starts only when Docker says that no container of the app runs. Each
// way into the undo is run against the fakes, with a database the update moved,
// so the tests can also see that nothing of the database was touched.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"
)

// newData is what the new version left in the app's data on both paths.
const newData = "v2.0 migrated"

var errStopBroken = errors.New("compose stop: the files are broken")

// rollbackPath is one way into undoUpdate, up to the moment just before it.
type rollbackPath struct {
	name string
	// doneErr is what the path returns when the undo ran: a failed update
	// still reports its failure, and a rollback by hand returns nil.
	doneErr string
	// undoFails is the sentence the owner reads when the undo did not start.
	undoFails string
	// versionBefore is the row's version while the undo has not run.
	versionBefore string
	// run brings the app to where the path goes wrong, calls arm just before
	// the undo, and returns what the path returned.
	run func(t *testing.T, e *testEnv, arm func()) (store.Instance, error)
}

var rollbackPaths = []rollbackPath{
	{
		name:          "failed update",
		doneErr:       "moose put the old version back",
		undoFails:     "could not put the old version back",
		versionBefore: "1.0",
		run: func(t *testing.T, e *testEnv, arm func()) (store.Instance, error) {
			inst, _ := installUpDB(t, e)
			e.versionsMigrate(t, "2.0")
			e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
			// The new version starts, and from here on the compose files
			// cannot be used. Its health check then fails the update.
			migrate := e.docker.composeUp
			e.docker.composeUp = func(ctx context.Context, dir, project string) (string, error) {
				out, err := migrate(ctx, dir, project)
				if b, _ := os.ReadFile(filepath.Join(dir, "compose.yml")); strings.Contains(string(b), upImage2) {
					arm()
				}
				return out, err
			}
			return inst, e.m.Update(context.Background(), inst.ID, nil)
		},
	},
	{
		name:          "rollback by hand",
		undoFails:     "could not go back to version 1.0. Start the app to try again",
		versionBefore: "2.0",
		run: func(t *testing.T, e *testEnv, arm func()) (store.Instance, error) {
			inst, _ := installUpDB(t, e)
			e.versionsMigrate(t)
			e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
			if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
				t.Fatalf("update: %v", err)
			}
			arm()
			return inst, e.m.Rollback(context.Background(), inst.ID, nil)
		},
	},
}

// breakCompose makes `compose stop` fail, with these containers of the app
// running.
func (e *testEnv) breakCompose(id string, running ...string) {
	e.docker.composeStopErr = errStopBroken
	e.docker.mu.Lock()
	defer e.docker.mu.Unlock()
	e.docker.running = map[string][]string{"moose-" + id: running}
}

// onlyInstance is the one instance of the test env.
func (e *testEnv) onlyInstance(t *testing.T) store.Instance {
	t.Helper()
	insts, err := e.store.List()
	if err != nil || len(insts) != 1 {
		t.Fatalf("instances = %+v %v", insts, err)
	}
	return insts[0]
}

// assertRolledBack: the old version runs, on the old data, with the database
// back where it was.
func assertRolledBack(t *testing.T, e *testEnv, id string) {
	t.Helper()
	r := e.row(t, id)
	if r.State != "running" || r.Version != "1.0" {
		t.Fatalf("after the rollback: state %q version %q", r.State, r.Version)
	}
	if got := readSnapFile(t, e.dataFile(id, "app.db")); got != "v1 data" {
		t.Fatalf("data = %q, want the old data", got)
	}
	if !strings.Contains(e.onDisk(t, id), upImage1) {
		t.Fatal("the old compose is not back")
	}
	if e.exists(id, updateRecordFile) {
		t.Fatal("the record is left after the rollback")
	}
	grants, _ := e.store.GetServiceGrants(id)
	if len(grants) != 1 || grants[0].Version != "16" {
		t.Fatalf("grant = %+v, want back on 16", grants)
	}
	if !anyContains(e.oneOffs("postgres:16"), "pg_restore") {
		t.Fatal("the snapshot was not restored into the 16 database")
	}
}

// assertNothingUndone: the undo did not start. The data, the files, the
// database and its grant are as the new version left them, and the record stays.
func assertNothingUndone(t *testing.T, e *testEnv, id, version string, restoresBefore int) {
	t.Helper()
	r := e.row(t, id)
	if r.State != "update_failed" || r.Version != version {
		t.Fatalf("row: state %q version %q, want update_failed %s", r.State, r.Version, version)
	}
	if got := readSnapFile(t, e.dataFile(id, "app.db")); got != newData {
		t.Fatalf("data = %q, want %q: the undo touched it", got, newData)
	}
	if !strings.Contains(e.onDisk(t, id), upImage2) {
		t.Fatal("the undo put the old compose back")
	}
	if !e.exists(id, updateRecordFile) || !e.exists(id, "compose.yml.prev") {
		t.Fatal("the record and the previous generation must stay for a retry")
	}
	grants, _ := e.store.GetServiceGrants(id)
	if len(grants) != 1 || grants[0].Version != "17" {
		t.Fatalf("grant = %+v: the service was moved back", grants)
	}
	if retired, _ := e.store.GetRetiredServiceGrants(id); len(retired) != 1 {
		t.Fatalf("retired = %+v, want the old database still kept", retired)
	}
	if got := len(restores(e)); got != restoresBefore {
		t.Fatalf("%d database restores or drops, want %d", got, restoresBefore)
	}
}

// restores is every call that changes a database the undo would change.
func restores(e *testEnv) []string {
	var out []string
	for _, c := range append(e.oneOffs("postgres:16"), e.oneOffs("postgres:17")...) {
		if strings.Contains(c, "DROP DATABASE") {
			out = append(out, c)
		}
	}
	for _, c := range e.oneOffs("postgres:16") {
		if strings.Contains(c, "pg_restore") {
			out = append(out, c)
		}
	}
	return out
}

func TestRollbackGoesOnWhenStopFails(t *testing.T) {
	cases := []struct {
		name    string
		running []string
		stop    func(e *testEnv) func(string) error
		stops   int
	}{
		{name: "nothing runs"},
		{name: "the direct stop works", running: []string{"c1", "c2"}, stops: 2},
		{
			name:    "the direct stop fails on a container that went away",
			running: []string{"c1"},
			stops:   1,
			stop: func(e *testEnv) func(string) error {
				return func(c string) error {
					e.docker.removeRunning(c)
					return errors.New("no such container")
				}
			},
		},
	}
	for _, path := range rollbackPaths {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				e := newTestEnv(t)
				var dataAtStop []string
				var before int
				var id string
				inst, err := path.run(t, e, func() {
					id = e.onlyInstance(t).ID
					before = len(e.docker.Calls())
					e.breakCompose(id, tc.running...)
					do := func(c string) error { e.docker.removeRunning(c); return nil }
					if tc.stop != nil {
						do = tc.stop(e)
					}
					e.docker.stopContainer = func(c string) error {
						dataAtStop = append(dataAtStop, readSnapFile(t, e.dataFile(id, "app.db")))
						return do(c)
					}
				})
				if path.doneErr == "" && err != nil || path.doneErr != "" && (err == nil || !strings.Contains(err.Error(), path.doneErr)) {
					t.Fatalf("error = %v, want %q", err, path.doneErr)
				}
				assertRolledBack(t, e, inst.ID)

				// The direct stops all come before the database is touched,
				// and they saw the data as the new version left it.
				if len(dataAtStop) != tc.stops {
					t.Fatalf("%d direct stops, want %d", len(dataAtStop), tc.stops)
				}
				for _, d := range dataAtStop {
					if d != newData {
						t.Fatalf("data at the direct stop = %q: the restore came first", d)
					}
				}
				lastStop, firstRestore := -1, -1
				for i, c := range e.docker.Calls()[before:] {
					switch {
					case c.method == "StopContainer" || c.method == "RunningContainers":
						lastStop = i
					case firstRestore < 0 && (strings.Contains(c.String(), "pg_restore") || strings.Contains(c.String(), "DROP DATABASE")):
						firstRestore = i
					}
				}
				if lastStop < 0 || firstRestore < lastStop {
					t.Fatalf("the check at %d, the first restore at %d: %v", lastStop, firstRestore, e.docker.methods()[before:])
				}
			})
		}
	}
}

func TestRollbackStopsBeforeAnythingChanges(t *testing.T) {
	cases := []struct {
		name string
		// block makes the check fail; unblock lets the retry through, with the
		// compose files still broken.
		block   func(e *testEnv)
		unblock func(e *testEnv)
	}{
		{
			name: "the containers keep running, the stop says so",
			block: func(e *testEnv) {
				e.docker.stopContainer = func(string) error { return errors.New("cannot stop") }
			},
			unblock: func(e *testEnv) { e.docker.stopContainer = nil },
		},
		{
			name: "the containers keep running, the stop says it worked",
			block: func(e *testEnv) {
				e.docker.stopContainer = func(string) error { return nil }
			},
			unblock: func(e *testEnv) { e.docker.stopContainer = nil },
		},
		{
			name:    "docker cannot answer",
			block:   func(e *testEnv) { e.docker.runningErr = errors.New("docker is gone") },
			unblock: func(e *testEnv) { e.docker.runningErr = nil },
		},
		{
			name: "docker stops answering after the direct stop",
			block: func(e *testEnv) {
				e.docker.stopContainer = func(string) error {
					e.docker.mu.Lock()
					defer e.docker.mu.Unlock()
					e.docker.runningErr = errors.New("docker is gone")
					return nil
				}
			},
			unblock: func(e *testEnv) {
				e.docker.stopContainer = nil
				e.docker.mu.Lock()
				defer e.docker.mu.Unlock()
				e.docker.runningErr = nil
			},
		},
	}
	for _, path := range rollbackPaths {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				e := newTestEnv(t)
				var restoresBefore int
				inst, err := path.run(t, e, func() {
					restoresBefore = len(restores(e))
					e.breakCompose(e.onlyInstance(t).ID, "c1")
					tc.block(e)
				})
				if err == nil || !strings.Contains(err.Error(), path.undoFails) {
					t.Fatalf("error = %v, want %q", err, path.undoFails)
				}
				assertNothingUndone(t, e, inst.ID, path.versionBefore, restoresBefore)
				if got := e.caddy.route(inst.ID); got != "splash:failed" {
					t.Errorf("route = %q, want the failed splash", got)
				}

				// A Start while it still cannot stop says so and changes nothing.
				if err := e.m.Start(ctx, inst.ID); err == nil || !strings.Contains(err.Error(), "could not stop the app") {
					t.Fatalf("start while the app cannot stop: %v", err)
				}
				assertNothingUndone(t, e, inst.ID, path.versionBefore, restoresBefore)

				// The files stay broken, but the containers can stop now.
				tc.unblock(e)
				if err := e.m.Start(ctx, inst.ID); err != nil {
					t.Fatalf("start: %v", err)
				}
				assertRolledBack(t, e, inst.ID)
			})
		}
	}
}

// interruptedUpdate leaves the app as a brain that stopped in the middle of an
// update leaves it: the new files and data, the row in `updating`.
func interruptedUpdate(t *testing.T, e *testEnv) store.Instance {
	t.Helper()
	ctx := context.Background()
	inst := installUp(t, e, "")
	oldPins, _ := e.store.GetInstanceImages(inst.ID)
	if err := e.m.keepPrevious(inst.ID, oldPins); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.TakeSnapshot(ctx, inst.ID); err != nil {
		t.Fatal(err)
	}
	rec := updateRecord{From: "1.0", To: "2.0", PrevKept: true, SnapshotTaken: true, Writing: true}
	if err := e.m.writeUpdateRecord(inst.ID, rec); err != nil {
		t.Fatal(err)
	}
	e.publish(t, "2.0", upImage2, upDigest2, "")
	app := mustLoadApp(t, e.m, "upapp")
	if err := e.m.writeInstanceDir(inst.ID, app.Manifest, app.Compose); err != nil {
		t.Fatal(err)
	}
	writeSnapFile(t, e.dataFile(inst.ID, "app.db"), "v2 migrated", 0o644)
	if err := e.store.SetState(inst.ID, "updating"); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestReconcileStopFailsNothingRuns(t *testing.T) {
	e := newTestEnv(t)
	inst := interruptedUpdate(t, e)
	e.breakCompose(inst.ID)
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" || r.UpdateFailure.Kind != store.UpdateFailedInterrupted {
		t.Fatalf("row: state %q version %q failure %+v", r.State, r.Version, r.UpdateFailure)
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v1 data" {
		t.Fatalf("data = %q, want the old data", got)
	}
}

func TestReconcileStopFailsContainersKeepRunning(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := interruptedUpdate(t, e)
	e.breakCompose(inst.ID, "c1")
	e.docker.stopContainer = func(string) error { return errors.New("cannot stop") }
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, inst.ID)
	if r.State != "update_failed" || r.UpdateFailure.Kind != store.UpdateFailedRestore {
		t.Fatalf("row: state %q failure %+v", r.State, r.UpdateFailure)
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v2 migrated" {
		t.Fatalf("data = %q: the undo touched it", got)
	}
	if !strings.Contains(e.onDisk(t, inst.ID), upImage2) || !e.exists(inst.ID, updateRecordFile) {
		t.Fatal("the undo changed the files, or the record is gone")
	}
	e.docker.stopContainer = nil
	if err := e.m.Start(ctx, inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if r := e.row(t, inst.ID); r.State != "running" || r.Version != "1.0" {
		t.Fatalf("after start: state %q version %q", r.State, r.Version)
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v1 data" {
		t.Fatalf("data = %q, want the old data", got)
	}
}

// TestReconcileRollbackStopFailsContainersKeepRunning: the same for a rollback
// by hand the brain did not finish.
func TestReconcileRollbackStopFailsContainersKeepRunning(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	id := inst.ID
	prev, _, err := e.m.readRecordFile(id, prevRecordFile)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := e.store.GetInstanceImages(id)
	rec := updateRecord{From: prev.From, To: prev.To, Rollback: true, PrevKept: true, SnapshotTaken: true, Writing: true, NewImages: cur}
	if err := e.m.writeUpdateRecord(id, rec); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetState(id, "updating"); err != nil {
		t.Fatal(err)
	}
	e.breakCompose(id, "c1")
	e.docker.runningErr = errors.New("docker is gone")
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, id); r.State != "update_failed" || r.Version != "2.0" || !e.exists(id, updateRecordFile) {
		t.Fatalf("row: state %q version %q record %v", r.State, r.Version, e.exists(id, updateRecordFile))
	}
	if got := readSnapFile(t, e.dataFile(id, "app.db")); got != newData {
		t.Fatalf("data = %q: the undo touched it", got)
	}
	e.docker.runningErr = nil
	if err := e.m.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if r := e.row(t, id); r.State != "running" || r.Version != "1.0" {
		t.Fatalf("after start: state %q version %q", r.State, r.Version)
	}
	if got := readSnapFile(t, e.dataFile(id, "app.db")); got != "v1 data" {
		t.Fatalf("data = %q, want the old data", got)
	}
}
