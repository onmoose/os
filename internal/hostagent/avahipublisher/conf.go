// Avahi interface-allowlist sync: keeps allow-interfaces in
// /etc/avahi/avahi-daemon.conf aligned with the NetworkManager LAN set
// (DISCOVERY.md # Interface scoping). Complementary to per-interface
// AddAddress, not an alternative — the allowlist also keeps avahi-daemon's
// *native* host record and the SMB advertisement off the mesh and Docker
// bridges, which per-app entry groups can't influence.
//
// avahi-daemon re-reads only its static *service files* on SIGHUP/--reload;
// avahi-daemon.conf itself is read once at startup, so an allowlist change
// requires a daemon restart — which destroys every committed entry group, so
// the restart is always followed by RepublishAll.
//
// No build tag: the conf rewrite is pure file manipulation (unit-tested
// everywhere); systemctl only runs at runtime on a real Linux host.
package avahipublisher

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/onmoose/moose/internal/hostagent/netstate"
)

// DefaultConfPath is where Debian's avahi-daemon package puts its config.
const DefaultConfPath = "/etc/avahi/avahi-daemon.conf"

// Sync reconciles avahi-daemon with the current LAN set: the conf allowlist
// (restarting the daemon when it changes) and the published entry groups.
// Wired in cmd/host-agent-real; Apply runs once at startup and then from the
// netstate watcher on every NetworkManager change.
type Sync struct {
	// Publisher's entry groups are re-announced after every Apply — committed
	// groups hold literal addresses, so any change the watcher saw may have
	// invalidated them, and a daemon restart destroys them outright.
	Publisher *DBusPublisher

	// LAN supplies the interface set (production:
	// netstate.NMProvider.LANInterfaces).
	LAN func() ([]netstate.LANInterface, error)

	// ConfPath overrides DefaultConfPath (tests).
	ConfPath string

	// restart and republish, when non-nil, replace the systemctl restart and
	// Publisher.RepublishAll — tests stub them so Apply is unit-testable
	// without root or DBus.
	restart   func() error
	republish func() error
}

// Apply recomputes the LAN set and reconciles avahi-daemon with it. Returns
// the first error; every step is idempotent, so the caller just retries on
// the next network change. Zero LAN interfaces (all links down, boot race) is
// a no-op: the conf keeps its last allowlist and nothing is re-announced —
// there is no LAN to announce on — until a later event brings a link back.
func (s *Sync) Apply() error {
	ifaces, err := s.LAN()
	if err != nil {
		return fmt.Errorf("avahipublisher: sync: %w", err)
	}
	if len(ifaces) == 0 {
		slog.Warn("avahi sync: no LAN interface is up; leaving allowlist and announcements unchanged")
		return nil
	}
	names := make([]string, len(ifaces))
	for i, li := range ifaces {
		names[i] = li.Name
	}

	confPath := s.ConfPath
	if confPath == "" {
		confPath = DefaultConfPath
	}
	changed, err := writeAllowInterfaces(confPath, names)
	if err != nil {
		return fmt.Errorf("avahipublisher: sync allowlist: %w", err)
	}
	if changed {
		slog.Info("avahi sync: allow-interfaces changed; restarting avahi-daemon",
			"path", confPath, "interfaces", strings.Join(names, ","))
		if err := s.doRestart(); err != nil {
			return fmt.Errorf("avahipublisher: restart avahi-daemon: %w", err)
		}
	}
	// Always re-announce: an IP-only change leaves the conf untouched but the
	// committed groups stale, and a restart destroyed them altogether.
	return s.doRepublish()
}

func (s *Sync) doRestart() error {
	if s.restart != nil {
		return s.restart()
	}
	out, err := exec.Command("systemctl", "restart", "avahi-daemon").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart avahi-daemon: %v: %s", err, out)
	}
	return nil
}

func (s *Sync) doRepublish() error {
	if s.republish != nil {
		return s.republish()
	}
	return s.Publisher.RepublishAll()
}

// writeAllowInterfaces rewrites the allow-interfaces key in the [server]
// section of the avahi-daemon conf at path, preserving every other line
// byte-for-byte. Returns changed=false (and writes nothing) when the active
// value already matches. A commented #allow-interfaces line is left alone as
// documentation; the managed line is inserted directly under [server]. The
// write is atomic: temp file in the same directory, then rename.
func writeAllowInterfaces(path string, names []string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	want := "allow-interfaces=" + strings.Join(names, ",")

	lines := strings.Split(string(data), "\n")
	inServer := false
	serverLine := -1 // index of the [server] header
	existing := -1   // index of the active allow-interfaces line
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inServer = trimmed == "[server]"
			if inServer {
				serverLine = i
			}
			continue
		}
		if inServer && strings.HasPrefix(trimmed, "allow-interfaces=") {
			existing = i
			break
		}
	}

	switch {
	case existing >= 0:
		if strings.TrimSpace(lines[existing]) == want {
			return false, nil
		}
		lines[existing] = want
	case serverLine >= 0:
		lines = append(lines[:serverLine+1], append([]string{want}, lines[serverLine+1:]...)...)
	default:
		// Conf without a [server] section — not Debian's shipped file, but
		// don't fail the sync over it.
		lines = append(lines, "[server]", want)
	}

	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".avahi-daemon.conf.*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if _, err := tmp.WriteString(strings.Join(lines, "\n")); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(info.Mode()); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}
