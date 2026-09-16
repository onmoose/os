package lifecycle

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"

	"gopkg.in/yaml.v3"
)

// SetResourceLimits records the per-instance cgroup limit policy for an app and
// is the seam a control-plane policy push (hosted) or a user-set memory cap
// (appliance) calls (ENVIRONMENT.md # Per-instance resource limits,
// APP_ISOLATION.md # Resource limits). It only persists the policy; the change
// is rendered onto the app's compose override and the container recreated on the
// next reconcile pass (reapplyResourceLimits), so it takes effect without a
// reinstall. Returns store.ErrNotFound for an unknown instance.
func (m *Manager) SetResourceLimits(id string, lim store.ResourceLimits) error {
	if lim.MemoryBytes < 0 || lim.NanoCPUs < 0 {
		return fmt.Errorf("resource limits must be non-negative")
	}
	if _, err := m.store.Get(id); err != nil {
		return err // ErrNotFound surfaces to the caller
	}
	return m.store.SetResourceLimits(id, lim)
}

// resourceLimitsStanza renders the compose `deploy.resources.limits` block for
// an app's main service from its persisted policy, or nil when nothing applies
// (the caller then omits it and the container bursts freely — APP_ISOLATION.md #
// Resource limits, the default runtime).
//
// The mechanism is profile-divergent per the locked runtime model: memory is
// honored in both profiles (the appliance's optional user-set cap and the hosted
// control-plane policy), but CPU is only ever capped in hosted. APP_ISOLATION.md
// is explicit that CPU is never capped on the appliance (it's time-shared;
// throttling only makes an app feel sluggish), so a stray CPU value is dropped
// there rather than rendered.
func resourceLimitsStanza(p profile.Profile, lim store.ResourceLimits) map[string]any {
	limits := map[string]any{}
	if lim.MemoryBytes > 0 {
		// A bare integer under `memory` is bytes — maps to HostConfig.Memory.
		limits["memory"] = lim.MemoryBytes
	}
	if p == profile.Hosted && lim.NanoCPUs > 0 {
		limits["cpus"] = formatCPUs(lim.NanoCPUs)
	}
	if len(limits) == 0 {
		return nil
	}
	return map[string]any{"resources": map[string]any{"limits": limits}}
}

// formatCPUs renders a NanoCPUs count as the decimal core count Compose's
// `cpus:` field expects (1_500_000_000 → "1.5"); docker compose maps it back to
// the container's HostConfig.NanoCpus.
func formatCPUs(nano int64) string {
	return strconv.FormatFloat(float64(nano)/1e9, 'f', -1, 64)
}

// reapplyResourceLimits patches an instance's compose.override.yml so the main
// service carries the cgroup limits from its persisted policy (or none),
// touching only that service's `deploy` stanza and leaving the rest of the
// brain-generated override intact. It reports whether the file changed — the
// reconcile pass `compose up -d`s to recreate the container only when it did, so
// a policy change applies without a reinstall (ENVIRONMENT.md # Per-instance
// resource limits).
//
// On a change it also returns a restore func that rewinds the override to its
// pre-patch bytes (nil when nothing changed). The live-instance reconcile path
// calls it when the follow-up ComposeUp fails: the file is both the input
// ComposeUp reads and the "already-applied" marker, so leaving it patched after
// a failed up would make the next pass see no change and never retry, stranding
// the container on its old limits.
func (m *Manager) reapplyResourceLimits(id string) (bool, func() error, error) {
	lim, err := m.store.GetResourceLimits(id)
	if err != nil {
		return false, nil, fmt.Errorf("get resource limits: %w", err)
	}
	main, err := m.mainService(id)
	if err != nil {
		return false, nil, err
	}
	ovPath := filepath.Join(m.instanceDir(id), "compose.override.yml")
	raw, err := os.ReadFile(ovPath)
	if err != nil {
		return false, nil, fmt.Errorf("read override: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return false, nil, fmt.Errorf("parse override: %w", err)
	}
	services, _ := doc["services"].(map[string]any)
	svc, _ := services[main].(map[string]any)
	if svc == nil {
		return false, nil, fmt.Errorf("override for %s missing main service %q", id, main)
	}
	// The brain owns the main service's whole `deploy` stanza (writeOverride sets
	// nothing else under it), so "no policy" means the key is absent.
	want := resourceLimitsStanza(m.profile, lim)
	existing, hasDeploy := svc["deploy"]
	if want == nil {
		if !hasDeploy {
			return false, nil, nil // already uncapped
		}
		delete(svc, "deploy")
	} else {
		// Compare via marshaled bytes so the freshly-built stanza (int64 memory)
		// and the round-tripped one (int memory) compare equal when unchanged.
		wantBytes, wantErr := yaml.Marshal(want)
		gotBytes, gotErr := yaml.Marshal(existing)
		if wantErr != nil || gotErr != nil {
			return false, nil, fmt.Errorf("marshal deploy stanza for comparison: want=%v got=%v", wantErr, gotErr)
		}
		if bytes.Equal(wantBytes, gotBytes) {
			return false, nil, nil
		}
		svc["deploy"] = want
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return false, nil, err
	}
	if err := os.WriteFile(ovPath, out, 0o644); err != nil {
		return false, nil, fmt.Errorf("write override: %w", err)
	}
	prev := raw
	restore := func() error {
		if err := os.WriteFile(ovPath, prev, 0o644); err != nil {
			return fmt.Errorf("restore override: %w", err)
		}
		return nil
	}
	return true, restore, nil
}

// mainService reads the persisted manifest for an instance and returns its
// main_service name — the service the resource-limit policy clamps.
func (m *Manager) mainService(id string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(m.instanceDir(id), "manifest.yml"))
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	var man manifest.Manifest
	if err := yaml.Unmarshal(raw, &man); err != nil {
		return "", fmt.Errorf("parse manifest: %w", err)
	}
	if man.MainService == "" {
		return "", fmt.Errorf("manifest for %s has no main_service", id)
	}
	return man.MainService, nil
}
