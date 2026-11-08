package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// migrateOffIngress takes an app installed before #608 off moose-ingress. Its
// saved override still attaches the main service to that network, and every
// later compose up reuses the file, so writeOverride alone would leave it there
// for good, able to reach the socket proxy (host root, THREAT_MODEL.md B2).
//
// It runs at every brain start and does two separate things. It cleans the
// override, then it takes the main container off the network if it is still
// on it, whatever the override said. The second step must not depend on the
// first: the file is clean after the first try, so a disconnect that failed, or
// a brain that died before it, is only put right by asking the container. An
// error from the first step does not stop the second. On a normal box this is
// one inspect and nothing else.
//
// It reports whether the override changed.
func (m *Manager) migrateOffIngress(ctx context.Context, id string) (bool, error) {
	changed, overrideErr := m.dropIngressFromOverride(id)
	main, err := m.mainService(id)
	if err != nil {
		if !changed && overrideErr == nil {
			// A clean override and a manifest that cannot be read: there is no
			// name to ask for. This must not warn at every start, and the next
			// start asks again. A running app already warns about its manifest
			// in reapplyResourceLimits.
			slog.Debug("reconcile: cannot name the main container, skipping the network check",
				"instance_id", id, "err", err)
			return false, nil
		}
		return changed, errors.Join(overrideErr, err)
	}
	return changed, errors.Join(overrideErr, m.disconnectFromIngress(ctx, id, main))
}

// dropIngressFromOverride rewrites the override: each service's moose-ingress
// attachment is dropped, and its aliases move to the app's own network, where
// Caddy now reaches it. The top-level moose-ingress entry goes too. It reports
// whether the file changed. An override with no moose-ingress is left as it is.
func (m *Manager) dropIngressFromOverride(id string) (bool, error) {
	ovPath := filepath.Join(m.instanceDir(id), "compose.override.yml")
	raw, err := os.ReadFile(ovPath)
	if err != nil {
		return false, fmt.Errorf("read override: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("parse override: %w", err)
	}
	appNet := "moose-app-" + id
	changed := false
	services, _ := doc["services"].(map[string]any)
	for _, v := range services {
		svc, _ := v.(map[string]any)
		nets, _ := svc["networks"].(map[string]any)
		ingress, ok := nets[ingressNetwork]
		if !ok {
			continue
		}
		delete(nets, ingressNetwork)
		if ingress != nil && nets[appNet] == nil {
			nets[appNet] = ingress
		} else if _, on := nets[appNet]; !on {
			nets[appNet] = nil
		}
		changed = true
	}
	if top, _ := doc["networks"].(map[string]any); top != nil {
		if _, ok := top[ingressNetwork]; ok {
			delete(top, ingressNetwork)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(ovPath, out, 0o644); err != nil {
		return false, fmt.Errorf("write override: %w", err)
	}
	return true, nil
}

// disconnectFromIngress takes the main container off moose-ingress if it is on
// it, so a running app is off the network at once, with no recreate. Docker
// keeps that disconnect across a container restart, and the next compose up
// runs on the fixed file.
//
// It asks the container, not the network: `docker network inspect` lists only
// running containers, and a stopped one that is still attached joins the
// network again when it starts. No container at all is not an error (a removed
// or a failed app). A failure to ask is an error, so it does not count as clean.
func (m *Manager) disconnectFromIngress(ctx context.Context, id, main string) error {
	// Only the main service ever joined moose-ingress, under this name. The
	// override has pinned the name since #83, before the first release, so every
	// box has it.
	container := "moose-" + id + "-" + main
	nets, err := m.docker.ContainerNetworks(ctx, container)
	if errors.Is(err, errNoSuchContainer) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check for %s: %w", ingressNetwork, err)
	}
	if !nets[ingressNetwork] {
		return nil
	}
	if err := m.docker.NetworkDisconnect(ctx, ingressNetwork, container); err != nil {
		return fmt.Errorf("disconnect from %s: %w", ingressNetwork, err)
	}
	slog.Info("app moved off the control-plane network", "instance_id", id)
	return nil
}
