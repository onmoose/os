package lifecycle

import (
	"context"
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
// It rewrites the override: each service's moose-ingress attachment is dropped,
// and its aliases move to the app's own network, where Caddy now reaches it.
// The top-level moose-ingress entry goes too. Then it disconnects the main
// container from moose-ingress, so a running app is off the network at once,
// with no recreate. Docker keeps that disconnect across a container restart,
// and the next compose up runs on the fixed file. It reports whether the
// override changed. An override with no moose-ingress is left as it is.
func (m *Manager) migrateOffIngress(ctx context.Context, id string) (bool, error) {
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
	main, err := m.mainService(id)
	if err != nil {
		return true, err
	}
	// Only the main service ever joined moose-ingress, under this name.
	if err := m.docker.NetworkDisconnect(ctx, ingressNetwork, "moose-"+id+"-"+main); err != nil {
		return true, fmt.Errorf("disconnect from %s: %w", ingressNetwork, err)
	}
	slog.Info("app moved off the control-plane network", "instance_id", id)
	return true, nil
}
