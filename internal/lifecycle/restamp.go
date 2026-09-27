package lifecycle

// Re-stamping the apps that use an account (DECISIONS.md 2026-09-26,
// INSTALL_SETUP.md piece 4). When a user edits or deletes an email or AI
// account, every app bound to it gets the new values, or loses the old ones,
// and the running apps restart so they read them. The API names the apps and
// commits the account change first; the functions here do the per-app part:
// write the brain's state, rewrite the files, recreate the running
// containers.
//
// Values that come from an account are resolved under the app's lock, from
// the account rows as they are at that moment, never from values captured
// before. Two quick edits of one account, or a config save racing an account
// edit, then end on the latest account whichever job commits last. The
// resolving itself stays in the API (it needs the provider data); lifecycle
// calls it back under the lock.
//
// One app failing does not stop the others. Each failure is logged, and the
// returned error names every app that could not be updated, so the job that
// runs this reports failure in words the user can act on.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// ConfigChange is what a config edit writes: the app's full set of config
// values, the slots whose binding is replaced or cleared, and the new binding
// rows for them.
type ConfigChange struct {
	Values   []store.InstanceConfig
	Slots    []string
	Bindings []store.AIBinding
}

// ConfigResolver works out a config edit from the app's manifest copy and its
// current values and bindings. UpdateConfig calls it under the app's lock, so
// what it reads is what the edit is applied to.
type ConfigResolver func(man *manifest.Manifest, current []store.InstanceConfig, bindings []store.AIBinding) (ConfigChange, error)

// UpdateConfig applies a config edit that may change AI bindings
// (INSTALL_SETUP.md piece 4). Under the app's lock it reads the current
// values and bindings, asks resolve for the change, and writes the values and
// the listed slots' bindings in one store transaction before the override is
// rewritten and the app recreated: brain commits first. A slot not listed
// keeps its binding. An error from resolve is returned as it is and nothing
// is written.
func (m *Manager) UpdateConfig(ctx context.Context, id string, resolve ConfigResolver) error {
	defer m.lockInstance(id)()
	inst, err := m.store.Get(id)
	if err != nil {
		return err
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	current, err := m.store.GetInstanceConfig(id)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	bound, err := m.store.ListInstanceAIBindings(id)
	if err != nil {
		return fmt.Errorf("read bindings: %w", err)
	}
	change, err := resolve(man, current, bound)
	if err != nil {
		return err
	}
	// The bindings the edit leaves alone must still be there when it commits:
	// the values for their slots came from them, and an account delete does
	// not take this lock.
	listed := map[string]bool{}
	for _, slot := range change.Slots {
		listed[slot] = true
	}
	var keep []store.AIBinding
	for _, b := range bound {
		if !listed[b.Slot] {
			keep = append(keep, b)
		}
	}
	if err := m.store.SetInstanceConfigAndAIBindings(id, change.Values, change.Slots, change.Bindings, keep); err != nil {
		if errors.Is(err, store.ErrBindingGone) {
			return errBindingGone
		}
		return fmt.Errorf("persist config: %w", err)
	}
	if err := m.restampConfigEnv(id, man); err != nil {
		return fmt.Errorf("rewrite override: %w", err)
	}
	if inst.State != "running" {
		slog.Info("app config updated (applies at next start)",
			"instance_id", id, "name", inst.Name)
		return nil
	}
	if err := m.recreateRunning(ctx, inst); err != nil {
		return err
	}
	slog.Info("app config updated", "instance_id", id, "name", inst.Name)
	return nil
}

// errBindingGone is the job error when an LLM provider account the save
// relied on was deleted while it ran. Nothing was written.
var errBindingGone = errors.New("an LLM provider account this app uses was deleted while saving, so nothing was changed. Check the settings and save again")

// SlotResolver resolves one AI binding of an app into the values of its
// slot, from the account as it is now. current is the app's stored values,
// for a field the resolver keeps as it is. The result maps app_env to value
// for the fields that get one; every other field of the slot is cleared.
type SlotResolver func(man *manifest.Manifest, b store.AIBinding, current map[string]string) (map[string]string, error)

// RestampAIAccount gives the apps bound to accountID their values again and
// restarts the running ones. It runs after an account's key or base URL
// changed. Under each app's lock it reads the app's bindings, and resolve
// reads the account, so an older job that runs after a newer edit still
// writes the newer key. A slot bound to another account by then is left
// alone. The other fields of the app keep their values.
func (m *Manager) RestampAIAccount(ctx context.Context, accountID string, ids []string, resolve SlotResolver) error {
	return m.restampEach(ctx, ids, "update app after AI account change failed", func(inst store.Instance) (bool, error) {
		man, err := m.loadInstanceManifest(inst.ID)
		if err != nil {
			return false, fmt.Errorf("load manifest: %w", err)
		}
		bound, err := m.store.ListInstanceAIBindings(inst.ID)
		if err != nil {
			return false, fmt.Errorf("read bindings: %w", err)
		}
		current, err := m.store.GetInstanceConfig(inst.ID)
		if err != nil {
			return false, fmt.Errorf("read config: %w", err)
		}
		values := make(map[string]string, len(current))
		for _, c := range current {
			values[c.AppEnv] = c.Value
		}
		roles := man.FillableRoles()
		var writes []store.SlotWrite
		for _, b := range bound {
			if b.AccountID != accountID {
				continue
			}
			slotValues, err := resolve(man, b, values)
			if err != nil {
				return false, fmt.Errorf("resolve %s: %w", b.Slot, err)
			}
			w := store.SlotWrite{Slot: b.Slot, Envs: []string{}}
			for _, f := range man.Config {
				r, ok := roles[f.AppEnv]
				if !ok || r.Slot() != b.Slot {
					continue
				}
				w.Envs = append(w.Envs, f.AppEnv)
				if v := slotValues[f.AppEnv]; v != "" {
					w.Values = append(w.Values, store.InstanceConfig{AppEnv: f.AppEnv, Value: v, Secret: f.Secret})
				}
			}
			writes = append(writes, w)
		}
		if len(writes) == 0 {
			return false, nil
		}
		// The store writes a slot only if it is still bound to accountID when
		// the transaction runs, so an account deleted since the read above
		// does not get its key written back.
		applied, err := m.store.ApplyAISlotValues(inst.ID, accountID, writes)
		if err != nil {
			return false, fmt.Errorf("persist config: %w", err)
		}
		if applied == 0 {
			return false, nil
		}
		if err := m.restampConfigEnv(inst.ID, man); err != nil {
			return false, fmt.Errorf("rewrite override: %w", err)
		}
		return true, nil
	})
}

// RestampConfig rewrites each app's override from its stored config values
// and restarts the running ones. It runs after an AI account was deleted: the
// store already lost the values the account gave, in the same transaction as
// the account (store.DeleteAIAccountAndValues), and this makes the files and
// the containers follow.
func (m *Manager) RestampConfig(ctx context.Context, ids []string) error {
	return m.restampEach(ctx, ids, "update app after AI account delete failed", func(inst store.Instance) (bool, error) {
		man, err := m.loadInstanceManifest(inst.ID)
		if err != nil {
			return false, fmt.Errorf("load manifest: %w", err)
		}
		if err := m.restampConfigEnv(inst.ID, man); err != nil {
			return false, fmt.Errorf("rewrite override: %w", err)
		}
		return true, nil
	})
}

// RestampMail rewrites each app's MOOSE_MAIL_* lines from its current binding
// and restarts the running ones. It runs after an email account was edited
// (the lines change) or deleted (the binding went with the account, so the
// lines are dropped). It needs only the binding and the .env, not the
// manifest, so a deleted account's credentials leave an app even when its
// manifest copy cannot be read.
func (m *Manager) RestampMail(ctx context.Context, ids []string) error {
	return m.restampEach(ctx, ids, "update app after email account change failed", func(inst store.Instance) (bool, error) {
		if err := m.rewriteEnvMail(inst.ID); err != nil {
			return false, fmt.Errorf("rewrite env: %w", err)
		}
		return true, nil
	})
}

// RestampAccounts is RestampConfig for the apps in configIDs and RestampMail
// for the apps in mailIDs, in one pass. It runs after a user delete, which can
// reach an app through both an AI and an email account. An app in both lists
// is rewritten for both before its one restart, so it never runs with half
// of the deleted user's values gone.
func (m *Manager) RestampAccounts(ctx context.Context, configIDs, mailIDs []string) error {
	config := map[string]bool{}
	mail := map[string]bool{}
	var ids []string
	for _, id := range configIDs {
		if !config[id] {
			config[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range mailIDs {
		if !mail[id] && !config[id] {
			ids = append(ids, id)
		}
		mail[id] = true
	}
	// Mail goes first because it needs no manifest. If the override then
	// fails, the app is still restarted without the mail lines, and the
	// error names it.
	return m.restampEach(ctx, ids, "update app after user delete failed", func(inst store.Instance) (bool, error) {
		changed := false
		if mail[inst.ID] {
			if err := m.rewriteEnvMail(inst.ID); err != nil {
				return false, fmt.Errorf("rewrite env: %w", err)
			}
			changed = true
		}
		if config[inst.ID] {
			man, err := m.loadInstanceManifest(inst.ID)
			if err != nil {
				return changed, fmt.Errorf("load manifest: %w", err)
			}
			if err := m.restampConfigEnv(inst.ID, man); err != nil {
				return changed, fmt.Errorf("rewrite override: %w", err)
			}
		}
		return true, nil
	})
}

// restampEach runs apply on each app under its lock, then recreates the app
// when apply changed something and the app is running, even if apply also
// failed on a later part. An app uninstalled in
// the meantime is skipped. A failure is logged and the loop goes on; the
// error at the end names the apps that failed. A failed recreate leaves the
// app marked pending-recreate (recreateRunning), so the reconcile pass
// retries it.
func (m *Manager) restampEach(ctx context.Context, ids []string, failMsg string, apply func(store.Instance) (bool, error)) error {
	var failed []string
	for _, id := range ids {
		name, err := m.restampOne(ctx, id, apply)
		if err == nil {
			continue
		}
		slog.Error(failMsg, "instance_id", id, "name", name, "err", err)
		if name == "" {
			name = id
		}
		failed = append(failed, name)
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("could not update %s. The rest were updated", strings.Join(failed, ", "))
}

// restampOne is one app of restampEach. It returns the app's name for the
// error message, empty when the app could not be read.
func (m *Manager) restampOne(ctx context.Context, id string, apply func(store.Instance) (bool, error)) (string, error) {
	defer m.lockInstance(id)()
	inst, err := m.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// apply may report a change and an error together: part of the work
	// landed (RestampAccounts). The app is restarted for that part, and the
	// error is still returned.
	changed, applyErr := apply(inst)
	if !changed || inst.State != "running" {
		return inst.Name, applyErr
	}
	if err := m.recreateRunning(ctx, inst); err != nil {
		return inst.Name, errors.Join(applyErr, err)
	}
	slog.Info("app restarted after account change", "instance_id", id, "name", inst.Name)
	return inst.Name, applyErr
}
