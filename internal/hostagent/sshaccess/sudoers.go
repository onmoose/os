package sshaccess

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultSudoersPath is the sudo rule moose owns. It holds at most one line:
// the account that may run sudo with no password prompt. The brain asks for it
// only for the hosted owner, whose password is random and unknown, so sudo would
// otherwise be a dead end for the one account the rescue path is for
// (DECISIONS.md 2026-10-11, USERS_AND_GROUPS.md # The hosted owner).
//
// One file, one owner. A call that sets the rule for one account replaces any
// other account the file named. A call that clears it for an account removes
// the file only when it names that account, so a call about someone else never
// takes the owner's rule away.
//
// The name has no dot on purpose. sudo skips any file in sudoers.d whose name
// holds a dot or ends in "~", so the candidate below (moose-owner.new) and the
// temp files writeFileAtomic makes are never read by sudo, even if a crash
// leaves one behind.
const DefaultSudoersPath = "/etc/sudoers.d/moose-owner"

// sudoersHeader marks the file as generated.
const sudoersHeader = "# Managed by moose. Present only while the hosted owner has SSH on; edits are not preserved."

// sudoersRuleSuffix is everything after the account name on the one rule line.
const sudoersRuleSuffix = " ALL=(ALL) NOPASSWD: ALL"

// sudoersMode is what sudo expects of a file in sudoers.d: read-only, and not
// writable by anyone. sudo refuses a file that is group- or world-writable.
const sudoersMode os.FileMode = 0o440

// sudoersUsername is the shape of a name allowed into the rule. It is stricter
// than managedKeysPath's check because here the name becomes sudoers syntax, not
// a path. A name like "ALL" or "%sudo" would be valid syntax with a very
// different meaning, and visudo would accept it. Debian's own default for
// account names is the same shape, and every name the brain derives fits it.
var sudoersUsername = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func (m *Manager) sudoersPath() string {
	if m.SudoersPath != "" {
		return m.SudoersPath
	}
	return DefaultSudoersPath
}

// renderSudoers produces the whole file for one account.
func renderSudoers(username string) string {
	var b strings.Builder
	b.WriteString(sudoersHeader + "\n")
	b.WriteString("# See USERS_AND_GROUPS.md # The hosted owner and DECISIONS.md 2026-10-11.\n")
	b.WriteString(username + sudoersRuleSuffix + "\n")
	return b.String()
}

// readSudoersUser returns the account the rule names, and whether the file
// exists at all. Only a file with exactly one rule line, in moose's shape, names
// an account. Anything else names nobody, even when its first line looks right:
// a second line could grant something moose never wrote. SetAccess then leaves
// such a file alone unless it is told to write one, and the start reconcile
// removes it.
func (m *Manager) readSudoersUser() (string, bool, error) {
	path := m.sudoersPath()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sshaccess: read %s: %w", path, err)
	}
	var rules []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rules = append(rules, line)
	}
	// A line too long to scan is not a file moose wrote, so it names nobody
	// too, and the reconcile removes it rather than trusting a partial read.
	if sc.Err() != nil {
		return "", true, nil
	}
	if len(rules) != 1 {
		return "", true, nil
	}
	name, ok := strings.CutSuffix(rules[0], sudoersRuleSuffix)
	if !ok || !sudoersUsername.MatchString(name) {
		return "", true, nil
	}
	return name, true, nil
}

// snapshotSudoers reads the rule and returns a function that puts it back, the
// same shape as snapshotDropIn. A missing file restores by removing.
func (m *Manager) snapshotSudoers() (func() error, error) {
	path := m.sudoersPath()
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, fmt.Errorf("sshaccess: read %s: %w", path, readErr)
	}
	had := readErr == nil
	return func() error { return restoreMode(path, previous, had, sudoersMode) }, nil
}

// applySudoers brings the rule in line with one account's request: present and
// naming the account when want is true, and gone when want is false and the
// file names this account.
//
// A new rule is checked by `visudo -cf` while it is still a candidate and only
// then renamed into place. sudo reads every file in sudoers.d on every call, so
// a broken file there would break sudo for every account on the box, the admins
// a rescue needs included. Checking first means a bad render is never live.
func (m *Manager) applySudoers(username string, want bool) error {
	path := m.sudoersPath()
	if !want {
		name, exists, err := m.readSudoersUser()
		if err != nil {
			return err
		}
		if !exists || name != username {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("sshaccess: remove %s: %w", path, err)
		}
		return nil
	}

	if !sudoersUsername.MatchString(username) {
		return fmt.Errorf("sshaccess: refusing a sudo rule for username %q", username)
	}
	// /etc/sudoers.d ships with the sudo package as 0750. Make it only if it is
	// missing, with the same mode, so sudo's own checks on the directory pass.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("sshaccess: mkdir %s: %w", filepath.Dir(path), err)
	}
	candidate := path + ".new"
	if err := writeFileAtomic(candidate, []byte(renderSudoers(username)), sudoersMode, -1, -1); err != nil {
		return fmt.Errorf("sshaccess: write %s: %w", candidate, err)
	}
	if out, err := m.run("visudo", "-c", "-f", candidate); err != nil {
		if rmErr := os.Remove(candidate); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("sshaccess: visudo rejected the rendered sudo rule: %w: %s (and the candidate could not be removed: %v)",
				err, strings.TrimSpace(string(out)), rmErr)
		}
		return fmt.Errorf("sshaccess: visudo rejected the rendered sudo rule: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(candidate, path); err != nil {
		_ = os.Remove(candidate)
		return fmt.Errorf("sshaccess: install %s: %w", path, err)
	}
	return nil
}

// ReconcileSudoAtStart makes the sudo rule match the SSH state the host already
// has, and reports whether it removed one. host-agent calls it at every start,
// on both profiles.
//
// It keeps no state of its own. The rule stays only when the account it names
// is enabled in the sshd drop-in, which is the enabled set (readDropIn).
// Anything else is removed: a rule for an account with SSH off, a file in a
// shape moose did not write, and a candidate a crash left behind. So a crash
// between two steps of SetAccess, or an /etc restored from an older copy, can
// never leave a rule for an account that has SSH off.
//
// It fails closed. When the drop-in cannot be read, the enabled set is unknown,
// so the rule is removed and the read error is returned too. Removing it costs
// the owner a prompt-free sudo until their next SSH Save; keeping it could leave
// root with no prompt for an account nobody can confirm is enabled.
//
// It never writes a rule, only removes one. Only the brain knows who the owner
// is, so only a set-access call can create it.
func (m *Manager) ReconcileSudoAtStart() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.sudoersPath()
	var errs []error
	if err := os.Remove(path + ".new"); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("sshaccess: remove %s: %w", path+".new", err))
	}

	name, exists, err := m.readSudoersUser()
	if err != nil {
		return false, errors.Join(append(errs, err)...)
	}
	if !exists {
		return false, errors.Join(errs...)
	}

	accounts, dropInErr := m.readDropIn()
	if dropInErr == nil && name != "" {
		for _, a := range accounts {
			if a.Username == name {
				return false, errors.Join(errs...)
			}
		}
	}
	if dropInErr != nil {
		errs = append(errs, fmt.Errorf("the SSH state could not be read, so the sudo rule was removed: %w", dropInErr))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, errors.Join(append(errs, fmt.Errorf("sshaccess: remove %s: %w", path, err))...)
	}
	return true, errors.Join(errs...)
}
