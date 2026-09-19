package manifest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Synthesize builds a manifest for a user-pasted (Door-2) compose
// (APP_MANIFEST.md # Custom container — synthetic manifest). It infers the main
// service when the compose has exactly one; otherwise the caller must name it.
// The permission set is admin-elected in the Door-2 form (DASHBOARD.md #
// Permissions); the caller passes it in (the default is internet-on, set by the
// API layer, not here) — no managed services.
func Synthesize(name string, composeBytes []byte, mainService string, mainPort int, perms Permissions) (*Manifest, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, fmt.Errorf("a name is required")
	}
	if mainPort == 0 {
		return nil, nil, fmt.Errorf("a main port is required")
	}

	services, err := ComposeServiceNames(composeBytes)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case mainService == "" && len(services) == 1:
		mainService = services[0]
	case mainService == "":
		return nil, nil, fmt.Errorf("compose has %d services (%s) — specify which is the main service",
			len(services), strings.Join(services, ", "))
	default:
		if !contains(services, mainService) {
			return nil, nil, fmt.Errorf("main service %q is not in the compose (services: %s)",
				mainService, strings.Join(services, ", "))
		}
	}

	slug := slugify(name)
	if slug == "" {
		return nil, nil, fmt.Errorf("name %q has no usable characters for a slug", name)
	}

	man := &Manifest{
		ID:              slug + "-" + entropy(),
		ManifestVersion: 1,
		Name:            name,
		Version:         "custom",
		ComposeFile:     "compose.yml",
		MainService:     mainService,
		MainPort:        mainPort,
		PreferredSlugs:  []string{slug},
		Permissions:     perms,
	}
	if err := man.validate(); err != nil {
		return nil, nil, err
	}
	return man, composeBytes, nil
}

// ComposeServiceNames returns the service names declared under `services:` in a
// compose document, erroring if the bytes aren't valid YAML or declare no
// services. Two consumers: Synthesize (Door-2 main-service inference) and the
// `moose manifest lint` CLI (confirming a manifest's main_service exists in its
// sibling compose).
func ComposeServiceNames(composeBytes []byte) ([]string, error) {
	var doc struct {
		Services map[string]yaml.Node `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return nil, fmt.Errorf("compose is not valid YAML: %w", err)
	}
	if len(doc.Services) == 0 {
		return nil, fmt.Errorf("compose declares no services")
	}
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	return names, nil
}

// ComposeImages returns the distinct `image:` references declared under
// `services:`, sorted, for the `moose manifest resolve` size resolver. A
// service without an `image:` is an error (admission rejects `build:`, so a
// curated compose always declares one). Distinct because services can share an
// image (a gateway + dashboard on the same binary) and it's sized once.
func ComposeImages(composeBytes []byte) ([]string, error) {
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return nil, fmt.Errorf("compose is not valid YAML: %w", err)
	}
	if len(doc.Services) == 0 {
		return nil, fmt.Errorf("compose declares no services")
	}
	seen := map[string]bool{}
	var imgs []string
	for name, svc := range doc.Services {
		img := strings.TrimSpace(svc.Image)
		if img == "" {
			return nil, fmt.Errorf("service %q has no image", name)
		}
		if !seen[img] {
			seen[img] = true
			imgs = append(imgs, img)
		}
	}
	sort.Strings(imgs)
	return imgs, nil
}

// InferMainPort makes a best-effort guess at the container-internal port the
// main service listens on, for prefilling the Door-2 form (DASHBOARD.md # Main
// port). It reads every signal the compose carries: a single `expose:` value,
// or — failing that — the container side of a single published `ports:` mapping
// (`8080:80` ⇒ `80`), mined out even though the mapping itself is an admission
// rejection (Caddy fronts every app; we read the container side for the prefill,
// we don't honor the host binding). Returns 0 when the compose is silent,
// declares several ports (ambiguous), or the value isn't a plain 1..65535 port —
// the form then asks. `main_port` stays required and editable regardless: moose
// can't read the image's real EXPOSE without pulling it, so this is
// prefill-and-confirm only.
func InferMainPort(composeBytes []byte, mainService string) int {
	var doc struct {
		Services map[string]struct {
			Expose []yaml.Node `yaml:"expose"`
			Ports  []yaml.Node `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeBytes, &doc); err != nil {
		return 0
	}
	svc, ok := doc.Services[mainService]
	if !ok {
		return 0
	}
	// expose: is the explicit container-internal declaration — prefer it.
	if len(svc.Expose) == 1 {
		if p := validPort(svc.Expose[0].Value); p != 0 {
			return p
		}
	}
	// Otherwise mine the container side of a single published ports: mapping.
	if len(svc.Ports) == 1 {
		if p := containerSideOf(svc.Ports[0]); p != 0 {
			return p
		}
	}
	return 0
}

// containerSideOf returns the container-internal port of one compose `ports:`
// entry, in either short or long syntax, or 0 if it isn't a single 1..65535
// port. Short: "8080:80", "127.0.0.1:8080:80", "80", "8080:80/tcp" — the segment
// after the last colon (proto suffix stripped). Long: { target: 80, ... }.
func containerSideOf(node yaml.Node) int {
	switch node.Kind {
	case yaml.ScalarNode:
		s := strings.SplitN(node.Value, "/", 2)[0] // drop any /tcp|/udp
		parts := strings.Split(s, ":")
		return validPort(parts[len(parts)-1])
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "target" {
				return validPort(node.Content[i+1].Value)
			}
		}
	}
	return 0
}

// validPort parses a trimmed string as a 1..65535 port, returning 0 on anything
// else (empty, a range like "8000-8005", non-numeric, or out of range).
func validPort(s string) int {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p < 1 || p > 65535 {
		return 0
	}
	return p
}

// permissionsOverlay is the YAML shape the Door-2 "Edit as YAML" toggle renders
// and parses (DASHBOARD.md # Form is a projection of the synthetic manifest): the
// synthetic manifest's permissions block, nothing else. The pasted compose keeps
// its own textarea and is never merged in; name/main_service/main_port stay
// dedicated form inputs in both modes. The escape hatch exists for permission
// fields the form omits — today that's `devices` (managed `services` /
// `health_probe` await their subsystems, # Known gaps).
type permissionsOverlay struct {
	Permissions Permissions `yaml:"permissions"`
}

// RenderPermissionsOverlay marshals an elected permission set to the YAML the
// "Edit as YAML" editor shows when the admin flips out of the form.
func RenderPermissionsOverlay(p Permissions) ([]byte, error) {
	return yaml.Marshal(permissionsOverlay{Permissions: p})
}

// ParsePermissionsOverlay parses an admin-edited overlay back to a Permissions,
// through the same ValidatePermissions gate the form path uses — a hand-typed
// folder target, unknown folder, or bad mode is rejected identically (the escape
// hatch escapes the form, not the sandbox; admission still runs at install).
// Unknown keys are rejected so a typo (`interent:`) surfaces instead of silently
// reading as false. An empty overlay is an empty (all-off) permission set.
func ParsePermissionsOverlay(data []byte) (Permissions, error) {
	if strings.TrimSpace(string(data)) == "" {
		return Permissions{}, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var o permissionsOverlay
	if err := dec.Decode(&o); err != nil {
		return Permissions{}, fmt.Errorf("permissions overlay is not valid YAML: %w", err)
	}
	if err := ValidatePermissions(&o.Permissions); err != nil {
		return Permissions{}, err
	}
	return o.Permissions, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func entropy() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
