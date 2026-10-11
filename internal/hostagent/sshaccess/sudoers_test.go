package sshaccess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

// ownerOn is the request the brain sends for the hosted owner turning SSH on.
func ownerOn(user string) protocol.SetSSHAccessRequest {
	return protocol.SetSSHAccessRequest{
		User: user, Enabled: true, AuthorizedKeys: []string{testKey}, PasswordlessSudo: true,
	}
}

func sudoersExists(t *testing.T, m *Manager) bool {
	t.Helper()
	_, err := os.Stat(m.sudoersPath())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat sudo rule: %v", err)
	}
	return err == nil
}

// writeRule plants a rule file the way an earlier run (or a crash) would have
// left it, without going through SetAccess.
func writeRule(t *testing.T, m *Manager, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(m.sudoersPath()), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(m.sudoersPath(), []byte(body), 0o440); err != nil {
		t.Fatalf("plant rule: %v", err)
	}
}

// The hosted owner's rule is one read-only line naming the account, and it is
// checked by visudo as a candidate before it is installed. sudo refuses a file in
// sudoers.d that anyone can write, and a broken file there breaks sudo for every
// account on the box, so both the mode and the check order matter.
func TestOwnerRuleIsCheckedThenInstalledReadOnly(t *testing.T) {
	m, ran := newManager(t, t.TempDir())
	mustSet(t, m, ownerOn("alex"))

	b, err := os.ReadFile(m.sudoersPath())
	if err != nil {
		t.Fatalf("rule not written: %v", err)
	}
	if !strings.Contains(string(b), "\nalex ALL=(ALL) NOPASSWD: ALL\n") {
		t.Fatalf("rule does not name alex with NOPASSWD:\n%s", b)
	}
	fi, err := os.Stat(m.sudoersPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o440 {
		t.Fatalf("rule mode = %o; want 440", fi.Mode().Perm())
	}

	candidate := m.sudoersPath() + ".new"
	if !containsCmd(*ran, "visudo -c -f "+candidate) {
		t.Fatalf("the candidate was not checked with visudo -cf before install: %v", *ran)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("the candidate was left behind: %v", err)
	}

	// The check runs before the daemon is touched, so a refused rule never
	// reaches a reload.
	var visudoAt, enableAt = -1, -1
	for i, c := range *ran {
		if strings.HasPrefix(c, "visudo") && visudoAt < 0 {
			visudoAt = i
		}
		if strings.HasPrefix(c, "systemctl enable") && enableAt < 0 {
			enableAt = i
		}
	}
	if visudoAt < 0 || enableAt < 0 || visudoAt > enableAt {
		t.Fatalf("visudo at %d, enable at %d; want the check first: %v", visudoAt, enableAt, *ran)
	}
}

// Without the flag no rule is written and visudo never runs. This is every call
// the appliance makes, and every call for an account that is not the hosted
// owner: they keep sudo with a password.
func TestNoRuleWithoutTheFlag(t *testing.T) {
	m, ran := newManager(t, t.TempDir())
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey}, RequirePassword: true,
	})
	if sudoersExists(t, m) {
		t.Fatal("a sudo rule was written for an account the brain did not flag")
	}
	if containsCmd(*ran, "visudo") {
		t.Fatalf("visudo ran with no rule to write: %v", *ran)
	}
}

// Turning SSH off removes the rule. So does an enabled call without the flag,
// which is what the brain sends when the account is no longer the owner.
func TestTurningSSHOffRemovesTheRule(t *testing.T) {
	for name, off := range map[string]protocol.SetSSHAccessRequest{
		"disabled":         {User: "alex", Enabled: false},
		"disabled, flag":   {User: "alex", Enabled: false, PasswordlessSudo: true},
		"enabled, no flag": {User: "alex", Enabled: true, AuthorizedKeys: []string{testKey}},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newManager(t, t.TempDir())
			mustSet(t, m, ownerOn("alex"))
			mustSet(t, m, off)
			if sudoersExists(t, m) {
				t.Fatal("the sudo rule survived")
			}
		})
	}
}

// A call about another account never takes the owner's rule away. The file
// holds one owner, and it is removed only by a call for the account it names.
func TestACallForAnotherAccountKeepsTheRule(t *testing.T) {
	m, _ := newManager(t, t.TempDir())
	mustSet(t, m, ownerOn("alex"))

	mustSet(t, m, protocol.SetSSHAccessRequest{User: "bo", Enabled: true, AuthorizedKeys: []string{testKey}})
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "bo", Enabled: false})

	name, _, err := m.readSudoersUser()
	if err != nil {
		t.Fatalf("read rule: %v", err)
	}
	if name != "alex" {
		t.Fatalf("rule names %q after calls for bo; want alex", name)
	}
}

// A rule visudo refuses is never installed, and the keys and drop-in this call
// already wrote go back with it. The brain rolls its row back on this error, so
// leaving them would give the account SSH the brain no longer records.
func TestVisudoRejectionPutsEverythingBack(t *testing.T) {
	keys := t.TempDir()
	m, _ := newManager(t, keys)
	// bo already has SSH on, so the drop-in exists and has to come back as it
	// was, not just disappear.
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "bo", Enabled: true, AuthorizedKeys: []string{testKey}})
	before := readDropInFile(t, m)

	var ran []string
	m.Runner = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "visudo" {
			return []byte("syntax error near line 3"), os.ErrInvalid
		}
		return nil, nil
	}
	err := m.SetAccess(ownerOn("alex"))
	if err == nil {
		t.Fatal("SetAccess accepted a rule visudo rejected")
	}
	if !strings.Contains(err.Error(), "visudo rejected") {
		t.Fatalf("error does not name the visudo refusal: %v", err)
	}

	if sudoersExists(t, m) {
		t.Error("a rule visudo rejected was installed")
	}
	if _, err := os.Stat(m.sudoersPath() + ".new"); !os.IsNotExist(err) {
		t.Errorf("the rejected candidate was left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(keys, "alex")); !os.IsNotExist(err) {
		t.Errorf("alex's key file survived a rejected rule: %v", err)
	}
	if got := readDropInFile(t, m); got != before {
		t.Errorf("the drop-in was not put back:\n%s\nwant:\n%s", got, before)
	}
	if containsCmd(ran, "systemctl") {
		t.Errorf("the daemon was touched after a rejected rule: %v", ran)
	}
}

// A name that would mean something else in sudoers syntax is refused, even
// though it is a safe path. "ALL" would grant every account; visudo accepts it.
func TestRuleRefusesANameThatIsSudoersSyntax(t *testing.T) {
	for _, name := range []string{"ALL", "%sudo", "alex,bo", "a b"} {
		t.Run(name, func(t *testing.T) {
			keys := t.TempDir()
			m, _ := newManager(t, keys)
			if err := m.SetAccess(ownerOn(name)); err == nil {
				t.Fatalf("SetAccess wrote a sudo rule for %q", name)
			}
			if sudoersExists(t, m) {
				t.Fatalf("a rule for %q is on disk", name)
			}
			if _, err := os.Stat(filepath.Join(keys, name)); !os.IsNotExist(err) {
				t.Fatalf("the key file for %q survived the refusal: %v", name, err)
			}
		})
	}
}

// A failed first enable takes the new rule back along with the keys and the
// drop-in. Without that the owner would keep prompt-free root on an account the
// brain has just rolled back to "SSH off".
func TestFailedDaemonStartRemovesTheNewRule(t *testing.T) {
	m, _ := failingSystemctl(t)
	if err := m.SetAccess(ownerOn("alex")); err == nil {
		t.Fatal("SetAccess reported success though the daemon never started")
	}
	if sudoersExists(t, m) {
		t.Fatal("the sudo rule survived a failed daemon start")
	}
}

// A failed disable puts the rule back, read-only, because the brain's rollback
// puts its row back to enabled and the owner keeps SSH.
func TestFailedDaemonStopRestoresTheRule(t *testing.T) {
	m, _ := newManager(t, t.TempDir())
	mustSet(t, m, ownerOn("alex"))

	m.Runner = func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" {
			return []byte("Failed to stop ssh.service"), os.ErrInvalid
		}
		return nil, nil
	}
	if err := m.SetAccess(protocol.SetSSHAccessRequest{User: "alex", Enabled: false}); err == nil {
		t.Fatal("SetAccess reported success though the daemon never stopped")
	}
	name, exists, err := m.readSudoersUser()
	if err != nil || !exists || name != "alex" {
		t.Fatalf("rule after a failed disable: name %q, exists %v, err %v; want alex's rule back", name, exists, err)
	}
	fi, err := os.Stat(m.sudoersPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o440 {
		t.Fatalf("restored rule mode = %o; want 440", fi.Mode().Perm())
	}
}

// At start the rule is kept only for an account the drop-in still enables.
// Anything else goes: so a crash between steps, or an /etc put back from an
// older copy, never leaves root with no prompt for an account with SSH off.
func TestReconcileSudoAtStart(t *testing.T) {
	cases := map[string]struct {
		enable      []string // accounts with SSH on, through SetAccess
		rule        string   // planted file body; "" plants nothing
		wantRemoved bool
		wantFile    bool
	}{
		"no rule":                          {enable: []string{"alex"}, wantFile: false},
		"rule for an enabled account":      {enable: []string{"alex"}, rule: renderSudoers("alex"), wantFile: true},
		"rule for an account with SSH off": {enable: []string{"bo"}, rule: renderSudoers("alex"), wantRemoved: true},
		"rule and no account enabled":      {rule: renderSudoers("alex"), wantRemoved: true},
		"moose's line plus another rule":   {enable: []string{"alex"}, rule: renderSudoers("alex") + "bo ALL=(ALL) NOPASSWD: ALL\n", wantRemoved: true},
		"a file moose did not write":       {enable: []string{"alex"}, rule: "%sudo ALL=(ALL) NOPASSWD: ALL\n", wantRemoved: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, _ := newManager(t, t.TempDir())
			for _, u := range c.enable {
				mustSet(t, m, protocol.SetSSHAccessRequest{User: u, Enabled: true, AuthorizedKeys: []string{testKey}})
			}
			if c.rule != "" {
				writeRule(t, m, c.rule)
			}
			removed, err := m.ReconcileSudoAtStart()
			if err != nil {
				t.Fatalf("ReconcileSudoAtStart: %v", err)
			}
			if removed != c.wantRemoved {
				t.Fatalf("removed = %v; want %v", removed, c.wantRemoved)
			}
			if got := sudoersExists(t, m); got != c.wantFile {
				t.Fatalf("rule on disk = %v; want %v", got, c.wantFile)
			}
		})
	}
}

// A candidate a crash left behind is removed at start. sudo would skip it (its
// name has a dot), but it is a file in /etc that nothing else would ever clean.
func TestReconcileSudoAtStartRemovesALeftoverCandidate(t *testing.T) {
	m, _ := newManager(t, t.TempDir())
	writeRule(t, m, renderSudoers("alex"))
	if err := os.Rename(m.sudoersPath(), m.sudoersPath()+".new"); err != nil {
		t.Fatalf("plant candidate: %v", err)
	}
	if _, err := m.ReconcileSudoAtStart(); err != nil {
		t.Fatalf("ReconcileSudoAtStart: %v", err)
	}
	if _, err := os.Stat(m.sudoersPath() + ".new"); !os.IsNotExist(err) {
		t.Fatalf("the leftover candidate is still there: %v", err)
	}
}

// When the SSH state cannot be read, the rule is removed and the error is
// reported. Not knowing who has SSH on is not a reason to keep prompt-free root.
func TestReconcileSudoAtStartFailsClosed(t *testing.T) {
	m, _ := newManager(t, t.TempDir())
	if err := os.MkdirAll(filepath.Dir(m.dropInPath()), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A drop-in without moose's header: readDropIn refuses it.
	if err := os.WriteFile(m.dropInPath(), []byte("AllowUsers alex\n"), 0o644); err != nil {
		t.Fatalf("plant drop-in: %v", err)
	}
	writeRule(t, m, renderSudoers("alex"))

	removed, err := m.ReconcileSudoAtStart()
	if err == nil {
		t.Fatal("an unreadable SSH state was not reported")
	}
	if !removed || sudoersExists(t, m) {
		t.Fatalf("removed = %v, rule on disk = %v; want the rule gone", removed, sudoersExists(t, m))
	}
}
