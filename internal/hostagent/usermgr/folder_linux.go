//go:build linux

package usermgr

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/onmoose/os/internal/hostagent"
)

// PrepareFolder implements hostagent.UserManager. It makes sure a personal
// folder source, <home>/<rel>, exists before the brain binds it into an app
// (POST /v1/users/{username}/prepare-folder, #519). The home comes from
// /etc/passwd, never from the caller. The handler has already checked rel with
// hostagent.ValidUserFolderPath.
func (m *LinuxUserManager) PrepareFolder(username, rel string) error {
	home, uid, gid, err := m.ResolveHome(username)
	if err != nil {
		return err
	}
	return prepareUnder(home, rel, uid, gid)
}

// prepareUnder walks rel below home one level at a time, as root.
//
//   - A missing level is created (mode 0755, less the umask) and owned by
//     uid:gid, so the app, which runs as the owner, can write into it.
//   - A level that already exists is left as it is, except the last one, which
//     is always owned by uid:gid. That is the level the app binds. Docker makes
//     a missing bind source root:root, so a box that ran an app before this op
//     existed can hold a root-owned ~/Documents the owner cannot write to.
//   - No level is ever followed if it is a symlink. The user owns the home and
//     can put a symlink anywhere in it, and a root process that followed
//     ~/Documents -> /etc would hand /etc to the user. Each level is opened
//     relative to the one before with O_NOFOLLOW, so there is no path string to
//     swap out between a check and a use.
//
// A level that is a file or a symlink returns hostagent.ErrNotADirectory.
func prepareUnder(home, rel string, uid, gid int) error {
	if !filepath.IsAbs(home) {
		return fmt.Errorf("usermgr: home %q is not an absolute path", home)
	}
	fd, err := syscall.Open(home, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("usermgr: open home %q: %w", home, err)
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			syscall.Close(fd)
			return fmt.Errorf("usermgr: bad level %q in %q", part, rel)
		}
		created := false
		next, err := openDirAt(fd, part)
		if errors.Is(err, syscall.ENOENT) {
			mkErr := syscall.Mkdirat(fd, part, 0o755)
			switch {
			case mkErr == nil:
				created = true
			case errors.Is(mkErr, syscall.EEXIST):
				// Made by something else just now. Treat it as a level that was
				// already there: the open below still refuses a symlink.
			default:
				syscall.Close(fd)
				return fmt.Errorf("usermgr: create %q in %q: %w", part, home, mkErr)
			}
			next, err = openDirAt(fd, part)
		}
		syscall.Close(fd)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return fmt.Errorf("%w: %q in %q", hostagent.ErrNotADirectory, strings.Join(parts[:i+1], "/"), home)
			}
			return fmt.Errorf("usermgr: open %q in %q: %w", part, home, err)
		}
		fd = next
		if created || i == len(parts)-1 {
			if err := syscall.Fchown(fd, uid, gid); err != nil {
				syscall.Close(fd)
				return fmt.Errorf("usermgr: chown %q in %q: %w", strings.Join(parts[:i+1], "/"), home, err)
			}
		}
	}
	return syscall.Close(fd)
}

// openDirAt opens one directory level relative to dirfd, refusing a symlink
// (ELOOP) or anything that is not a directory (ENOTDIR).
func openDirAt(dirfd int, name string) (int, error) {
	return syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}
