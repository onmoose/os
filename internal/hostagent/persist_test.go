package hostagent

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

// newPersistentAgent builds a fake agent backed by statePath, mirroring how
// cmd/host-agent wires EnablePersistence. Each call is a fresh process stand-in:
// reuse the same path across two calls to simulate a restart.
func newPersistentAgent(t *testing.T, statePath string) (*Agent, *http.ServeMux) {
	t.Helper()
	a := New(nil, NewFakePublisher(".local"))
	a.Verifier = NewFakeVerifier(a)
	if err := a.EnablePersistence(statePath); err != nil {
		t.Fatalf("EnablePersistence: %v", err)
	}
	mux := http.NewServeMux()
	a.Mount(mux)
	return a, mux
}

// TestPersistence_PasswordSurvivesRestart reproduces the reported bug: create an
// account, "restart" the agent, and verify the password still validates. Before
// the JSON-file backing, the second agent's password map was empty, so login
// failed even though the brain still held the user + session rows.
func TestPersistence_PasswordSurvivesRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "fake-shadow.json")

	// First "process": create the account.
	_, mux1 := newPersistentAgent(t, statePath)
	if w := post(t, mux1, "/v1/auth/set-password", protocol.SetPasswordRequest{
		User: "andrei", Password: "hunter2",
	}); w.Code != http.StatusOK {
		t.Fatalf("set-password: want 200, got %d", w.Code)
	}

	// Second "process": same backing file, empty in-memory map until load.
	_, mux2 := newPersistentAgent(t, statePath)
	w := post(t, mux2, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "andrei", Password: "hunter2",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("verify-password: want 200, got %d", w.Code)
	}
	if got := decodeBody[protocol.VerifyPasswordResponse](t, w); !got.Valid {
		t.Fatal("password did not survive restart: want valid=true")
	}
}

// TestPersistence_DeleteSurvivesRestart confirms deletion is also durable: a
// deleted user must not come back to life after a restart.
func TestPersistence_DeleteSurvivesRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "fake-shadow.json")

	_, mux1 := newPersistentAgent(t, statePath)
	post(t, mux1, "/v1/auth/set-password", protocol.SetPasswordRequest{User: "andrei", Password: "hunter2"})
	if w := post(t, mux1, "/v1/auth/delete-user", protocol.DeleteUserRequest{User: "andrei"}); w.Code != http.StatusOK {
		t.Fatalf("delete-user: want 200, got %d", w.Code)
	}

	_, mux2 := newPersistentAgent(t, statePath)
	w := post(t, mux2, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "andrei", Password: "hunter2",
	})
	if got := decodeBody[protocol.VerifyPasswordResponse](t, w); got.Valid {
		t.Fatal("deleted user came back after restart: want valid=false")
	}
}

// TestPersistence_DisabledByDefault confirms the maps stay in-memory when
// EnablePersistence is never called — the test/real-binary path is unchanged.
func TestPersistence_DisabledByDefault(t *testing.T) {
	a := New(nil, NewFakePublisher(".local"))
	if a.statePath != "" {
		t.Fatalf("statePath should be empty by default, got %q", a.statePath)
	}
	// persistLocked must be a no-op (no panic, no file) when disabled.
	a.mu.Lock()
	a.persistLocked()
	a.mu.Unlock()
}
