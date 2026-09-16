// Package servicehealth is host-agent's locus-B service-down detector
// (HEALTH.md # Detector catalog, the service-down row). It runs
// `systemctl is-active` over a profile-specific allowlist of host systemd units
// and emits one `service-down` finding (with the unit name as instance_key) for
// each unit that isn't active. The brain reconciles them under the report's
// `services` category and surfaces them as `state`-category issues, debouncing them (raise
// on 2 consecutive bad samples).
//
// The detector reports the instantaneous state every poll; debounce and
// clear-on-recover live in the brain (internal/health), not here.
package servicehealth

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/onmoose/os/internal/protocol"
)

// issueServiceDown is the registered issue ID raised per non-active unit.
const issueServiceDown = "service-down"

// ApplianceUnits is the host-unit allowlist the service-down detector watches on
// the appliance profile (HEALTH.md # Detector catalog, the locus-B row: Docker,
// Avahi, chrony, Samba). The names are the Debian systemd unit names.
//
// Two units that an older list named are intentionally absent:
//   - caddy — Caddy is a brain-managed container (moose-caddy), not a host
//     systemd unit, on every profile; there is no caddy.service for
//     `systemctl is-active` to query, so watching it here only ever yields a
//     phantom service-down. Its liveness is a locus-C check (HEALTH.md
//     # Locus C — deferred until the brain owns Caddy's container lifecycle).
//   - host-agent — a dead host-agent can't report on itself, so checking it
//     here is pointless.
var ApplianceUnits = []string{
	"docker.service",
	"avahi-daemon.service",
	"chrony.service",
	"smbd.service",
}

// HostedUnits is the allowlist for the lean hosted-cloud profile. The cloud
// image installs none of Avahi, Samba, or chrony — they are cut, not
// disabled (ENVIRONMENT.md # How the profile is realized) — and Caddy is a
// container there too, so docker.service is the only core host systemd unit a
// hosted box runs. Watching the appliance units on a hosted box would raise a
// permanent service-down for each absent unit on an otherwise-healthy box.
var HostedUnits = []string{
	"docker.service",
}

// Reporter implements hostagent.ServiceReporter. isActive is injectable so
// tests can drive unit states without a real systemd.
type Reporter struct {
	units    []string
	isActive func(unit string) (active bool, state string)
}

// New returns a Reporter over the given host-unit allowlist, backed by real
// `systemctl is-active`. Callers pass the profile's unit set — ApplianceUnits
// from the appliance wiring, HostedUnits from the slim cloud wiring.
func New(units []string) *Reporter {
	return &Reporter{units: units, isActive: systemctlIsActive}
}

// Read returns one service-down finding per non-active core unit. It always
// returns a usable slice (nil when every unit is active) — inactive units are
// data, not errors. `systemctl is-active` reports the unit's state on stdout
// even when the unit is down (and exits non-zero).
func (r *Reporter) Read() []protocol.Finding {
	var findings []protocol.Finding
	for _, unit := range r.units {
		active, state := r.isActive(unit)
		if active {
			continue
		}
		findings = append(findings, protocol.Finding{
			ID:          issueServiceDown,
			InstanceKey: unit,
			Details:     fmt.Sprintf("%s is %s", unit, state),
		})
	}
	return findings
}

// systemctlIsActive runs `systemctl is-active <unit>` and reports whether the
// unit is active plus the raw state word it printed ("active", "inactive",
// "failed", "activating", …). systemctl prints the state on stdout and exits
// non-zero for any non-active state, so the exit error is expected and ignored;
// only an empty stdout (systemctl missing or produced nothing) is mapped to a
// synthetic "unknown" state.
func systemctlIsActive(unit string) (bool, string) {
	out, _ := exec.Command("systemctl", "is-active", unit).Output()
	state := strings.TrimSpace(string(out))
	if state == "" {
		state = "unknown"
	}
	return state == "active", state
}
