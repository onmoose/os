package hostagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/onmoose/os/internal/hostagent/netstate"
	"github.com/onmoose/os/internal/protocol"
)

// --- stub verifier ---

type stubVerifier struct {
	valid bool
	err   error
}

func (s *stubVerifier) Verify(_, _ string) (bool, error) {
	return s.valid, s.err
}

// --- helpers ---

func newTestAgent(v PasswordVerifier) (*Agent, *http.ServeMux) {
	a := New(v, NewFakePublisher(".local"))
	mux := http.NewServeMux()
	a.Mount(mux)
	return a, mux
}

func post(t *testing.T, mux *http.ServeMux, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(w.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

// --- verify-password tests ---

func TestVerifyPasswordDelegatesToVerifier_HappyPath(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{valid: true})
	w := post(t, mux, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "alice", Password: "correct",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.VerifyPasswordResponse](t, w)
	if !resp.Valid {
		t.Error("want valid=true")
	}
}

func TestVerifyPasswordDelegatesToVerifier_WrongCredentials(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{valid: false})
	w := post(t, mux, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "alice", Password: "wrong",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.VerifyPasswordResponse](t, w)
	if resp.Valid {
		t.Error("want valid=false")
	}
}

// TestVerifyPasswordVerifierError checks that a transport/config error from the
// verifier returns {valid: false} (not 5xx) — per BRAIN_HOST_PROTOCOL.md:
// "never reveal why verification failed."
func TestVerifyPasswordVerifierError_ReturnsValidFalseNotFiveHundred(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{valid: false, err: errors.New("pam config broken")})
	w := post(t, mux, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "alice", Password: "anything",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (not 5xx), got %d", w.Code)
	}
	resp := decodeBody[protocol.VerifyPasswordResponse](t, w)
	if resp.Valid {
		t.Error("want valid=false on verifier error")
	}
}

// --- set-password / fake-map tests ---

func TestSetPasswordAndVerifyWithFakeVerifier(t *testing.T) {
	a := New(nil, NewFakePublisher(".local")) // verifier set after construction
	a.Verifier = NewFakeVerifier(a)
	mux := http.NewServeMux()
	a.Mount(mux)

	// Set a password.
	w := post(t, mux, "/v1/auth/set-password", protocol.SetPasswordRequest{
		User: "bob", Password: "s3cret",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("set-password: want 200, got %d", w.Code)
	}

	// Correct password → valid.
	w = post(t, mux, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "bob", Password: "s3cret",
	})
	resp := decodeBody[protocol.VerifyPasswordResponse](t, w)
	if !resp.Valid {
		t.Error("want valid=true after set-password")
	}

	// Wrong password → not valid.
	w = post(t, mux, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "bob", Password: "wrong",
	})
	resp = decodeBody[protocol.VerifyPasswordResponse](t, w)
	if resp.Valid {
		t.Error("want valid=false for wrong password")
	}
}

// --- stub user manager ---

type stubUserMgr struct {
	calls                  []struct{ user, password string }
	roleCalls              []struct{ user, role string }
	deleteCalls            []string
	resolveHomeCalls       []string
	existsCalls            []string
	existing               map[string]bool
	wellKnownIdentityCalls int
	allocateCalls          []string
	releaseCalls           []int
	err                    error
	roleErr                error
	deleteErr              error
	resolveHomeErr         error
	wellKnownIdentityErr   error
	allocateErr            error
	releaseErr             error
	// resolveHomeResult is returned on ResolveHome success; zero value = /home/<user>, 3000, 3000.
	resolveHomeResult *struct {
		home     string
		uid, gid int
	}
	// wellKnownIdentityResult is returned on WellKnownIdentity success; zero value = 2000, 2000, 2001.
	wellKnownIdentityResult *struct {
		appUID, appGID, sharedGID int
	}
}

func (s *stubUserMgr) UpsertPassword(user, password string) error {
	s.calls = append(s.calls, struct{ user, password string }{user, password})
	return s.err
}

func (s *stubUserMgr) UserExists(user string) (bool, error) {
	s.existsCalls = append(s.existsCalls, user)
	return s.existing[user], s.err
}

func (s *stubUserMgr) SetRole(user, role string) error {
	s.roleCalls = append(s.roleCalls, struct{ user, role string }{user, role})
	return s.roleErr
}

func (s *stubUserMgr) DeleteUser(user string) error {
	s.deleteCalls = append(s.deleteCalls, user)
	return s.deleteErr
}

func (s *stubUserMgr) ResolveHome(user string) (string, int, int, error) {
	s.resolveHomeCalls = append(s.resolveHomeCalls, user)
	if s.resolveHomeErr != nil {
		return "", 0, 0, s.resolveHomeErr
	}
	if s.resolveHomeResult != nil {
		return s.resolveHomeResult.home, s.resolveHomeResult.uid, s.resolveHomeResult.gid, nil
	}
	return "/home/" + user, 3000, 3000, nil
}

func (s *stubUserMgr) WellKnownIdentity() (int, int, int, error) {
	s.wellKnownIdentityCalls++
	if s.wellKnownIdentityErr != nil {
		return 0, 0, 0, s.wellKnownIdentityErr
	}
	if s.wellKnownIdentityResult != nil {
		return s.wellKnownIdentityResult.appUID, s.wellKnownIdentityResult.appGID, s.wellKnownIdentityResult.sharedGID, nil
	}
	return 2000, 2000, 2001, nil
}

func (s *stubUserMgr) AllocateAppService(instanceID string) (int, int, error) {
	s.allocateCalls = append(s.allocateCalls, instanceID)
	if s.allocateErr != nil {
		return 0, 0, s.allocateErr
	}
	return 2100, 2100, nil
}

func (s *stubUserMgr) ReleaseAppService(uid int) error {
	s.releaseCalls = append(s.releaseCalls, uid)
	return s.releaseErr
}

func TestSetPassword_DelegatesToUserMgrWhenSet(t *testing.T) {
	mgr := &stubUserMgr{}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/auth/set-password", protocol.SetPasswordRequest{
		User: "cindy", Password: "s3cret",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if len(mgr.calls) != 1 || mgr.calls[0].user != "cindy" || mgr.calls[0].password != "s3cret" {
		t.Errorf("UpsertPassword not called as expected: %+v", mgr.calls)
	}

	// In-memory bcrypt map must NOT be populated when UserMgr is wired —
	// /etc/shadow (via PAM) is the source of truth.
	a.mu.Lock()
	_, present := a.passwords["cindy"]
	a.mu.Unlock()
	if present {
		t.Error("in-memory bcrypt map written when UserMgr is wired; should be skipped")
	}
}

func TestSetPassword_UserMgrError_Returns500(t *testing.T) {
	mgr := &stubUserMgr{err: errors.New("useradd: group 'moose' does not exist")}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/auth/set-password", protocol.SetPasswordRequest{
		User: "cindy", Password: "pw",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	// Body must NOT leak the underlying system error.
	if bytes.Contains(w.Body.Bytes(), []byte("useradd")) || bytes.Contains(w.Body.Bytes(), []byte("moose")) {
		t.Errorf("response leaked system detail: %s", w.Body.String())
	}
}

func TestSetPasswordMissingFields(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := post(t, mux, "/v1/auth/set-password", protocol.SetPasswordRequest{
		User: "", Password: "",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// --- set-role tests ---

func TestSetRole_DelegatesToUserMgrWhenSet(t *testing.T) {
	mgr := &stubUserMgr{}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/auth/set-role", protocol.SetRoleRequest{
		User: "cindy", Role: "admin",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if len(mgr.roleCalls) != 1 || mgr.roleCalls[0].user != "cindy" || mgr.roleCalls[0].role != "admin" {
		t.Errorf("SetRole not called as expected: %+v", mgr.roleCalls)
	}

	// In-memory roles map must NOT be populated when UserMgr is wired.
	a.mu.Lock()
	_, present := a.roles["cindy"]
	a.mu.Unlock()
	if present {
		t.Error("in-memory roles map written when UserMgr is wired; should be skipped")
	}
}

func TestSetRole_UserMgrError_Returns500(t *testing.T) {
	mgr := &stubUserMgr{roleErr: errors.New("gpasswd: group sudo does not exist")}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/auth/set-role", protocol.SetRoleRequest{
		User: "cindy", Role: "admin",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("gpasswd")) || bytes.Contains(w.Body.Bytes(), []byte("sudo")) {
		t.Errorf("response leaked system detail: %s", w.Body.String())
	}
}

func TestSetRole_HappyPath(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := post(t, mux, "/v1/auth/set-role", protocol.SetRoleRequest{
		User: "carol", Role: "admin",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestSetRole_InvalidRole(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := post(t, mux, "/v1/auth/set-role", protocol.SetRoleRequest{
		User: "carol", Role: "superuser",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// --- delete-user tests ---

func TestDeleteUser_RemovesFromFakeMap(t *testing.T) {
	a := New(nil, NewFakePublisher(".local"))
	a.Verifier = NewFakeVerifier(a)
	mux := http.NewServeMux()
	a.Mount(mux)

	// Seed a user.
	post(t, mux, "/v1/auth/set-password", protocol.SetPasswordRequest{
		User: "dave", Password: "pw",
	})

	// Delete.
	w := post(t, mux, "/v1/auth/delete-user", protocol.DeleteUserRequest{User: "dave"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	// Verify no longer valid.
	w = post(t, mux, "/v1/auth/verify-password", protocol.VerifyPasswordRequest{
		User: "dave", Password: "pw",
	})
	resp := decodeBody[protocol.VerifyPasswordResponse](t, w)
	if resp.Valid {
		t.Error("want valid=false after delete-user")
	}
}

func TestDeleteUser_IdempotentOnUnknownUser(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := post(t, mux, "/v1/auth/delete-user", protocol.DeleteUserRequest{User: "nobody"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 (idempotent), got %d", w.Code)
	}
}

func TestDeleteUser_EmptyUser_Returns400(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := post(t, mux, "/v1/auth/delete-user", protocol.DeleteUserRequest{User: ""})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestDeleteUser_DelegatesToUserMgrWhenSet(t *testing.T) {
	mgr := &stubUserMgr{}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	// Seed the in-memory map to prove the wired path does NOT touch it.
	a.passwords["cindy"] = []byte("seed")
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/auth/delete-user", protocol.DeleteUserRequest{User: "cindy"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if len(mgr.deleteCalls) != 1 || mgr.deleteCalls[0] != "cindy" {
		t.Errorf("DeleteUser not called as expected: %+v", mgr.deleteCalls)
	}
	a.mu.Lock()
	_, present := a.passwords["cindy"]
	a.mu.Unlock()
	if !present {
		t.Error("in-memory passwords map was modified when UserMgr is wired; should be skipped")
	}
}

func TestDeleteUser_UserMgrError_Returns500(t *testing.T) {
	mgr := &stubUserMgr{deleteErr: errors.New("userdel: user 'cindy' is currently used by process 4242")}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/auth/delete-user", protocol.DeleteUserRequest{User: "cindy"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("userdel")) || bytes.Contains(w.Body.Bytes(), []byte("4242")) {
		t.Errorf("response leaked system detail: %s", w.Body.String())
	}
}

// --- resolve-home tests ---

// The fake branch resolves to the dev operator's *own* uid/gid + home dir (not a
// synthetic fakeUID + /home/<user>), so the unprivileged dev brain — running as
// the same operator — owns every bind dir it creates and Part A's chowns are
// no-op successes (#147).
func TestResolveHome_FakeBranch_ReturnsOperatorIdentity(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	w := get(t, mux, "/v1/users/alice/home")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.ResolveHomeResponse](t, w)
	wantHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if resp.HomePath != wantHome {
		t.Errorf("home_path: want operator home %q, got %q", wantHome, resp.HomePath)
	}
	if resp.UID != os.Getuid() || resp.GID != os.Getgid() {
		t.Errorf("identity: want operator %d:%d, got %d:%d",
			os.Getuid(), os.Getgid(), resp.UID, resp.GID)
	}
}

func TestResolveHome_FakeBranch_StableAcrossCalls(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	w1 := get(t, mux, "/v1/users/bob/home")
	w2 := get(t, mux, "/v1/users/bob/home")
	r1 := decodeBody[protocol.ResolveHomeResponse](t, w1)
	r2 := decodeBody[protocol.ResolveHomeResponse](t, w2)
	if r1.UID != r2.UID || r1.HomePath != r2.HomePath {
		t.Errorf("fake result not stable: first=%+v second=%+v", r1, r2)
	}
}

func TestResolveHome_DelegatesToUserMgrWhenSet(t *testing.T) {
	mgr := &stubUserMgr{}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := get(t, mux, "/v1/users/cindy/home")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.ResolveHomeResponse](t, w)
	if resp.HomePath != "/home/cindy" {
		t.Errorf("home_path: want /home/cindy, got %q", resp.HomePath)
	}
	if len(mgr.resolveHomeCalls) != 1 || mgr.resolveHomeCalls[0] != "cindy" {
		t.Errorf("ResolveHome not called as expected: %v", mgr.resolveHomeCalls)
	}
}

func TestResolveHome_UnknownUser_Returns404(t *testing.T) {
	mgr := &stubUserMgr{resolveHomeErr: ErrUnknownUser}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := get(t, mux, "/v1/users/ghost/home")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
	resp := decodeBody[protocol.Error](t, w)
	if resp.Code != "unknown-user" {
		t.Errorf("code: want unknown-user, got %q", resp.Code)
	}
}

func TestResolveHome_UserMgrError_Returns500(t *testing.T) {
	mgr := &stubUserMgr{resolveHomeErr: errors.New("nss lookup failed")}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := get(t, mux, "/v1/users/dave/home")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("nss lookup")) {
		t.Errorf("response leaked system detail: %s", w.Body.String())
	}
}

// --- stub publisher ---

type stubPublisher struct {
	publishErr   error
	unpublishErr error
	published    []string
}

func (s *stubPublisher) Publish(slug string) (string, error) {
	if s.publishErr != nil {
		return "", s.publishErr
	}
	s.published = append(s.published, slug)
	return slug + ".local", nil
}

func (s *stubPublisher) Unpublish(slug string) error {
	return s.unpublishErr
}

// --- discovery / system tests ---

func TestSystemStatus_ReturnsOK(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := get(t, mux, "/v1/system/status")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var s protocol.SystemStatus
	if err := json.NewDecoder(w.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Hostname == "" {
		t.Error("want non-empty hostname")
	}
	// With no DiskSpace reporter wired, Disks is an empty (non-nil) slice — the
	// brain reads it as "no Storage section", never phantom bars.
	if s.Disks == nil {
		t.Error("want non-nil empty disks slice, got nil")
	}
	if len(s.Disks) != 0 {
		t.Errorf("want no disks without a reporter, got %+v", s.Disks)
	}
}

func TestSystemStatus_DisksFromReporter(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	a.Disk = NewFakeDiskReporter(412<<30, 1<<40) // the data-drive (DataDisk*) seam
	a.DiskSpace = NewFakeDiskSpaceReporter(
		protocol.DiskSpace{Label: "System", FreeBytes: 18 << 30, TotalBytes: 64 << 30},
		protocol.DiskSpace{Label: "Data", FreeBytes: 412 << 30, TotalBytes: 1 << 40},
	)
	w := get(t, mux, "/v1/system/status")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	s := decodeBody[protocol.SystemStatus](t, w)
	if s.DataDiskFreeBytes != 412<<30 {
		t.Errorf("DataDiskFreeBytes: got %d", s.DataDiskFreeBytes)
	}
	if len(s.Disks) != 2 {
		t.Fatalf("want 2 disks, got %+v", s.Disks)
	}
	if s.Disks[0].Label != "System" || s.Disks[0].TotalBytes != 64<<30 {
		t.Errorf("System entry: got %+v", s.Disks[0])
	}
	if s.Disks[1].Label != "Data" || s.Disks[1].FreeBytes != 412<<30 {
		t.Errorf("Data entry: got %+v", s.Disks[1])
	}
}

func TestSystemResources_ReturnsAllowlistedSample(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := get(t, mux, "/v1/system/resources")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	s := decodeBody[protocol.SystemResources](t, w)

	if s.TsNs <= 0 {
		t.Errorf("ts_ns must be a positive monotonic timestamp, got %d", s.TsNs)
	}
	if s.Mem.TotalBytes <= 0 || s.Mem.UsedBytes <= 0 {
		t.Errorf("mem levels must be populated, got %+v", s.Mem)
	}
	// The allowlist is host-agent's job: the fake reports one physical NIC and
	// one whole-disk device, never lo/docker0/veth* or a partition.
	if len(s.Net) != 1 || s.Net[0].Iface != "eth0" {
		t.Errorf("net: want one allowlisted iface eth0, got %+v", s.Net)
	}
	if len(s.Disk) != 1 || s.Disk[0].Dev != "sda" {
		t.Errorf("disk: want one whole-disk device sda, got %+v", s.Disk)
	}
}

func TestDiscoveryState_InterfacesEmptyWithoutProvider(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	s := decodeBody[protocol.DiscoveryState](t, get(t, mux, "/v1/discovery/state"))
	// Empty list, not null: "not measured" must still be valid JSON for the
	// brain's decoder.
	if s.Interfaces == nil || len(s.Interfaces) != 0 {
		t.Errorf("want empty interfaces without a provider, got %#v", s.Interfaces)
	}
}

func TestDiscoveryState_InterfacesFromNetState(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	a.Net = NewFakeNetState(
		netstate.LANInterface{Name: "eno1", Index: 2, IPv4: "192.168.2.160"},
		netstate.LANInterface{Name: "wlp2s0", Index: 3, IPv4: "192.168.2.161"},
	)
	s := decodeBody[protocol.DiscoveryState](t, get(t, mux, "/v1/discovery/state"))
	if len(s.Interfaces) != 2 || s.Interfaces[0] != "eno1" || s.Interfaces[1] != "wlp2s0" {
		t.Errorf("want [eno1 wlp2s0], got %v", s.Interfaces)
	}
}

// The brain diffs successive samples, so two reads must not go backwards in time
// (a negative ts_ns delta would null every rate).
func TestSystemResources_TimestampIsMonotonic(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	first := decodeBody[protocol.SystemResources](t, get(t, mux, "/v1/system/resources"))
	second := decodeBody[protocol.SystemResources](t, get(t, mux, "/v1/system/resources"))
	if second.TsNs < first.TsNs {
		t.Errorf("ts_ns went backwards: first=%d second=%d", first.TsNs, second.TsNs)
	}
}

// --- system gpu ---

// No reporter wired → present: false, still a clean 200. "No detector" means
// "no usable GPU", which the brain turns into the gpu: true install refusal.
func TestSystemGPU_NoReporter_ReportsNoGPU(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := get(t, mux, "/v1/system/gpu")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	g := decodeBody[protocol.SystemGPU](t, w)
	if g.Present || g.Vendor != "" || g.RenderGID != 0 {
		t.Errorf("want zero no-GPU report with no reporter wired, got %+v", g)
	}
}

func TestSystemGPU_DelegatesToReporter(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	fake := NewFakeGPUReporter(protocol.SystemGPU{Present: true, Vendor: "intel", RenderGID: 104})
	a.GPU = fake

	g := decodeBody[protocol.SystemGPU](t, get(t, mux, "/v1/system/gpu"))
	if !g.Present || g.Vendor != "intel" || g.RenderGID != 104 {
		t.Errorf("want synthetic intel iGPU report, got %+v", g)
	}

	// The toggle the capacity-refusal path tests against.
	fake.Set(protocol.SystemGPU{})
	g = decodeBody[protocol.SystemGPU](t, get(t, mux, "/v1/system/gpu"))
	if g.Present {
		t.Errorf("want present: false after Set to no-GPU, got %+v", g)
	}
}

// --- update-target read (#443) ---

// With no reporter wired the endpoint says "not measured", never "you are up to
// date". A 501 or an error would be worse: the brain would have nothing to show
// and no state to show it in.
func TestSystemUpdateTarget_NoReporter(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	w := get(t, mux, "/v1/system/update-target")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := decodeBody[protocol.UpdateTarget](t, w); got.State != protocol.UpdateTargetUnknown {
		t.Errorf("state = %q, want %q", got.State, protocol.UpdateTargetUnknown)
	}
}

func TestSystemUpdateTarget_DelegatesToReporter(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	fake := NewFakeUpdateTargetReporter(protocol.UpdateTarget{
		State:   protocol.UpdateTargetAvailable,
		Running: protocol.ControlPlanePair{Brain: "ghcr.io/onmoose/brain@sha256:aa", UI: "ghcr.io/onmoose/ui@sha256:bb"},
		Target:  &protocol.ControlPlaneOffer{Version: "v0.8.0", BrainImage: "ghcr.io/onmoose/brain@sha256:cc", UIImage: "ghcr.io/onmoose/ui@sha256:dd"},
	})
	a.UpdateTarget = fake

	got := decodeBody[protocol.UpdateTarget](t, get(t, mux, "/v1/system/update-target"))
	if got.State != protocol.UpdateTargetAvailable {
		t.Fatalf("state = %q, want %q", got.State, protocol.UpdateTargetAvailable)
	}
	if got.Target == nil || got.Target.Version != "v0.8.0" {
		t.Errorf("target = %+v, want the offered v0.8.0", got.Target)
	}

	// The states the dashboard has to render are all reachable through the same
	// read, including the one that needs a human.
	fake.Set(protocol.UpdateTarget{State: protocol.UpdateTargetDisabled, Detail: "seed update_target_url has no host"})
	got = decodeBody[protocol.UpdateTarget](t, get(t, mux, "/v1/system/update-target"))
	if got.State != protocol.UpdateTargetDisabled || got.Detail == "" {
		t.Errorf("got %+v, want the disabled state with a reason", got)
	}
}

// --- system-resources sampler seam ---

type stubSampler struct {
	res protocol.SystemResources
	err error
}

func (s *stubSampler) Sample() (protocol.SystemResources, error) { return s.res, s.err }

// When a System sampler is wired (cmd/host-agent-real injects
// procsource.Sampler), the handler serves its snapshot verbatim instead of the
// synthetic counters.
func TestSystemResources_DelegatesToSampler(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	a.System = &stubSampler{res: protocol.SystemResources{
		TsNs:    42,
		CPU:     protocol.CPUCounters{TotalJiffies: 1000, IdleJiffies: 800},
		LoadAvg: [3]float64{1.5, 1.0, 0.5},
		Mem:     protocol.MemCounters{TotalBytes: 100, AvailableBytes: 60, UsedBytes: 40},
		Net:     []protocol.NetCounters{{Iface: "enp3s0", RxBytes: 7, TxBytes: 9}},
		Disk:    []protocol.DiskCounters{{Dev: "nvme0n1", ReadBytes: 512, WriteBytes: 1024}},
		UptimeS: 84021,
	}}
	w := get(t, mux, "/v1/system/resources")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	s := decodeBody[protocol.SystemResources](t, w)
	if s.TsNs != 42 || s.CPU.TotalJiffies != 1000 || s.UptimeS != 84021 {
		t.Errorf("sampler snapshot not served verbatim: %+v", s)
	}
	if len(s.Net) != 1 || s.Net[0].Iface != "enp3s0" || len(s.Disk) != 1 || s.Disk[0].Dev != "nvme0n1" {
		t.Errorf("net/disk not served verbatim: net=%+v disk=%+v", s.Net, s.Disk)
	}
}

// A sampler error is a 500 the brain's poller logs and skips (keeping its
// previous rate baseline) — never a silent fall-through to synthetic counters,
// which would corrupt the rate diff with a fake baseline.
func TestSystemResources_SamplerError_Returns500(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	a.System = &stubSampler{err: errors.New("proc unreadable")}
	w := get(t, mux, "/v1/system/resources")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "sample-failed" {
		t.Errorf("code = %q, want sample-failed", resp.Code)
	}
}

func TestPublishUnpublish_RoundTrip(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	w := post(t, mux, "/v1/discovery/publish", protocol.PublishRequest{Slug: "whoami"})
	if w.Code != http.StatusOK {
		t.Fatalf("publish: want 200, got %d", w.Code)
	}
	pr := decodeBody[protocol.PublishResponse](t, w)
	if pr.Name != "whoami.local" {
		t.Errorf("want whoami.local, got %q", pr.Name)
	}

	w = post(t, mux, "/v1/discovery/unpublish", protocol.UnpublishRequest{Slug: "whoami"})
	if w.Code != http.StatusOK {
		t.Fatalf("unpublish: want 200, got %d", w.Code)
	}
}

// TestPublish_DelegatesToPublisher verifies that the publish handler calls the
// injected Publisher and surfaces errors as 500.
func TestPublish_DelegatesToPublisher(t *testing.T) {
	pub := &stubPublisher{}
	a := New(&stubVerifier{}, pub)
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/discovery/publish", protocol.PublishRequest{Slug: "photos"})
	if w.Code != http.StatusOK {
		t.Fatalf("publish: want 200, got %d", w.Code)
	}
	pr := decodeBody[protocol.PublishResponse](t, w)
	if pr.Name != "photos.local" {
		t.Errorf("name: want photos.local, got %q", pr.Name)
	}
	if pr.State != "established" {
		t.Errorf("state: want established, got %q", pr.State)
	}
	if len(pub.published) != 1 || pub.published[0] != "photos" {
		t.Errorf("publisher not called with slug: %v", pub.published)
	}
}

// TestPublish_PublisherError_Returns500 verifies that a Publisher failure
// returns 500 rather than silently succeeding.
func TestPublish_PublisherError_Returns500(t *testing.T) {
	pub := &stubPublisher{publishErr: errors.New("disk full")}
	a := New(&stubVerifier{}, pub)
	mux := http.NewServeMux()
	a.Mount(mux)

	w := post(t, mux, "/v1/discovery/publish", protocol.PublishRequest{Slug: "notes"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("publish with broken publisher: want 500, got %d", w.Code)
	}
}

// TestFakePublisher_MatchesCurrentBehavior verifies FakePublisher returns the
// expected name and doesn't error on valid slugs or Unpublish.
func TestFakePublisher_MatchesCurrentBehavior(t *testing.T) {
	fp := NewFakePublisher(".local")

	name, err := fp.Publish("whoami")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if name != "whoami.local" {
		t.Errorf("name: want whoami.local, got %q", name)
	}

	if err := fp.Unpublish("whoami"); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
}

// --- /v1/health/system ---

// TestSystemHealth_NoSourcesReportsStorageOnly verifies the locus-B report
// contract with nothing wired: the storage category is always present and
// non-nil ("storage looks healthy" per BOOT.md), and the services category is
// absent — so the brain reads "services not measured" rather than "all up" —
// all behind a parseable 200 the poll loop never has to retry.
func TestSystemHealth_NoSourcesReportsStorageOnly(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	w := get(t, mux, "/v1/health/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	sh := decodeBody[protocol.SystemHealth](t, w)
	storage, ok := sh.Categories[protocol.HealthCategoryStorage]
	if !ok {
		t.Fatal("storage category must always be present")
	}
	if storage == nil {
		t.Error("storage findings must be a non-nil slice (empty is fine)")
	}
	if len(storage) != 0 {
		t.Errorf("storage: want empty, got %v", storage)
	}
	if _, present := sh.Categories[protocol.HealthCategoryServices]; present {
		t.Error("services category must be absent when no reporter is wired")
	}
	if _, present := sh.Categories[protocol.HealthCategoryTime]; present {
		t.Error("time category must be absent when no reporter is wired")
	}
	if _, present := sh.Categories[protocol.HealthCategoryResources]; present {
		t.Error("resources category must be absent when no reporter is wired")
	}
	if _, present := sh.Categories[protocol.HealthCategorySystem]; present {
		t.Error("system category must be absent when no reporter is wired")
	}
	if sh.CheckedAt == "" {
		t.Error("checked_at must be set even with no sources")
	}
}

// TestSystemHealth_StorageFromFakeSource verifies storage findings flow through
// the storage category verbatim.
func TestSystemHealth_StorageFromFakeSource(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	src := NewFakeHealthSource()
	src.Set([]protocol.Finding{
		{ID: "data-drive-missing", Details: "enrolled abc-123 not attached"},
	})
	a.Health = src

	w := get(t, mux, "/v1/health/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	sh := decodeBody[protocol.SystemHealth](t, w)
	storage := sh.Categories[protocol.HealthCategoryStorage]
	if len(storage) != 1 || storage[0].ID != "data-drive-missing" {
		t.Fatalf("storage category: want data-drive-missing, got %v", storage)
	}
}

// TestSystemHealth_ServicesFromReporter verifies the services category appears
// once a ServiceReporter is wired and carries its findings (per-unit
// instance_key) verbatim.
func TestSystemHealth_ServicesFromReporter(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	svc := NewFakeServiceReporter()
	svc.Set([]protocol.Finding{
		{ID: "service-down", InstanceKey: "docker.service", Details: "docker.service is failed"},
	})
	a.Services = svc

	w := get(t, mux, "/v1/health/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	sh := decodeBody[protocol.SystemHealth](t, w)
	services, ok := sh.Categories[protocol.HealthCategoryServices]
	if !ok {
		t.Fatal("services category must be present when a reporter is wired")
	}
	if len(services) != 1 || services[0].ID != "service-down" || services[0].InstanceKey != "docker.service" {
		t.Fatalf("services category: want service-down/docker.service, got %v", services)
	}
}

// TestSystemHealth_TimeFromReporter verifies the time category appears once a
// ClockReporter is wired and carries the clock-not-synced finding verbatim (the
// clock-not-synced detector, locus B).
func TestSystemHealth_TimeFromReporter(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	clk := NewFakeClockReporter()
	clk.Set([]protocol.Finding{
		{ID: "clock-not-synced", Details: "last synced 7h0m0s ago"},
	})
	a.Time = clk

	w := get(t, mux, "/v1/health/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	sh := decodeBody[protocol.SystemHealth](t, w)
	tm, ok := sh.Categories[protocol.HealthCategoryTime]
	if !ok {
		t.Fatal("time category must be present when a reporter is wired")
	}
	if len(tm) != 1 || tm[0].ID != "clock-not-synced" {
		t.Fatalf("time category: want clock-not-synced, got %v", tm)
	}
}

// TestSystemHealth_ResourcesFromReporter verifies the resources category appears
// once a RAMReporter is wired and carries the ram-pressure finding verbatim (the
// ram-pressure detector, locus B).
func TestSystemHealth_ResourcesFromReporter(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	ram := NewFakeRAMReporter()
	ram.Set([]protocol.Finding{
		{ID: "ram-pressure", Details: "memory stall 34% over the last 60s"},
	})
	a.Resources = ram

	w := get(t, mux, "/v1/health/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	sh := decodeBody[protocol.SystemHealth](t, w)
	resources, ok := sh.Categories[protocol.HealthCategoryResources]
	if !ok {
		t.Fatal("resources category must be present when a reporter is wired")
	}
	if len(resources) != 1 || resources[0].ID != "ram-pressure" {
		t.Fatalf("resources category: want ram-pressure, got %v", resources)
	}
}

// TestSystemHealth_SystemFromReporter verifies the system category appears once
// a RebootReporter is wired and carries the reboot-required finding verbatim,
// package list in Details (the reboot-required detector, locus B, issue #40).
func TestSystemHealth_SystemFromReporter(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	rb := NewFakeRebootReporter()
	rb.Set([]protocol.Finding{
		{ID: "reboot-required", Details: "linux-image-6.8.0, libc6"},
	})
	a.Reboot = rb

	w := get(t, mux, "/v1/health/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	sh := decodeBody[protocol.SystemHealth](t, w)
	system, ok := sh.Categories[protocol.HealthCategorySystem]
	if !ok {
		t.Fatal("system category must be present when a reporter is wired")
	}
	if len(system) != 1 || system[0].ID != "reboot-required" || system[0].Details != "linux-image-6.8.0, libc6" {
		t.Fatalf("system category: want reboot-required with package list, got %v", system)
	}
}

// TestSystemHealth_AlwaysReturns200OnSourceError verifies that even a storage
// source error produces a parseable 200 with a present, non-nil storage
// category — the brain's polling loop must never have to retry on a 5xx.
type erroringHealthSource struct{}

func (erroringHealthSource) Read() (protocol.StorageHealth, error) {
	return protocol.StorageHealth{}, errors.New("simulated source failure")
}

func TestSystemHealth_AlwaysReturns200OnSourceError(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	a.Health = erroringHealthSource{}

	w := get(t, mux, "/v1/health/system")
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 even on source error, got %d", w.Code)
	}
	sh := decodeBody[protocol.SystemHealth](t, w)
	storage, ok := sh.Categories[protocol.HealthCategoryStorage]
	if !ok || storage == nil {
		t.Fatal("storage category must be present and non-nil even on source error")
	}
}

// --- well-known-identity tests ---

// The fake branch resolves the moose-app service identity to the dev operator's
// own uid/gid (not fixed 2000/2001) so a household-scope folder app runs as an
// identity the unprivileged dev brain owns — Part A's chowns are then no-op
// successes (#147).
func TestWellKnownIdentity_FakeBranch_ReturnsOperatorIdentity(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	w := get(t, mux, "/v1/identity/well-known")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.WellKnownIdentityResponse](t, w)
	if resp.MooseAppUID != os.Getuid() || resp.MooseAppGID != os.Getgid() {
		t.Errorf("moose-app identity: want operator %d:%d, got %d:%d",
			os.Getuid(), os.Getgid(), resp.MooseAppUID, resp.MooseAppGID)
	}
	if resp.MooseSharedGID != os.Getgid() {
		t.Errorf("moose_shared_gid: want operator egid %d, got %d", os.Getgid(), resp.MooseSharedGID)
	}
}

func TestWellKnownIdentity_DelegatesToUserMgrWhenSet(t *testing.T) {
	mgr := &stubUserMgr{wellKnownIdentityResult: &struct{ appUID, appGID, sharedGID int }{1500, 1500, 1501}}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := get(t, mux, "/v1/identity/well-known")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.WellKnownIdentityResponse](t, w)
	if resp.MooseAppUID != 1500 || resp.MooseAppGID != 1500 || resp.MooseSharedGID != 1501 {
		t.Errorf("unexpected response: %+v", resp)
	}
	if mgr.wellKnownIdentityCalls != 1 {
		t.Errorf("WellKnownIdentity not called once: called %d times", mgr.wellKnownIdentityCalls)
	}
}

func TestWellKnownIdentity_UserMgrError_Returns500(t *testing.T) {
	mgr := &stubUserMgr{wellKnownIdentityErr: errors.New("lookup moose-app user: user: unknown user moose-app")}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := get(t, mux, "/v1/identity/well-known")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("moose-app")) {
		t.Errorf("response leaked system detail: %s", w.Body.String())
	}
}

// --- app-service identity tests ---

func allocate(t *testing.T, mux *http.ServeMux, instanceID string) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, mux, "/v1/identity/app-service",
		protocol.AllocateAppServiceIdentityRequest{InstanceID: instanceID})
}

func release(t *testing.T, mux *http.ServeMux, uid int) *httptest.ResponseRecorder {
	t.Helper()
	return post(t, mux, "/v1/identity/app-service/release",
		protocol.ReleaseAppServiceIdentityRequest{UID: uid})
}

// The fake branch resolves a service_user app's identity to the dev operator's
// own uid/gid (not a band UID), so the unprivileged dev brain — running as the
// same operator — owns the bind dirs it creates and Part A's chowns are no-op
// successes (#147). A band UID would be un-chownable in dev and crash-loop the
// app.
func TestAllocateAppService_FakeBranch_ReturnsOperatorIdentity(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	w := allocate(t, mux, "inst_a")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.AllocateAppServiceIdentityResponse](t, w)
	if resp.UID != os.Getuid() || resp.GID != os.Getgid() {
		t.Errorf("identity: want operator %d:%d, got %d:%d",
			os.Getuid(), os.Getgid(), resp.UID, resp.GID)
	}
	// Every instance resolves to the same operator in dev (no per-instance band
	// reservation to track).
	other := decodeBody[protocol.AllocateAppServiceIdentityResponse](t, allocate(t, mux, "inst_b"))
	if other != resp {
		t.Errorf("second instance: got %+v, want operator %+v", other, resp)
	}
}

func TestAllocateAppService_MissingInstanceID_Returns400(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	if w := allocate(t, mux, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestReleaseAppService_OutOfBandUID_Returns400(t *testing.T) {
	mgr := &stubUserMgr{}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	for _, uid := range []int{0, 1000, protocol.AppServiceUIDMin - 1, protocol.AppServiceUIDMax + 1} {
		if w := release(t, mux, uid); w.Code != http.StatusBadRequest {
			t.Errorf("release(%d): want 400, got %d", uid, w.Code)
		}
	}
	// The band guard sits in front of the manager: nothing may be delegated.
	if len(mgr.releaseCalls) != 0 {
		t.Errorf("out-of-band release reached UserMgr: %v", mgr.releaseCalls)
	}
}

func TestReleaseAppService_FakeBranch_Idempotent(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	// The fake release is an unconditional 200 no-op (no band guard, no
	// reservation): the operator identity it hands out is not a reservation, so
	// releasing it — or any UID, repeatedly — is harmless.
	first := decodeBody[protocol.AllocateAppServiceIdentityResponse](t, allocate(t, mux, "inst_idem"))
	if w := release(t, mux, first.UID); w.Code != http.StatusOK {
		t.Fatalf("first release: want 200, got %d", w.Code)
	}
	if w := release(t, mux, first.UID); w.Code != http.StatusOK {
		t.Fatalf("double release: want 200, got %d", w.Code)
	}
}

func TestAllocateAppService_DelegatesToUserMgrWhenSet(t *testing.T) {
	mgr := &stubUserMgr{}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := allocate(t, mux, "inst_a")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeBody[protocol.AllocateAppServiceIdentityResponse](t, w)
	if resp.UID != 2100 || resp.GID != 2100 {
		t.Errorf("unexpected response: %+v", resp)
	}
	if len(mgr.allocateCalls) != 1 || mgr.allocateCalls[0] != "inst_a" {
		t.Errorf("AllocateAppService calls = %v, want [inst_a]", mgr.allocateCalls)
	}

	if w := release(t, mux, 2100); w.Code != http.StatusOK {
		t.Fatalf("release: want 200, got %d", w.Code)
	}
	if len(mgr.releaseCalls) != 1 || mgr.releaseCalls[0] != 2100 {
		t.Errorf("ReleaseAppService calls = %v, want [2100]", mgr.releaseCalls)
	}
}

func TestAllocateAppService_UserMgrError_Returns500(t *testing.T) {
	mgr := &stubUserMgr{allocateErr: errors.New("useradd: UID 2100 is not unique")}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := allocate(t, mux, "inst_a")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("useradd")) {
		t.Errorf("response leaked system detail: %s", w.Body.String())
	}
}

func TestReleaseAppService_UserMgrError_Returns500(t *testing.T) {
	mgr := &stubUserMgr{releaseErr: errors.New("userdel: user moose-svc-2100 is currently used by process 4242")}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	w := release(t, mux, 2100)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("userdel")) {
		t.Errorf("response leaked system detail: %s", w.Body.String())
	}
}

// The fake branch answers from the accounts this agent itself created. That is
// what makes it usable as the brain's existence probe in the dev loop, and it
// is exactly where resolve-home cannot stand in: resolve-home answers for every
// name, so as a probe it would report every name taken and the brain's
// account-name derivation would never terminate.
func TestUserExists_FakeBranch_KnowsOnlyItsOwnAccounts(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})

	w := get(t, mux, "/v1/users/cindy/exists")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if decodeBody[protocol.UserExistsResponse](t, w).Exists {
		t.Error("unknown user reported as existing")
	}

	post(t, mux, "/v1/auth/set-password", protocol.SetPasswordRequest{User: "cindy", Password: "hunter2"})

	w = get(t, mux, "/v1/users/cindy/exists")
	if !decodeBody[protocol.UserExistsResponse](t, w).Exists {
		t.Error("created user reported as missing")
	}

	// resolve-home, by contrast, answers for a name that does not exist. Pinned
	// here so the two are not quietly collapsed into one route later.
	if w := get(t, mux, "/v1/users/ghost/home"); w.Code != http.StatusOK {
		t.Errorf("fake resolve-home for an unknown user = %d; want 200", w.Code)
	}
}

func TestUserExists_DelegatesToUserMgrWhenSet(t *testing.T) {
	mgr := &stubUserMgr{existing: map[string]bool{"plex": true}}
	a := New(&stubVerifier{}, NewFakePublisher(".local"))
	a.UserMgr = mgr
	mux := http.NewServeMux()
	a.Mount(mux)

	if !decodeBody[protocol.UserExistsResponse](t, get(t, mux, "/v1/users/plex/exists")).Exists {
		t.Error("plex should exist")
	}
	if decodeBody[protocol.UserExistsResponse](t, get(t, mux, "/v1/users/cindy/exists")).Exists {
		t.Error("cindy should not exist")
	}
	if len(mgr.existsCalls) != 2 {
		t.Errorf("UserExists called %d times; want 2", len(mgr.existsCalls))
	}
}
