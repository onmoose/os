// Package avahipublisher — allowlist sync tests (no build tag, no DBus, no
// root): the conf rewrite is pure file manipulation and Sync's restart and
// republish seams are stubbed.
package avahipublisher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/hostagent/netstate"
)

// debianDefaultConf mirrors the shape of Debian's shipped avahi-daemon.conf:
// a [server] section with commented defaults, then other sections.
const debianDefaultConf = `[server]
#host-name=foo
#allow-interfaces=eth0
use-ipv4=yes
use-ipv6=yes

[wide-area]
enable-wide-area=yes
`

func writeTempConf(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "avahi-daemon.conf")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWriteAllowInterfaces_InsertsUnderServer(t *testing.T) {
	path := writeTempConf(t, debianDefaultConf)

	changed, err := writeAllowInterfaces(path, []string{"eno1", "wlp2s0"})
	if err != nil {
		t.Fatalf("writeAllowInterfaces: %v", err)
	}
	if !changed {
		t.Error("want changed=true on first write")
	}

	got, _ := os.ReadFile(path)
	text := string(got)
	if !strings.Contains(text, "[server]\nallow-interfaces=eno1,wlp2s0\n") {
		t.Errorf("managed line not inserted directly under [server]:\n%s", text)
	}
	// The commented default stays as documentation; everything else survives.
	for _, keep := range []string{"#allow-interfaces=eth0", "use-ipv4=yes", "[wide-area]", "enable-wide-area=yes"} {
		if !strings.Contains(text, keep) {
			t.Errorf("rewrite lost line %q:\n%s", keep, text)
		}
	}
}

func TestWriteAllowInterfaces_ReplacesExisting(t *testing.T) {
	path := writeTempConf(t, "[server]\nallow-interfaces=eth0\nuse-ipv4=yes\n")

	changed, err := writeAllowInterfaces(path, []string{"eno1"})
	if err != nil {
		t.Fatalf("writeAllowInterfaces: %v", err)
	}
	if !changed {
		t.Error("want changed=true when the value differs")
	}

	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "allow-interfaces=eth0") {
		t.Errorf("old value survived:\n%s", got)
	}
	if !strings.Contains(string(got), "allow-interfaces=eno1") {
		t.Errorf("new value missing:\n%s", got)
	}
	if strings.Count(string(got), "allow-interfaces=") != 1 {
		t.Errorf("want exactly one active allow-interfaces line:\n%s", got)
	}
}

func TestWriteAllowInterfaces_NoOpWhenCurrent(t *testing.T) {
	path := writeTempConf(t, "[server]\nallow-interfaces=eno1\n")
	before, _ := os.ReadFile(path)

	changed, err := writeAllowInterfaces(path, []string{"eno1"})
	if err != nil {
		t.Fatalf("writeAllowInterfaces: %v", err)
	}
	if changed {
		t.Error("want changed=false when the value already matches")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("file rewritten despite matching value")
	}
}

func TestWriteAllowInterfaces_NoServerSection(t *testing.T) {
	path := writeTempConf(t, "[wide-area]\nenable-wide-area=yes\n")

	changed, err := writeAllowInterfaces(path, []string{"eno1"})
	if err != nil {
		t.Fatalf("writeAllowInterfaces: %v", err)
	}
	if !changed {
		t.Error("want changed=true")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "[server]\nallow-interfaces=eno1") {
		t.Errorf("missing appended [server] section:\n%s", got)
	}
}

func TestWriteAllowInterfaces_MissingFile(t *testing.T) {
	if _, err := writeAllowInterfaces(filepath.Join(t.TempDir(), "nope.conf"), []string{"eno1"}); err == nil {
		t.Error("want error for a missing conf file (avahi-daemon not installed?)")
	}
}

// --- Sync.Apply ---------------------------------------------------------------

func lanStub(ifaces ...netstate.LANInterface) func() ([]netstate.LANInterface, error) {
	return func() ([]netstate.LANInterface, error) { return ifaces, nil }
}

func TestSyncApply_ChangedSetRestartsAndRepublishes(t *testing.T) {
	path := writeTempConf(t, debianDefaultConf)
	restarts, republishes := 0, 0
	s := &Sync{
		LAN:       lanStub(netstate.LANInterface{Name: "eno1", Index: 2, IPv4: "192.168.2.160"}),
		ConfPath:  path,
		restart:   func() error { restarts++; return nil },
		republish: func() error { republishes++; return nil },
	}

	if err := s.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if restarts != 1 || republishes != 1 {
		t.Errorf("first Apply: want 1 restart + 1 republish, got %d/%d", restarts, republishes)
	}

	// Same set again: conf already current → no restart, but announcements are
	// still replayed (an IP-only change is invisible at conf level).
	if err := s.Apply(); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if restarts != 1 {
		t.Errorf("unchanged set: want no extra restart, got %d total", restarts)
	}
	if republishes != 2 {
		t.Errorf("unchanged set: want republish on every Apply, got %d total", republishes)
	}
}

func TestSyncApply_ZeroInterfacesIsNoOp(t *testing.T) {
	path := writeTempConf(t, "[server]\nallow-interfaces=eno1\n")
	before, _ := os.ReadFile(path)
	s := &Sync{
		LAN:       lanStub(),
		ConfPath:  path,
		restart:   func() error { t.Error("restart called with zero LAN interfaces"); return nil },
		republish: func() error { t.Error("republish called with zero LAN interfaces"); return nil },
	}

	if err := s.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("conf rewritten despite zero LAN interfaces")
	}
}

func TestSyncApply_RestartFailureStopsBeforeRepublish(t *testing.T) {
	path := writeTempConf(t, debianDefaultConf)
	s := &Sync{
		LAN:       lanStub(netstate.LANInterface{Name: "eno1", Index: 2, IPv4: "192.168.2.160"}),
		ConfPath:  path,
		restart:   func() error { return errors.New("unit masked") },
		republish: func() error { t.Error("republish called although restart failed"); return nil },
	}

	if err := s.Apply(); err == nil || !strings.Contains(err.Error(), "unit masked") {
		t.Errorf("want restart error surfaced, got %v", err)
	}
}

func TestSyncApply_LANErrorSurfaces(t *testing.T) {
	s := &Sync{
		LAN:      func() ([]netstate.LANInterface, error) { return nil, errors.New("NM gone") },
		ConfPath: writeTempConf(t, debianDefaultConf),
	}
	if err := s.Apply(); err == nil || !strings.Contains(err.Error(), "NM gone") {
		t.Errorf("want LAN error surfaced, got %v", err)
	}
}
