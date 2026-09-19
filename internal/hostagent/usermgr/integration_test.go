//go:build usermgrtest

// Integration test for LinuxUserManager. Exercises real useradd + chpasswd
// against the host's /etc/passwd and /etc/shadow.
//
// Requirements:
//   - Must run as root (useradd/chpasswd touch /etc/passwd, /etc/shadow).
//   - A `moose` group must exist (created by the box build, not by host-agent).
//   - Intended for the nspawn test lane; do NOT run on a developer laptop.
//
// Invoke with: `sudo go test -tags usermgrtest ./internal/hostagent/usermgr/`
// (or via `make test-usermgr`, which runs it under systemd-nspawn).
package usermgr

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

const testUser = "moose-usermgrtest"

func TestUpsertPassword_CreateThenUpdate(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (useradd/chpasswd)")
	}
	if _, err := exec.LookPath("useradd"); err != nil {
		t.Skip("useradd not available")
	}
	t.Cleanup(func() {
		_ = exec.Command("userdel", "-r", testUser).Run()
	})

	// Pre-clean in case a previous run left the user behind.
	_ = exec.Command("userdel", "-r", testUser).Run()

	m := &LinuxUserManager{PrimaryGroup: pickGroup(t)}

	// Create path.
	if err := m.UpsertPassword(testUser, "initial-p@ss"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := user.Lookup(testUser); err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if !shadowHasHash(t, testUser) {
		t.Fatal("/etc/shadow entry missing or empty after create")
	}

	// Update path: same user, new password. Must not error.
	if err := m.UpsertPassword(testUser, "rotated-p@ss"); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestSetRole_PromoteDemoteIdempotent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (gpasswd)")
	}
	if _, err := exec.LookPath("gpasswd"); err != nil {
		t.Skip("gpasswd not available")
	}
	if _, err := user.LookupGroup("sudo"); err != nil {
		t.Skip("sudo group absent on this host (stock Debian should have it)")
	}
	t.Cleanup(func() {
		_ = exec.Command("userdel", "-r", testUser).Run()
	})
	_ = exec.Command("userdel", "-r", testUser).Run()

	m := &LinuxUserManager{PrimaryGroup: pickGroup(t)}

	// Need a user to operate on.
	if err := m.UpsertPassword(testUser, "x"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Member by default (not in sudo).
	in, err := isInGroup(testUser, "sudo")
	if err != nil {
		t.Fatalf("isInGroup: %v", err)
	}
	if in {
		t.Fatal("freshly created user should not be in sudo")
	}

	// Promote → in sudo.
	if err := m.SetRole(testUser, "admin"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	in, _ = isInGroup(testUser, "sudo")
	if !in {
		t.Fatal("after admin SetRole, user must be in sudo")
	}

	// Promote again → still in sudo, no error (gpasswd -a is idempotent).
	if err := m.SetRole(testUser, "admin"); err != nil {
		t.Fatalf("promote re-add: %v", err)
	}

	// Demote → not in sudo.
	if err := m.SetRole(testUser, "member"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	in, _ = isInGroup(testUser, "sudo")
	if in {
		t.Fatal("after member SetRole, user must not be in sudo")
	}

	// Demote again → no error (pre-check makes it idempotent).
	if err := m.SetRole(testUser, "member"); err != nil {
		t.Fatalf("demote-when-already-out: %v", err)
	}
}

func TestDeleteUser_CreateDeleteIdempotent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (useradd/userdel)")
	}
	if _, err := exec.LookPath("userdel"); err != nil {
		t.Skip("userdel not available")
	}
	t.Cleanup(func() {
		_ = exec.Command("userdel", "-r", "-f", testUser).Run()
	})
	_ = exec.Command("userdel", "-r", "-f", testUser).Run()

	m := &LinuxUserManager{PrimaryGroup: pickGroup(t)}

	// Create.
	if err := m.UpsertPassword(testUser, "x"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := user.Lookup(testUser); err != nil {
		t.Fatalf("user not created: %v", err)
	}

	// Delete: user gone from /etc/passwd.
	if err := m.DeleteUser(testUser); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := user.Lookup(testUser); err == nil {
		t.Fatal("user still present after DeleteUser")
	}

	// Delete again: idempotent — unknown user must return nil
	// (BRAIN_HOST_PROTOCOL.md # Auth endpoints).
	if err := m.DeleteUser(testUser); err != nil {
		t.Fatalf("delete-when-already-gone: %v", err)
	}
}

// pickGroup returns a primary group that exists on the host. Prefers "moose"
// (the real default), falls back to "nogroup" so the test still runs on a
// stock Debian nspawn that hasn't been provisioned with the moose group yet.
func pickGroup(t *testing.T) string {
	t.Helper()
	for _, g := range []string{"moose", "nogroup", "users"} {
		if _, err := user.LookupGroup(g); err == nil {
			return g
		}
	}
	t.Fatal("no usable primary group on this host (tried moose, nogroup, users)")
	return ""
}

func shadowHasHash(t *testing.T, slug string) bool {
	t.Helper()
	b, err := os.ReadFile("/etc/shadow")
	if err != nil {
		t.Fatalf("read /etc/shadow: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) < 2 || fields[0] != slug {
			continue
		}
		// Field 2 is the hash. Empty / "!" / "*" all mean no usable password.
		h := fields[1]
		return h != "" && h != "!" && h != "*"
	}
	return false
}
