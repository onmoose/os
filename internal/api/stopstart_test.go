package api

import (
	"net/http"
	"testing"

	"github.com/onmoose/moose/internal/store"
)

// Stop/start authorization + transition guards. The harness builds the server
// with life=nil, so these cover only the synchronous paths that return before
// the job goroutine runs (auth rejections + 409 guards). The happy path is
// exercised at the lifecycle layer (lifecycle_stopstart_test.go).

func TestStopStartRequireAuth(t *testing.T) {
	h := newHarness(t) // no setup → no session
	for _, path := range []string{"/api/v1/apps/i1/stop", "/api/v1/apps/i1/start"} {
		resp := h.do("POST", path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestStopMemberCannotControlHousehold(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_m", "mara", "pw123456")
	h.loginAs("mara", "pw123456")
	h.seedInstance("i1", "whoami", "whoami", "u_admin", store.ScopeHousehold)

	resp := h.do("POST", "/api/v1/apps/i1/stop", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member stop household = %d, want 403", resp.StatusCode)
	}
}

func TestStopUnknownApp404(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")

	resp := h.do("POST", "/api/v1/apps/ghost/stop", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop unknown app = %d, want 404", resp.StatusCode)
	}
}

func TestStopMemberOtherPersonalLeaks404(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_m", "mara", "pw123456")
	h.loginAs("mara", "pw123456")
	// A personal instance owned by someone else: a member must get 404, not 403,
	// so the existence of another user's app isn't disclosed.
	h.seedInstance("i1", "whoami", "whoami", "u_other", store.ScopePersonal)

	resp := h.do("POST", "/api/v1/apps/i1/stop", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member stop other's personal = %d, want 404", resp.StatusCode)
	}
}

func TestStop409WhenNotRunning(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.seedInstance("i1", "whoami", "whoami", "u_admin", store.ScopeHousehold)
	if err := h.st.SetState("i1", "stopped"); err != nil {
		t.Fatalf("set state: %v", err)
	}

	resp := h.do("POST", "/api/v1/apps/i1/stop", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stop already-stopped = %d, want 409", resp.StatusCode)
	}
}

func TestStart409WhenRunning(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.seedInstance("i1", "whoami", "whoami", "u_admin", store.ScopeHousehold)

	resp := h.do("POST", "/api/v1/apps/i1/start", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("start running = %d, want 409", resp.StatusCode)
	}
}
