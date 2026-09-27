package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"

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

// freshKey makes a new public key line, for tests that need more distinct keys
// than the two pasted above.
func freshKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap key: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " test@fresh"
}

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

// sshState reads the signed-in account's SSH screen state.
func (h *harness) sshState(t *testing.T) SSHAccessDTO {
	t.Helper()
	resp := h.do("GET", "/api/v1/me/ssh", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get ssh = %d", resp.StatusCode)
	}
	return decodeJSON[SSHAccessDTO](t, resp)
}

// keep names every key the account holds, which is how a Save that leaves the
// key list alone describes it.
func keep(state SSHAccessDTO) []map[string]any {
	out := []map[string]any{}
	for _, k := range state.Keys {
		out = append(out, map[string]any{"id": k.ID})
	}
	return out
}

// addKey saves the current state plus one new key: what the screen sends when the
// only change is a pasted key.
func (h *harness) addKey(t *testing.T, key string) SSHAccessDTO {
	t.Helper()
	cur := h.sshState(t)
	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled":          cur.Enabled,
		"require_password": cur.RequirePassword,
		"keys":             append(keep(cur), map[string]any{"public_key": key}),
	})
	if resp.StatusCode != 200 {
		t.Fatalf("add key = %d", resp.StatusCode)
	}
	return decodeJSON[SSHAccessDTO](t, resp)
}

// enable turns SSH on and keeps every held key.
func (h *harness) enable(t *testing.T) SSHAccessDTO {
	t.Helper()
	cur := h.sshState(t)
	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "keys": keep(cur)})
	if resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}
	return decodeJSON[SSHAccessDTO](t, resp)
}

// errorLocation returns the first field location in a huma error body.
func errorLocation(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Errors []struct {
			Location string `json:"location"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, raw)
	}
	if len(body.Errors) == 0 {
		return ""
	}
	return body.Errors[0].Location
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return raw
}

// countAudited counts the audit events for one action and outcome.
func countAudited(t *testing.T, h *harness, action string, success bool) int {
	t.Helper()
	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action && e.Success == success {
			n++
		}
	}
	return n
}

func assertAudited(t *testing.T, h *harness, action string, success bool) {
	t.Helper()
	if countAudited(t, h, action, success) == 0 {
		t.Fatalf("no %s audit event with success=%v", action, success)
	}
}

// On hosted a public key is the mandatory factor, enforced by the brain and not
// only by the UI. Enabling with no key would leave an account reachable by
// password alone on a port the open internet can reach.
func TestHostedEnableWithoutKeyIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "keys": []any{}})
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

	body := h.enable(t)
	if !body.Enabled || body.KeyRequired {
		t.Fatalf("appliance state = %+v; want enabled and key_required false", body)
	}
	calls := h.sshCallsSnapshot()
	if len(calls) != 1 || !calls[0].Enabled || calls[0].User != "alex" {
		t.Fatalf("host call = %+v; want one enable for alex", calls)
	}
}

// The body is the whole state, so the key set is required. An old client that
// sent only the flag must be refused, not read as "no keys" and allowed to wipe
// every key the account holds.
func TestSaveWithoutAKeySetIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": false})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("save with no keys field = %d; want 422", resp.StatusCode)
	}
	if keys, _ := h.st.ListSSHKeys("u_alex"); len(keys) != 1 {
		t.Fatalf("keys after the refused save = %d; want 1", len(keys))
	}
}

// The optional password rides to the host as require_password, which is what
// makes it a second required method rather than an alternative one.
func TestRequirePasswordReachesTheHost(t *testing.T) {
	h := hostedSSHHarness(t)
	cur := h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": true, "require_password": true, "keys": keep(cur),
	})
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
// and is never stored. The refusal names the entry, so the screen can show it
// under the key at fault.
func TestPrivateKeyPasteIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)
	cur := h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": false,
		"keys": append(keep(cur), map[string]any{
			"public_key": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----",
		}),
	})
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("private key paste = %d; want 422", resp.StatusCode)
	}
	raw := readAll(t, resp)
	if loc := errorLocation(t, raw); loc != "body.keys[1].public_key" {
		t.Fatalf("error location = %q; want body.keys[1].public_key", loc)
	}
	if !strings.Contains(string(raw), "private key") {
		t.Fatalf("error does not say it is a private key: %s", raw)
	}
	keys, err := h.st.ListSSHKeys("u_alex")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys after the refused paste = %d; want the 1 held before (%+v)", len(keys), keys)
	}
}

// authorized_keys options (command=, from=, and the rest) change what a key can
// do. The stored line is re-serialised from the parsed key, so a paste cannot
// smuggle them in.
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
// key was already there rather than wonder which one is live. Both shapes: a
// paste of a key the account holds, and the same new key twice in one Save.
func TestDuplicateKeyIsConflict(t *testing.T) {
	h := hostedSSHHarness(t)
	cur := h.addKey(t, testKeyA)

	for name, keys := range map[string][]map[string]any{
		"already held": append(keep(cur), map[string]any{"public_key": testKeyA}),
		"twice in one save": append(keep(cur),
			map[string]any{"public_key": testKeyB}, map[string]any{"public_key": testKeyB}),
	} {
		t.Run(name, func(t *testing.T) {
			resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": false, "keys": keys})
			defer resp.Body.Close()
			if resp.StatusCode != 409 {
				t.Fatalf("duplicate key = %d; want 409", resp.StatusCode)
			}
			if got, _ := h.st.ListSSHKeys("u_alex"); len(got) != 1 {
				t.Fatalf("keys after the refused save = %d; want 1", len(got))
			}
		})
	}
	assertAudited(t, h, audit.ActionSSHKeyAdd, false)
}

// Removing a key and pasting it back in the same Save is not a duplicate. The
// store removes before it adds, so the unique index never sees both.
func TestRemovingAndReaddingAKeyInOneSaveIsAllowed(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": false, "keys": []map[string]any{{"public_key": testKeyA}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("remove and re-add = %d; want 200", resp.StatusCode)
	}
	if got := decodeJSON[SSHAccessDTO](t, resp); len(got.Keys) != 1 {
		t.Fatalf("keys = %d; want 1", len(got.Keys))
	}
}

// A request built from a key list that has since changed names a key the account
// no longer holds. Refuse it rather than guess: the screen reloads and the user
// saves again against what is really there.
func TestUnknownKeyIDIsConflict(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": false, "keys": []map[string]any{{"id": "nope"}},
	})
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("unknown key id = %d; want 409", resp.StatusCode)
	}
	if got, _ := h.st.ListSSHKeys("u_alex"); len(got) != 1 {
		t.Fatalf("the held key was removed by a refused save: %+v", got)
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
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

// Turning SSH off pushes once more, so the host revokes. Skipping it because the
// account is now off would leave sshd admitting it.
func TestTurningSSHOffRevokesOnTheHost(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)
	cur := h.enable(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": false, "keys": keep(cur)})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("disable = %d", resp.StatusCode)
	}
	calls := h.sshCallsSnapshot()
	if last := calls[len(calls)-1]; last.Enabled {
		t.Fatalf("last host call = %+v; want a revoke", last)
	}
}

// On hosted, a Save that would leave an enabled account with no key is refused
// and audited. This is the final-state guard: it replaces the old per-delete
// last-key guard, and like that one it is a guard rejection, so it audits.
func TestHostedEnabledWithAnEmptyKeySetIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)
	h.enable(t)
	before := len(h.sshCallsSnapshot())

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "keys": []any{}})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("enabled with no keys = %d; want 422", resp.StatusCode)
	}
	if keys, _ := h.st.ListSSHKeys("u_alex"); len(keys) != 1 {
		t.Fatalf("key was removed anyway: %+v", keys)
	}
	if calls := h.sshCallsSnapshot(); len(calls) != before {
		t.Fatalf("refused save reached the host: %+v", calls[before:])
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// Key rotation: remove the only key and add its replacement in one Save. The old
// per-delete guard refused the removal on its own, so rotating needed a spare key
// first. Only the end state is checked now, and it has a key.
func TestHostedKeyRotationInOneSave(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)
	h.enable(t)
	before := len(h.sshCallsSnapshot())

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": true, "keys": []map[string]any{{"public_key": testKeyB, "label": "desktop"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("rotation = %d; want 200", resp.StatusCode)
	}
	got := decodeJSON[SSHAccessDTO](t, resp)
	if len(got.Keys) != 1 || got.Keys[0].Label != "desktop" || !strings.HasPrefix(testKeyB, got.Keys[0].PublicKey) {
		t.Fatalf("keys after rotation = %+v; want only the new key", got.Keys)
	}

	calls := h.sshCallsSnapshot()
	if len(calls) != before+1 {
		t.Fatalf("rotation made %d host calls; want exactly 1", len(calls)-before)
	}
	last := calls[len(calls)-1]
	if !last.Enabled || len(last.AuthorizedKeys) != 1 || !strings.HasPrefix(testKeyB, last.AuthorizedKeys[0]) {
		t.Fatalf("host call = %+v; want enabled with only the new key", last)
	}
	assertAudited(t, h, audit.ActionSSHKeyAdd, true)
	assertAudited(t, h, audit.ActionSSHKeyDelete, true)
}

// A second key makes the first removable while SSH stays on.
func TestSecondKeyMakesTheFirstRemovable(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)
	cur := h.addKey(t, testKeyB)
	h.enable(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": true, "keys": []map[string]any{{"id": cur.Keys[1].ID}},
	})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("remove with a spare key = %d; want 200", resp.StatusCode)
	}
	calls := h.sshCallsSnapshot()
	last := calls[len(calls)-1]
	if len(last.AuthorizedKeys) != 1 {
		t.Fatalf("host got %d keys after the revoke; want 1", len(last.AuthorizedKeys))
	}
}

// One Save on the appliance: turn SSH on, add a key and remove another. It is one
// request, one host call, and one audit record for each thing that changed, so
// Activity still shows which keys moved.
func TestApplianceOneSaveIsOneHostCall(t *testing.T) {
	h := applianceSSHHarness(t)
	h.addKey(t, testKeyA)
	cur := h.addKey(t, testKeyB)
	third := freshKey(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": true,
		"keys":    []map[string]any{{"id": cur.Keys[1].ID}, {"public_key": third}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("save = %d; want 200", resp.StatusCode)
	}
	got := decodeJSON[SSHAccessDTO](t, resp)
	if !got.Enabled || len(got.Keys) != 2 {
		t.Fatalf("state = %+v; want enabled with two keys", got)
	}
	// Kept key first, then the new one: the order the user saw them in.
	if got.Keys[0].ID != cur.Keys[1].ID {
		t.Fatalf("kept key is not first: %+v", got.Keys)
	}

	calls := h.sshCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("host calls = %d; want 1 (the key adds were made while SSH was off)", len(calls))
	}
	if !calls[0].Enabled || !calls[0].RequirePassword || len(calls[0].AuthorizedKeys) != 2 {
		t.Fatalf("host call = %+v; want enabled, password required, two keys", calls[0])
	}

	// Three key adds (two set-up saves, one here), one delete, one access change.
	if n := countAudited(t, h, audit.ActionSSHKeyAdd, true); n != 3 {
		t.Errorf("ssh.key.add records = %d; want 3", n)
	}
	if n := countAudited(t, h, audit.ActionSSHKeyDelete, true); n != 1 {
		t.Errorf("ssh.key.delete records = %d; want 1", n)
	}
	if n := countAudited(t, h, audit.ActionSSHAccessSet, true); n != 1 {
		t.Errorf("ssh.access.set records = %d; want 1 (only this save turned SSH on)", n)
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

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "keys": []any{}})
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

// A Save the host never applied is undone in full. The removed key comes back
// with its original timestamp, so the list keeps its order, and the added key
// goes. The keys are live on the host as they were either way, since the push is
// what failed, so leaving the brain changed would only hide that.
func TestHostFailureUndoesTheWholeSave(t *testing.T) {
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
	older := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	for _, k := range []store.SSHKey{
		{ID: "k_a", UserID: "u_fail", PublicKey: testKeyA, Fingerprint: "fp_a", AddedAt: older},
		{ID: "k_b", UserID: "u_fail", PublicKey: testKeyB, Fingerprint: "fp_b", AddedAt: older.Add(time.Hour)},
	} {
		if err := h.st.AddSSHKey(k); err != nil {
			t.Fatalf("seed key: %v", err)
		}
	}

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": true,
		"keys":    []map[string]any{{"id": "k_b"}, {"public_key": freshKey(t)}},
	})
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("host failure = %d; want 502", resp.StatusCode)
	}
	keys, err := h.st.ListSSHKeys("u_fail")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != "k_a" || keys[1].ID != "k_b" {
		t.Fatalf("keys after the failed save = %+v; want k_a then k_b, as before", keys)
	}
	if !keys[0].AddedAt.Equal(older.UTC()) {
		t.Fatalf("restored key was re-stamped: added_at = %v, want %v", keys[0].AddedAt, older.UTC())
	}
	access, _ := h.st.SSHAccessFor("u_fail")
	if !access.Enabled {
		t.Fatalf("access row changed by a failed save: %+v", access)
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
	assertAudited(t, h, audit.ActionSSHKeyAdd, false)
}

// Every write here is elevation-class, and a rejection audits so the Activity
// view can answer "did someone try to open a shell into this box?". A refused
// Save names each key it tried to add or remove, the same trail the separate
// key routes left before they were folded into this one.
func TestSSHWritesRequireElevation(t *testing.T) {
	h := newHarness(t)
	if err := h.st.CreateUser(store.User{ID: "u_alex", Username: "alex", DisplayName: "alex", Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := h.st.AddSSHKey(store.SSHKey{ID: "k_old", UserID: "u_alex", PublicKey: testKeyB, Fingerprint: "fp_old"}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	h.seedPassword("alex", "pass1")
	h.loginAs("alex", "pass1") // signed in, deliberately not elevated

	// Turn SSH on, drop k_old, add a new key: one of each record.
	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{
		"enabled": true, "keys": []map[string]any{{"public_key": testKeyA}},
	})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("unelevated save = %d; want 403", resp.StatusCode)
	}
	if keys, _ := h.st.ListSSHKeys("u_alex"); len(keys) != 1 || keys[0].ID != "k_old" {
		t.Fatalf("an unelevated save changed the keys: %+v", keys)
	}
	assertAudited(t, h, audit.ActionSSHAccessSet, false)
	assertAudited(t, h, audit.ActionSSHKeyAdd, false)
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// The removed routes stay removed: the screen has one write, and a leftover
// per-key route would be a second way to change keys that skips the final-state
// guard.
func TestPerKeyRoutesAreGone(t *testing.T) {
	h := hostedSSHHarness(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/me/ssh/keys"},
		{"DELETE", "/api/v1/me/ssh/keys/whatever"},
	} {
		resp := h.do(c.method, c.path, map[string]any{"public_key": testKeyA})
		resp.Body.Close()
		if resp.StatusCode != 404 && resp.StatusCode != 405 {
			t.Fatalf("%s %s = %d; want the route gone", c.method, c.path, resp.StatusCode)
		}
	}
}

// The panel state tells the UI which factor this profile makes mandatory, so the
// two cannot drift.
func TestKeyRequiredReflectsTheProfile(t *testing.T) {
	hosted := hostedSSHHarness(t)
	if got := hosted.sshState(t); !got.KeyRequired {
		t.Fatalf("hosted key_required = false; want true")
	}

	appliance := applianceSSHHarness(t)
	if got := appliance.sshState(t); got.KeyRequired {
		t.Fatalf("appliance key_required = true; want false")
	}
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
	body["keys"] = keep(h.addKey(t, testKeyA))
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
	cur := h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "require_password": false, "keys": keep(cur)})
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

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "keys": []any{}})
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
