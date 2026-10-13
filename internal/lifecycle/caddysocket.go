package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/onmoose/os/internal/caddy"
	"github.com/onmoose/os/internal/protocol"

	"gopkg.in/yaml.v3"
)

// The admin socket move for a box made before #608 (#612).
//
// Such a box keeps the staged caddy.json and compose.yml it got at first boot,
// with Caddy's admin API on TCP 0.0.0.0:2019 and no bind for the socket dir.
// Caddy sits on every app's network since #608, so an app there can reach the
// admin API and rewrite routes. At its own startup, once, before its compose
// up, the brain moves the box to the socket: it makes the socket dir, edits
// one line of each file, brings Caddy up on them and checks the socket
// answers. If it does not, it puts the files back and the box stays on TCP
// until the next brain start (DECISIONS.md 2026-10-13, UPDATES.md # 8.3).

const (
	// caddyAdminSocketListen is the admin.listen a box made since #608 has,
	// in Caddy's own address form.
	caddyAdminSocketListen = "unix/" + protocol.CaddySocketMount + "/admin.sock"
	// caddySocketBind is the compose bind that puts the socket dir into Caddy.
	caddySocketBind = "./" + protocol.CaddySocketDir + ":" + protocol.CaddySocketMount
	// caddyTCPBackupSuffix names the copies of the two files as they were
	// before the move: caddy.json.tcp-admin and compose.yml.tcp-admin. Kept
	// after a move that worked, so a person can see what the box had.
	caddyTCPBackupSuffix = ".tcp-admin"
	// caddySocketDirMode keeps the socket dir to its owner, like host-agent
	// does (brainlaunch.EnsureCaddySocketDir).
	caddySocketDirMode = 0o700
	// caddySocketWait bounds the wait for the admin API on the socket after
	// Caddy is recreated. Caddy answers within about a second of starting.
	caddySocketWait = 20 * time.Second
	// caddyRestoreTimeout bounds the Caddy recreate that puts it back on TCP,
	// whatever is left of the caller's budget. Kept short: a brain recreated
	// by an update has 60s to answer /healthz, and this runs before it serves.
	caddyRestoreTimeout = 30 * time.Second
	// caddyServiceName is the Caddy service in the control-plane compose.
	caddyServiceName = "caddy"
)

// MigrateCaddyAdminSocket moves a box whose staged caddy.json still puts
// Caddy's admin API on TCP to the unix socket (#612). Call it once, at brain
// startup, after Docker answers and before EnsureControlPlane, and never
// again: the control-plane project is the brain's only at startup (UPDATES.md
// # 3, step 3c).
//
// It returns the socket address the brain must dial from now on ("unix://"
// plus the host path) when the move worked. It returns "" and no error when
// the box needs nothing: a box made since #608, or one already moved. It
// returns "" and an error when the box stays on TCP: it could not start the
// move, or the socket did not answer and the files were put back. The next
// brain start tries again.
//
// The steps, in this order on purpose:
//
//  1. The socket dir, mode 0700, owned by the host id Caddy's root maps to.
//     The base comes from host-agent and is checked against Docker
//     (hostIdentity), like every install.
//  2. Copies of both files as they are, next to them. An existing copy is
//     kept: after a crash between the two writes below, the compose on disk
//     is already edited, and the first copy is the true original.
//  3. compose.yml first, then caddy.json. Each is a one-line edit checked by
//     a parse, written atomically. caddy.json goes last because it is the one
//     the brain reads to pick its address: a crash between the two leaves a
//     box that still says TCP, which this function then finishes next time.
//  4. A forced recreate of Caddy alone, and a wait for the admin API on the
//     socket. Forced, because Caddy reads caddy.json only when it starts, and
//     a plain compose up leaves Caddy alone when the compose already had the
//     bind (that same crash case). The caller's EnsureControlPlane follows.
//  5. If either fails, the original files go back and Caddy is recreated
//     again, on TCP.
func (m *Manager) MigrateCaddyAdminSocket(ctx context.Context, dir string) (string, error) {
	jsonPath := filepath.Join(dir, controlPlaneCaddyConfig)
	composePath := filepath.Join(dir, controlPlaneComposeFile)
	origJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		return "", fmt.Errorf("read staged caddy config: %w", err)
	}
	listen, err := caddyAdminListen(origJSON)
	if err != nil {
		return "", fmt.Errorf("staged caddy config %s: %w", jsonPath, err)
	}
	if strings.HasPrefix(listen, "unix/") {
		return "", nil
	}

	origCompose, err := os.ReadFile(composePath)
	if err != nil {
		return "", fmt.Errorf("read control-plane compose: %w", err)
	}
	// Both edits are worked out before anything is written, so a file this
	// code cannot edit safely changes nothing on disk.
	newJSON, err := caddyJSONWithSocket(origJSON, listen)
	if err != nil {
		return "", fmt.Errorf("staged caddy config %s: %w", jsonPath, err)
	}
	newCompose, err := composeWithCaddySocket(origCompose)
	if err != nil {
		return "", fmt.Errorf("control-plane compose %s: %w", composePath, err)
	}

	_, base, err := m.hostIdentity(ctx)
	if err != nil {
		return "", fmt.Errorf("read the owner for the caddy socket dir: %w", err)
	}
	if err := m.ensureCaddySocketDir(dir, base); err != nil {
		return "", err
	}

	for p, b := range map[string][]byte{jsonPath: origJSON, composePath: origCompose} {
		backup := p + caddyTCPBackupSuffix
		if _, err := os.Lstat(backup); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("check the copy of %s: %w", p, err)
		}
		if err := writeFileAtomic(backup, b, 0o644); err != nil {
			return "", fmt.Errorf("keep a copy of %s: %w", p, err)
		}
	}
	slog.Info("caddy admin: moving the admin API from TCP to the unix socket", "state_dir", dir)
	if !bytes.Equal(newCompose, origCompose) {
		if err := writeFileAtomic(composePath, newCompose, 0o644); err != nil {
			return "", fmt.Errorf("write control-plane compose: %w", err)
		}
	}
	if err := writeFileAtomic(jsonPath, newJSON, 0o644); err != nil {
		return "", errors.Join(fmt.Errorf("write staged caddy config: %w", err),
			m.restoreCaddyTCP(ctx, dir, origJSON, origCompose, false))
	}

	addr, err := ControlPlaneCaddyAdmin(dir)
	if err == nil {
		err = m.recreateCaddy(ctx, dir)
	}
	if err == nil {
		err = m.waitCaddyAdmin(ctx, addr)
	}
	if err != nil {
		return "", errors.Join(fmt.Errorf("caddy admin socket did not come up: %w", err),
			m.restoreCaddyTCP(ctx, dir, origJSON, origCompose, true))
	}
	slog.Info("caddy admin: moved to the unix socket", "state_dir", dir, "url", addr)
	return addr, nil
}

// restoreCaddyTCP puts the original files back, caddy.json first so the box
// says TCP again before anything else, and, when Caddy may already run on the
// new files, recreates Caddy on TCP. It gets its own budget: the caller's may
// be spent by the wait that just failed.
func (m *Manager) restoreCaddyTCP(ctx context.Context, dir string, origJSON, origCompose []byte, up bool) error {
	var errs []error
	if err := writeFileAtomic(filepath.Join(dir, controlPlaneCaddyConfig), origJSON, 0o644); err != nil {
		errs = append(errs, fmt.Errorf("put back staged caddy config: %w", err))
	}
	if err := writeFileAtomic(filepath.Join(dir, controlPlaneComposeFile), origCompose, 0o644); err != nil {
		errs = append(errs, fmt.Errorf("put back control-plane compose: %w", err))
	}
	if up && len(errs) == 0 {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), caddyRestoreTimeout)
		defer cancel()
		if err := m.recreateCaddy(rctx, dir); err != nil {
			errs = append(errs, fmt.Errorf("bring caddy back up on TCP: %w", err))
		}
	}
	if len(errs) == 0 {
		slog.Warn("caddy admin: put the TCP admin files back; the box stays on TCP until the next brain start", "state_dir", dir)
	}
	return errors.Join(errs...)
}

// recreateCaddy force-recreates the Caddy service of the control-plane
// project, so it starts with the mounts and the caddy.json on disk now.
func (m *Manager) recreateCaddy(ctx context.Context, dir string) error {
	out, err := m.docker.ControlPlaneRecreate(ctx, dir, controlPlaneProject, caddyServiceName)
	if err != nil {
		return fmt.Errorf("recreate caddy: %w\n%s", err, out)
	}
	return nil
}

// waitCaddyAdmin waits for the admin API at addr to answer. Tests replace it
// through m.caddyAdminWait.
func (m *Manager) waitCaddyAdmin(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, caddySocketWait)
	defer cancel()
	if m.caddyAdminWait != nil {
		return m.caddyAdminWait(ctx, addr)
	}
	return caddy.New(addr).WaitReady(ctx)
}

// ensureCaddySocketDir makes <dir>/caddy a real dir, mode 0700, owned
// owner:owner, and sets the owner and mode again on one that exists. It is
// host-agent's brainlaunch.EnsureCaddySocketDir, which the brain may not
// import (internal/hostagent is host-side code). A host-agent from before
// #608 never makes this dir, so the brain must.
func (m *Manager) ensureCaddySocketDir(dir string, owner int) error {
	p := filepath.Join(dir, protocol.CaddySocketDir)
	fi, err := os.Lstat(p)
	switch {
	case err == nil && !fi.IsDir():
		return fmt.Errorf("caddy socket dir %q is not a real directory (a file or a symlink is in the way)", p)
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(p, caddySocketDirMode); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create caddy socket dir %q: %w", p, err)
		}
		slog.Info("caddy socket dir created", "state_dir", p, "uid", owner)
	case err != nil:
		return fmt.Errorf("check caddy socket dir %q: %w", p, err)
	}
	if err := m.chown(p, owner, owner); err != nil {
		return fmt.Errorf("set owner of caddy socket dir %q: %w", p, err)
	}
	if err := os.Chmod(p, caddySocketDirMode); err != nil {
		return fmt.Errorf("set mode of caddy socket dir %q: %w", p, err)
	}
	return nil
}

// caddyAdminListen reads admin.listen from a caddy.json. An empty one is an
// error: Caddy would then listen on localhost:2019 inside its container, which
// the brain cannot reach, so it is not a box this code knows how to move.
func caddyAdminListen(b []byte) (string, error) {
	var doc struct {
		Admin struct {
			Listen string `json:"listen"`
		} `json:"admin"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if doc.Admin.Listen == "" {
		return "", errors.New("it sets no admin.listen")
	}
	return doc.Admin.Listen, nil
}

// caddyJSONWithSocket returns orig with admin.listen moved from listen to the
// socket. It replaces the one JSON string, so the file keeps its layout, and
// refuses when that string is not there exactly once. The result is parsed
// back and must equal the original with only admin.listen changed.
func caddyJSONWithSocket(orig []byte, listen string) ([]byte, error) {
	from, err := json.Marshal(listen)
	if err != nil {
		return nil, err
	}
	to, err := json.Marshal(caddyAdminSocketListen)
	if err != nil {
		return nil, err
	}
	if n := bytes.Count(orig, from); n != 1 {
		return nil, fmt.Errorf("the admin address %s appears %d times, want once", from, n)
	}
	out := bytes.Replace(orig, from, to, 1)

	var want, got map[string]any
	if err := json.Unmarshal(orig, &want); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("the edited file does not parse: %w", err)
	}
	admin, ok := want["admin"].(map[string]any)
	if !ok {
		return nil, errors.New("admin is not an object")
	}
	admin["listen"] = caddyAdminSocketListen
	if !reflect.DeepEqual(want, got) {
		return nil, errors.New("the edit changed more than admin.listen")
	}
	return out, nil
}

// composeWithCaddySocket returns orig with the socket-dir bind added to the
// caddy service's volumes, or orig unchanged when the bind is there already.
//
// Like RewriteUIImage on the host side, it adds one line and never marshals
// the YAML, which would drop every comment in the file. The new line goes
// right after the last item of the volumes list, at the items' indent. The
// result is parsed back and must equal the original with only that one item
// added at the end of the list.
func composeWithCaddySocket(orig []byte) ([]byte, error) {
	var before map[string]any
	if err := yaml.Unmarshal(orig, &before); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	vols, err := caddyVolumes(before)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if v == caddySocketBind {
			return orig, nil
		}
	}

	lines := strings.Split(string(orig), "\n")
	at, indent, err := caddyVolumesEnd(lines)
	if err != nil {
		return nil, err
	}
	added := strings.Repeat(" ", indent) + "- " + caddySocketBind
	lines = append(lines[:at+1], append([]string{added}, lines[at+1:]...)...)
	out := []byte(strings.Join(lines, "\n"))

	var after map[string]any
	if err := yaml.Unmarshal(out, &after); err != nil {
		return nil, fmt.Errorf("the edited file does not parse: %w", err)
	}
	afterVols, err := caddyVolumes(after)
	if err != nil {
		return nil, fmt.Errorf("the edited file: %w", err)
	}
	if n := len(afterVols); n == 0 || afterVols[n-1] != caddySocketBind {
		return nil, fmt.Errorf("the edited file does not end caddy's volumes with %q", caddySocketBind)
	}
	// Take the new item off and the two must be the same document.
	svc := after["services"].(map[string]any)[caddyServiceName].(map[string]any)
	svc["volumes"] = afterVols[:len(afterVols)-1]
	if !reflect.DeepEqual(before, after) {
		return nil, errors.New("the edit changed more than caddy's volumes")
	}
	return out, nil
}

// caddyVolumes returns the caddy service's volumes from a parsed compose.
func caddyVolumes(doc map[string]any) ([]any, error) {
	services, ok := doc["services"].(map[string]any)
	if !ok {
		return nil, errors.New("it has no services")
	}
	svc, ok := services[caddyServiceName].(map[string]any)
	if !ok {
		return nil, errors.New("it has no caddy service")
	}
	vols, ok := svc["volumes"].([]any)
	if !ok {
		return nil, errors.New("the caddy service has no volumes list")
	}
	return vols, nil
}

// caddyVolumesEnd finds the last line of the caddy service's volumes list
// and the indent of its items. The scan is by indent, as in uiImageLine:
// services at one indent under `services:`, their keys one indent in, and the
// list items at or past the key's indent. Blank and comment lines are skipped,
// so the new item lands before any comment that follows the list.
func caddyVolumesEnd(lines []string) (last, itemIndent int, err error) {
	inServices := false
	svcLevel := -1 // indent of the services' own keys
	caddyIndent := -1
	keyIndent := -1
	inVolumes := false
	itemIndent = -1
	last = -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case inVolumes:
			if itemIndent < 0 {
				if !strings.HasPrefix(trimmed, "-") || indent < keyIndent {
					return 0, 0, errors.New("the caddy service's volumes are not a block list")
				}
				itemIndent, last = indent, i
				continue
			}
			if indent > itemIndent || (indent == itemIndent && strings.HasPrefix(trimmed, "-")) {
				last = i
				continue
			}
			return last, itemIndent, nil
		case caddyIndent >= 0:
			if indent <= caddyIndent {
				return 0, 0, errors.New("the caddy service has no volumes key")
			}
			if keyIndent < 0 {
				keyIndent = indent
			}
			if indent != keyIndent {
				continue
			}
			if v, ok := strings.CutPrefix(trimmed, "volumes:"); ok {
				// Only a comment may follow the key: `volumes: [...]` is a
				// flow list, which a line edit cannot extend.
				if v = strings.TrimSpace(v); v != "" && !strings.HasPrefix(v, "#") {
					return 0, 0, errors.New("the caddy service's volumes are not a block list")
				}
				inVolumes = true
			}
		case indent == 0:
			inServices = trimmed == "services:"
		case inServices:
			if svcLevel < 0 {
				svcLevel = indent
			}
			if indent == svcLevel && trimmed == caddyServiceName+":" {
				caddyIndent = indent
			}
		}
	}
	if itemIndent >= 0 {
		return last, itemIndent, nil
	}
	return 0, 0, errors.New("could not find the caddy service's volumes list")
}

// writeFileAtomic writes data to a temp file in the same dir and renames it
// over path, so a reader sees the old file or the new one, never half. The
// host side has its own copy (internal/hostagent/controlplane), which the
// brain may not import.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(name, perm); err != nil {
		os.Remove(name)
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("rename %s to %s: %w", name, path, err)
	}
	return nil
}
