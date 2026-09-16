package servicehealth

import (
	"reflect"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

// TestRead_ReportsOnlyNonActiveUnits verifies the reporter emits one
// service-down finding per non-active unit (carrying the unit name as
// instance_key and its raw state in details) and skips active units.
func TestRead_ReportsOnlyNonActiveUnits(t *testing.T) {
	r := &Reporter{
		units: []string{"docker.service", "foo.service", "smbd.service"},
		isActive: func(unit string) (bool, string) {
			if unit == "foo.service" {
				return false, "failed"
			}
			return true, "active"
		},
	}

	got := r.Read()
	want := []protocol.Finding{
		{ID: "service-down", InstanceKey: "foo.service", Details: "foo.service is failed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read: got %v, want %v", got, want)
	}
}

// TestRead_AllActiveReturnsNil verifies a healthy box produces no findings —
// the reporter reports instantaneous state, so "everything up" is an empty
// (nil) result, not an error.
func TestRead_AllActiveReturnsNil(t *testing.T) {
	r := &Reporter{
		units:    ApplianceUnits,
		isActive: func(string) (bool, string) { return true, "active" },
	}

	got := r.Read()
	if got != nil {
		t.Errorf("all-active: want nil findings, got %v", got)
	}
}

// TestRead_OneFindingPerDownUnit verifies the per-unit instance_key scoping:
// two down units yield two distinct service-down findings, so docker-down and
// caddy-down are separate issues in the brain's registry rather than collapsing
// into one.
func TestRead_OneFindingPerDownUnit(t *testing.T) {
	r := &Reporter{
		units:    []string{"docker.service", "foo.service"},
		isActive: func(string) (bool, string) { return false, "inactive" },
	}

	got := r.Read()
	if len(got) != 2 {
		t.Fatalf("want one finding per down unit (2), got %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, f := range got {
		if f.ID != "service-down" {
			t.Errorf("finding id: want service-down, got %q", f.ID)
		}
		if f.InstanceKey == "" {
			t.Errorf("finding must carry the unit as instance_key, got %+v", f)
		}
		seen[f.InstanceKey] = true
	}
	if !seen["docker.service"] || !seen["foo.service"] {
		t.Errorf("want a finding for each down unit, got keys %v", seen)
	}
}

// TestNew_WatchesGivenUnits guards that the constructor watches exactly the
// allowlist it is handed, so each profile's wiring controls its own unit set.
func TestNew_WatchesGivenUnits(t *testing.T) {
	r := New(HostedUnits)
	if !reflect.DeepEqual(r.units, HostedUnits) {
		t.Errorf("New(HostedUnits).units: got %v, want %v", r.units, HostedUnits)
	}
}

// TestAllowlists_NoPhantomOrSelfUnits guards the two defects this detector has
// to avoid raising a permanent false service-down on a healthy box:
//   - caddy is a brain-managed container, not a host systemd unit, so it must
//     not appear in any systemctl allowlist (HEALTH.md # Locus C).
//   - host-agent can't report on its own state, so it is never watched here.
//
// It also pins the hosted profile to docker-only — the lean cloud image ships
// none of Avahi/chrony/Samba, so watching them there is a guaranteed false
// positive.
func TestAllowlists_NoPhantomOrSelfUnits(t *testing.T) {
	for name, units := range map[string][]string{
		"appliance": ApplianceUnits,
		"hosted":    HostedUnits,
	} {
		for _, u := range units {
			if u == "caddy.service" {
				t.Errorf("%s: caddy is a container, not a host unit — must not be watched via systemctl", name)
			}
			if u == "host-agent.service" {
				t.Errorf("%s: host-agent must not be in the allowlist — it can't report on itself", name)
			}
		}
	}
	if !reflect.DeepEqual(HostedUnits, []string{"docker.service"}) {
		t.Errorf("HostedUnits: want docker-only (lean image cuts Avahi/chrony/Samba), got %v", HostedUnits)
	}
}
