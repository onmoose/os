package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// setupAdmin bootstraps an admin via the wire (POST /setup) and leaves a
// valid session in h.jar. Returns the UserDTO from the response body.
func (h *harness) setupAdmin(username, password string) UserDTO {
	h.t.Helper()
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": username, "password": password,
	})
	if resp.StatusCode != 200 {
		h.t.Fatalf("setup %s: %d", username, resp.StatusCode)
	}
	body := decodeJSON[struct {
		User UserDTO `json:"user"`
	}](h.t, resp)
	return body.User
}

// loginAs clears the current jar and logs in as username/password.
func (h *harness) loginAs(username, password string) {
	h.t.Helper()
	// Clear old cookies.
	jar, _ := newJar()
	h.jar = jar
	resp := h.do("POST", "/api/v1/login", map[string]string{
		"username": username, "password": password,
	})
	if resp.StatusCode != 200 {
		h.t.Fatalf("loginAs %s: %d", username, resp.StatusCode)
	}
	resp.Body.Close()
}

// --- List users ---

func TestListUsersAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	// list-users has no elevation gate; skip elevate here.

	const bobID = "u_bob001"
	h.addMember(bobID, "bob", "bobpass")

	// Admin (alice) can list users.
	resp := h.do("GET", "/api/v1/users", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list users as admin = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Users []UserDTO `json:"users"`
	}](t, resp)
	if len(body.Users) != 2 {
		t.Fatalf("list users: got %d, want 2", len(body.Users))
	}

	// Bob (member) gets 403.
	h.loginAs("bob", "bobpass")
	resp = h.do("GET", "/api/v1/users", nil)
	if resp.StatusCode != 403 {
		t.Fatalf("list users as member = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestListUsersRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	// New jar, no session.
	jar, _ := newJar()
	h.jar = jar
	resp := h.do("GET", "/api/v1/users", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("list users unauthenticated = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Create user ---

func TestCreateUserHappyPath(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "bobpass", "role": "member",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("create user: %d", resp.StatusCode)
	}
	body := decodeJSON[UserDTO](t, resp)
	if body.Username != "bob" || body.Role != store.RoleMember {
		t.Fatalf("create user body = %+v", body)
	}

	// The created user can log in.
	h.loginAs("bob", "bobpass")
	resp = h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("bob me = %d", resp.StatusCode)
	}
	me := decodeJSON[UserDTO](t, resp)
	if me.Username != "bob" {
		t.Fatalf("me = %+v", me)
	}
}

func TestCreateUserDefaultsToMember(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "charlie", "password": "charliepass",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("create user: %d", resp.StatusCode)
	}
	body := decodeJSON[UserDTO](t, resp)
	if body.Role != store.RoleMember {
		t.Fatalf("default role = %q; want member", body.Role)
	}
}

func TestCreateUserDuplicateUsername409(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p1",
	}).Body.Close()

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p2",
	})
	if resp.StatusCode != 409 {
		t.Fatalf("duplicate username = %d; want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateUserInvalidRole422(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p1", "role": "superuser",
	})
	if resp.StatusCode != 422 {
		t.Fatalf("invalid role = %d; want 422", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateUserMemberForbidden(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "eve", "password": "pass",
	})
	if resp.StatusCode != 403 {
		t.Fatalf("member create user = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateUserRollsBackOnHostFailure(t *testing.T) {
	// Spin up a harness where set-password always 500s.
	h := newHarnessWithBrokenSetPassword(t)
	h.setupAdminDirect("alice")
	// setupAdminDirect seeds the password as a placeholder; elevate uses
	// verify-password which works in this harness (only set-password is broken).
	// We need to seed a real bcrypt hash so verify-password succeeds.
	hash, _ := bcrypt.GenerateFromPassword([]byte("alicepass"), bcrypt.MinCost)
	h.pmu.Lock()
	h.pwds["alice"] = hash
	h.pmu.Unlock()
	h.elevate("alicepass")

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p",
	})
	if resp.StatusCode != 502 {
		t.Fatalf("create with broken host = %d; want 502", resp.StatusCode)
	}
	resp.Body.Close()

	users, err := h.st.ListUsers()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, u := range users {
		if u.Username == "bob" {
			t.Fatal("bob user row survived host failure; rollback broken")
		}
	}
	// Best-effort host cleanup: covers the useradd-succeeded-then-chpasswd-
	// failed sliver inside UpsertPassword
	// (docs/progress/0017-host-agent-delete-user.md).
	h.pmu.Lock()
	got := append([]string(nil), (*h.deleteCalls)...)
	h.pmu.Unlock()
	if len(got) != 1 || got[0] != "bob" {
		t.Fatalf("rollback did not call host.DeleteUser(%q) exactly once; got %v", "bob", got)
	}
}

// --- Update role (PATCH /api/v1/users/:id) ---

func TestUpdateRoleHappyPath(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "bobpass", "role": "member",
	}).Body.Close()

	bob, _ := h.st.GetUserByUsername("bob")

	resp := h.do("PATCH", fmt.Sprintf("/api/v1/users/%s", bob.ID), map[string]string{
		"role": "admin",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("promote bob = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[UserDTO](t, resp)
	if body.Role != store.RoleAdmin {
		t.Fatalf("promoted role = %q; want admin", body.Role)
	}

	// Verify in the store too.
	updated, _ := h.st.GetUser(bob.ID)
	if updated.Role != store.RoleAdmin {
		t.Fatalf("store role = %q; want admin", updated.Role)
	}
}

func TestUpdateRoleLastAdminGuard(t *testing.T) {
	h := newHarness(t)
	alice := h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("PATCH", fmt.Sprintf("/api/v1/users/%s", alice.ID), map[string]string{
		"role": "member",
	})
	if resp.StatusCode != 409 {
		t.Fatalf("demote last admin = %d; want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUpdateRoleNoSelfDemote(t *testing.T) {
	h := newHarness(t)
	alice := h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	// Add a second admin so the last-admin guard doesn't fire.
	h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p", "role": "admin",
	}).Body.Close()

	resp := h.do("PATCH", fmt.Sprintf("/api/v1/users/%s", alice.ID), map[string]string{
		"role": "member",
	})
	if resp.StatusCode != 409 {
		t.Fatalf("self-demote = %d; want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUpdateRoleMemberForbidden(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	alice, _ := h.st.GetUserByUsername("alice")
	resp := h.do("PATCH", fmt.Sprintf("/api/v1/users/%s", alice.ID), map[string]string{
		"role": "member",
	})
	if resp.StatusCode != 403 {
		t.Fatalf("member patch = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUpdateRoleNotFound(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("PATCH", "/api/v1/users/u_ghost", map[string]string{"role": "member"})
	if resp.StatusCode != 404 {
		t.Fatalf("patch ghost = %d; want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Delete user ---

func TestDeleteUserHappyPath(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p",
	})
	bob := decodeJSON[UserDTO](t, resp)

	resp = h.do("DELETE", fmt.Sprintf("/api/v1/users/%s", bob.ID), nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete bob = %d; want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify gone from store.
	if _, err := h.st.GetUser(bob.ID); err != store.ErrNotFound {
		t.Fatalf("user still in store after delete: %v", err)
	}
}

func TestDeleteUserLastAdminGuard(t *testing.T) {
	h := newHarness(t)
	alice := h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("DELETE", fmt.Sprintf("/api/v1/users/%s", alice.ID), nil)
	if resp.StatusCode != 409 {
		t.Fatalf("delete last admin = %d; want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDeleteUserNoSelfDelete(t *testing.T) {
	h := newHarness(t)
	alice := h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	// Add second admin so last-admin guard doesn't fire.
	h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p", "role": "admin",
	}).Body.Close()

	resp := h.do("DELETE", fmt.Sprintf("/api/v1/users/%s", alice.ID), nil)
	if resp.StatusCode != 409 {
		t.Fatalf("self-delete = %d; want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDeleteUserMemberForbidden(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	alice, _ := h.st.GetUserByUsername("alice")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("DELETE", fmt.Sprintf("/api/v1/users/%s", alice.ID), nil)
	if resp.StatusCode != 403 {
		t.Fatalf("member delete = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDeleteUserNotFound(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("DELETE", "/api/v1/users/u_ghost", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("delete ghost = %d; want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Reset user password (admin) ---

func TestResetUserPasswordHappyPath(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "oldpass",
	})
	bob := decodeJSON[UserDTO](t, resp)

	resp = h.do("POST", fmt.Sprintf("/api/v1/users/%s/password", bob.ID), map[string]string{
		"password": "newpass",
	})
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		t.Fatalf("reset password = %d; want 200 or 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Old password should no longer work.
	jar, _ := newJar()
	h.jar = jar
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "bob", "password": "oldpass",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("old password still works: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// New password works.
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "bob", "password": "newpass",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("new password: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestResetUserPasswordNotFound(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	resp := h.do("POST", "/api/v1/users/u_does_not_exist/password", map[string]string{
		"password": "anything",
	})
	if resp.StatusCode != 404 {
		t.Fatalf("reset on unknown id = %d; want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestResetUserPasswordMemberForbidden(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	alice, _ := h.st.GetUserByUsername("alice")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("POST", fmt.Sprintf("/api/v1/users/%s/password", alice.ID), map[string]string{
		"password": "newpass",
	})
	if resp.StatusCode != 403 {
		t.Fatalf("member reset = %d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Self-service password change ---

func TestChangeMyPasswordHappyPath(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	resp := h.do("POST", "/api/v1/me/password", map[string]string{
		"current_password": "hunter2", "new_password": "newpass",
	})
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		t.Fatalf("change password = %d; want 200 or 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Old password no longer works.
	jar, _ := newJar()
	h.jar = jar
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "alice", "password": "hunter2",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("old password still works: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "alice", "password": "newpass",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("new password: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestChangeMyPasswordWrongCurrent401(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")

	resp := h.do("POST", "/api/v1/me/password", map[string]string{
		"current_password": "wrongpass", "new_password": "newpass",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("wrong current password = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestChangeMyPasswordRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "hunter2")
	jar, _ := newJar()
	h.jar = jar

	resp := h.do("POST", "/api/v1/me/password", map[string]string{
		"current_password": "hunter2", "new_password": "x",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated change = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestChangeMyPasswordAuditsFailure(t *testing.T) {
	h := newHarness(t)
	alice := h.setupAdmin("alice", "hunter2")

	h.do("POST", "/api/v1/me/password", map[string]string{
		"current_password": "wrong", "new_password": "x",
	}).Body.Close()

	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Action == "user.password.change" && e.TargetID == alice.ID && !e.Success {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("audit failure event for wrong password not found")
	}
}

// --- Audit assertions for user.create and user.delete ---

func TestCreateUserAuditEvent(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "p",
	})
	bob := decodeJSON[UserDTO](t, resp)

	events, _ := h.st.ListAuditEvents(store.AuditFilter{Limit: 50})
	var found bool
	for _, e := range events {
		if e.Action == "user.create" && e.TargetID == bob.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("user.create audit event not found")
	}
}

func TestDeleteUserAuditEvent(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	resp := h.do("POST", "/api/v1/users", map[string]string{"username": "bob", "password": "p"})
	bob := decodeJSON[UserDTO](t, resp)

	h.do("DELETE", fmt.Sprintf("/api/v1/users/%s", bob.ID), nil).Body.Close()

	events, _ := h.st.ListAuditEvents(store.AuditFilter{Limit: 50})
	var found bool
	for _, e := range events {
		if e.Action == "user.delete" && e.TargetID == bob.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("user.delete audit event not found")
	}
}

func TestUpdateRoleRollsBackOnHostFailure(t *testing.T) {
	h := newHarnessWithBrokenSetRole(t)
	h.setupAdminDirect("alice")
	// Seed a real password so elevate's verify-password succeeds.
	hash, _ := bcrypt.GenerateFromPassword([]byte("alicepass"), bcrypt.MinCost)
	h.pmu.Lock()
	h.pwds["alice"] = hash
	h.pmu.Unlock()
	h.elevate("alicepass")

	bob := store.User{
		ID: "u_bob", Username: "bob", Role: store.RoleMember, CreatedAt: time.Now(),
	}
	if err := h.st.CreateUser(bob); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	resp := h.do("PATCH", "/api/v1/users/u_bob", map[string]string{"role": "admin"})
	if resp.StatusCode != 502 {
		t.Fatalf("patch with broken host = %d; want 502", resp.StatusCode)
	}
	resp.Body.Close()

	updated, err := h.st.GetUser("u_bob")
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if updated.Role != store.RoleMember {
		t.Fatalf("bob role after rollback = %q; want member (rollback broken)", updated.Role)
	}
}

// TestCreateUserRollsBackOnSetRoleFailure: SetPassword succeeds but SetRole
// 500s. createUser must roll the brain row back so admins can retry.
func TestCreateUserRollsBackOnSetRoleFailure(t *testing.T) {
	h := newHarnessWithBrokenSetRole(t)
	h.setupAdminDirect("alice")
	// Seed a real password so elevate's verify-password succeeds.
	hash, _ := bcrypt.GenerateFromPassword([]byte("alicepass"), bcrypt.MinCost)
	h.pmu.Lock()
	h.pwds["alice"] = hash
	h.pmu.Unlock()
	h.elevate("alicepass")

	resp := h.do("POST", "/api/v1/users", map[string]string{
		"username": "bob", "password": "bobpass", "role": "member",
	})
	if resp.StatusCode != 502 {
		t.Fatalf("createUser with broken set-role = %d; want 502", resp.StatusCode)
	}
	resp.Body.Close()

	// Brain row must be gone.
	users, err := h.st.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.Username == "bob" {
			t.Fatal("bob row survived set-role failure; rollback broken")
		}
	}
	// Best-effort host cleanup: SetPassword already created the Linux
	// account, so without this call the user would be orphaned on the host
	// (docs/progress/0017-host-agent-delete-user.md).
	h.pmu.Lock()
	got := append([]string(nil), (*h.deleteCalls)...)
	h.pmu.Unlock()
	if len(got) != 1 || got[0] != "bob" {
		t.Fatalf("rollback did not call host.DeleteUser(%q) exactly once; got %v", "bob", got)
	}
}

// --- harness helpers for broken-host tests ---

func newHarnessWithBrokenSetPassword(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pwds := map[string][]byte{}
	var pmu sync.Mutex
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	// set-password 500s for non-"alice" users (alice is seeded directly).
	mux.HandleFunc("POST /v1/auth/set-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.User == "alice" {
			h, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.MinCost)
			pmu.Lock()
			pwds[req.User] = h
			pmu.Unlock()
			_ = json.NewEncoder(w).Encode(struct{}{})
			return
		}
		http.Error(w, `{"code":"boom","message":"nope"}`, 500)
	})
	mux.HandleFunc("POST /v1/auth/verify-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.VerifyPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		h, ok := pwds[req.User]
		pmu.Unlock()
		valid := ok && bcrypt.CompareHashAndPassword(h, []byte(req.Password)) == nil
		_ = json.NewEncoder(w).Encode(protocol.VerifyPasswordResponse{Valid: valid})
	})
	mux.HandleFunc("POST /v1/auth/set-role", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	deleteCalls := []string{}
	mux.HandleFunc("POST /v1/auth/delete-user", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.DeleteUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		deleteCalls = append(deleteCalls, req.User)
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})

	hostHTTP := &http.Server{Handler: mux}
	go func() { _ = hostHTTP.Serve(ln) }()
	t.Cleanup(func() { _ = hostHTTP.Close() })

	host := hostclient.New(sock)
	cat := catalog.New(t.TempDir())
	bus := events.NewBus()
	authMgr := auth.NewManager(st)
	srv := NewServer(st, cat, nil, bus, authMgr, host, audit.New(st), nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := newJar()
	return &harness{srv: ts, jar: jar, t: t, pwds: pwds, pmu: &pmu, st: st, deleteCalls: &deleteCalls}
}

// newHarnessWithBrokenSetRole returns a harness whose host-agent always 500s
// on /v1/auth/set-role. set-password works normally so setupAdminDirect's seed
// path stays functional.
func newHarnessWithBrokenSetRole(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pwds := map[string][]byte{}
	var pmu sync.Mutex
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/set-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.MinCost)
		pmu.Lock()
		pwds[req.User] = h
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	mux.HandleFunc("POST /v1/auth/verify-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.VerifyPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		h, ok := pwds[req.User]
		pmu.Unlock()
		valid := ok && bcrypt.CompareHashAndPassword(h, []byte(req.Password)) == nil
		_ = json.NewEncoder(w).Encode(protocol.VerifyPasswordResponse{Valid: valid})
	})
	mux.HandleFunc("POST /v1/auth/set-role", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"boom","message":"nope"}`, 500)
	})
	deleteCalls := []string{}
	mux.HandleFunc("POST /v1/auth/delete-user", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.DeleteUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		deleteCalls = append(deleteCalls, req.User)
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})

	hostHTTP := &http.Server{Handler: mux}
	go func() { _ = hostHTTP.Serve(ln) }()
	t.Cleanup(func() { _ = hostHTTP.Close() })

	host := hostclient.New(sock)
	cat := catalog.New(t.TempDir())
	bus := events.NewBus()
	authMgr := auth.NewManager(st)
	srv := NewServer(st, cat, nil, bus, authMgr, host, audit.New(st), nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := newJar()
	return &harness{srv: ts, jar: jar, t: t, pwds: pwds, pmu: &pmu, st: st, deleteCalls: &deleteCalls}
}

// setupAdminDirect inserts an admin row directly into the store (bypassing
// the broken host) so we have an authenticated session for subsequent calls.
func (h *harness) setupAdminDirect(username string) {
	h.t.Helper()
	u := store.User{
		ID: "u_direct", Username: username, Role: store.RoleAdmin,
		CreatedAt: time.Now(),
	}
	if err := h.st.CreateFirstAdmin(u); err != nil {
		h.t.Fatalf("createFirstAdmin direct: %v", err)
	}
	// Seed the in-memory password so the harness set-password handler has it.
	h.pmu.Lock()
	h.pwds[username] = []byte("alice-bcrypt-placeholder")
	h.pmu.Unlock()
	// Issue a session directly via store.
	authMgr := auth.NewManager(h.st)
	sess, err := authMgr.Issue(u.ID)
	if err != nil {
		h.t.Fatalf("issue session: %v", err)
	}
	c := authMgr.Cookie(sess.Token)
	// Inject the cookie into the jar.
	u2, _ := http.NewRequest("GET", h.srv.URL+"/", nil)
	h.jar.SetCookies(u2.URL, []*http.Cookie{c})
}
