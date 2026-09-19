//go:build linux

// Package avahipublisher publishes per-app A-record aliases via the Avahi DBus
// API. It replaces the earlier static-file approach (the false start) which
// could not publish raw A records — Avahi static files announce *services*, not
// bare hostname aliases. See DECISIONS.md entry 2026-05-24 and
// docs/progress/avahi-dbus-publisher.md for the full story.
//
// # Mechanism
//
// For each app slug the publisher:
//  1. Calls org.freedesktop.Avahi.Server.EntryGroupNew to get a fresh group.
//  2. Calls org.freedesktop.Avahi.EntryGroup.AddAddress once per LAN
//     interface, announcing that interface's own IPv4 on that interface —
//     the same shape avahi-daemon uses for its native host record.
//  3. Calls org.freedesktop.Avahi.EntryGroup.Commit to start the announcement.
//
// Groups are stored in p.groups[slug] together with the announced name.
// Unpublish calls EntryGroup.Free, which withdraws the announcement and
// releases the DBus object.
//
// # Which interfaces, which addresses
//
// The LAN set comes from the LAN field — in production netstate.NMProvider,
// NetworkManager's active ethernet/WiFi connections (DISCOVERY.md # Interface
// scoping). Per-interface announcement makes the bridge problem structurally
// impossible: a Docker bridge is never in the set, so no record is ever
// announced on or for it, and a multi-homed box (ethernet + WiFi) announces
// each side's own address to the clients on that side (#130).
//
// The set is re-read on every Publish — no caching — so an app published
// after a DHCP lease change announces the current address. Zero LAN
// interfaces (all links down, boot race) is a Publish error: the install
// surfaces it, and the NM watcher's RepublishAll retries on the next event.
//
// When LAN is nil, Publish falls back to #129's single-address behavior: the
// kernel's route-chosen source address (netstate.DetectIPv4) announced with
// interface index -1 ("all interfaces"). No production binary takes this path
// today — cmd/host-agent-real and cmd/moose-network-verify always set LAN, and
// the dev inner loop uses FakePublisher, not this type. It is the preserved
// #129 compatibility shim and is exercised only by tests.
//
// # IP changes and avahi-daemon restarts
//
// Committed entry groups hold the literal addresses they were committed with;
// `avahi-daemon --reload` does not rewrite them, and an avahi-daemon restart
// destroys them outright. RepublishAll re-announces every published name with
// the current LAN set — the NM watcher calls it on state changes and after
// the allowlist sync restarts avahi-daemon (see conf.go). DBus groups are
// also lost when host-agent itself restarts; the brain re-publishes all
// running instances at startup via lifecycle.Reconcile. Mid-life host-agent
// restart while the brain is running is a known gap — see
// docs/progress/avahi-dbus-publisher.md.
package avahipublisher

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/onmoose/os/internal/hostagent/netstate"
)

// Avahi DBus constants from <avahi-common/defs.h>.
const (
	avahiInterfaceUnspec int32 = -1
	avahiProtoUnspec     int32 = -1

	// AddAddress publish flag. NO_REVERSE = don't publish the reverse PTR
	// record. Avahi already owns 192.168.x.x → <hostname>.local via the host's
	// own address registration; re-publishing the reverse for a second name
	// triggers "Local name collision" because the PTR is unique by IP.
	// Forward A record collisions are handled by Avahi's own probing.
	avahiPublishFlagsAlias uint32 = 16
)

// avahiService and avahiServer are the DBus service/path/interface constants.
const (
	avahiService         = "org.freedesktop.Avahi"
	avahiServerPath      = dbus.ObjectPath("/")
	avahiServerIface     = "org.freedesktop.Avahi.Server"
	avahiEntryGroupIface = "org.freedesktop.Avahi.EntryGroup"
)

// ErrCollision is returned by Publish when Avahi reports a name collision for
// the requested hostname. Callers that need to react differently (e.g. slug
// rename) can check with errors.Is.
var ErrCollision = errors.New("avahipublisher: name collision")

// addressTarget is one A record to announce: an interface index (or
// avahiInterfaceUnspec in the no-NM fallback) and the address to announce
// there.
type addressTarget struct {
	iface int32
	ip    string
}

// publishedGroup is the live state for one published slug: the Avahi
// EntryGroup object and the name that won (primary or collision fallback) —
// RepublishAll re-announces that exact name, never re-running the fallback,
// so the name the brain stored stays true.
type publishedGroup struct {
	path dbus.ObjectPath
	name string
}

// DBusPublisher publishes per-app A-record aliases via Avahi's DBus API.
// Zero value is not usable — construct via &DBusPublisher{HostSuffix: ...}.
// Safe for concurrent use.
type DBusPublisher struct {
	// HostSuffix is appended to each slug to form the announced hostname
	// (production: protocol.AppHostSuffix, ".local").
	HostSuffix string

	// LAN, when non-nil, supplies the LAN interface set to announce on
	// (production: netstate.NMProvider.LANInterfaces). nil means no
	// NetworkManager — the dev inner loop — and Publish falls back to one
	// route-probed address on interface -1 (#129 behavior).
	LAN func() ([]netstate.LANInterface, error)

	// detectIP, when non-nil, replaces fallback local-IP detection — tests
	// stub it so they don't depend on the host's routing table. nil
	// (production) means netstate.DetectIPv4. Only consulted when LAN is nil.
	detectIP func() (string, error)

	mu     sync.Mutex
	conn   *dbus.Conn
	groups map[string]publishedGroup // slug → live announcement
}

// Publish announces A records for the slug on every LAN interface (each with
// that interface's own address) and returns the announced hostname.
//
// The primary name is "<slug>" + HostSuffix (e.g. "photos.local"). If Avahi
// reports a name collision (another device on the LAN already owns it), Publish
// retries once with a box-qualified fallback, "<slug>-<box>" + HostSuffix (e.g.
// "photos-moose.local"), where <box> is this host's name. The caller (the
// brain) must use the *returned* name for the URL it shows and the Caddy route
// it writes — it may differ from the primary on collision. See DISCOVERY.md.
//
// If the slug is already published (e.g. replay on brain restart), the old
// group is freed first and a fresh one is committed — idempotent.
//
// Returns ErrCollision only if both the primary and the fallback collide.
//
// Limitation: collision is detected on the synchronous AddAddress error. Avahi
// can also report a conflict asynchronously after Commit (EntryGroup state
// COLLISION); that path is not yet watched here — see the progress doc.
func (p *DBusPublisher) Publish(slug string) (string, error) {
	if !slugRE.MatchString(slug) {
		return "", fmt.Errorf("avahipublisher: invalid slug %q (must match [a-z0-9-]+)", slug)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureConn(); err != nil {
		return "", err
	}

	targets, err := p.announceTargets()
	if err != nil {
		return "", err
	}

	// If already published, free the old group first (idempotent replay).
	if old, ok := p.groups[slug]; ok {
		_ = p.freeGroup(old.path)
		delete(p.groups, slug)
	}

	hostname := slug + p.HostSuffix
	groupPath, err := p.tryPublish(hostname, targets)
	if errors.Is(err, ErrCollision) {
		fallback := slug + "-" + p.boxLabel() + p.HostSuffix
		slog.Warn("avahi name collision; retrying with box-qualified name",
			"slug", slug, "name", hostname, "fallback", fallback)
		hostname = fallback
		groupPath, err = p.tryPublish(hostname, targets)
	}
	if err != nil {
		return "", err
	}

	if p.groups == nil {
		p.groups = make(map[string]publishedGroup)
	}
	p.groups[slug] = publishedGroup{path: groupPath, name: hostname}
	slog.Info("avahi publish", "slug", slug, "name", hostname, "targets", targetsForLog(targets))
	return hostname, nil
}

// RepublishAll re-announces every published name with the current LAN set.
// Called by the avahi sync (conf.go) after an NM state change — committed
// entry groups hold the literal old addresses — and after an avahi-daemon
// restart, which destroys all entry groups server-side (freeing the stale
// group object is best-effort there).
//
// Each name is re-announced exactly as stored: the collision fallback never
// re-runs, so the name the brain persisted cannot silently change. A name
// that now collides (another device claimed it while we were down) fails its
// replay; the failure is logged, the slug stays tracked, and the next replay
// retries. Returns the first error encountered after attempting every slug.
func (p *DBusPublisher) RepublishAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.groups) == 0 {
		return nil
	}
	if err := p.ensureConn(); err != nil {
		return err
	}
	targets, err := p.announceTargets()
	if err != nil {
		return err
	}

	var firstErr error
	for slug, g := range p.groups {
		_ = p.freeGroup(g.path) // stale after an avahi restart — best-effort
		groupPath, err := p.tryPublish(g.name, targets)
		if err != nil {
			slog.Error("avahi republish failed; will retry on next network change",
				"slug", slug, "name", g.name, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		p.groups[slug] = publishedGroup{path: groupPath, name: g.name}
		slog.Info("avahi republish", "slug", slug, "name", g.name, "targets", targetsForLog(targets))
	}
	return firstErr
}

// announceTargets resolves what to announce where. Called with p.mu held.
func (p *DBusPublisher) announceTargets() ([]addressTarget, error) {
	if p.LAN == nil {
		// No NetworkManager (dev inner loop): one route-probed address,
		// announced on all interfaces (#129).
		ip, err := p.fallbackIPv4()
		if err != nil {
			return nil, err
		}
		return []addressTarget{{iface: avahiInterfaceUnspec, ip: ip}}, nil
	}
	ifaces, err := p.LAN()
	if err != nil {
		return nil, fmt.Errorf("avahipublisher: LAN interface set: %w", err)
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("avahipublisher: no LAN interface is up (all links down?); nothing to announce on")
	}
	targets := make([]addressTarget, 0, len(ifaces))
	for _, li := range ifaces {
		targets = append(targets, addressTarget{iface: int32(li.Index), ip: li.IPv4})
	}
	return targets, nil
}

// tryPublish creates a fresh entry group, adds one A record per target, and
// commits it. On an Avahi collision it returns ErrCollision (wrapped) so
// Publish can retry with a box-qualified name. The group is freed on any error.
func (p *DBusPublisher) tryPublish(hostname string, targets []addressTarget) (dbus.ObjectPath, error) {
	groupPath, err := p.newEntryGroup()
	if err != nil {
		return "", fmt.Errorf("avahipublisher: EntryGroupNew: %w", err)
	}

	for _, t := range targets {
		if err := p.addAddress(groupPath, t.iface, hostname, t.ip); err != nil {
			_ = p.freeGroup(groupPath)
			if isCollision(err) {
				return "", fmt.Errorf("%w: %s", ErrCollision, hostname)
			}
			return "", fmt.Errorf("avahipublisher: AddAddress(%s, if%d, %s): %w", hostname, t.iface, t.ip, err)
		}
	}

	if err := p.commitGroup(groupPath); err != nil {
		_ = p.freeGroup(groupPath)
		return "", fmt.Errorf("avahipublisher: Commit(%s): %w", hostname, err)
	}
	return groupPath, nil
}

// boxLabel returns this host's name as a single DNS label for use in the
// collision-fallback name ("<slug>-<box>.local"). Sanitization lives in
// sanitizeBoxLabel (slug.go) so it can be unit-tested without DBus.
func (p *DBusPublisher) boxLabel() string {
	h, err := os.Hostname()
	if err != nil {
		return "moose"
	}
	return sanitizeBoxLabel(h)
}

// Unpublish withdraws the A records for the given slug.
// Idempotent: if the slug was never published (or already unpublished), returns nil.
func (p *DBusPublisher) Unpublish(slug string) error {
	if !slugRE.MatchString(slug) {
		return fmt.Errorf("avahipublisher: invalid slug %q (must match [a-z0-9-]+)", slug)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	g, ok := p.groups[slug]
	if !ok {
		return nil // already gone — idempotent
	}

	if err := p.freeGroup(g.path); err != nil {
		return fmt.Errorf("avahipublisher: Free(%s): %w", slug, err)
	}
	delete(p.groups, slug)
	slog.Info("avahi unpublish", "slug", slug)
	return nil
}

// Close frees all entry groups and closes the DBus connection.
// Should be called (via defer) when host-agent shuts down.
func (p *DBusPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for slug, g := range p.groups {
		if err := p.freeGroup(g.path); err != nil {
			slog.Warn("avahi close: free group", "slug", slug, "err", err)
		}
	}
	p.groups = nil

	if p.conn != nil {
		err := p.conn.Close()
		p.conn = nil
		return err
	}
	return nil
}

// --- internal helpers (all called with p.mu held) ---

// ensureConn connects to the system bus if not already connected.
func (p *DBusPublisher) ensureConn() error {
	if p.conn != nil {
		return nil
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("avahipublisher: connect system bus: %w", err)
	}
	p.conn = conn
	return nil
}

// fallbackIPv4 returns the box's single LAN IPv4 for the no-NM path,
// re-detected on every call (no cache): apps published after a DHCP lease
// change must announce the current address.
func (p *DBusPublisher) fallbackIPv4() (string, error) {
	if p.detectIP != nil {
		return p.detectIP()
	}
	return netstate.DetectIPv4()
}

// newEntryGroup calls org.freedesktop.Avahi.Server.EntryGroupNew and returns
// the resulting object path.
func (p *DBusPublisher) newEntryGroup() (dbus.ObjectPath, error) {
	server := p.conn.Object(avahiService, avahiServerPath)
	var groupPath dbus.ObjectPath
	if err := server.Call(avahiServerIface+".EntryGroupNew", 0).Store(&groupPath); err != nil {
		return "", err
	}
	return groupPath, nil
}

// addAddress calls org.freedesktop.Avahi.EntryGroup.AddAddress on the given
// group, announcing the record on one interface (or all, for iface -1).
func (p *DBusPublisher) addAddress(groupPath dbus.ObjectPath, iface int32, hostname, ip string) error {
	group := p.conn.Object(avahiService, groupPath)
	return group.Call(avahiEntryGroupIface+".AddAddress", 0,
		iface,
		avahiProtoUnspec,
		avahiPublishFlagsAlias,
		hostname,
		ip,
	).Err
}

// commitGroup calls org.freedesktop.Avahi.EntryGroup.Commit.
func (p *DBusPublisher) commitGroup(groupPath dbus.ObjectPath) error {
	group := p.conn.Object(avahiService, groupPath)
	return group.Call(avahiEntryGroupIface+".Commit", 0).Err
}

// freeGroup calls org.freedesktop.Avahi.EntryGroup.Free, withdrawing the
// announcement. Best-effort: errors are returned but not fatal on shutdown.
func (p *DBusPublisher) freeGroup(groupPath dbus.ObjectPath) error {
	group := p.conn.Object(avahiService, groupPath)
	return group.Call(avahiEntryGroupIface+".Free", 0).Err
}

// isCollision reports whether the DBus error is an Avahi name-collision error.
func isCollision(err error) bool {
	if err == nil {
		return false
	}
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		return dbusErr.Name == "org.freedesktop.Avahi.CollisionFailure"
	}
	return false
}

// targetsForLog renders the announce targets as "if2=192.168.2.160" pairs for
// the structured publish/republish log lines.
func targetsForLog(targets []addressTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = fmt.Sprintf("if%d=%s", t.iface, t.ip)
	}
	return out
}
