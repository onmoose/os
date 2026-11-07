package lifecycle

// The rollback by hand (UPDATES.md # 4 Update mechanics per app, # 6, #634).
// For 7 days after a good update, a person can put the app back on the
// version it ran before, on the data it had then. It is for an update that
// passed its health check but broke something the next day.
//
// It uses what the update kept: the pre-update snapshot (#347), the previous
// generation (the *.prev files and the previous image pins, #631) and the
// update's record (update.prev.json), which says what the update added. So
// the rollback is the second half of a failed update's rollback, run later:
// the same undoUpdate, with a record marked Rollback in update.json, so a
// brain that stops in the middle finishes it at the next start.
//
// Data written since the update is lost, because the snapshot is from before
// it. The dashboard says so before the person confirms.
//
// After a rollback the app's automatic update is off, so the nightly window
// does not apply the same version again the next night (DECISIONS.md
// 2026-11-07). The version it left is stored as declined, so the update
// prompt does not ask about it again.
//
// The 7 days start when the snapshot was taken (store.LastUpdate.At), and the
// hourly prune removes the snapshot, the previous generation and its images in
// the same pass (pruneRollbackPoint), so the button and what it needs go
// together.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// ErrNoRollback is returned when there is no earlier version to go back to:
// the app was not updated, its 7 days are over, or what the rollback needs is
// gone.
var ErrNoRollback = errors.New("there is no earlier version of this app to go back to")

// ErrRollbackBusy is returned for an app that is being installed, updated or
// removed.
var ErrRollbackBusy = errors.New("the app is busy. Try again when it has finished")

// RollbackUntil reports until when inst can be rolled back by hand, and
// whether that is still ahead of now. It reads the row only; the rollback
// itself checks the files.
func RollbackUntil(inst store.Instance, now time.Time) (time.Time, bool) {
	if inst.LastUpdate.From == "" || inst.LastUpdate.At.IsZero() || inst.Version == manifest.CustomVersion {
		return time.Time{}, false
	}
	until := inst.LastUpdate.At.Add(SnapshotKeep)
	return until, now.Before(until)
}

// RollbackState reports whether an app in this state can be rolled back.
// A stopped or failed app can: the rollback ends with it running.
func RollbackState(state string) bool {
	switch state {
	case "running", "stopped", "failed":
		return true
	}
	return false
}

// Rollback puts an instance back on the version it ran before its last good
// update, on the data it had then. It returns a plain error the dashboard can
// show. Once the instance's lock is let go, it runs the update check, so the
// app shows the catalog's version again.
func (m *Manager) Rollback(ctx context.Context, id string, progress func(step string)) error {
	step := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	err := m.rollback(ctx, id, step)
	if err == nil {
		m.CheckUpdates(context.WithoutCancel(ctx))
	}
	return err
}

func (m *Manager) rollback(ctx context.Context, id string, step func(string)) error {
	defer m.lockInstance(id)()
	release, err := m.acquireHeavy(ctx)
	if err != nil {
		return err
	}
	defer release()

	inst, err := m.store.Get(id)
	if err != nil {
		return err
	}
	if !RollbackState(inst.State) {
		return ErrRollbackBusy
	}
	if _, ok := RollbackUntil(inst, time.Now()); !ok {
		return ErrNoRollback
	}
	step("checking_rollback")
	prev, ok, err := m.readRecordFile(id, prevRecordFile)
	switch {
	case err != nil:
		slog.Warn("rollback: read the update's record", "instance_id", id, "err", err)
		return ErrNoRollback
	case !ok || prev.From != inst.LastUpdate.From || prev.To != inst.Version:
		slog.Warn("rollback: the update's record does not match the app", "instance_id", id, "version", inst.Version)
		return ErrNoRollback
	}
	if _, err := readSnapshot(m.snapshotsDir(id), snapshotBase(prev.From)); err != nil {
		slog.Warn("rollback: the snapshot is gone", "instance_id", id, "snapshot", snapshotBase(prev.From), "err", err)
		return ErrNoRollback
	}
	prevPins, err := m.store.GetInstancePrevImages(id)
	if err != nil {
		return err
	}
	if len(prevPins) == 0 {
		slog.Warn("rollback: the previous image pins are gone", "instance_id", id)
		return ErrNoRollback
	}
	oldMan, err := m.loadManifestFile(id, "manifest.yml.prev")
	if err != nil {
		slog.Warn("rollback: read the previous manifest", "instance_id", id, "err", err)
		return ErrNoRollback
	}
	curPins, err := m.store.GetInstanceImages(id)
	if err != nil {
		return err
	}

	// The record says a rollback is under way, so the startup pass and a
	// Start finish it. The snapshot is complete and the previous generation
	// is whole, so the undo restores both.
	rec := updateRecord{
		From: prev.From, To: prev.To, Rollback: true,
		PrevKept: true, SnapshotTaken: true, Writing: true,
		AddedServices: prev.AddedServices, AddedSecrets: prev.AddedSecrets, AddedConfig: prev.AddedConfig,
		NetworkRemade: prev.NetworkRemade, NewImages: curPins,
	}
	prevState := inst.State
	if err := m.store.SetState(id, "updating"); err != nil {
		return fmt.Errorf("set state updating: %w", err)
	}
	inst.State = "updating"
	m.emitState(inst, prevState)
	if err := m.writeUpdateRecord(id, rec); err != nil {
		slog.Warn("rollback: write the record", "instance_id", id, "err", err)
		if err := m.store.SetState(id, prevState); err != nil {
			slog.Warn("rollback: set the state back", "instance_id", id, "err", err)
		}
		inst.State = prevState
		m.emitState(inst, "updating")
		return errors.New("moose could not start going back to the earlier version")
	}
	slog.Info("app rollback started", "instance_id", id, "name", inst.Name, "version", inst.Version, "new_version", rec.From)

	// From here the rollback runs to its end even when the caller is gone.
	ctx = context.WithoutCancel(ctx)
	host := m.routeHost(inst)
	if err := m.caddy.AddSplashRoute(ctx, id, host, inst.Name, "updating"); err != nil {
		slog.Warn("rollback: caddy splash flip failed (continuing)", "instance_id", id, "host", host, "err", err)
	}
	step("restoring_snapshot")
	if err := m.undoUpdate(ctx, id, rec); err != nil {
		slog.Error("app rollback failed", "instance_id", id, "version", rec.To, "new_version", rec.From, "err", err)
		m.endRollbackFailed(ctx, inst, oldMan)
		return fmt.Errorf("moose could not go back to version %s. Start the app to try again", rec.From)
	}
	// The old files and data are back: the row says so first (the brain
	// commits first), then the previous generation goes.
	if err := m.recordRollback(id, rec); err != nil {
		m.endRollbackFailed(ctx, inst, oldMan)
		return fmt.Errorf("moose could not record the move back to version %s. Start the app to try again", rec.From)
	}
	m.finishRecord(id, rec)
	inst.Version, inst.Name = rec.From, oldMan.Name

	step("compose_up")
	if err := m.startOld(ctx, inst, oldMan); err != nil {
		slog.Warn("app rollback: the earlier version did not start", "instance_id", id, "version", rec.From, "err", err)
		if err := m.caddy.AddSplashRoute(ctx, id, host, oldMan.Name, "failed"); err != nil {
			slog.Warn("rollback: caddy splash flip failed", "instance_id", id, "host", host, "err", err)
		}
		if err := m.store.SetState(id, "failed"); err != nil {
			slog.Warn("rollback: set state failed", "instance_id", id, "err", err)
		}
		inst.State = "failed"
		m.emitState(inst, "updating")
		m.publishUpdateChanged(id)
		return fmt.Errorf("moose went back to version %s, but it did not start. Start the app to try again", rec.From)
	}
	inst.State = "running"
	m.emitState(inst, "updating")
	m.publishUpdateChanged(id)
	slog.Info("app rolled back", "instance_id", id, "name", oldMan.Name, "version", rec.To, "new_version", rec.From)
	return nil
}

// recordRollback writes the row of a finished rollback: the version it went
// back to, with its name from the manifest now on disk.
func (m *Manager) recordRollback(id string, rec updateRecord) error {
	name := ""
	if man, err := m.loadInstanceManifest(id); err == nil {
		name = man.Name
	} else {
		slog.Warn("rollback: read the manifest for the name", "instance_id", id, "err", err)
		if inst, err := m.store.Get(id); err == nil {
			name = inst.Name
		}
	}
	if err := m.store.FinishInstanceRollback(id, rec.From, name, rec.To); err != nil {
		slog.Error("rollback: record it", "instance_id", id, "err", err)
		return err
	}
	return nil
}

// endRollbackFailed ends a rollback that could not finish: the row is
// update_failed and update.json stays, so a Start finishes it
// (finishRollback). No update failure is recorded: no update was tried.
func (m *Manager) endRollbackFailed(ctx context.Context, inst store.Instance, man *manifest.Manifest) {
	host := m.routeHost(inst)
	if err := m.caddy.AddSplashRoute(ctx, inst.ID, host, man.Name, "failed"); err != nil {
		slog.Warn("rollback: caddy splash flip failed", "instance_id", inst.ID, "host", host, "err", err)
	}
	if err := m.store.SetState(inst.ID, "update_failed"); err != nil {
		slog.Warn("rollback: set state update_failed", "instance_id", inst.ID, "err", err)
	}
	inst.State = "update_failed"
	m.emitState(inst, "updating")
	m.publishUpdateChanged(inst.ID)
}

// recoverRollback finishes, at startup, a rollback the brain did not finish.
// The undo can run again, so it simply runs it to its end.
func (m *Manager) recoverRollback(ctx context.Context, inst store.Instance, rec updateRecord) {
	if err := m.undoUpdate(ctx, inst.ID, rec); err != nil {
		slog.Error("reconcile: could not finish a rollback", "instance_id", inst.ID, "version", rec.From, "err", err)
		m.setStateAtStartup(inst, "update_failed")
		return
	}
	if err := m.recordRollback(inst.ID, rec); err != nil {
		m.setStateAtStartup(inst, "update_failed")
		return
	}
	m.finishRecord(inst.ID, rec)
	if out, err := m.composeUpInstance(ctx, inst.ID, nil); err != nil {
		slog.Warn("reconcile: compose up after a rollback", "instance_id", inst.ID, "err", err, "output", out)
	}
	inst.State, inst.Version = "running", rec.From
	m.emitState(inst, "updating")
	m.publishUpdateChanged(inst.ID)
	slog.Info("reconcile: rollback finished", "instance_id", inst.ID, "version", rec.To, "new_version", rec.From)
}

func (m *Manager) setStateAtStartup(inst store.Instance, state string) {
	if err := m.store.SetState(inst.ID, state); err != nil {
		slog.Warn("reconcile: set state", "instance_id", inst.ID, "err", err)
	}
	prev := inst.State
	inst.State = state
	m.emitState(inst, prev)
}

func (m *Manager) loadManifestFile(id, name string) (*manifest.Manifest, error) {
	b, err := os.ReadFile(filepath.Join(m.instanceDir(id), name))
	if err != nil {
		return nil, err
	}
	return manifest.Parse(b)
}

// pruneRollbackPoint removes the previous generation of an instance once its
// rollback by hand is over: 7 days after the snapshot, or as soon as the
// snapshot is gone. Its images go when nothing else uses them. An instance
// with update.json is left alone: a rollback that did not finish needs its
// previous generation. The caller holds the instance's lock.
//
// A previous generation with no row record (an update made by a brain from
// before #634) goes by the age of its manifest.yml.prev.
func (m *Manager) pruneRollbackPoint(inst store.Instance, now time.Time) bool {
	id := inst.ID
	if _, ok, err := m.readUpdateRecord(id); ok || err != nil {
		return false
	}
	pins, err := m.store.GetInstancePrevImages(id)
	if err != nil {
		slog.Warn("snapshot prune: read the previous image pins", "instance_id", id, "err", err)
		return false
	}
	prevFile := filepath.Join(m.instanceDir(id), "manifest.yml.prev")
	info, statErr := os.Stat(prevFile)
	if len(pins) == 0 && errors.Is(statErr, fs.ErrNotExist) {
		if inst.LastUpdate.From != "" {
			// Nothing left to roll back to.
			if err := m.store.ClearInstanceLastUpdate(id); err != nil {
				slog.Warn("snapshot prune: clear the last update", "instance_id", id, "err", err)
			}
			return true
		}
		return false
	}
	var expired bool
	if inst.LastUpdate.From != "" {
		_, ok := RollbackUntil(inst, now)
		_, snapErr := readSnapshot(m.snapshotsDir(id), snapshotBase(inst.LastUpdate.From))
		expired = !ok || snapErr != nil
	} else {
		expired = statErr != nil || now.Sub(info.ModTime()) >= SnapshotKeep
	}
	if !expired {
		return false
	}
	m.dropPrevious(id)
	m.reclaimImages(context.Background(), id, pins)
	slog.Info("snapshot prune: the earlier version can no longer be rolled back to", "instance_id", id, "version", inst.LastUpdate.From)
	m.publishUpdateChanged(id)
	return true
}
