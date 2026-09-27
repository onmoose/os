// Package usermgr implements hostagent.UserManager backed by Linux shell-outs
// (useradd, chpasswd). It is intentionally isolated so that the shared
// internal/hostagent package has no /etc/shadow dependency; only
// cmd/host-agent-real imports it.
package usermgr

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"

	"github.com/onmoose/os/internal/hostagent"
	"github.com/onmoose/os/internal/protocol"
)

// LinuxUserManager implements hostagent.UserManager against the local system.
//
// Behavior of UpsertPassword:
//   - Looks up the user via user.Lookup.
//   - If missing: shells out to `useradd --create-home --shell <Shell>
//     --gid <PrimaryGroup> <slug>` to create the account.
//   - Sets the password via `chpasswd` reading "<slug>:<password>\n" on stdin.
//     chpasswd goes through PAM, which honors Samba sync if the host has
//     `unix password sync = yes` configured (see AUTH.md # Samba password
//     backend).
//
// UID assignment: relies on /etc/login.defs (UID_MIN ≥ 3000 on a moose OS,
// per FIRST_RUN.md # Identity & display names). Not forced via -u so the
// system picks the next free UID in range.
//
// Use-case folder creation (Photos/, Documents/, ...) is NOT done here —
// that's STORAGE.md territory and tracked as a follow-up in
// docs/progress/0015-host-agent-set-password.md.
type LinuxUserManager struct {
	// Shell is the login shell to assign to new users. Empty → "/bin/bash".
	Shell string
	// PrimaryGroup is the primary group for new users. Empty → "moose".
	// The host is expected to already have this group present (provisioned
	// by the box build, not by host-agent).
	PrimaryGroup string
	// AdminGroup is the Linux group that conveys sudo / shell-rescue
	// capability. Empty → "sudo" (per USERS_AND_GROUPS.md # Roles and the
	// CLAUDE.md "admins in `sudo`" load-bearing decision).
	AdminGroup string
}

func (m *LinuxUserManager) adminGroup() string {
	if m.AdminGroup == "" {
		return "sudo"
	}
	return m.AdminGroup
}

// UpsertPassword implements hostagent.UserManager.
func (m *LinuxUserManager) UpsertPassword(slug, password string) error {
	if slug == "" {
		return fmt.Errorf("usermgr: empty slug")
	}
	_, lookupErr := user.Lookup(slug)
	if lookupErr != nil {
		// user.UnknownUserError is the only error we treat as "create";
		// any other lookup failure (e.g., nss config broken) should fail loud.
		var unknown user.UnknownUserError
		if !errors.As(lookupErr, &unknown) {
			return fmt.Errorf("usermgr: lookup %q: %w", slug, lookupErr)
		}
		if err := runCmd("useradd", m.useraddArgs(slug)...); err != nil {
			return fmt.Errorf("usermgr: useradd %q: %w", slug, err)
		}
	}
	if err := runChpasswd(slug, password); err != nil {
		return fmt.Errorf("usermgr: chpasswd %q: %w", slug, err)
	}
	return nil
}

// useraddArgs builds the argument list for the useradd shell-out. Exposed for
// unit testing — the integration test exercises the real command.
func (m *LinuxUserManager) useraddArgs(slug string) []string {
	shell := m.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	gid := m.PrimaryGroup
	if gid == "" {
		gid = "moose"
	}
	return []string{"--create-home", "--shell", shell, "--gid", gid, slug}
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SetRole implements hostagent.UserManager. admin → ensure slug is in the
// admin group (gpasswd -a, idempotent on re-add). member → ensure slug is NOT
// in the admin group (gpasswd -d, with a pre-check so demoting a non-member is
// a no-op instead of an error).
//
// Demotion does not kill live SSH sessions for the demoted admin; group
// membership is cached at session start. Accepted in USERS_AND_GROUPS.md #
// Known sharp edges.
func (m *LinuxUserManager) SetRole(slug, role string) error {
	if slug == "" {
		return fmt.Errorf("usermgr: empty slug")
	}
	group := m.adminGroup()
	switch role {
	case "admin":
		if err := runCmd("gpasswd", "-a", slug, group); err != nil {
			return fmt.Errorf("usermgr: gpasswd -a %q %q: %w", slug, group, err)
		}
		return nil
	case "member":
		in, err := isInGroup(slug, group)
		if err != nil {
			return fmt.Errorf("usermgr: check membership %q in %q: %w", slug, group, err)
		}
		if !in {
			return nil
		}
		if err := runCmd("gpasswd", "-d", slug, group); err != nil {
			return fmt.Errorf("usermgr: gpasswd -d %q %q: %w", slug, group, err)
		}
		return nil
	default:
		return fmt.Errorf("usermgr: bad role %q", role)
	}
}

// isInGroup reports whether slug is listed as a member of group in /etc/group.
// Parses the file directly rather than using user.LookupGroupId+gids because
// /etc/group's member list is the authoritative source for supplementary
// memberships like `sudo`, and we don't depend on nsswitch / nscd lookups.
func isInGroup(slug, group string) (bool, error) {
	b, err := os.ReadFile("/etc/group")
	if err != nil {
		return false, err
	}
	return parseGroupMembership(b, slug, group), nil
}

// parseGroupMembership is the pure parser, package-private and tested from within the package.
// /etc/group format: name:passwd:gid:user1,user2,...
//
// Returns false for an empty slug — `strings.Split("", ",")` yields `[""]`,
// which would otherwise match the empty member field of a group like
// `moose:x:3000:` and surprise callers. Upstream guards already reject empty
// slugs, but the helper should be safe in isolation.
func parseGroupMembership(content []byte, slug, group string) bool {
	if slug == "" {
		return false
	}
	for _, line := range strings.Split(string(content), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[0] != group {
			continue
		}
		for _, m := range strings.Split(fields[3], ",") {
			if strings.TrimSpace(m) == slug {
				return true
			}
		}
		return false
	}
	return false
}

// DeleteUser implements hostagent.UserManager. Shells out to
// `userdel -r -f <slug>`:
//   - -r removes the home dir + mail spool. Per the v1 product call
//     (docs/progress/0017-host-agent-delete-user.md): a deleted account loses
//     its data with it. The "transfer / preserve files" flow is a future
//     follow-up that will branch BEFORE calling this.
//   - -f forces removal even if the user owns running processes (the SSH
//     session matrix is documented in 0017); without -f userdel exits 8 on a
//     logged-in user, turning routine deletes into 500s.
//
// Idempotent per BRAIN_HOST_PROTOCOL.md # Auth endpoints: unknown user returns
// nil (the brain handler maps that to 200).
//
// nscd caveat: host-agent-real is CGO-built, so user.Lookup goes through
// glibc's getpwnam_r and honors nscd's passwd cache when nscd is running.
// A just-deleted user could still appear cached (we'd call userdel again,
// harmless under -f); a just-created user could appear cached as missing
// (UpsertPassword would re-run useradd → "already exists"). nscd is not
// installed on a stock moose box; if that ever changes, switch to direct
// /etc/passwd parsing (same shape as isInGroup).
func (m *LinuxUserManager) DeleteUser(slug string) error {
	if slug == "" {
		return fmt.Errorf("usermgr: empty slug")
	}
	if _, err := user.Lookup(slug); err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil
		}
		return fmt.Errorf("usermgr: lookup %q: %w", slug, err)
	}
	if err := runCmd("userdel", "-r", "-f", slug); err != nil {
		return fmt.Errorf("usermgr: userdel %q: %w", slug, err)
	}
	return nil
}

// ResolveHome implements hostagent.UserManager. Reads /etc/passwd via
// os/user.Lookup and returns the user's home directory, UID, and GID.
// Returns hostagent.ErrUnknownUser when the user does not exist so the
// calling handler can return 404 rather than 500.
// UserExists implements hostagent.UserManager: does /etc/passwd (via NSS)
// already hold this name. A lookup failure that is not "unknown user" is
// returned as an error rather than reported as "free", because treating a
// broken NSS as an empty passwd file would hand the brain a name that is in
// fact taken.
func (m *LinuxUserManager) UserExists(username string) (bool, error) {
	if username == "" {
		return false, fmt.Errorf("usermgr: empty username")
	}
	if _, err := user.Lookup(username); err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return false, nil
		}
		return false, fmt.Errorf("usermgr: lookup %q: %w", username, err)
	}
	return true, nil
}

func (m *LinuxUserManager) ResolveHome(username string) (home string, uid, gid int, err error) {
	if username == "" {
		return "", 0, 0, fmt.Errorf("usermgr: empty username")
	}
	u, lookupErr := user.Lookup(username)
	if lookupErr != nil {
		var unknown user.UnknownUserError
		if errors.As(lookupErr, &unknown) {
			return "", 0, 0, hostagent.ErrUnknownUser
		}
		return "", 0, 0, fmt.Errorf("usermgr: lookup %q: %w", username, lookupErr)
	}
	parsedUID, err := strconv.Atoi(u.Uid)
	if err != nil {
		return "", 0, 0, fmt.Errorf("usermgr: parse uid %q for %q: %w", u.Uid, username, err)
	}
	parsedGID, err := strconv.Atoi(u.Gid)
	if err != nil {
		return "", 0, 0, fmt.Errorf("usermgr: parse gid %q for %q: %w", u.Gid, username, err)
	}
	return u.HomeDir, parsedUID, parsedGID, nil
}

// WellKnownIdentity implements hostagent.UserManager. Resolves the moose-app
// system user (UID/GID) and the moose-shared group (GID) from the host's
// /etc/passwd and /etc/group via os/user. These system accounts are provisioned
// by the box build, not by host-agent; the lookups here are read-only.
//
// NOTE: moose-app and moose-shared are not present on the dev box — only the
// fake branch runs in the dev loop. This implementation is correct for prod and
// must compile cleanly even when the accounts are absent.
func (m *LinuxUserManager) WellKnownIdentity() (appUID, appGID, sharedGID int, err error) {
	u, err := user.Lookup("moose-app")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("lookup moose-app user: %w", err)
	}
	parsedUID, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse moose-app uid %q: %w", u.Uid, err)
	}
	parsedGID, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse moose-app gid %q: %w", u.Gid, err)
	}
	g, err := user.LookupGroup("moose-shared")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("lookup moose-shared group: %w", err)
	}
	parsedSharedGID, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse moose-shared gid %q: %w", g.Gid, err)
	}
	return parsedUID, parsedGID, parsedSharedGID, nil
}

// svcAccountPrefix names the system accounts that back app-service identity
// reservations: moose-svc-<uid>. Naming by UID (not instance ID) keeps the
// account name short and valid regardless of instance-ID length; the
// instance ↔ UID mapping lives on the brain's instance row, and the GECOS
// comment carries the instance ID for debuggability.
const svcAccountPrefix = "moose-svc-"

func svcGecos(instanceID string) string { return "moose app-service for " + instanceID }

// AllocateAppService implements hostagent.UserManager. Reserves the first
// free UID/GID pair in the app-service band [protocol.AppServiceUIDMin,
// AppServiceUIDMax] by creating a real system account + group named
// moose-svc-<uid> — the /etc/passwd entry IS the durable reservation, so the
// band's state survives host-agent restarts without any side state.
// Idempotent per instance: if an account's GECOS already carries this
// instance ID, its pair is returned instead of allocating a second one.
//
// /etc/passwd and /etc/group are parsed directly (same call as isInGroup) so
// the free-number scan doesn't depend on nsswitch/nscd lookups.
func (m *LinuxUserManager) AllocateAppService(instanceID string) (uid, gid int, err error) {
	if instanceID == "" {
		return 0, 0, fmt.Errorf("usermgr: empty instance id")
	}
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return 0, 0, fmt.Errorf("usermgr: read /etc/passwd: %w", err)
	}
	group, err := os.ReadFile("/etc/group")
	if err != nil {
		return 0, 0, fmt.Errorf("usermgr: read /etc/group: %w", err)
	}
	if existing := findAppServiceByGecos(passwd, svcGecos(instanceID)); existing != 0 {
		return existing, existing, nil
	}
	n, err := firstFreeAppServiceID(passwd, group)
	if err != nil {
		return 0, 0, err
	}
	name := svcAccountPrefix + strconv.Itoa(n)
	if err := runCmd("groupadd", "--gid", strconv.Itoa(n), name); err != nil {
		return 0, 0, fmt.Errorf("usermgr: groupadd %q: %w", name, err)
	}
	if err := runCmd("useradd", "--system", "--uid", strconv.Itoa(n), "--gid", strconv.Itoa(n),
		"--no-create-home", "--shell", "/usr/sbin/nologin",
		"--comment", svcGecos(instanceID), name); err != nil {
		// Don't leave a groupless half-reservation behind.
		_ = runCmd("groupdel", name)
		return 0, 0, fmt.Errorf("usermgr: useradd %q: %w", name, err)
	}
	return n, n, nil
}

// ReleaseAppService implements hostagent.UserManager. Deletes the
// moose-svc-<uid> account + group, returning the number to the band.
// Idempotent: a missing account returns nil. Only accounts in the band and
// named with the moose-svc- prefix are ever touched — this must never be
// usable to delete an arbitrary user.
func (m *LinuxUserManager) ReleaseAppService(uid int) error {
	if uid < protocol.AppServiceUIDMin || uid > protocol.AppServiceUIDMax {
		return fmt.Errorf("usermgr: uid %d outside the app-service band", uid)
	}
	name := svcAccountPrefix + strconv.Itoa(uid)
	if _, err := user.Lookup(name); err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			// Account gone; the group may still exist if a past release was
			// interrupted between userdel and groupdel.
			if _, gerr := user.LookupGroup(name); gerr == nil {
				if err := runCmd("groupdel", name); err != nil {
					return fmt.Errorf("usermgr: groupdel %q: %w", name, err)
				}
			}
			return nil
		}
		return fmt.Errorf("usermgr: lookup %q: %w", name, err)
	}
	if err := runCmd("userdel", name); err != nil {
		return fmt.Errorf("usermgr: userdel %q: %w", name, err)
	}
	// userdel only removes the primary group when USERGROUPS_ENAB applies;
	// delete it explicitly and tolerate "already gone".
	if _, err := user.LookupGroup(name); err == nil {
		if err := runCmd("groupdel", name); err != nil {
			return fmt.Errorf("usermgr: groupdel %q: %w", name, err)
		}
	}
	return nil
}

// firstFreeAppServiceID is the pure free-number scan: the smallest n in the
// app-service band that is neither a UID in passwd nor a GID in group.
// Errors when the band is exhausted (900 slots — a box would need 900 live
// service_user instances).
func firstFreeAppServiceID(passwd, group []byte) (int, error) {
	taken := map[int]bool{}
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 {
			if n, err := strconv.Atoi(fields[2]); err == nil {
				taken[n] = true
			}
		}
	}
	for _, line := range strings.Split(string(group), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 {
			if n, err := strconv.Atoi(fields[2]); err == nil {
				taken[n] = true
			}
		}
	}
	for n := protocol.AppServiceUIDMin; n <= protocol.AppServiceUIDMax; n++ {
		if !taken[n] {
			return n, nil
		}
	}
	return 0, fmt.Errorf("usermgr: app-service band exhausted")
}

// findAppServiceByGecos returns the UID of the moose-svc-* account whose
// GECOS field matches, or 0 when none does — the idempotency probe for
// AllocateAppService.
//
// /etc/passwd uses ":" as its field separator, so a ":" in the instance ID
// would split the GECOS across multiple fields. We reassemble it from field
// index 4 to the last-two (home:shell) boundary, which handles any colons in
// the value. A valid passwd entry has at least 7 fields.
func findAppServiceByGecos(passwd []byte, gecos string) int {
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 || !strings.HasPrefix(fields[0], svcAccountPrefix) {
			continue
		}
		if strings.Join(fields[4:len(fields)-2], ":") != gecos {
			continue
		}
		if n, err := strconv.Atoi(fields[2]); err == nil {
			return n
		}
	}
	return 0
}

func runChpasswd(slug, password string) error {
	cmd := exec.Command("chpasswd")
	cmd.Stdin = strings.NewReader(slug + ":" + password + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("chpasswd: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
