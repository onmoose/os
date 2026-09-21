package api

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// sshFailUser makes the harness's /v1/ssh/set-access mock answer 500, so the
// host-502 and rollback paths are reachable.
const sshFailUser = "hostfail"

// A real key pair's public halves, generated once and pasted here. Two distinct
// keys so the duplicate and multi-key cases are exercised with material that
// actually parses.
const (
	testKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIfJnhGAA/rWbxmvMGuZvXV6in+czTK5F8Ie7QGTKOT+ alex@laptop"
	testKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHmywREXaNctQmxNs8UMGg8mSDO4MP1SfJnIhUAeEoY9 alex@desktop"
)

// hostedSSHHarness is a hosted-profile harness with a signed-in, elevated admin.
func hostedSSHHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, func(s *Server) { s.SetEnvironment(profile.Hosted, "cindy-fox", nil) })
	seedAdminSession(t, h)
	return h
}

// applianceSSHHarness is the same, on the default appliance profile.
func applianceSSHHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	seedAdminSession(t, h)
	return h
}

// seedAdminSession creates an admin directly in the store (so it works on hosted,
// where /setup is disabled), signs in, and elevates.
func seedAdminSession(t *testing.T, h *harness) {
	t.Helper()
	if err := h.st.CreateUser(store.User{
		ID: "u_alex", Username: "alex", DisplayName: "alex", Role: store.RoleAdmin,
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h.seedPassword("alex", "pass1")
	h.loginAs("alex", "pass1")
	h.elevate("pass1")
}

// seedPassword writes a bcrypt hash straight into the harness's fake host-agent,
// so an account created directly in the store can sign in. Needed because
// /setup — the usual way an admin gets a password — is disabled on hosted.
func (h *harness) seedPassword(username, password string) {
	h.t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		h.t.Fatalf("hash password: %v", err)
	}
	h.pmu.Lock()
	h.pwds[username] = hash
	h.pmu.Unlock()
}

func (h *harness) sshCallsSnapshot() []protocol.SetSSHAccessRequest {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	out := make([]protocol.SetSSHAccessRequest, len(*h.sshCalls))
	copy(out, *h.sshCalls)
	return out
}

func (h *harness) addKey(t *testing.T, key string) SSHAccessDTO {
	t.Helper()
	resp := h.do("POST", "/api/v1/me/ssh/keys", map[string]string{"public_key": key})
	if resp.StatusCode != 200 {
		t.Fatalf("add key = %d", resp.StatusCode)
	}
	return decodeJSON[SSHAccessDTO](t, resp)
}

// On hosted a public key is the mandatory factor, enforced by the brain and not
// only by the UI. Enabling with no key would leave an account reachable by
// password alone on a port the open internet can reach.
func TestHostedEnableWithoutKeyIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true})
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("hosted enable with no key = %d; want 422", resp.StatusCode)
	}
	if calls := h.sshCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("refused enable still reached the host: %+v", calls)
	}
	assertAudited(t, h, audit.ActionSSHAccessSet, false)
}

// The appliance keeps the password as its mandatory factor, so enabling with no
// key is allowed there. :22 is LAN- and mesh-scoped by nftables, which is the
// perimeter the one-password model was designed around.
func TestApplianceEnableWithoutKeyIsAllowed(t *testing.T) {
	h := applianceSSHHarness(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true})
	if resp.StatusCode != 200 {
		t.Fatalf("appliance enable with no key = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[SSHAccessDTO](t, resp)
	if !body.Enabled || body.KeyRequired {
		t.Fatalf("appliance state = %+v; want enabled and key_required false", body)
	}
	calls := h.sshCallsSnapshot()
	if len(calls) != 1 || !calls[0].Enabled || calls[0].User != "alex" {
		t.Fatalf("host call = %+v; want one enable for alex", calls)
	}
}

// The optional password rides to the host as require_password, which is what
// makes it a second required method rather than an alternative one.
func TestRequirePasswordReachesTheHost(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "require_password": true})
	if resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}
	resp.Body.Close()

	calls := h.sshCallsSnapshot()
	last := calls[len(calls)-1]
	if !last.RequirePassword || len(last.AuthorizedKeys) != 1 {
		t.Fatalf("host call = %+v; want require_password with one key", last)
	}
}

// A private key is the worst paste a user can make, so it gets its own message
// and is never stored.
func TestPrivateKeyPasteIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)

	resp := h.do("POST", "/api/v1/me/ssh/keys", map[string]string{
		"public_key": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("private key paste = %d; want 422", resp.StatusCode)
	}
	keys, err := h.st.ListSSHKeys("u_alex")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("a private key was stored: %+v", keys)
	}
}

// authorized_keys options (command=, from=, …) change what a key can do. The
// stored line is re-serialised from the parsed key, so a paste cannot smuggle
// them in.
func TestAuthorizedKeysOptionsAreStripped(t *testing.T) {
	h := hostedSSHHarness(t)

	body := h.addKey(t, `command="/bin/sh",no-pty `+testKeyA)
	if len(body.Keys) != 1 {
		t.Fatalf("keys = %d; want 1", len(body.Keys))
	}
	stored := body.Keys[0].PublicKey
	if strings.Contains(stored, "command=") || strings.Contains(stored, "no-pty") {
		t.Fatalf("stored line kept authorized_keys options: %q", stored)
	}
	if !strings.HasPrefix(stored, "ssh-ed25519 ") {
		t.Fatalf("stored line = %q; want a bare key line", stored)
	}
}

// The same key twice is a 409, not a silent success: the user should learn the
// key was already there rather than wonder which one is live.
func TestDuplicateKeyIsConflict(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("POST", "/api/v1/me/ssh/keys", map[string]string{"public_key": testKeyA})
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("duplicate key = %d; want 409", resp.StatusCode)
	}
}

// Adding a key while SSH is off changes nothing the host needs to know, so it
// must not push an authorized_keys file for an account sshd is not admitting.
func TestKeyAddDoesNotTouchHostWhileDisabled(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	if calls := h.sshCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("key add on a disabled account reached the host: %+v", calls)
	}
}

// Removing the last key on hosted while SSH is on would leave an enabled account
// with no mandatory factor. Refuse, rather than silently turning SSH off.
func TestHostedLastKeyCannotBeRemovedWhileEnabled(t *testing.T) {
	h := hostedSSHHarness(t)
	body := h.addKey(t, testKeyA)
	keyID := body.Keys[0].ID

	if resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}); resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/"+keyID, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("removing the only key = %d; want 422", resp.StatusCode)
	}
	keys, _ := h.st.ListSSHKeys("u_alex")
	if len(keys) != 1 {
		t.Fatalf("key was removed anyway: %+v", keys)
	}
}

// A host failure rolls the brain row back, so the two sides cannot disagree
// about who has a shell (CLAUDE.md # Brain commits first).
func TestHostFailureRollsBackTheAccessRow(t *testing.T) {
	h := newHarness(t)
	if err := h.st.CreateUser(store.User{ID: "u_fail", Username: sshFailUser, DisplayName: sshFailUser, Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h.seedPassword(sshFailUser, "pass1")
	h.loginAs(sshFailUser, "pass1")
	h.elevate("pass1")

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true})
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("host failure = %d; want 502", resp.StatusCode)
	}
	access, err := h.st.SSHAccessFor("u_fail")
	if err != nil {
		t.Fatalf("read access: %v", err)
	}
	if access.Enabled {
		t.Fatal("brain row stayed enabled after the host refused")
	}
	assertAudited(t, h, audit.ActionSSHAccessSet, false)
}

// Deleting a user revokes their SSH on the host first. The brain's rows cascade
// away, but sshd's AllowUsers and the account's key file do not — a later user
// with the same name would inherit a deleted account's key.
func TestDeletingAUserRevokesTheirSSH(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(t, h)
	if err := h.st.CreateUser(store.User{ID: "u_bob", Username: "bob", DisplayName: "bob", Role: store.RoleMember}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := h.st.SetSSHAccess("u_bob", true, false); err != nil {
		t.Fatalf("seed access: %v", err)
	}
	if err := h.st.AddSSHKey(store.SSHKey{
		ID: "k_bob", UserID: "u_bob", PublicKey: testKeyA, Fingerprint: "fp_bob",
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	resp := h.do("DELETE", "/api/v1/users/u_bob", nil)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete user = %d; want 204", resp.StatusCode)
	}

	var revoked bool
	for _, c := range h.sshCallsSnapshot() {
		if c.User == "bob" && !c.Enabled && len(c.AuthorizedKeys) == 0 {
			revoked = true
		}
	}
	if !revoked {
		t.Fatalf("no ssh revoke reached the host: %+v", h.sshCallsSnapshot())
	}
}

// An account that never had SSH on is not pushed at all. Nothing was sent to the
// host for it, and calling here would fail on a box with no sshd installed.
func TestDeletingAUserWithoutSSHDoesNotCallTheHost(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(t, h)
	if err := h.st.CreateUser(store.User{ID: "u_cid", Username: "cid", DisplayName: "cid", Role: store.RoleMember}); err != nil {
		t.Fatalf("create member: %v", err)
	}

	resp := h.do("DELETE", "/api/v1/users/u_cid", nil)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete user = %d; want 204", resp.StatusCode)
	}
	if calls := h.sshCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("host was called for an account with no SSH: %+v", calls)
	}
}

// A delete the host never applied is put back. The key is still live on the host
// whichever way this goes, so dropping the row would only hide it: the panel
// would show the key gone, a retry would 404, and nothing re-reads the host.
func TestHostFailureRestoresTheDeletedKey(t *testing.T) {
	h := newHarness(t)
	if err := h.st.CreateUser(store.User{ID: "u_fail", Username: sshFailUser, DisplayName: sshFailUser, Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h.seedPassword(sshFailUser, "pass1")
	h.loginAs(sshFailUser, "pass1")
	h.elevate("pass1")

	// Seeded straight into the store: every route that would set this up goes
	// through the same host mock, which answers 500 for this account.
	if err := h.st.SetSSHAccess("u_fail", true, false); err != nil {
		t.Fatalf("seed access: %v", err)
	}
	for _, k := range []store.SSHKey{
		{ID: "k_a", UserID: "u_fail", PublicKey: testKeyA, Fingerprint: "fp_a"},
		{ID: "k_b", UserID: "u_fail", PublicKey: testKeyB, Fingerprint: "fp_b"},
	} {
		if err := h.st.AddSSHKey(k); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/k_a", nil)
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("host failure = %d; want 502", resp.StatusCode)
	}
	keys, err := h.st.ListSSHKeys("u_fail")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys after the failed delete = %d; want 2 (%+v)", len(keys), keys)
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// Every write here is elevation-class, and a rejection audits so the Activity
// view can answer "did someone try to open a shell into this box?".
func TestSSHWritesRequireElevation(t *testing.T) {
	h := newHarness(t)
	if err := h.st.CreateUser(store.User{ID: "u_alex", Username: "alex", DisplayName: "alex", Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h.seedPassword("alex", "pass1")
	h.loginAs("alex", "pass1") // signed in, deliberately not elevated

	// Bodies are valid on purpose: huma validates the schema before the handler
	// runs, so an empty body would 422 and never reach the elevation gate this
	// test is about.
	for _, c := range []struct {
		method, path string
		body         map[string]any
	}{
		{"PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}},
		{"POST", "/api/v1/me/ssh/keys", map[string]any{"public_key": testKeyA}},
		{"DELETE", "/api/v1/me/ssh/keys/whatever", nil},
	} {
		resp := h.do(c.method, c.path, c.body)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("%s %s unelevated = %d; want 403", c.method, c.path, resp.StatusCode)
		}
	}
	assertAudited(t, h, audit.ActionSSHAccessSet, false)
	assertAudited(t, h, audit.ActionSSHKeyAdd, false)
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// The panel state tells the UI which factor this profile makes mandatory, so the
// two cannot drift.
func TestKeyRequiredReflectsTheProfile(t *testing.T) {
	hosted := hostedSSHHarness(t)
	resp := hosted.do("GET", "/api/v1/me/ssh", nil)
	if got := decodeJSON[SSHAccessDTO](t, resp); !got.KeyRequired {
		t.Fatalf("hosted key_required = false; want true")
	}

	appliance := applianceSSHHarness(t)
	resp = appliance.do("GET", "/api/v1/me/ssh", nil)
	if got := decodeJSON[SSHAccessDTO](t, resp); got.KeyRequired {
		t.Fatalf("appliance key_required = true; want false")
	}
}

func assertAudited(t *testing.T, h *harness, action string, success bool) {
	t.Helper()
	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, e := range events {
		if e.Action == action && e.Success == success {
			return
		}
	}
	t.Fatalf("no %s audit event with success=%v", action, success)
}

// A failed elevation-class delete leaves a trace, including one against an id
// that is not there — which is what probing another account's key ids would look
// like from the Activity view.
func TestFailedKeyDeleteIsAudited(t *testing.T) {
	h := hostedSSHHarness(t)

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/nope", nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("delete unknown key = %d; want 404", resp.StatusCode)
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// The last-key guard is a guard rejection in the CLAUDE.md sense, the same shape
// as the last-admin guard, so it audits rather than passing silently as a plain
// validation failure.
func TestLastKeyGuardIsAudited(t *testing.T) {
	h := hostedSSHHarness(t)
	body := h.addKey(t, testKeyA)
	if resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}); resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/"+body.Keys[0].ID, nil)
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("removing the only key = %d; want 422", resp.StatusCode)
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// A second key makes the first removable, which is the escape hatch the guard
// leaves open.
func TestSecondKeyMakesTheFirstRemovable(t *testing.T) {
	h := hostedSSHHarness(t)
	first := h.addKey(t, testKeyA)
	h.addKey(t, testKeyB)
	if resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}); resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/"+first.Keys[0].ID, nil)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete with a spare key = %d; want 204", resp.StatusCode)
	}
	calls := h.sshCallsSnapshot()
	last := calls[len(calls)-1]
	if len(last.AuthorizedKeys) != 1 {
		t.Fatalf("host got %d keys after the revoke; want 1", len(last.AuthorizedKeys))
	}
}

// A user with SSH off is still deleted under the SSH lock. Without it, the
// account could turn SSH on between the "is it enabled?" read and the delete:
// the cascade would drop the brain's rows while sshd kept the account and its
// key file, which is exactly the leftover the revoke exists to prevent.
func TestDeletingADisabledUserStillHoldsTheSSHLock(t *testing.T) {
	h := newHarness(t)
	seedAdminSession(t, h)
	if err := h.st.CreateUser(store.User{ID: "u_dan", Username: "dan", DisplayName: "dan", Role: store.RoleMember}); err != nil {
		t.Fatalf("create member: %v", err)
	}

	// Stand in for an SSH write that is already in flight.
	h.apiSrv.sshWrites.Lock()

	done := make(chan int, 1)
	go func() {
		resp := h.do("DELETE", "/api/v1/users/u_dan", nil)
		resp.Body.Close()
		done <- resp.StatusCode
	}()

	select {
	case code := <-done:
		h.apiSrv.sshWrites.Unlock()
		t.Fatalf("delete finished (%d) while an SSH write held the lock", code)
	case <-time.After(100 * time.Millisecond):
	}

	h.apiSrv.sshWrites.Unlock()
	select {
	case code := <-done:
		if code != 204 {
			t.Fatalf("delete user = %d; want 204", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete never finished after the lock was released")
	}
}

// deleteFailUser makes the harness's /v1/auth/delete-user mock answer 500, so the
// brain's delete rollback is reachable.
const deleteFailUser = "delfail"

// Deleting a user revokes their SSH on the host BEFORE the brain row goes, because
// the revoke reads state the delete is about to cascade away. That inverts the
// usual brain-commits-first order, so every later failure owes a compensating
// re-push. Without one, a delete that fails after the revoke leaves the account
// enabled in the brain and revoked on the host, and nothing re-reads the host to
// notice: the user silently loses SSH while the dashboard still shows it on.
func TestFailedDeleteRestoresTheAccountsSSH(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addMember("u_bob", deleteFailUser, "pw-bob")

	// Bob has SSH on with one key. Written straight to the store: /me/ssh is
	// self-service and this test is about the admin's delete, not Bob's session.
	if err := h.st.SetSSHAccess("u_bob", true, true); err != nil {
		t.Fatalf("seed ssh access: %v", err)
	}
	added := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	if err := h.st.AddSSHKey(store.SSHKey{
		ID: "k_bob", UserID: "u_bob", Label: "laptop",
		PublicKey: testKeyA, Fingerprint: "SHA256:bob", AddedAt: added,
	}); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}

	resp := h.do("DELETE", "/api/v1/users/u_bob", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("delete with a failing host = %d; want 502", resp.StatusCode)
	}

	// The user row came back, as it did before this fix.
	if _, err := h.st.GetUser("u_bob"); err != nil {
		t.Fatalf("user row not restored after a failed delete: %v", err)
	}

	// And so did their SSH. The cascade took both rows with the user, so a restore
	// that put back only the user would hand Bob an account whose keys had been
	// destroyed by an operation that reported failure.
	access, err := h.st.SSHAccessFor("u_bob")
	if err != nil {
		t.Fatalf("read ssh access: %v", err)
	}
	if !access.Enabled || !access.RequirePassword {
		t.Fatalf("ssh access not restored: %+v", access)
	}
	keys, err := h.st.ListSSHKeys("u_bob")
	if err != nil {
		t.Fatalf("list ssh keys: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != "k_bob" {
		t.Fatalf("ssh keys not restored: %+v", keys)
	}
	// The original timestamp, not the restore's. ListSSHKeys orders by added_at, so
	// re-stamping would silently reorder the user's key list after a failure that
	// said nothing happened.
	if !keys[0].AddedAt.Equal(added.UTC()) {
		t.Fatalf("restored key was re-stamped: added_at = %v, want %v", keys[0].AddedAt, added.UTC())
	}

	// The host was put back too: revoked on the way down, re-pushed on the way out.
	calls := h.sshCallsSnapshot()
	if len(calls) < 2 {
		t.Fatalf("expected a revoke and a compensating re-push; got %+v", calls)
	}
	last := calls[len(calls)-1]
	if last.User != deleteFailUser || !last.Enabled || !last.RequirePassword {
		t.Fatalf("host was not re-pushed the account's previous state: %+v", last)
	}
	if len(last.AuthorizedKeys) != 1 {
		t.Fatalf("re-push carried %d keys, want 1: %+v", len(last.AuthorizedKeys), last)
	}
}

// A delete that never touched SSH must not push anything to the host on its
// failure path either — a disabled account has nothing to restore, and a box with
// no sshd installed would fail the call.
func TestFailedDeleteOfAnAccountWithoutSSHPushesNothing(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addMember("u_bob", deleteFailUser, "pw-bob")

	resp := h.do("DELETE", "/api/v1/users/u_bob", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("delete with a failing host = %d; want 502", resp.StatusCode)
	}
	if calls := h.sshCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("a delete of an SSH-less account reached the ssh seam: %+v", calls)
	}
}

// The appliance's mandatory factor is the moose password, with the key as the
// optional second lock (AUTH.md # Device access, the profile table). Adding a
// key must not quietly replace the password with it.
//
// The brain is what has to hold this. host-agent's renderer, given a key and
// require_password false, correctly writes "AuthenticationMethods publickey" —
// it does not know the profile and must not guess which factor is mandatory —
// so the only place the appliance row can be enforced is here, before the push.
func TestApplianceKeepsThePasswordWhenAKeyIsAdded(t *testing.T) {
	// Both request shapes, because they are not the same request even though
	// they decode to the same boolean today. Omitting the field is what a panel
	// actually sends — it is `omitempty` — and keeping that case separate means a
	// later presence-sensitive decoder cannot break the default path unnoticed.
	//
	// A harness each, not one shared: the host-call slice accumulates, and a
	// shared one would let a shape that stops calling the host altogether pass on
	// the other shape's leftover call.
	for name, body := range map[string]map[string]any{
		"field omitted":    {"enabled": true},
		"field sent false": {"enabled": true, "require_password": false},
	} {
		t.Run(name, func(t *testing.T) {
			assertApplianceKeepsThePassword(t, body)
		})
	}
}

func assertApplianceKeepsThePassword(t *testing.T, body map[string]any) {
	t.Helper()
	h := applianceSSHHarness(t)
	h.addKey(t, testKeyA)
	before := len(h.sshCallsSnapshot())

	resp := h.do("PUT", "/api/v1/me/ssh", body)
	if resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	calls := h.sshCallsSnapshot()
	if len(calls) != before+1 {
		t.Fatalf("this request made %d host calls, want exactly 1 — an assertion on a "+
			"leftover call would pass without testing this request", len(calls)-before)
	}
	last := calls[len(calls)-1]
	if !last.RequirePassword {
		t.Errorf("host got require_password=false; the appliance would render "+
			"AuthenticationMethods publickey and drop the mandatory factor (call = %+v)", last)
	}
	if len(last.AuthorizedKeys) != 1 {
		t.Errorf("host got %d keys, want 1", len(last.AuthorizedKeys))
	}

	// The stored row has to agree, or GET /me/ssh reports a posture sshd is not
	// running and every later re-push sends the wrong thing.
	got := decodeJSON[SSHAccessDTO](t, resp)
	if !got.RequirePassword {
		t.Errorf("DTO require_password = false, want true — the panel would show a lock the box does not have")
	}
}

// Hosted is untouched: there the key is mandatory and the password is the
// account's own choice, so a false stays false and the account authenticates
// with the key alone.
func TestHostedKeepsTheAccountsPasswordChoice(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "require_password": false})
	if resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	calls := h.sshCallsSnapshot()
	last := calls[len(calls)-1]
	if last.RequirePassword {
		t.Errorf("host got require_password=true on hosted; the account asked for key-only (call = %+v)", last)
	}
	if got := decodeJSON[SSHAccessDTO](t, resp).RequirePassword; got {
		t.Errorf("DTO require_password = true, want false")
	}
}

// An appliance account with no key is password-only, and that is already the
// mandatory factor, so the resolved value is true there too. Worth pinning
// because it is the state every appliance account starts in.
func TestApplianceWithoutAKeyStillRequiresThePassword(t *testing.T) {
	h := applianceSSHHarness(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true})
	if resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	calls := h.sshCallsSnapshot()
	last := calls[len(calls)-1]
	if !last.RequirePassword || len(last.AuthorizedKeys) != 0 {
		t.Errorf("host call = %+v; want require_password with no keys", last)
	}
}
