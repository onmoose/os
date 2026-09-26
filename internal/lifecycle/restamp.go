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
	if err := m.store.SetInstanceConfigAndAIBindings(id, change.Values, change.Slots, change.Bindings); err != nil {
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

// SlotResolver resolves one AI binding of an app into the values of its
// slot, from the account as it is now. Values maps app_env to value for the
// fields that get one; every other field of the slot is cleared.
type SlotResolver func(man *manifest.Manifest, b store.AIBinding) (map[string]string, error)

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
		changed := false
		for _, b := range bound {
			if b.AccountID != accountID {
				continue
			}
			slotValues, err := resolve(man, b)
			if err != nil {
				return false, fmt.Errorf("resolve %s: %w", b.Slot, err)
			}
			for _, f := range man.Config {
				r, ok := roles[f.AppEnv]
				if !ok || r.Slot() != b.Slot {
					continue
				}
				if v := slotValues[f.AppEnv]; v != "" {
					values[f.AppEnv] = v
				} else {
					delete(values, f.AppEnv)
				}
				changed = true
			}
		}
		if !changed {
			return false, nil
		}
		var cfg []store.InstanceConfig
		for _, f := range man.Config {
			if v, ok := values[f.AppEnv]; ok {
				cfg = append(cfg, store.InstanceConfig{AppEnv: f.AppEnv, Value: v, Secret: f.Secret})
			}
		}
		if err := m.store.SetInstanceConfig(inst.ID, cfg); err != nil {
			return false, fmt.Errorf("persist config: %w", err)
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

// restampEach runs apply on each app under its lock, then recreates the app
// when apply changed something and the app is running. An app uninstalled in
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
	changed, err := apply(inst)
	if err != nil {
		return inst.Name, err
	}
	if !changed || inst.State != "running" {
		return inst.Name, nil
	}
	if err := m.recreateRunning(ctx, inst); err != nil {
		return inst.Name, err
	}
	slog.Info("app restarted after account change", "instance_id", id, "name", inst.Name)
	return inst.Name, nil
}
