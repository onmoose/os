// Package netstate provides the box's LAN-facing network state: which
// interfaces face the LAN and with what IPv4 address. It is the shared source
// for everything that must agree on "the LAN side of the box" — per-interface
// Avahi announcements and the avahi-daemon.conf interface allowlist
// (DISCOVERY.md # Interface scoping), the discovery-state report, and the
// future WiFi-setup surface.
//
// Two providers:
//
//   - NMProvider (nm_linux.go) reads NetworkManager over the system DBus. NM
//     owns every interface on a moose box (BOOT.md # NetworkManager owns the
//     network stack), so its active ethernet/WiFi connections *are* the LAN
//     set. Mesh interfaces, Docker bridges, and veths are structurally
//     excluded: they are never active NM connections of those types.
//
//   - DetectIPv4 (probe.go) is the no-NM fallback (dev inner loop, boxes
//     without NM): the kernel's route-chosen source address via a connected
//     UDP probe, with interface enumeration as a last resort (#129). It
//     yields one address, not a set — callers fall back to single-address,
//     all-interfaces behavior.
package netstate

// LANInterface is one LAN-facing interface: the device of an active
// NetworkManager ethernet or WiFi connection, with its kernel interface index
// and primary IPv4 address.
type LANInterface struct {
	Name  string // kernel interface name, e.g. "eno1"
	Index int    // kernel interface index (Avahi AddAddress takes this)
	IPv4  string // primary IPv4 address, dotted quad
}
