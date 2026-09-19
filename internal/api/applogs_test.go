package api

import (
	"net/http"
	"testing"

	"github.com/onmoose/os/internal/store"
)

// TestLogVisibility pins the per-app-log authorization matrix. It is STRICTER
// than canSee: a member may see a household app in the launcher, but its logs
// are admins-only; another member's personal app is 404 (leak guard), not 403.
func TestLogVisibility(t *testing.T) {
	admin := store.User{ID: "u_admin", Role: store.RoleAdmin}
	alex := store.User{ID: "u_alex", Role: store.RoleMember}
	mara := store.User{ID: "u_mara", Role: store.RoleMember}
	household := store.Instance{ID: "h1", Scope: store.ScopeHousehold, OwnerUserID: "u_admin"}
	alexPersonal := store.Instance{ID: "p1", Scope: store.ScopePersonal, OwnerUserID: "u_alex"}

	cases := []struct {
		name string
		u    store.User
		i    store.Instance
		want int
	}{
		{"admin sees household logs", admin, household, http.StatusOK},
		{"admin sees any personal logs", admin, alexPersonal, http.StatusOK},
		{"owner sees own personal logs", alex, alexPersonal, http.StatusOK},
		{"member denied household logs", mara, household, http.StatusForbidden},
		{"member 404 on another's personal (leak guard)", mara, alexPersonal, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := logVisibility(identity(tc.u), tc.i); got != tc.want {
				t.Errorf("logVisibility = %d, want %d", got, tc.want)
			}
		})
	}
}

// The denial paths return before the handler touches s.life / s.applogs (both
// nil in this harness), so they exercise the wire behavior without a live
// lifecycle or host-agent.

func TestAppLogRequiresAuth(t *testing.T) {
	h := newHarness(t)
	// No setup, no login → no session cookie. The auth middleware fences the
	// route before the handler runs, so no instance need exist.
	resp := h.do("GET", "/api/v1/apps/anything/log", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated log stream = %d, want 401", resp.StatusCode)
	}
}

func TestAppLogMemberDeniedHouseholdLogs(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_mara", "mara", "pw123456")
	h.seedInstance("h1", "jellyfin", "jellyfin", "u_admin", store.ScopeHousehold)

	h.loginAs("mara", "pw123456")
	resp := h.do("GET", "/api/v1/apps/h1/log", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member household logs = %d, want 403", resp.StatusCode)
	}
}

func TestAppLogLeakGuardOnOthersPersonal(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_alex", "alex", "pw123456")
	h.addMember("u_mara", "mara", "pw123456")
	h.seedInstance("p1", "immich", "immich--alex", "u_alex", store.ScopePersonal)

	h.loginAs("mara", "pw123456")
	resp := h.do("GET", "/api/v1/apps/p1/log", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member other-personal logs = %d, want 404 (leak guard)", resp.StatusCode)
	}
}

func TestAppLogUnknownInstance(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.loginAs("admin", "hunter2")

	resp := h.do("GET", "/api/v1/apps/nope/log", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown instance logs = %d, want 404", resp.StatusCode)
	}
}
