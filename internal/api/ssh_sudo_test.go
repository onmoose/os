package api

import (
	"testing"
	"time"

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
	h.addMember("u_bob", deleteFailUser, "pw-bob")
	h.makeOwner(t, "u_bob")
	if err := h.st.SetSSHAccess("u_bob", true, false); err != nil {
		t.Fatalf("seed ssh access: %v", err)
	}
	if err := h.st.AddSSHKey(store.SSHKey{
		ID: "k_bob", UserID: "u_bob", PublicKey: testKeyA, Fingerprint: "SHA256:bob", AddedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}

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
