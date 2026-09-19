package api

import (
	"testing"

	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// tzFailSentinel is referenced by the harness's set-timezone mock (auth_test.go):
// a syntactically-valid zone the mock answers 500, exercising the brain's
// host-502 path without an unreachable socket.
const tzFailSentinel = "Fail/Boom"

// --- POST /system/timezone -------------------------------------------------

func TestSetTimezone_AppliesAndReturnsZone(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2") // leaves an admin session in the jar

	resp := h.do("POST", "/api/v1/system/timezone", map[string]string{"timezone": "Europe/Stockholm"})
	if resp.StatusCode != 200 {
		t.Fatalf("set timezone = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Timezone string `json:"timezone"`
	}](t, resp)
	if body.Timezone != "Europe/Stockholm" {
		t.Errorf("echoed zone = %q; want Europe/Stockholm", body.Timezone)
	}
	// The zone reached the host-agent.
	h.pmu.Lock()
	calls := append([]string(nil), *h.tzCalls...)
	h.pmu.Unlock()
	if len(calls) != 1 || calls[0] != "Europe/Stockholm" {
		t.Errorf("host set-timezone calls = %v; want [Europe/Stockholm]", calls)
	}
}

func TestSetTimezone_TrimsAndAccepts(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	resp := h.do("POST", "/api/v1/system/timezone", map[string]string{"timezone": "  UTC  "})
	if resp.StatusCode != 200 {
		t.Fatalf("set timezone (padded) = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Timezone string `json:"timezone"`
	}](t, resp)
	if body.Timezone != "UTC" {
		t.Errorf("echoed zone = %q; want trimmed UTC", body.Timezone)
	}
}

func TestSetTimezone_InvalidZone422(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	resp := h.do("POST", "/api/v1/system/timezone", map[string]string{"timezone": "Not A Zone!"})
	if resp.StatusCode != 422 {
		t.Fatalf("invalid zone = %d; want 422", resp.StatusCode)
	}
	resp.Body.Close()
	// A 422 is a pure validation rejection — nothing should have hit the host.
	h.pmu.Lock()
	n := len(*h.tzCalls)
	h.pmu.Unlock()
	if n != 0 {
		t.Errorf("host called %d times on invalid zone; want 0", n)
	}
}

// A comma in the zone name must be a brain-side 422, not a host 502. Regression
// guard: the validator's character class once read `+-` as an ASCII range that
// swept in `,`, so a comma slipped past the brain and only timedatectl rejected
// it (yielding the wrong 502 status for what is malformed input).
func TestSetTimezone_CommaRejected422(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	resp := h.do("POST", "/api/v1/system/timezone", map[string]string{"timezone": "Europe/Foo,Bar"})
	if resp.StatusCode != 422 {
		t.Fatalf("comma zone = %d; want 422 (brain-side rejection, not host 502)", resp.StatusCode)
	}
	resp.Body.Close()
	h.pmu.Lock()
	n := len(*h.tzCalls)
	h.pmu.Unlock()
	if n != 0 {
		t.Errorf("host called %d times on comma zone; want 0", n)
	}
}

func TestSetTimezone_HostError502(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	resp := h.do("POST", "/api/v1/system/timezone", map[string]string{"timezone": tzFailSentinel})
	if resp.StatusCode != 502 {
		t.Fatalf("host failure = %d; want 502", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSetTimezone_RequiresAdmin(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("POST", "/api/v1/system/timezone", map[string]string{"timezone": "UTC"})
	if resp.StatusCode != 403 {
		t.Fatalf("member set timezone = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSetTimezone_RequiresAuth(t *testing.T) {
	h := newHarness(t) // no session

	resp := h.do("POST", "/api/v1/system/timezone", map[string]string{"timezone": "UTC"})
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated set timezone = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- POST /system/telemetry ------------------------------------------------

func TestSetTelemetry_Persists(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	resp := h.do("POST", "/api/v1/system/telemetry", map[string]bool{"enabled": true})
	if resp.StatusCode != 200 {
		t.Fatalf("set telemetry on = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Enabled bool `json:"enabled"`
	}](t, resp)
	if !body.Enabled {
		t.Error("echoed enabled = false; want true")
	}
	if v, _ := h.st.GetBoxMeta(store.BoxMetaTelemetryConsent); v != "true" {
		t.Errorf("persisted consent = %q; want true", v)
	}

	// Toggling back off persists "false" (not an unset key).
	resp = h.do("POST", "/api/v1/system/telemetry", map[string]bool{"enabled": false})
	if resp.StatusCode != 200 {
		t.Fatalf("set telemetry off = %d; want 200", resp.StatusCode)
	}
	resp.Body.Close()
	if v, _ := h.st.GetBoxMeta(store.BoxMetaTelemetryConsent); v != "false" {
		t.Errorf("persisted consent after off = %q; want false", v)
	}
}

func TestSetTelemetry_RequiresAdmin(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("POST", "/api/v1/system/telemetry", map[string]bool{"enabled": true})
	if resp.StatusCode != 403 {
		t.Fatalf("member set telemetry = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSetTelemetry_RequiresAuth(t *testing.T) {
	h := newHarness(t) // no session

	resp := h.do("POST", "/api/v1/system/telemetry", map[string]bool{"enabled": true})
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated set telemetry = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- POST /system/first-run-complete --------------------------------------

func TestCompleteFirstRun_PersistsAndReflectedInState(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	// Before completion, /auth/state reports the wizard is still pending.
	st := h.authStateBody()
	if st.FirstRunComplete {
		t.Fatal("first_run_complete true before completing the wizard")
	}

	resp := h.do("POST", "/api/v1/system/first-run-complete", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("complete first run = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		FirstRunComplete bool `json:"first_run_complete"`
	}](t, resp)
	if !body.FirstRunComplete {
		t.Error("echoed first_run_complete = false; want true")
	}
	if v, _ := h.st.GetBoxMeta(store.BoxMetaFirstRunComplete); v != "true" {
		t.Errorf("persisted marker = %q; want true", v)
	}

	// /auth/state now reflects completion.
	if st := h.authStateBody(); !st.FirstRunComplete {
		t.Error("/auth/state first_run_complete = false after completion; want true")
	}

	// Idempotent: re-marking an already-complete box is a no-op 200.
	resp = h.do("POST", "/api/v1/system/first-run-complete", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("re-complete = %d; want idempotent 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCompleteFirstRun_RequiresAdmin(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("POST", "/api/v1/system/first-run-complete", nil)
	if resp.StatusCode != 403 {
		t.Fatalf("member complete first run = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCompleteFirstRun_RequiresAuth(t *testing.T) {
	h := newHarness(t) // no session

	resp := h.do("POST", "/api/v1/system/first-run-complete", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated complete first run = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- GET /auth/state profile + first_run_complete --------------------------

func TestAuthState_DefaultsApplianceProfile(t *testing.T) {
	h := newHarness(t) // zero-valued env ⇒ appliance
	st := h.authStateBody()
	if st.Profile != string(profile.Appliance) {
		t.Errorf("profile = %q; want appliance", st.Profile)
	}
	if st.HasUsers || st.FirstRunComplete {
		t.Errorf("fresh box: has_users=%v first_run_complete=%v; want both false", st.HasUsers, st.FirstRunComplete)
	}
}

func TestAuthState_HostedProfile(t *testing.T) {
	h := hostedHarness(t)
	if st := h.authStateBody(); st.Profile != string(profile.Hosted) {
		t.Errorf("profile = %q; want hosted", st.Profile)
	}
}

// --- POST /setup recovery toggle (FIRST_RUN.md # Step 2a) ------------------

func TestSetup_RecoveryExplicitTrue_GeneratesCode(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/v1/setup", map[string]any{
		"username": "alice", "password": "hunter2", "recovery": true,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup recovery:true = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		RecoveryCode string `json:"recovery_code"`
	}](t, resp)
	if len(body.RecoveryCode) != 24 {
		t.Errorf("recovery code len = %d; want 24", len(body.RecoveryCode))
	}
}

func TestSetup_RecoveryOff_NoCodeAndCannotRecover(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/v1/setup", map[string]any{
		"username": "alice", "password": "hunter2", "recovery": false,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup recovery:false = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		RecoveryCode string `json:"recovery_code"`
	}](t, resp)
	if body.RecoveryCode != "" {
		t.Errorf("recovery code = %q; want empty when recovery off", body.RecoveryCode)
	}

	// The admin row carries no recovery hash, so the public recover flow can't
	// be redeemed — any code 401s (the empty hash never matches).
	u, err := h.st.GetUserByUsername("alice")
	if err != nil {
		t.Fatalf("lookup alice: %v", err)
	}
	if u.RecoveryHash != "" {
		t.Errorf("recovery hash = %q; want empty when recovery off", u.RecoveryHash)
	}
	resp = h.do("POST", "/api/v1/recover", map[string]string{
		"username": "alice", "recovery_code": "ABCD-EFGH-IJKL-MNOP", "new_password": "newpass12",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("recover with no code on file = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// authStateBody fetches GET /auth/state (public) and decodes the wizard-gating
// fields the dashboard reads at boot.
func (h *harness) authStateBody() struct {
	HasUsers         bool   `json:"has_users"`
	FirstRunComplete bool   `json:"first_run_complete"`
	Profile          string `json:"profile"`
} {
	h.t.Helper()
	resp := h.do("GET", "/api/v1/auth/state", nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("auth/state = %d; want 200", resp.StatusCode)
	}
	return decodeJSON[struct {
		HasUsers         bool   `json:"has_users"`
		FirstRunComplete bool   `json:"first_run_complete"`
		Profile          string `json:"profile"`
	}](h.t, resp)
}
