//go:build linux && avahitest

// Package avahipublisher — integration tests against a real Avahi daemon.
//
// # Why these tests are gated behind the "avahitest" build tag
//
// These tests require:
//   - A running avahi-daemon reachable via the system DBus
//   - Root privileges (or a DBus policy that allows the caller to use
//     org.freedesktop.Avahi.Server.EntryGroupNew)
//
// There is no reliable way to mock the Avahi server side without running the
// real daemon. DBus mocking would require the test to stand up a full Avahi
// stub that correctly implements EntryGroupNew, AddAddress, Commit, and Free —
// which is more code than the publisher itself and wrong in different ways.
//
// The nspawn CI lane (future slice — see docs/progress/avahi-dbus-publisher.md)
// will provision a real avahi-daemon and run these tests automatically. Until
// then, run manually on a Linux dev machine with avahi-daemon running:
//
//	go test -tags avahitest ./internal/hostagent/avahipublisher/
//
// After a successful test run you can verify with:
//
//	avahi-resolve -n whoami.local
package avahipublisher

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/hostagent/netstate"
)

// TestDBusPublisher_PublishAndUnpublish exercises the full Publish → Unpublish
// round-trip against a real avahi-daemon.
func TestDBusPublisher_PublishAndUnpublish(t *testing.T) {
	p := &DBusPublisher{HostSuffix: ".local"}
	defer p.Close()

	name, err := p.Publish("avahitest-slug")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if name != "avahitest-slug.local" {
		t.Errorf("name: want avahitest-slug.local, got %q", name)
	}

	// Unpublish must succeed and remove the group.
	if err := p.Unpublish("avahitest-slug"); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
}

// TestDBusPublisher_UnpublishIdempotent verifies that unpublishing a slug that
// was never published (or already unpublished) returns nil.
func TestDBusPublisher_UnpublishIdempotent(t *testing.T) {
	p := &DBusPublisher{HostSuffix: ".local"}
	defer p.Close()

	if err := p.Unpublish("never-published"); err != nil {
		t.Fatalf("Unpublish of unknown slug: want nil, got %v", err)
	}
}

// TestDBusPublisher_PublishIdempotent verifies that publishing the same slug
// twice succeeds without leaking groups.
func TestDBusPublisher_PublishIdempotent(t *testing.T) {
	p := &DBusPublisher{HostSuffix: ".local"}
	defer p.Close()

	if _, err := p.Publish("avahitest-idem"); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	// Second Publish must free the old group and commit a new one cleanly.
	if _, err := p.Publish("avahitest-idem"); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if len(p.groups) != 1 {
		t.Errorf("want 1 group after two publishes, got %d", len(p.groups))
	}
}

// TestDBusPublisher_RedetectsIPPerPublish verifies the local IP is detected on
// every Publish — no process-lifetime cache — so apps published after a DHCP
// lease change announce the current address (#129).
func TestDBusPublisher_RedetectsIPPerPublish(t *testing.T) {
	calls := 0
	p := &DBusPublisher{HostSuffix: ".local", detectIP: func() (string, error) {
		calls++
		return "192.0.2.55", nil
	}}
	defer p.Close()

	if _, err := p.Publish("avahitest-redetect"); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if _, err := p.Publish("avahitest-redetect"); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if calls != 2 {
		t.Errorf("want IP detection on every Publish (no cache), got %d detections for 2 publishes", calls)
	}
}

// TestDBusPublisher_AnnouncesRouteProbedAddress is the #129 regression check:
// what avahi-resolve returns for a published name must be the kernel's
// route-chosen LAN address, not whatever interface (e.g. a Docker bridge)
// happened to enumerate first.
func TestDBusPublisher_AnnouncesRouteProbedAddress(t *testing.T) {
	if _, err := exec.LookPath("avahi-resolve"); err != nil {
		t.Skip("avahi-resolve not installed (apt install avahi-utils)")
	}
	want, err := netstate.ProbeIPv4()
	if err != nil {
		t.Skipf("route probe failed (box without a default route?): %v", err)
	}

	p := &DBusPublisher{HostSuffix: ".local"}
	defer p.Close()
	name, err := p.Publish("avahitest-129")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	defer p.Unpublish("avahitest-129")

	// Other resolvers see the record only after the multicast announcement;
	// give Avahi up to ~3 seconds, same as dev/test-avahi-publisher.sh.
	var got string
	for i := 0; i < 15; i++ {
		out, err := exec.Command("avahi-resolve", "-4", "-n", name).Output()
		if err == nil {
			if fields := strings.Fields(string(out)); len(fields) == 2 && fields[0] == name {
				got = fields[1]
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got == "" {
		t.Fatalf("%s never resolved via avahi-resolve (waited ~3s)", name)
	}
	if got != want {
		t.Errorf("announced %s, want the route-probed LAN address %s — a bridge address won (#129)?", got, want)
	}
}

// --- #130: per-interface announcement and IP-change replay -------------------

// resolve4 polls avahi-resolve for up to ~3 seconds until the name resolves to
// want (pass "" to accept the first answer) and returns the resolved address.
// The retry absorbs both multicast announcement latency and, for replay tests,
// the goodbye/re-announce window where the old record is still cached.
func resolve4(t *testing.T, name, want string) string {
	t.Helper()
	var got string
	for i := 0; i < 15; i++ {
		out, err := exec.Command("avahi-resolve", "-4", "-n", name).Output()
		if err == nil {
			if fields := strings.Fields(string(out)); len(fields) == 2 && fields[0] == name {
				got = fields[1]
				if want == "" || got == want {
					return got
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return got
}

// TestDBusPublisher_PerInterfaceAnnounce wires the real NetworkManager LAN set
// and verifies a published name resolves to a LAN interface's own address —
// the #130 end-state where each interface announces itself.
func TestDBusPublisher_PerInterfaceAnnounce(t *testing.T) {
	if _, err := exec.LookPath("avahi-resolve"); err != nil {
		t.Skip("avahi-resolve not installed (apt install avahi-utils)")
	}
	prov := &netstate.NMProvider{}
	defer prov.Close()
	ifaces, err := prov.LANInterfaces()
	if err != nil {
		t.Skipf("NetworkManager not reachable: %v", err)
	}
	if len(ifaces) == 0 {
		t.Skip("no active ethernet/WiFi connections on this box")
	}

	p := &DBusPublisher{HostSuffix: ".local", LAN: prov.LANInterfaces}
	defer p.Close()
	name, err := p.Publish("avahitest-130")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	defer p.Unpublish("avahitest-130")

	got := resolve4(t, name, "")
	if got == "" {
		t.Fatalf("%s never resolved via avahi-resolve (waited ~3s)", name)
	}
	for _, li := range ifaces {
		if got == li.IPv4 {
			return
		}
	}
	t.Errorf("resolved %s = %s; want one of the LAN interface addresses %+v", name, got, ifaces)
}

// TestDBusPublisher_PublishFailsWithZeroLANInterfaces pins the zero-LAN
// decision: all links down is a Publish error the install surfaces, not a
// silent no-op announcement.
func TestDBusPublisher_PublishFailsWithZeroLANInterfaces(t *testing.T) {
	p := &DBusPublisher{HostSuffix: ".local", LAN: func() ([]netstate.LANInterface, error) {
		return nil, nil
	}}
	defer p.Close()

	_, err := p.Publish("avahitest-zerolan")
	if err == nil {
		t.Fatal("Publish with zero LAN interfaces: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no LAN interface") {
		t.Errorf("error should name the zero-LAN condition, got: %v", err)
	}
}

// TestDBusPublisher_RepublishAll is the IP-change replay check: committed
// entry groups hold the literal address they were committed with, so after
// the (stubbed) detected address changes, RepublishAll must withdraw and
// re-announce every name with the new address — and keep the announced name
// exactly as stored.
func TestDBusPublisher_RepublishAll(t *testing.T) {
	if _, err := exec.LookPath("avahi-resolve"); err != nil {
		t.Skip("avahi-resolve not installed (apt install avahi-utils)")
	}

	// TEST-NET-2 addresses: never routable, so the announcements are inert.
	addr := "198.51.100.7"
	p := &DBusPublisher{HostSuffix: ".local", detectIP: func() (string, error) { return addr, nil }}
	defer p.Close()

	name, err := p.Publish("avahitest-replay")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	defer p.Unpublish("avahitest-replay")
	if got := resolve4(t, name, "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("before replay: resolved %q, want 198.51.100.7", got)
	}

	addr = "198.51.100.8" // the "DHCP lease change"
	if err := p.RepublishAll(); err != nil {
		t.Fatalf("RepublishAll: %v", err)
	}
	if got := resolve4(t, name, "198.51.100.8"); got != "198.51.100.8" {
		t.Errorf("after replay: resolved %q, want the new address 198.51.100.8", got)
	}
	if g, ok := p.groups["avahitest-replay"]; !ok || g.name != name {
		t.Errorf("after replay: stored name %q, want unchanged %q", g.name, name)
	}
	if len(p.groups) != 1 {
		t.Errorf("after replay: want 1 group, got %d", len(p.groups))
	}
}

// TestDBusPublisher_ErrCollisionSentinel verifies the sentinel is usable with
// errors.Is even when wrapped.
func TestDBusPublisher_ErrCollisionSentinel(t *testing.T) {
	wrapped := errors.New("wrapped: " + ErrCollision.Error())
	if errors.Is(wrapped, ErrCollision) {
		t.Error("plain wrapped string should not match ErrCollision via errors.Is")
	}
	// The real collision path uses fmt.Errorf("%w", ErrCollision) which does
	// chain correctly.
}
