//go:build linux

package usermgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/onmoose/os/internal/folderlist"
	"github.com/onmoose/os/internal/hostagent"
)

// PrepareFolder implements hostagent.UserManager. It makes sure a personal
// folder source, <home>/<rel>, exists before the brain binds it into an app
// (POST /v1/users/{username}/prepare-folder, #519). The home comes from
// /etc/passwd, never from the caller. The handler has already checked rel with
// hostagent.ValidUserFolderPath.
func (m *LinuxUserManager) PrepareFolder(username, rel string) (bool, error) {
	home, uid, gid, err := m.ResolveHome(username)
	if err != nil {
		return false, err
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
// madeTop is true when this call made the first level (#658): a first level
// that was already there, even one made by something else a moment ago, is
// reported as found, so the brain never records a folder it did not make.
func prepareUnder(home, rel string, uid, gid int) (madeTop bool, err error) {
	if !filepath.IsAbs(home) {
		return false, fmt.Errorf("usermgr: home %q is not an absolute path", home)
	}
	fd, err := syscall.Open(home, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("usermgr: open home %q: %w", home, err)
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		// The handler has already refused these (ValidUserFolderPath). Checked
		// again here, because this is the code that runs as root in the home.
		if part == "" || strings.HasPrefix(part, ".") || (i == 0 && part == "bin") {
			syscall.Close(fd)
			return false, fmt.Errorf("usermgr: bad level %q in %q", part, rel)
		}
		created := false
		next, err := openDirAt(fd, part)
		if errors.Is(err, syscall.ENOENT) {
			mkErr := syscall.Mkdirat(fd, part, 0o755)
			switch {
			case mkErr == nil:
				created = true
				if i == 0 {
					madeTop = true
				}
			case errors.Is(mkErr, syscall.EEXIST):
				// Made by something else just now. Treat it as a level that was
				// already there: the open below still refuses a symlink.
			default:
				syscall.Close(fd)
				return false, fmt.Errorf("usermgr: create %q in %q: %w", part, home, mkErr)
			}
			next, err = openDirAt(fd, part)
		}
		syscall.Close(fd)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return false, fmt.Errorf("%w: %q in %q", hostagent.ErrNotADirectory, strings.Join(parts[:i+1], "/"), home)
			}
			return false, fmt.Errorf("usermgr: open %q in %q: %w", part, home, err)
		}
		fd = next
		if created || i == len(parts)-1 {
			if err := syscall.Fchown(fd, uid, gid); err != nil {
				syscall.Close(fd)
				return false, fmt.Errorf("usermgr: chown %q in %q: %w", strings.Join(parts[:i+1], "/"), home, err)
			}
		}
	}
	return madeTop, syscall.Close(fd)
}

// ListFolders implements hostagent.UserManager (GET
// /v1/users/{username}/folders, #658). The home comes from /etc/passwd, never
// from the caller. The handler has already checked rel with
// hostagent.ValidListFolderPath.
func (m *LinuxUserManager) ListFolders(username, rel string) ([]string, bool, error) {
	home, _, _, err := m.ResolveHome(username)
	if err != nil {
		return nil, false, err
	}
	return listUnder(home, rel)
}

// listUnder lists one level of folders at home/rel, as root. It walks rel one
// level at a time with O_NOFOLLOW, as prepareUnder does, so a symlink on the
// way is hostagent.ErrNotADirectory and there is no path string to swap. It
// makes nothing and changes nothing. A missing level is
// hostagent.ErrFolderNotFound. It reads only names, and keeps the real
// directories without a leading dot (folderlist.Filter): the entry types come
// from the directory itself, so a symlink is never taken for a folder.
func listUnder(home, rel string) ([]string, bool, error) {
	if !filepath.IsAbs(home) {
		return nil, false, fmt.Errorf("usermgr: home %q is not an absolute path", home)
	}
	fd, err := syscall.Open(home, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, fmt.Errorf("usermgr: open home %q: %w", home, err)
	}
	if rel != "" {
		parts := strings.Split(rel, "/")
		for i, part := range parts {
			// The handler has already refused these (ValidListFolderPath).
			if part == "" || strings.HasPrefix(part, ".") {
				syscall.Close(fd)
				return nil, false, fmt.Errorf("usermgr: bad level %q in %q", part, rel)
			}
			next, err := openDirAt(fd, part)
			syscall.Close(fd)
			switch {
			case errors.Is(err, syscall.ENOENT):
				return nil, false, fmt.Errorf("%w: %q in %q", hostagent.ErrFolderNotFound, strings.Join(parts[:i+1], "/"), home)
			case errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR):
				return nil, false, fmt.Errorf("%w: %q in %q", hostagent.ErrNotADirectory, strings.Join(parts[:i+1], "/"), home)
			case err != nil:
				return nil, false, fmt.Errorf("usermgr: open %q in %q: %w", part, home, err)
			}
			fd = next
		}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(home, filepath.FromSlash(rel)))
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, false, fmt.Errorf("usermgr: read %q in %q: %w", rel, home, err)
	}
	names, truncated := folderlist.Filter(entries)
	return names, truncated, nil
}

// openDirAt opens one directory level relative to dirfd, refusing a symlink
// (ELOOP) or anything that is not a directory (ENOTDIR).
func openDirAt(dirfd int, name string) (int, error) {
	return syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}
