package api

import (
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// makeOwner records userID as the box owner, the way the portal sign-in does
// (recordSSOOwner), without driving the handshake.
func (h *harness) makeOwner(t *testing.T, userID string) {
	t.Helper()
	if err := h.st.SetBoxMeta(store.BoxMetaOwnerUserID, userID); err != nil {
		t.Fatalf("record owner: %v", err)
	}
}

// addOwnerWithSSH creates a second admin, records it as the box owner and
// gives it SSH on with one key, all straight in the store: the tests that use it
// are about another admin acting on the owner, not the owner's own session.
func (h *harness) addOwnerWithSSH(t *testing.T, id, username string) {
	t.Helper()
	if err := h.st.CreateUser(store.User{
		ID: id, Username: username, DisplayName: username, Role: store.RoleAdmin, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h.makeOwner(t, id)
	if err := h.st.SetSSHAccess(id, true, false); err != nil {
		t.Fatalf("seed ssh access: %v", err)
	}
	if err := h.st.AddSSHKey(store.SSHKey{
		ID: "k_" + id, UserID: id, PublicKey: testKeyA, Fingerprint: "SHA256:" + id, AddedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}
}

func (h *harness) setRole(t *testing.T, id, role string) int {
	t.Helper()
	resp := h.do("PATCH", "/api/v1/users/"+id, map[string]any{"role": role})
	resp.Body.Close()
	return resp.StatusCode
}

func lastSSHCall(t *testing.T, h *harness) protocol.SetSSHAccessRequest {
	t.Helper()
	calls := h.sshCallsSnapshot()
	if len(calls) == 0 {
		t.Fatal("no ssh call reached the host")
	}
	return calls[len(calls)-1]
}

// The hosted owner's password is random and unknown, so sudo with a password is
// a dead end for them. With SSH on, the brain asks host-agent for a prompt-free
// rule, and turning SSH off takes it away (DECISIONS.md 2026-10-11).
func TestHostedOwnerGetsPasswordlessSudoWhileSSHIsOn(t *testing.T) {
	h := hostedSSHHarness(t)
	h.makeOwner(t, "u_alex")
	h.addKey(t, testKeyA)
	cur := h.enable(t)

	if on := lastSSHCall(t, h); !on.Enabled || !on.PasswordlessSudo {
		t.Fatalf("enable for the hosted owner = %+v; want enabled with passwordless_sudo", on)
	}

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": false, "keys": keep(cur)})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("disable = %d", resp.StatusCode)
	}
	if off := lastSSHCall(t, h); off.Enabled || off.PasswordlessSudo {
		t.Fatalf("disable for the hosted owner = %+v; want no passwordless_sudo", off)
	}
}

// Accounts the owner creates have real passwords, so they keep the sudo prompt.
// So does every account on a hosted box with no owner recorded yet.
func TestHostedNonOwnerGetsNoSudoFlag(t *testing.T) {
	for name, owner := range map[string]string{
		"another account is the owner": "u_owner",
		"no owner recorded":            "",
	} {
		t.Run(name, func(t *testing.T) {
			h := hostedSSHHarness(t)
			if owner != "" {
				h.makeOwner(t, owner)
			}
			h.addKey(t, testKeyA)
			h.enable(t)
			if on := lastSSHCall(t, h); !on.Enabled || on.PasswordlessSudo {
				t.Fatalf("enable = %+v; want enabled with no passwordless_sudo", on)
			}
		})
	}
}

// The appliance never sends the flag, even for an account the box meta names:
// admins there have the password they chose and keep sudo with it.
func TestApplianceNeverSendsTheSudoFlag(t *testing.T) {
	h := applianceSSHHarness(t)
	h.makeOwner(t, "u_alex")
	h.addKey(t, testKeyA)
	h.enable(t)
	for _, c := range h.sshCallsSnapshot() {
		if c.PasswordlessSudo {
			t.Fatalf("the appliance sent passwordless_sudo: %+v", c)
		}
	}
}

// Deleting the owner revokes the rule with their SSH, and a delete that fails
// on the host puts it back with the compensating re-push. The re-push goes
// through the same applySSH as a Save, so it cannot forget the flag.
func TestOwnerDeleteRevokesAndItsRollbackRestoresTheSudoFlag(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addOwnerWithSSH(t, "u_bob", deleteFailUser)

	resp := h.do("DELETE", "/api/v1/users/u_bob", nil)
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("delete with a failing host = %d; want 502", resp.StatusCode)
	}

	calls := h.sshCallsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("want a revoke and a re-push; got %+v", calls)
	}
	if revoke := calls[0]; revoke.Enabled || revoke.PasswordlessSudo {
		t.Fatalf("revoke = %+v; want disabled with no passwordless_sudo", revoke)
	}
	if repush := calls[1]; !repush.Enabled || !repush.PasswordlessSudo {
		t.Fatalf("re-push = %+v; want the owner's rule back", repush)
	}
}

// The rule names the account, not the sudo group, so demoting the owner has to
// remove it. Otherwise another admin's demotion would leave a member with root
// and no prompt. Promoting them back puts it back.
func TestDemotingTheOwnerRemovesTheSudoRule(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addOwnerWithSSH(t, "u_own", "own")

	if code := h.setRole(t, "u_own", store.RoleMember); code != 200 {
		t.Fatalf("demote owner = %d; want 200", code)
	}
	down := lastSSHCall(t, h)
	if down.User != "own" || !down.Enabled || down.PasswordlessSudo {
		t.Fatalf("push after demotion = %+v; want SSH kept on with no passwordless_sudo", down)
	}

	if code := h.setRole(t, "u_own", store.RoleAdmin); code != 200 {
		t.Fatalf("promote owner = %d; want 200", code)
	}
	if up := lastSSHCall(t, h); up.User != "own" || !up.Enabled || !up.PasswordlessSudo {
		t.Fatalf("push after promotion = %+v; want the owner's rule back", up)
	}
}

// A role change for anyone but the owner, or for an owner with SSH off, sends
// nothing to the SSH seam: none of them can hold a rule.
func TestRoleChangeOfANonOwnerDoesNotTouchSSH(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addOwnerWithSSH(t, "u_own", "own")
	h.addMember("u_bob", "bob", "pw-bob")
	if err := h.st.SetSSHAccess("u_bob", true, false); err != nil {
		t.Fatalf("seed ssh access: %v", err)
	}

	if code := h.setRole(t, "u_bob", store.RoleAdmin); code != 200 {
		t.Fatalf("promote bob = %d; want 200", code)
	}
	if calls := h.sshCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("a non-owner's role change reached the ssh seam: %+v", calls)
	}
}

// When the push that removes the rule fails, host-agent keeps the rule as it
// was, so the demotion is undone on both sides and audited as a failure. A
// member must never be left holding the owner's rule.
func TestFailedSudoPushUndoesTheDemotion(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addOwnerWithSSH(t, "u_fail", sshFailUser)

	if code := h.setRole(t, "u_fail", store.RoleMember); code != 502 {
		t.Fatalf("demote with a failing ssh push = %d; want 502", code)
	}
	u, err := h.st.GetUser("u_fail")
	if err != nil {
		t.Fatalf("get owner: %v", err)
	}
	if u.Role != store.RoleAdmin {
		t.Fatalf("owner role after a failed push = %q; want admin back", u.Role)
	}
	h.pmu.Lock()
	hostRole := h.roles[sshFailUser]
	h.pmu.Unlock()
	if hostRole != store.RoleAdmin {
		t.Fatalf("host role after a failed push = %q; want admin back", hostRole)
	}
	assertAudited(t, h, audit.ActionUserRoleChange, false)
}
