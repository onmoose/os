package sshaccess

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onmoose/moose/internal/protocol"
)

// newManager builds a Manager pointed at a temp drop-in, with sshd and systemctl
// faked. Every command is recorded so the daemon-lifecycle assertions can read
// what would have run.
// testKey is a real ed25519 public key, so anything that parses it agrees with
// what a box would see.
const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIfJnhGAA/rWbxmvMGuZvXV6in+czTK5F8Ie7QGTKOT+ alex@laptop"

func newManager(t *testing.T, keysDir string) (*Manager, *[]string) {
	t.Helper()
	var ran []string
	m := &Manager{
		DropInPath: filepath.Join(t.TempDir(), "sshd_config.d", "moose-allowed.conf"),
		KeysDir:    keysDir,
		Runner: func(name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			if name == "systemctl" && len(args) > 0 && args[0] == "is-active" {
				return []byte("active\n"), nil
			}
			return nil, nil
		},
	}
	return m, &ran
}

func mustSet(t *testing.T, m *Manager, req protocol.SetSSHAccessRequest) {
	t.Helper()
	if err := m.SetAccess(req); err != nil {
		t.Fatalf("SetAccess(%+v): %v", req, err)
	}
}

func readDropInFile(t *testing.T, m *Manager) string {
	t.Helper()
	b, err := os.ReadFile(m.dropInPath())
	if err != nil {
		t.Fatalf("read drop-in: %v", err)
	}
	return string(b)
}

// The optional factor must render as a comma-joined AuthenticationMethods, which
// sshd reads as "all of these are required". A space-separated list would mean
// "any of these" — the exact misreading that would turn the second lock into a
// second door and undo the hosted key requirement (AUTH.md # Device access).
func TestMethodsAreRequiredNotAlternatives(t *testing.T) {
	cases := map[string]struct {
		acct account
		want string
	}{
		"key only":         {account{Username: "a", KeyCount: 1}, "publickey"},
		"key and password": {account{Username: "a", KeyCount: 1, RequirePassword: true}, "publickey,password"},
		"password only":    {account{Username: "a", KeyCount: 0}, "password"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := methods(c.acct)
			if got != c.want {
				t.Fatalf("methods = %q; want %q", got, c.want)
			}
			if strings.Contains(got, " ") {
				t.Fatalf("methods %q is space-separated; sshd would read that as "+
					"'any of these', making the second factor an alternative", got)
			}
		})
	}
}

// An empty enabled set must lock everyone out explicitly. Omitting AllowUsers
// means "every account" to sshd, which is the opposite of what an empty set
// means, so a hand-started sshd would admit the whole machine.
func TestRenderEmptySetDeniesEveryone(t *testing.T) {
	out := render(nil, "/etc/ssh/moose-authorized-keys")
	if !strings.Contains(out, "DenyUsers *") {
		t.Fatalf("empty set did not deny everyone:\n%s", out)
	}
	if strings.Contains(out, "AllowUsers") {
		t.Fatalf("empty set wrote an AllowUsers line:\n%s", out)
	}
}

// Every global keyword has to precede the first Match block: sshd scopes
// everything after a Match line to that block, so a global written afterwards
// would silently become the last account's policy.
func TestRenderGlobalsPrecedeMatchBlocks(t *testing.T) {
	out := render([]account{{Username: "alex", KeyCount: 1}}, "/etc/ssh/moose-authorized-keys")
	firstMatch := strings.Index(out, "Match User ")
	if firstMatch < 0 {
		t.Fatalf("no Match block rendered:\n%s", out)
	}
	for _, global := range []string{"PermitRootLogin", "PasswordAuthentication", "AllowUsers", "PubkeyAuthentication", "KbdInteractiveAuthentication"} {
		if at := strings.Index(out, global); at < 0 || at > firstMatch {
			t.Fatalf("global %q at %d is not before the first Match at %d:\n%s", global, at, firstMatch, out)
		}
	}
}

// Enabling the first account starts the daemon and disabling the last stops it —
// this is what opens and closes :22, and on hosted it is the only control over
// that port (ENVIRONMENT.md # Access & files).
func TestDaemonFollowsTheEnabledSet(t *testing.T) {
	home := t.TempDir()
	m, ran := newManager(t, home)

	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop"},
	})
	if !containsCmd(*ran, "systemctl enable --now") {
		t.Fatalf("first enable did not start the daemon: %v", *ran)
	}

	*ran = nil
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "bo", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY bo@desktop"},
	})
	if containsCmd(*ran, "systemctl disable") {
		t.Fatalf("adding a second account stopped the daemon: %v", *ran)
	}

	*ran = nil
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "alex", Enabled: false})
	if containsCmd(*ran, "systemctl disable") {
		t.Fatalf("daemon stopped while bo was still enabled: %v", *ran)
	}

	*ran = nil
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "bo", Enabled: false})
	if !containsCmd(*ran, "systemctl disable --now") {
		t.Fatalf("disabling the last account did not stop the daemon: %v", *ran)
	}
	if got := readDropInFile(t, m); !strings.Contains(got, "DenyUsers *") {
		t.Fatalf("drop-in did not close after the last account left:\n%s", got)
	}
}

// A running sshd is reloaded, never restarted: a restart drops live sessions,
// and the admin fixing something over SSH is exactly who is connected.
func TestRunningDaemonIsReloadedNotRestarted(t *testing.T) {
	m, ran := newManager(t, t.TempDir())
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop"},
	})
	if !containsCmd(*ran, "systemctl reload") {
		t.Fatalf("config change did not reload sshd: %v", *ran)
	}
	if containsCmd(*ran, "systemctl restart") {
		t.Fatalf("config change restarted sshd, dropping live sessions: %v", *ran)
	}
}

// The rendered config must be validated before it is allowed to take effect. A
// bad render that reaches a reload takes sshd down and locks out the accounts
// that were working a moment ago.
func TestBadRenderIsRefusedBeforeReload(t *testing.T) {
	var ran []string
	m := &Manager{
		DropInPath: filepath.Join(t.TempDir(), "moose-allowed.conf"),
		Runner: func(name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			if name == "sshd" {
				return []byte("bad configuration option"), os.ErrInvalid
			}
			return nil, nil
		},
		KeysDir: t.TempDir(),
	}
	err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop"},
	})
	if err == nil {
		t.Fatal("SetAccess accepted a config sshd -t rejected")
	}
	if containsCmd(ran, "systemctl reload") || containsCmd(ran, "systemctl enable") {
		t.Fatalf("daemon was touched after a failed validation: %v", ran)
	}
}

// The enabled set survives a restart because it is read back from the rendered
// file — host-agent keeps no state of its own.
func TestStateIsReadBackFromTheRenderedFile(t *testing.T) {
	home := t.TempDir()
	m, _ := newManager(t, home)
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, RequirePassword: true,
		AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop", "ssh-ed25519 AAAAKEY2 alex@desktop"},
	})

	fresh := &Manager{DropInPath: m.DropInPath, KeysDir: m.KeysDir, Runner: m.Runner}
	st, err := fresh.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if len(st.Users) != 1 {
		t.Fatalf("users = %d; want 1", len(st.Users))
	}
	got := st.Users[0]
	if got.Username != "alex" || got.KeyCount != 2 || !got.RequirePassword {
		t.Fatalf("recovered %+v; want alex with 2 keys and require_password", got)
	}
	if !st.DaemonRunning {
		t.Fatal("DaemonRunning false while systemctl reported active")
	}
}

// A config a person wrote by hand is never overwritten: doing so would destroy
// their work and could lock them out of their own box.
func TestUnmanagedConfigIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "moose-allowed.conf")
	if err := os.WriteFile(path, []byte("AllowUsers someone\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &Manager{DropInPath: path, Runner: func(string, ...string) ([]byte, error) { return nil, nil }}
	if err := m.SetAccess(protocol.SetSSHAccessRequest{User: "alex", Enabled: true, RequirePassword: true}); err == nil {
		t.Fatal("SetAccess overwrote a config it did not write")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(b) != "AllowUsers someone\n" {
		t.Fatalf("hand-written config was modified: %q", b)
	}
}

// moose's keys live in a root-owned file, never in the account's home. The home
// directory is a path the account controls and can replace with a symlink between
// any check and any use, so writing there as root is a privilege-escalation path
// rather than a hardening problem. Owning the file removes the user from it.
func TestKeysAreWrittenOutsideTheUsersHome(t *testing.T) {
	keysDir := t.TempDir()
	m, _ := newManager(t, keysDir)

	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})
	assertContains(t, filepath.Join(keysDir, "alex"), testKey)

	// And sshd is pointed at both that file and the user's own, so keys they added
	// from their shell keep working without moose ever touching that file.
	conf := readDropInFile(t, m)
	if !strings.Contains(conf, "AuthorizedKeysFile "+filepath.Join(keysDir, "alex")+" .ssh/authorized_keys") {
		t.Fatalf("drop-in does not point sshd at both key files:\n%s", conf)
	}
}

// A username that could climb out of the managed directory is refused rather
// than joined blindly.
func TestUnsafeUsernameIsRefused(t *testing.T) {
	m, _ := newManager(t, t.TempDir())
	for _, name := range []string{"../root", "a/b", ".", ".."} {
		err := m.SetAccess(protocol.SetSSHAccessRequest{
			User: name, Enabled: true, AuthorizedKeys: []string{testKey},
		})
		if err == nil {
			t.Fatalf("SetAccess accepted username %q", name)
		}
	}
}

// Disabling an account removes its key file. A stale key on a disabled account
// is a credential nobody is tracking.
func TestDisablingRemovesTheKeyFile(t *testing.T) {
	keysDir := t.TempDir()
	m, _ := newManager(t, keysDir)
	keyFile := filepath.Join(keysDir, "alex")

	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("keys not written: %v", err)
	}
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "alex", Enabled: false})
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatalf("key file survived a disable: %v", err)
	}
}

// Two accounts enabled at once must both survive. Every call re-renders one
// drop-in that holds the whole enabled set, so an unsynchronised read-modify-write
// would silently drop whichever account lost the race.
func TestConcurrentEnablesDoNotLoseAnAccount(t *testing.T) {
	m, _ := newManager(t, t.TempDir())

	var wg sync.WaitGroup
	for _, name := range []string{"alex", "bo", "cy", "di"} {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if err := m.SetAccess(protocol.SetSSHAccessRequest{
				User: u, Enabled: true, AuthorizedKeys: []string{testKey},
			}); err != nil {
				t.Errorf("SetAccess(%s): %v", u, err)
			}
		}(name)
	}
	wg.Wait()

	st, err := m.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if len(st.Users) != 4 {
		t.Fatalf("enabled accounts = %d; want 4 — a concurrent write dropped one", len(st.Users))
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), want) {
		t.Fatalf("%s does not contain %q:\n%s", path, want, b)
	}
}

func containsCmd(ran []string, prefix string) bool {
	for _, c := range ran {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// A rejected render must leave the key file exactly as it was, not only the
// drop-in. The keys are written first on purpose — an account must never be named
// in a config a moment before the key that authenticates it exists — which means
// the key file is already committed when the config step runs. Without the undo,
// a failed validation leaves the two disagreeing.
//
// The first-ever enable is the case that matters most: nothing is rendered, so a
// key file left behind belongs to an account the config does not name at all.
func TestRejectedRenderLeavesNoKeyFileBehind(t *testing.T) {
	keys := t.TempDir()
	m := &Manager{
		DropInPath: filepath.Join(t.TempDir(), "moose-allowed.conf"),
		KeysDir:    keys,
		Runner: func(name string, args ...string) ([]byte, error) {
			if name == "sshd" {
				return []byte("bad configuration option"), os.ErrInvalid
			}
			return nil, nil
		},
	}
	if err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	}); err == nil {
		t.Fatal("SetAccess accepted a config sshd -t rejected")
	}
	if _, err := os.Stat(filepath.Join(keys, "alex")); !os.IsNotExist(err) {
		t.Fatalf("key file survived a rejected render: stat err = %v", err)
	}
}

// The same undo, for an account that already had keys: a rejected render must not
// leave the new set live under the old config. Here the account is enabled and
// working, and a second call that sshd refuses tries to replace its keys.
func TestRejectedRenderRestoresThePreviousKeys(t *testing.T) {
	keys := t.TempDir()
	m, _ := newManager(t, keys)
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})

	const replacement = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB alex@desktop"
	m.Runner = func(name string, args ...string) ([]byte, error) {
		if name == "sshd" {
			return []byte("bad configuration option"), os.ErrInvalid
		}
		return nil, nil
	}
	if err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{replacement},
	}); err == nil {
		t.Fatal("SetAccess accepted a config sshd -t rejected")
	}

	got, err := os.ReadFile(filepath.Join(keys, "alex"))
	if err != nil {
		t.Fatalf("key file missing after a rejected render: %v", err)
	}
	if strings.Contains(string(got), "alex@desktop") {
		t.Fatalf("the rejected render's keys are live on the host: %q", got)
	}
	if !strings.Contains(string(got), "alex@laptop") {
		t.Fatalf("the previous keys were not restored: %q", got)
	}
}

// failingSystemctl is a runner where sshd validates fine and every systemctl
// call fails. That is the shape of a real daemon failure: the render is good,
// so both file writes commit, and the box only refuses at the last step.
func failingSystemctl(t *testing.T) (*Manager, string) {
	t.Helper()
	keys := t.TempDir()
	m := &Manager{
		DropInPath: filepath.Join(t.TempDir(), "sshd_config.d", "moose-allowed.conf"),
		KeysDir:    keys,
		Runner: func(name string, args ...string) ([]byte, error) {
			if name == "systemctl" {
				return []byte("Failed to start ssh.service"), os.ErrInvalid
			}
			return nil, nil
		},
	}
	return m, keys
}

// A first-ever enable whose daemon call fails must leave nothing behind.
//
// The brain rolls its row back on this error, so an account left in the drop-in
// with its key on disk is access nobody has a record of. It does not stay
// dormant either: the next account to enable SSH starts the daemon, and this one
// comes up with it — granted by a call about a different user.
func TestFailedDaemonStartLeavesNoAccountBehind(t *testing.T) {
	m, keys := failingSystemctl(t)

	if err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	}); err == nil {
		t.Fatal("SetAccess reported success though the daemon never started")
	}

	if _, err := os.Stat(filepath.Join(keys, "alex")); !os.IsNotExist(err) {
		t.Errorf("key file survived a failed daemon start: stat err = %v", err)
	}
	if _, err := os.Stat(m.dropInPath()); !os.IsNotExist(err) {
		t.Errorf("drop-in survived a failed first enable: stat err = %v", err)
	}
}

// A failed disable has to keep the account, because the brain's rollback puts
// its row back to enabled. Dropping the entry here would leave the dashboard
// showing access the host is no longer configured for, and the account's key
// gone with it.
func TestFailedDaemonStopKeepsThePreviousAccount(t *testing.T) {
	keys := t.TempDir()
	m, _ := newManager(t, keys)
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})

	m.Runner = func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" {
			return []byte("Failed to stop ssh.service"), os.ErrInvalid
		}
		return nil, nil
	}
	if err := m.SetAccess(protocol.SetSSHAccessRequest{User: "alex", Enabled: false}); err == nil {
		t.Fatal("SetAccess reported success though the daemon never stopped")
	}

	got := readDropInFile(t, m)
	if !strings.Contains(got, "Match User alex") {
		t.Errorf("the account was dropped though the disable failed; the brain still has it enabled:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(keys, "alex")); err != nil {
		t.Errorf("key file removed though the disable failed: %v", err)
	}
}

// The undo reconciles the daemon, it does not only put the files back. A failed
// call can still have changed the run state, so leaving sshd running a set that
// no longer exists on disk would be half an undo.
func TestUndoReconcilesTheDaemonToThePreviousSet(t *testing.T) {
	var ran []string
	keys := t.TempDir()
	m := &Manager{
		DropInPath: filepath.Join(t.TempDir(), "sshd_config.d", "moose-allowed.conf"),
		KeysDir:    keys,
		Runner: func(name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			// Only the enable fails, so the undo's own systemctl call can run and
			// be observed. A runner that failed every call could not show this.
			if name == "systemctl" && len(args) > 0 && args[0] == "enable" {
				return []byte("Failed to start ssh.service"), os.ErrInvalid
			}
			return nil, nil
		},
	}

	if err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	}); err == nil {
		t.Fatal("SetAccess reported success though the daemon never started")
	}

	// The previous enabled set was empty, so the daemon must be brought back down
	// rather than left in whatever state the failed enable reached.
	var disabled bool
	for _, c := range ran {
		if strings.HasPrefix(c, "systemctl disable") {
			disabled = true
		}
	}
	if !disabled {
		t.Errorf("the undo did not reconcile the daemon; ran = %v", ran)
	}
}

// When the undo itself fails the error has to say so. The brain will still roll
// its row back, so this is the one case where the two sides genuinely cannot be
// reconciled and the message is all a human has to go on.
func TestFailedUndoIsReported(t *testing.T) {
	m, _ := failingSystemctl(t)

	err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})
	if err == nil {
		t.Fatal("SetAccess reported success though the daemon never started")
	}
	if !strings.Contains(err.Error(), "could not be put back") {
		t.Errorf("error does not say the host was left inconsistent: %v", err)
	}
}

// The undo's two file restores are independent, so one failing must not skip
// the other — and the daemon must not be reconciled against a drop-in that
// could not be put back.
//
// The failure is injected through the runner: the systemctl call that fails
// also removes the drop-in's directory, so the restore that follows has nowhere
// to write. That is contrived, but it is the only way to reach a half-failed
// undo from a unit test, and the branch it covers is the one that decides
// whether a failed change goes live.
func TestUndoRestoresTheKeysEvenWhenTheDropInCannotBeRestored(t *testing.T) {
	keys := t.TempDir()
	m, _ := newManager(t, keys)

	// An account already enabled, so the drop-in exists and its restore is a
	// write rather than a remove. A remove would succeed against a missing
	// directory and the failure could not be injected at all.
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})

	var ran []string
	m.Runner = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "systemctl" && len(args) > 0 && args[0] == "enable" {
			if err := os.RemoveAll(filepath.Dir(m.dropInPath())); err != nil {
				t.Fatalf("inject: %v", err)
			}
			return []byte("Failed to start ssh.service"), os.ErrInvalid
		}
		return nil, nil
	}

	err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "bob", Enabled: true, AuthorizedKeys: []string{testKey},
	})
	if err == nil {
		t.Fatal("SetAccess reported success though the daemon never started")
	}
	if !strings.Contains(err.Error(), "restore the drop-in") {
		t.Errorf("error does not name the failed drop-in restore: %v", err)
	}

	// The key restore is a separate path and had to be attempted anyway.
	if _, statErr := os.Stat(filepath.Join(keys, "bob")); !os.IsNotExist(statErr) {
		t.Errorf("bob's key file was left behind because the drop-in restore failed first: %v", statErr)
	}

	// And the daemon was left alone, because the config on disk is still this
	// call's render. Reconciling against it would put the failed change live.
	for _, c := range ran {
		if strings.HasPrefix(c, "systemctl disable") || strings.HasPrefix(c, "systemctl reload") {
			t.Errorf("the daemon was reconciled against a drop-in that could not be restored; ran = %v", ran)
		}
	}
}

// The mirror of the case above, and the one that actually decides whether
// rejected keys go live. If the key file restore fails while the drop-in goes
// back cleanly, the restored config points at a path still holding this call's
// new keys — so reloading sshd would make keys authenticate that SetAccess has
// already reported as failed.
func TestDaemonIsNotReloadedWhenTheKeysCannotBeRestored(t *testing.T) {
	keys := t.TempDir()
	m, _ := newManager(t, keys)
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})

	const replacement = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHmywREXaNctQmxNs8UMGg8mSDO4MP1SfJnIhUAeEoY9 alex@desktop"

	var ran []string
	mark := -1
	m.Runner = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "systemctl" && len(args) > 0 && args[0] == "reload" {
			// Only the first reload injects, and mark is set once. An undo that
			// wrongly reconciles reaches this branch a second time, and letting
			// it move mark would hide exactly what this test is looking for.
			if mark < 0 {
				// Take the keys directory away as the daemon call fails, so the
				// restore that follows cannot write the old key file back while
				// the drop-in's own restore still succeeds.
				if err := os.RemoveAll(keys); err != nil {
					t.Fatalf("inject: %v", err)
				}
				mark = len(ran)
			}
			return []byte("Failed to reload ssh.service"), os.ErrInvalid
		}
		return nil, nil
	}

	err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{replacement},
	})
	if err == nil {
		t.Fatal("SetAccess reported success though the reload failed")
	}
	if !strings.Contains(err.Error(), "restore the key file") {
		t.Errorf("error does not name the failed key restore: %v", err)
	}
	if mark < 0 {
		t.Fatal("the reload was never attempted, so this test proved nothing")
	}
	if len(ran) != mark {
		t.Errorf("the daemon was reconciled though the keys could not be restored, "+
			"which would make the rejected key live; ran after the failure = %v", ran[mark:])
	}
}

// Stopping is never gated on a restore, because stopping reads nothing.
//
// This is the case that matters most on hosted, where the daemon's run state is
// the only control over :22. A first enable that fails after the unit has
// started must still bring it down, even if a file could not be put back — the
// alternative is an open port for a call that failed and whose previous state
// had nobody enabled at all.
func TestFailedFirstEnableStillStopsTheDaemon(t *testing.T) {
	keys := t.TempDir()
	m, _ := newManager(t, keys)

	var ran []string
	m.Runner = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if name == "systemctl" && len(args) > 0 && args[0] == "enable" {
			// Make the key restore fail, without touching the drop-in. Its
			// restore is a remove, and a remove only fails on a directory that
			// is not empty, so put one there.
			path := filepath.Join(keys, "alex")
			if err := os.Remove(path); err != nil {
				t.Fatalf("inject: %v", err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatalf("inject: %v", err)
			}
			if err := os.WriteFile(filepath.Join(path, "blocker"), []byte("x"), 0o644); err != nil {
				t.Fatalf("inject: %v", err)
			}
			return []byte("Failed to start ssh.service"), os.ErrInvalid
		}
		return nil, nil
	}

	err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})
	if err == nil {
		t.Fatal("SetAccess reported success though the daemon never started")
	}
	if !strings.Contains(err.Error(), "restore the key file") {
		t.Fatalf("the key restore was expected to fail, so this test proved nothing: %v", err)
	}

	var stopped bool
	for _, c := range ran {
		if strings.HasPrefix(c, "systemctl disable") {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("the daemon was left running after a failed first enable, so :22 stays open; ran = %v", ran)
	}
}
