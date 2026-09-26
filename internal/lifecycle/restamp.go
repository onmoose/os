package lifecycle

// Re-stamping the apps that use an account (DECISIONS.md 2026-09-26,
// INSTALL_SETUP.md piece 4). When a user edits or deletes an email or AI
// account, every app bound to it gets the new values, or loses the old ones,
// and the running apps restart so they read them. The API names the apps and
// commits the account change first; the functions here do the per-app part:
// write the brain's state, rewrite the files, recreate the running
// containers.
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

// AISlotValues is the new values of one AI slot of one app, resolved again
// from the account the slot is bound to. Values maps app_env to value for the
// fields of the slot that get one; every other field of the slot is cleared.
type AISlotValues struct {
	InstanceID string
	Slot       string
	Values     map[string]string
}

// SetConfigAndAIBindings is SetConfig for an edit that also changes AI
// bindings (INSTALL_SETUP.md piece 4). The config values and the bindings of
// the listed slots are written in one store transaction, before the override
// is rewritten and the app recreated: brain commits first. Each slot in slots
// loses its binding, and then bindings are stored. A slot not listed keeps
// its binding. The caller (API) resolves and checks everything first.
func (m *Manager) SetConfigAndAIBindings(ctx context.Context, id string, cfg []store.InstanceConfig, slots []string, bindings []store.AIBinding) error {
	defer m.lockInstance(id)()
	inst, err := m.store.Get(id)
	if err != nil {
		return err
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if err := m.store.SetInstanceConfigAndAIBindings(id, cfg, slots, bindings); err != nil {
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

// RestampAIAccount writes new slot values into the apps bound to accountID
// and restarts the running ones. It runs after an account's key or base URL
// changed. Under each app's lock it checks that the slot is still bound to
// accountID, so a slot the user rebound in the meantime is left alone. The
// other fields of the app keep their values.
func (m *Manager) RestampAIAccount(ctx context.Context, accountID string, updates []AISlotValues) error {
	byInstance := map[string][]AISlotValues{}
	var ids []string
	for _, u := range updates {
		if _, ok := byInstance[u.InstanceID]; !ok {
			ids = append(ids, u.InstanceID)
		}
		byInstance[u.InstanceID] = append(byInstance[u.InstanceID], u)
	}
	return m.restampEach(ctx, ids, "update app after AI account change failed", func(inst store.Instance, man *manifest.Manifest) (bool, error) {
		bound, err := m.store.ListInstanceAIBindings(inst.ID)
		if err != nil {
			return false, fmt.Errorf("read bindings: %w", err)
		}
		stillBound := map[string]bool{}
		for _, b := range bound {
			if b.AccountID == accountID {
				stillBound[b.Slot] = true
			}
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
		for _, u := range byInstance[inst.ID] {
			if !stillBound[u.Slot] {
				continue
			}
			for _, f := range man.Config {
				r, ok := roles[f.AppEnv]
				if !ok || r.Slot() != u.Slot {
					continue
				}
				if v, ok := u.Values[f.AppEnv]; ok && v != "" {
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
	return m.restampEach(ctx, ids, "update app after AI account delete failed", func(inst store.Instance, man *manifest.Manifest) (bool, error) {
		if err := m.restampConfigEnv(inst.ID, man); err != nil {
			return false, fmt.Errorf("rewrite override: %w", err)
		}
		return true, nil
	})
}

// RestampMail rewrites each app's MOOSE_MAIL_* lines from its current binding
// and restarts the running ones. It runs after an email account was edited
// (the lines change) or deleted (the binding went with the account, so the
// lines are dropped).
func (m *Manager) RestampMail(ctx context.Context, ids []string) error {
	return m.restampEach(ctx, ids, "update app after email account change failed", func(inst store.Instance, _ *manifest.Manifest) (bool, error) {
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
func (m *Manager) restampEach(ctx context.Context, ids []string, failMsg string, apply func(store.Instance, *manifest.Manifest) (bool, error)) error {
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
func (m *Manager) restampOne(ctx context.Context, id string, apply func(store.Instance, *manifest.Manifest) (bool, error)) (string, error) {
	defer m.lockInstance(id)()
	inst, err := m.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return inst.Name, fmt.Errorf("load manifest: %w", err)
	}
	changed, err := apply(inst, man)
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
