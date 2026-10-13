package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onmoose/os/internal/protocol"

	"gopkg.in/yaml.v3"
)

// controlPlaneComposeFile is the staged compose the brain reconciles the
// control-plane stack from (# EnsureControlPlane). The name is fixed by the
// image build, which writes it to MOOSE_CONTROL_PLANE_DIR.
const controlPlaneComposeFile = "compose.yml"

// uiServiceName is the compose service that runs the dashboard bundle
// (CONTROL_PLANE.md # the dashboard UI is a brain-launched container).
const uiServiceName = "moose-ui"

// ControlPlaneUIImage reports the image reference the staged control-plane
// compose pins for the dashboard service, e.g. "moose-ui:dev" today or
// "ghcr.io/onmoose/ui:v0.6.0" once an update has rewritten it.
//
// Why read the file rather than ask Docker what is running: this compose is the
// **declaration** the brain reconciles to on every startup, and it is the same
// file the control-plane updater rewrites before recreating containers
// (UPDATES.md # 8.3). Reading it means the version this reports and the version
// the box converges to are the same fact by construction, with no second source
// to drift. The cost is that it reports intent, not observed reality — if a
// recreate failed halfway, the file has already moved and the container has
// not. That window is exactly what the updater's health-check-then-revert
// closes, so the two are designed to agree.
//
// dir is empty in dev (the UI is Vite, there is no compose — see
// EnsureControlPlane), and callers get an empty string with no error for that
// case: "not applicable here" is not a failure. A missing or malformed file
// when dir IS set is a real error and is returned.
func ControlPlaneUIImage(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	p := filepath.Join(dir, controlPlaneComposeFile)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read control-plane compose: %w", err)
	}

	// Deliberately a narrow struct rather than a full compose model: this needs
	// exactly one field, and parsing the whole schema would couple the brain to
	// compose's shape for no gain.
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("parse control-plane compose %s: %w", p, err)
	}

	svc, ok := doc.Services[uiServiceName]
	if !ok {
		return "", fmt.Errorf("control-plane compose %s has no %q service", p, uiServiceName)
	}
	img := strings.TrimSpace(svc.Image)
	if img == "" {
		return "", fmt.Errorf("control-plane compose %s pins no image for %q", p, uiServiceName)
	}
	// A ${VAR}-interpolated ref is resolved by `docker compose`, not by us, so
	// reporting it raw would be a lie dressed as a version. The UI service is
	// pinned literally today; say so plainly if that ever changes.
	if strings.Contains(img, "${") {
		return "", fmt.Errorf("control-plane compose %s pins %q for %q, which this reader does not resolve", p, img, uiServiceName)
	}
	return img, nil
}

// controlPlaneCaddyConfig is the staged Caddy bootstrap config, next to the
// control-plane compose.
const controlPlaneCaddyConfig = "caddy.json"

// ControlPlaneCaddyAdmin reports the address the brain dials Caddy's admin API
// on, read from the staged caddy.json in dir. It returns "unix://<host path>"
// when that file puts the admin API on a unix socket under
// protocol.CaddySocketMount, and "" when it uses TCP, so the caller keeps its
// configured address.
//
// Why read the file rather than trust MOOSE_CADDY_ADMIN: the staged files are
// copied to a box once, at first boot, and an OS update never replaces them
// (BUILD.md # As built). A box made before #608 keeps a TCP admin API for its
// whole life, and a box made after it has the socket, whatever brain or
// host-agent it runs later. The file is the one thing that says which, so a
// brain reading it is never pointed at a listener Caddy does not have.
//
// dir is empty in dev, and callers skip this then.
func ControlPlaneCaddyAdmin(dir string) (string, error) {
	p := filepath.Join(dir, controlPlaneCaddyConfig)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read staged caddy config: %w", err)
	}
	var doc struct {
		Admin struct {
			Listen string `json:"listen"`
		} `json:"admin"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("parse staged caddy config %s: %w", p, err)
	}
	// Caddy writes a unix address as "unix/<path>", with an optional
	// "|<mode>" for the socket's file mode.
	sock, ok := strings.CutPrefix(doc.Admin.Listen, "unix/")
	if !ok {
		return "", nil
	}
	sock, _, _ = strings.Cut(sock, "|")
	rel, ok := strings.CutPrefix(sock, protocol.CaddySocketMount+"/")
	if !ok || rel == "" {
		return "", fmt.Errorf("staged caddy config %s puts the admin socket at %q, outside %s, where the brain cannot reach it", p, sock, protocol.CaddySocketMount)
	}
	return "unix://" + filepath.Join(dir, protocol.CaddySocketDir, rel), nil
}
