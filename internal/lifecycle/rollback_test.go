package lifecycle

// The rollback by hand, the older generation a failed update keeps, the
// 7-day cleanup of the previous generation, and the update notifications
// (#634), against the fakes.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/notify"
	"github.com/onmoose/os/internal/store"
)

const (
	upImage3  = "traefik/whoami:v1.10.5"
	upDigest3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// versionsMigrate makes each new version write its own data when it starts,
// and the versions in bad never become healthy.
func (e *testEnv) versionsMigrate(t *testing.T, bad ...string) {
	t.Helper()
	images := map[string]string{"2.0": upImage2, "3.0": upImage3}
	e.docker.composeUp = func(_ context.Context, dir, _ string) (string, error) {
		b, _ := os.ReadFile(filepath.Join(dir, "compose.yml"))
		for v, img := range images {
			if strings.Contains(string(b), img) {
				writeSnapFile(t, filepath.Join(dir, "data", "app.db"), "v"+v+" migrated", 0o644)
			}
		}
		return "", nil
	}
	e.docker.inspect = func(id, _ string) (bool, string, error) {
		for _, v := range bad {
			if strings.Contains(e.onDisk(t, id), images[v]) {
				return true, "unhealthy", nil
			}
		}
		return true, "healthy", nil
	}
}

func (e *testEnv) removedImage(digest string) bool {
	for _, c := range e.docker.Calls() {
		if c.method == "RemoveImage" && strings.Contains(c.String(), digest) {
			return true
		}
	}
	return false
}

// updateTo2 installs 1.0 and updates it to 2.0, which goes well.
func updateTo2(t *testing.T, e *testEnv, bad ...string) store.Instance {
	t.Helper()
	inst := installUp(t, e, "")
	e.versionsMigrate(t, bad...)
	e.publish(t, "2.0", upImage2, upDigest2, "")
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update to 2.0: %v", err)
	}
	return inst
}

func TestGoodUpdateKeepsARollbackPoint(t *testing.T) {
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	r := e.row(t, inst.ID)
	snap, err := readSnapshot(e.m.snapshotsDir(inst.ID), snapshotBase("1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if r.LastUpdate.From != "1.0" || !r.LastUpdate.At.Equal(snap.CreatedAt.Truncate(time.Second)) || r.LastUpdate.ByWindow {
		t.Fatalf("last update %+v, want from 1.0 at the snapshot's time %v", r.LastUpdate, snap.CreatedAt)
	}
	if until, ok := RollbackUntil(r, time.Now()); !ok || until.Sub(r.LastUpdate.At) != SnapshotKeep {
		t.Fatalf("rollback until %v %v", until, ok)
	}
	rec, ok, err := e.m.readRecordFile(inst.ID, prevRecordFile)
	if err != nil || !ok || rec.From != "1.0" || rec.To != "2.0" || rec.OlderKept || len(rec.DroppedImages) != 0 {
		t.Fatalf("kept record %+v %v %v", rec, ok, err)
	}
	if e.exists(inst.ID, updateRecordFile) || e.exists(inst.ID, "compose.yml.prev"+olderSuffix) {
		t.Fatal("a good update left update.json or an older copy")
	}
}

func TestRollbackByHand(t *testing.T) {
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	writeSnapFile(t, e.dataFile(inst.ID, "app.db"), "written after the update", 0o644)

	var steps []string
	if err := e.m.Rollback(context.Background(), inst.ID, func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	r := e.row(t, inst.ID)
	switch {
	case r.State != "running" || r.Version != "1.0":
		t.Fatalf("after the rollback: state %q version %q", r.State, r.Version)
	case !r.AutoUpdateOff:
		t.Fatal("the automatic update is still on after a rollback")
	case r.UpdateDeclined != "2.0":
		t.Fatalf("declined %q, want 2.0", r.UpdateDeclined)
	case r.LastUpdate != (store.LastUpdate{}):
		t.Fatalf("still a rollback point: %+v", r.LastUpdate)
	}
	// The update check ran after the lock was let go: the catalog's 2.0 is
	// offered again, by hand.
	if r.Update.Version != "2.0" || r.Update.Status != store.UpdateAvailable {
		t.Fatalf("update after the rollback: %+v", r.Update)
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v1 data" {
		t.Fatalf("data = %q, want the data from before the update", got)
	}
	if !strings.Contains(e.onDisk(t, inst.ID), upImage1) {
		t.Fatal("the old compose is not back")
	}
	if pins, _ := e.store.GetInstanceImages(inst.ID); !slices.Equal(pinDigests(t, pins), []string{upDigest1}) {
		t.Fatalf("pins %v", pins)
	}
	if prev, _ := e.store.GetInstancePrevImages(inst.ID); len(prev) != 0 {
		t.Fatalf("previous pins %v", prev)
	}
	for _, f := range []string{"compose.yml.prev", "manifest.yml.prev", prevRecordFile, updateRecordFile} {
		if e.exists(inst.ID, f) {
			t.Errorf("%s left after the rollback", f)
		}
	}
	if !e.removedImage(upDigest2) || e.removedImage(upDigest1) {
		t.Error("want the 2.0 image removed and the 1.0 image kept")
	}
	if got := e.caddy.route(inst.ID); got != "upstream:moose-"+inst.ID+"-app:80" {
		t.Errorf("route = %q", got)
	}
	if !slices.Contains(steps, "restoring_snapshot") {
		t.Errorf("steps %v", steps)
	}
	// One generation: there is nothing more to go back to.
	if err := e.m.Rollback(context.Background(), inst.ID, nil); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("second rollback: %v, want ErrNoRollback", err)
	}
}

// TestFailedUpdateKeepsTheOlderRollback: 1.0 to 2.0 goes well, 2.0 to 3.0
// fails and is rolled back. The way back to 1.0 is still there (#631's known
// gap, closed here).
func TestFailedUpdateKeepsTheOlderRollback(t *testing.T) {
	e := newTestEnv(t)
	inst := updateTo2(t, e, "3.0")
	e.publish(t, "3.0", upImage3, upDigest3, "")
	if err := e.m.Update(context.Background(), inst.ID, nil); err == nil {
		t.Fatal("the update to 3.0 went through")
	}
	r := e.row(t, inst.ID)
	if r.Version != "2.0" || r.LastUpdate.From != "1.0" {
		t.Fatalf("after the failed update: version %q last %+v", r.Version, r.LastUpdate)
	}
	if prev, _ := e.store.GetInstancePrevImages(inst.ID); !slices.Equal(pinDigests(t, prev), []string{upDigest1}) {
		t.Fatalf("previous pins %v, want 1.0's", prev)
	}
	if b, _ := os.ReadFile(filepath.Join(e.m.instanceDir(inst.ID), "compose.yml.prev")); !strings.Contains(string(b), upImage1) {
		t.Fatal("compose.yml.prev is not 1.0's")
	}
	if rec, ok, _ := e.m.readRecordFile(inst.ID, prevRecordFile); !ok || rec.From != "1.0" || rec.To != "2.0" {
		t.Fatalf("kept record %+v", rec)
	}
	for _, f := range updateFiles {
		if e.exists(inst.ID, f+".prev"+olderSuffix) {
			t.Errorf("%s.prev.old left", f)
		}
	}
	if !e.removedImage(upDigest3) || e.removedImage(upDigest1) {
		t.Error("want the 3.0 image removed and the 1.0 image kept")
	}
	if err := e.m.Rollback(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("rollback to 1.0: %v", err)
	}
	if r := e.row(t, inst.ID); r.Version != "1.0" || readSnapFile(t, e.dataFile(inst.ID, "app.db")) != "v1 data" {
		t.Fatalf("after the rollback: version %q", r.Version)
	}
}

func TestRollbackRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("never updated", func(t *testing.T) {
		e := newTestEnv(t)
		inst := installUp(t, e, "")
		if err := e.m.Rollback(ctx, inst.ID, nil); !errors.Is(err, ErrNoRollback) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("seven days over", func(t *testing.T) {
		e := newTestEnv(t)
		inst := updateTo2(t, e)
		old := store.LastUpdate{From: "1.0", At: time.Now().Add(-SnapshotKeep - time.Minute)}
		if err := e.store.FinishInstanceUpdate(inst.ID, "2.0", inst.Name, old); err != nil {
			t.Fatal(err)
		}
		if _, ok := RollbackUntil(e.row(t, inst.ID), time.Now()); ok {
			t.Fatal("RollbackUntil says yes after 7 days")
		}
		if err := e.m.Rollback(ctx, inst.ID, nil); !errors.Is(err, ErrNoRollback) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("snapshot gone", func(t *testing.T) {
		e := newTestEnv(t)
		inst := updateTo2(t, e)
		if err := os.RemoveAll(e.m.snapshotsDir(inst.ID)); err != nil {
			t.Fatal(err)
		}
		if err := e.m.Rollback(ctx, inst.ID, nil); !errors.Is(err, ErrNoRollback) {
			t.Fatalf("got %v", err)
		}
		if r := e.row(t, inst.ID); r.State != "running" || r.Version != "2.0" {
			t.Fatalf("a refused rollback changed the row: %s %s", r.State, r.Version)
		}
	})
	t.Run("busy", func(t *testing.T) {
		e := newTestEnv(t)
		inst := updateTo2(t, e)
		if err := e.store.SetState(inst.ID, "uninstalling"); err != nil {
			t.Fatal(err)
		}
		if err := e.m.Rollback(ctx, inst.ID, nil); !errors.Is(err, ErrRollbackBusy) {
			t.Fatalf("got %v", err)
		}
	})
}

// TestRollbackOfAStoppedApp ends with the earlier version running.
func TestRollbackOfAStoppedApp(t *testing.T) {
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	if err := e.store.SetState(inst.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Rollback(context.Background(), inst.ID, nil); err != nil {
		t.Fatal(err)
	}
	if r := e.row(t, inst.ID); r.State != "running" || r.Version != "1.0" {
		t.Fatalf("state %q version %q", r.State, r.Version)
	}
}

// TestRollbackRestoreFailsThenStartFinishesIt: a rollback whose undo fails
// leaves update_failed and its record, and a Start finishes it.
func TestRollbackRestoreFailsThenStartFinishesIt(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	// The record of the snapshot is there, its tar is not: the restore fails.
	tar := filepath.Join(e.m.snapshotsDir(inst.ID), snapshotBase("1.0")+".tar")
	if err := os.Rename(tar, tar+".away"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Rollback(ctx, inst.ID, nil); err == nil || !strings.Contains(err.Error(), "could not go back to version 1.0") {
		t.Fatalf("rollback: %v", err)
	}
	if r := e.row(t, inst.ID); r.State != "update_failed" || !e.exists(inst.ID, updateRecordFile) || r.UpdateFailure != (store.UpdateFailure{}) {
		t.Fatalf("state %q, record %v, failure %+v", r.State, e.exists(inst.ID, updateRecordFile), r.UpdateFailure)
	}
	if err := os.Rename(tar+".away", tar); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(ctx, inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" || e.exists(inst.ID, updateRecordFile) || !r.AutoUpdateOff {
		t.Fatalf("after Start: state %q version %q", r.State, r.Version)
	}
	if readSnapFile(t, e.dataFile(inst.ID, "app.db")) != "v1 data" {
		t.Fatal("Start did not restore the data")
	}
}

// TestReconcileFinishesAnInterruptedRollback: a brain that stopped in the
// middle of a rollback by hand finishes it at the next start, and records no
// update failure.
func TestReconcileFinishesAnInterruptedRollback(t *testing.T) {
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
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	r := e.row(t, id)
	if r.State != "running" || r.Version != "1.0" || r.UpdateFailure != (store.UpdateFailure{}) || !r.AutoUpdateOff {
		t.Fatalf("after the startup pass: state %q version %q failure %+v auto off %v", r.State, r.Version, r.UpdateFailure, r.AutoUpdateOff)
	}
	if readSnapFile(t, e.dataFile(id, "app.db")) != "v1 data" || e.exists(id, updateRecordFile) || e.exists(id, prevRecordFile) {
		t.Fatal("the startup pass did not finish the rollback")
	}
}

// TestPruneRemovesTheRollbackPoint: the hourly prune keeps the previous
// generation for 7 days, then removes it with the snapshot and the images.
func TestPruneRemovesTheRollbackPoint(t *testing.T) {
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	e.m.PruneSnapshots(time.Now())
	if r := e.row(t, inst.ID); r.LastUpdate.From != "1.0" || !e.exists(inst.ID, "compose.yml.prev") {
		t.Fatal("the prune removed a rollback point inside its 7 days")
	}
	e.m.PruneSnapshots(time.Now().Add(SnapshotKeep + time.Minute))
	r := e.row(t, inst.ID)
	if r.LastUpdate != (store.LastUpdate{}) {
		t.Fatalf("last update %+v after 7 days", r.LastUpdate)
	}
	if prev, _ := e.store.GetInstancePrevImages(inst.ID); len(prev) != 0 {
		t.Fatalf("previous pins %v after 7 days", prev)
	}
	for _, f := range []string{"compose.yml.prev", "manifest.yml.prev", ".env.prev", "compose.override.yml.prev", prevRecordFile} {
		if e.exists(inst.ID, f) {
			t.Errorf("%s left after 7 days", f)
		}
	}
	if slices.Contains(snapshotFiles(t, e, inst.ID), "pre-update-1.0.json") {
		t.Error("the snapshot is left after 7 days")
	}
	if !e.removedImage(upDigest1) || e.removedImage(upDigest2) {
		t.Error("want the 1.0 image removed and the 2.0 image kept")
	}
}

// TestPruneLeavesAnUnfinishedRollback: an app with update.json keeps its
// previous generation, which the rollback still needs.
func TestPruneLeavesAnUnfinishedRollback(t *testing.T) {
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	if err := e.m.writeUpdateRecord(inst.ID, updateRecord{From: "1.0", To: "2.0", Rollback: true}); err != nil {
		t.Fatal(err)
	}
	e.m.PruneSnapshots(time.Now().Add(SnapshotKeep + time.Minute))
	if !e.exists(inst.ID, "compose.yml.prev") {
		t.Fatal("the prune removed the previous generation of an unfinished rollback")
	}
}

// fakeUpdateNotifier records what the lifecycle tells.
type fakeUpdateNotifier struct {
	mu                         sync.Mutex
	applied, failed, approvals []notify.AppUpdate
	done                       []string
}

func (f *fakeUpdateNotifier) AppUpdateApplied(u notify.AppUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, u)
}

func (f *fakeUpdateNotifier) AppUpdateFailed(u notify.AppUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, u)
}

func (f *fakeUpdateNotifier) AppUpdateDone(id, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = append(f.done, id+"@"+version)
}

func (f *fakeUpdateNotifier) AppUpdateNeedsApproval(u notify.AppUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals = append(f.approvals, u)
}

// TestNightlyNotifiesAndMarksTheWindow: the window's good update and its
// failures are told to the owner and marked as the window's; the third
// failure in a row says paused.
func TestNightlyNotifiesAndMarksTheWindow(t *testing.T) {
	sto := stockholm(t)
	r := newNightRig(t, "Europe/Stockholm")
	fn := &fakeUpdateNotifier{}
	r.e.m.SetUpdateNotifier(fn)
	r.app(t, "a", 1, func(i *store.Instance) { i.Scope, i.OwnerUserID = store.ScopePersonal, "u_1" })
	r.app(t, "b", 2, nil)
	r.fail["b"] = true
	r.n.update = func(_ context.Context, id string) error {
		if r.fail[id] {
			row, _ := r.e.store.Get(id)
			if err := r.e.store.RecordInstanceUpdateFailure(id, store.UpdateFailure{Version: row.Update.Version, Kind: store.UpdateFailedHealth, Message: "x", At: time.Now()}); err != nil {
				return err
			}
			return errors.New("the new version did not become healthy")
		}
		return r.e.store.FinishInstanceUpdate(id, "2.0", "App "+id, store.LastUpdate{From: "1.0", At: time.Now()})
	}
	r.host.done("2026-11-07")
	r.step(time.Date(2026, 11, 7, 3, 0, 0, 0, sto))
	r.step(time.Date(2026, 11, 7, 3, 1, 0, 0, sto))

	if len(fn.applied) != 1 || fn.applied[0] != (notify.AppUpdate{InstanceID: "a", Name: "App a", OwnerUserID: "u_1", From: "1.0", To: "2.0"}) {
		t.Fatalf("applied %+v", fn.applied)
	}
	if len(fn.failed) != 1 || fn.failed[0].InstanceID != "b" || !fn.failed[0].Household || fn.failed[0].Paused {
		t.Fatalf("failed %+v", fn.failed)
	}
	if !r.e.row(t, "a").LastUpdate.ByWindow || !r.e.row(t, "b").UpdateFailure.ByWindow {
		t.Fatal("the outcomes are not marked as the window's")
	}
	// Two more nights of failures: the third says paused.
	for _, day := range []int{8, 9} {
		night := time.Date(2026, 11, day, 3, 0, 0, 0, sto)
		r.host.done(night.Format("2006-01-02"))
		r.step(night)
	}
	if len(fn.failed) != 3 || !fn.failed[2].Paused {
		t.Fatalf("failed %+v, want the third paused", fn.failed)
	}
}

// TestCheckTellsANewApprovalOnce: an update that newly waits for a yes is
// told once; the same version found again is not told again.
func TestCheckTellsANewApprovalOnce(t *testing.T) {
	e := newTestEnv(t)
	fn := &fakeUpdateNotifier{}
	e.m.SetUpdateNotifier(fn)
	inst := installUp(t, e, "")
	// 2.0 asks for internet.
	e.writeCatalogApp(t, "upapp", upCompose(upImage2), strings.Replace(upManifest("2.0", upImage2, upDigest2, ""), "internet: false", "internet: true", 1))
	e.m.CheckUpdates(context.Background())
	e.m.CheckUpdates(context.Background())
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateNeedsApproval {
		t.Fatalf("check: %+v", r.Update)
	}
	if len(fn.approvals) != 1 || fn.approvals[0].To != "2.0" || fn.approvals[0].From != "1.0" {
		t.Fatalf("approvals %+v", fn.approvals)
	}
}

// TestRecoveryAfterTheUndoDoesNotRunItAgain: a failed 2.0 to 3.0 update
// whose undo finished, and whose older copy (1.0) is back in .prev, when the
// brain stopped before update.json was removed. The startup pass must not
// copy .prev (now 1.0) over the current files: the app stays on 2.0.
func TestRecoveryAfterTheUndoDoesNotRunItAgain(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	id := inst.ID
	prev, _ := e.store.GetInstancePrevImages(id)
	cur, _ := e.store.GetInstanceImages(id)
	rec := updateRecord{From: "2.0", To: "3.0", PrevKept: true, Writing: true, OlderKept: true, UndoDone: true,
		DroppedImages: prev, NewImages: []store.InstanceImage{{Service: "app", Image: upImage3, Digest: upDigest3}}}
	if err := e.m.writeUpdateRecord(id, rec); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetState(id, "updating"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.onDisk(t, id), upImage2) {
		t.Fatal("the startup pass copied the older version over the current files")
	}
	if pins, _ := e.store.GetInstanceImages(id); !slices.Equal(pinDigests(t, pins), pinDigests(t, cur)) {
		t.Fatalf("pins %v, want 2.0's", pins)
	}
	if r := e.row(t, id); r.State != "running" || r.Version != "2.0" || e.exists(id, updateRecordFile) {
		t.Fatalf("state %q version %q", r.State, r.Version)
	}
}

// TestRollbackTakesTheCurrentAccountValues: a setting changed after the
// update (as an AI account delete clears its key) must not come back with the
// old override when the app is rolled back.
func TestRollbackTakesTheCurrentAccountValues(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := installUp(t, e, "config:\n  - app_env: API_KEY\n    title: Key\n    description: The key.\n    service: app\n")
	if err := e.store.SetInstanceConfig(inst.ID, []store.InstanceConfig{{AppEnv: "API_KEY", Value: "old-key"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.restampConfigEnv(inst.ID, mustManifest(t, e, inst.ID)); err != nil {
		t.Fatal(err)
	}
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, "config:\n  - app_env: API_KEY\n    title: Key\n    description: The key.\n    service: app\n")
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	// The account went away after the update: the value is gone from the store.
	if err := e.store.SetInstanceConfig(inst.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Rollback(ctx, inst.ID, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	ov, _ := os.ReadFile(filepath.Join(e.m.instanceDir(inst.ID), "compose.override.yml"))
	if strings.Contains(string(ov), "old-key") {
		t.Fatalf("the rollback brought back a removed value:\n%s", ov)
	}
}

func mustManifest(t *testing.T, e *testEnv, id string) *manifest.Manifest {
	t.Helper()
	m, err := e.m.loadInstanceManifest(id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestPruneKeepsTheSnapshotOfAnUnfinishedRollback: past the 7 days, an app
// with update.json keeps its snapshot too, so a Start can still finish.
func TestPruneKeepsTheSnapshotOfAnUnfinishedRollback(t *testing.T) {
	e := newTestEnv(t)
	inst := updateTo2(t, e)
	if err := e.m.writeUpdateRecord(inst.ID, updateRecord{From: "1.0", To: "2.0", Rollback: true}); err != nil {
		t.Fatal(err)
	}
	e.m.PruneSnapshots(time.Now().Add(SnapshotKeep + time.Minute))
	if !slices.Contains(snapshotFiles(t, e, inst.ID), "pre-update-1.0.json") {
		t.Fatal("the prune removed the snapshot an unfinished rollback needs")
	}
}

// TestAPersonsUpdateResolvesItsNotifications: Allow & update or Update, not
// only the window, ends the wait for a yes of the version it installed.
func TestAPersonsUpdateResolvesItsNotifications(t *testing.T) {
	e := newTestEnv(t)
	fn := &fakeUpdateNotifier{}
	e.m.SetUpdateNotifier(fn)
	inst := updateTo2(t, e)
	if !slices.Equal(fn.done, []string{inst.ID + "@2.0"}) {
		t.Fatalf("done %v", fn.done)
	}
}
