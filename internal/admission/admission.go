// Package admission enforces the compose admission policy (APP_LIFECYCLE.md #
// admission policy) before any app is installed. It runs for BOTH doors:
// catalog CI runs the same checks at publish time, and the brain enforces them
// at install. Rejections name the exact offending service + field.
package admission

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/onmoose/os/internal/manifest"
)

// Error is a rejection with a stable, user-facing message naming the field.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func reject(format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

type rawService struct {
	Ports       []any  `yaml:"ports"`
	Privileged  bool   `yaml:"privileged"`
	CapAdd      []any  `yaml:"cap_add"`
	NetworkMode string `yaml:"network_mode"`
	Pid         string `yaml:"pid"`
	Ipc         string `yaml:"ipc"`
	UsernsMode  string `yaml:"userns_mode"`
	Build       any    `yaml:"build"`
	Volumes     []any  `yaml:"volumes"`
	Extends     any    `yaml:"extends"`
	User        any    `yaml:"user"` // string or bare YAML int
	Deploy      struct {
		Replicas *int `yaml:"replicas"`
	} `yaml:"deploy"`
}

type composeDoc struct {
	Services map[string]rawService `yaml:"services"`
}

// Check validates the compose syntax via `docker compose config` and then
// applies the structural rejection rules to the verbatim YAML. It inspects the
// raw (un-normalized) compose so relative bind paths aren't rewritten to
// absolute by compose's normalization.
func Check(ctx context.Context, composeBytes []byte) error {
	if err := validateSyntax(ctx, composeBytes); err != nil {
		return err
	}
	return CheckStructure(ctx, composeBytes)
}

// CheckManifest applies the manifest-side admission rules — declarations that
// are illegal regardless of the compose content. Door-symmetric like Check:
// lifecycle's shared install transaction runs it for both doors (a Door-2
// synthetic manifest never sets service_user, so it passes trivially).
func CheckManifest(man *manifest.Manifest) error {
	if man.ServiceUser && len(man.Permissions.Folders) > 0 {
		return reject("manifest sets service_user: true together with a folders grant — a folder app already runs as a managed non-root identity (APP_MANIFEST.md # B); remove service_user")
	}
	return nil
}

// CheckStructure runs only the structural rejection rules — no daemon needed.
// Used by unit tests and as the admission seam in lifecycle tests, where
// shelling out to `docker compose config -q` would turn pure unit tests into
// flaky integration tests.
func CheckStructure(_ context.Context, composeBytes []byte) error {
	var doc composeDoc
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return reject("compose is not valid YAML: %v", err)
	}
	if len(doc.Services) == 0 {
		return reject("compose declares no services")
	}
	// Iterate in sorted order so rejection messages are stable across runs;
	// without this, table-driven tests would be flaky on the "first failing
	// service" message.
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sortStrings(names)
	for _, name := range names {
		if err := checkService(name, doc.Services[name]); err != nil {
			return err
		}
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func checkService(name string, svc rawService) error {
	switch {
	case len(svc.Ports) > 0:
		return reject("service %q declares host ports (ports:) — moose routes apps via Caddy on internal networks; remove the ports mapping", name)
	case svc.Privileged:
		return reject("service %q sets privileged: true — Tier-3 apps run unprivileged; capability-needing apps belong in Tier 2", name)
	case len(svc.CapAdd) > 0:
		return reject("service %q uses cap_add — Tier-3 apps get no added capabilities", name)
	case svc.Build != nil:
		return reject("service %q declares build: — apps must ship a prebuilt image, not a Dockerfile", name)
	case svc.Extends != nil:
		return reject("service %q uses extends: — apps must be self-contained in one compose file", name)
	case svc.Deploy.Replicas != nil && *svc.Deploy.Replicas > 1:
		// moose is a single-node appliance: a second replica buys no
		// availability and Caddy routes to one upstream alias per instance.
		// The main service additionally has its container_name pinned for the
		// per-app Logs tail (APP_LIFECYCLE.md # Locked: override file contents),
		// which compose refuses to scale — so reject the unsupported config at
		// admission, naming the field, instead of failing opaquely at `up`.
		return reject("service %q sets deploy.replicas: %d — moose is a single-node appliance and runs one replica per service; remove deploy.replicas", name, *svc.Deploy.Replicas)
	}
	if m := svc.NetworkMode; m == "host" || m == "none" || strings.HasPrefix(m, "container:") {
		return reject("service %q sets network_mode: %s — not allowed; moose manages app networking", name, m)
	}
	for _, ns := range []struct{ field, val string }{
		{"pid", svc.Pid}, {"ipc", svc.Ipc}, {"userns_mode", svc.UsernsMode},
	} {
		if ns.val == "host" {
			return reject("service %q sets %s: host — host namespace sharing is not allowed", name, ns.field)
		}
	}
	if err := checkUser(name, svc.User); err != nil {
		return err
	}
	for _, v := range svc.Volumes {
		if err := checkVolume(name, v); err != nil {
			return err
		}
	}
	return nil
}

// checkUser rejects a numeric `user:` (any numeric UID or GID component).
// A compose-named number is read in the HOST user namespace — moose runs no
// userns remap — so it could alias a real host principal: a system account,
// or a moose user in the 3000+ range (APP_ISOLATION.md # Runtime identity &
// data ownership). moose owns the runtime UID; the brain's override pins
// `user:` on every service regardless. A non-numeric name (`user: postgres`)
// is left alone — it resolves in the container's own /etc/passwd and the
// override wins anyway.
func checkUser(name string, u any) error {
	val, ok := u.(string)
	if !ok {
		if u == nil {
			return nil
		}
		// Bare YAML scalar like `user: 1000` (or 1000.5; any non-string is
		// numeric as far as compose's uid syntax is concerned).
		return reject("service %q sets a numeric user: (%v) — moose assigns every app's runtime UID; remove it (a folderless app that needs a non-root identity declares service_user: true in its manifest)", name, u)
	}
	for _, part := range strings.SplitN(val, ":", 2) {
		if part == "" {
			continue
		}
		if _, err := strconv.Atoi(part); err == nil {
			return reject("service %q sets a numeric user: (%q) — moose assigns every app's runtime UID; remove it (a folderless app that needs a non-root identity declares service_user: true in its manifest)", name, val)
		}
	}
	return nil
}

// checkVolume allows only relative bind mounts (./… under the instance dir).
// Absolute host paths and named volumes are rejected (APP_MANIFEST.md: bind
// mounts only, no Docker named volumes for app data).
func checkVolume(svc string, v any) error {
	switch t := v.(type) {
	case string:
		src, _, _ := strings.Cut(t, ":")
		return classifyBindSource(svc, src)
	case map[string]any:
		typ, _ := t["type"].(string)
		src, _ := t["source"].(string)
		if typ == "volume" || (typ == "" && src != "" && !isPath(src)) {
			return reject("service %q mounts named volume %q — use a relative bind mount like ./data/%s instead", svc, src, src)
		}
		if typ == "bind" || isPath(src) {
			return classifyBindSource(svc, src)
		}
		return nil
	default:
		return nil
	}
}

func classifyBindSource(svc, src string) error {
	switch {
	case strings.HasPrefix(src, "/"):
		return reject("service %q binds absolute host path %q — only relative paths under the app's data dir (./data/…) are allowed", svc, src)
	case isPath(src):
		return nil // relative bind: ./foo or ../foo
	case src == "":
		return nil
	default:
		return reject("service %q mounts named volume %q — use a relative bind mount like ./data/%s instead", svc, src, src)
	}
}

// isPath reports whether a volume source is a filesystem path (bind) rather
// than a named-volume reference. Compose treats sources starting with . or /
// as paths; everything else is a named volume.
func isPath(src string) bool {
	return strings.HasPrefix(src, "/") || strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || src == "."
}

// validateSyntax shells out to `docker compose config -q`, which parses and
// validates the file (catching malformed compose before we write any state).
func validateSyntax(ctx context.Context, composeBytes []byte) error {
	dir, err := os.MkdirTemp("", "moose-admit-")
	if err != nil {
		return fmt.Errorf("admission tempdir: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, composeBytes, 0o600); err != nil {
		return fmt.Errorf("admission write: %w", err)
	}
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", path, "config", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		return reject("invalid compose file: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
