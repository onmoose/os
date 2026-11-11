//go:build linux

package usermgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/onmoose/os/internal/hostagent"
	"github.com/onmoose/os/internal/protocol"
)

// folderPinRoot is where a pinned folder source is bind-mounted. It is a var,
// not a const, so the Linux integration test can point it at a temp dir. The
// real path is under /run, so a reboot clears it; moose-folder-pins.service
// restores the pins from each instance's folder-pins.json before Docker starts.
var folderPinRoot = "/run/moose/folder-pins"

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
		// The handler has already refused these (ValidUserFolderPath). Checked
		// again here, because this is the code that runs as root in the home.
		if part == "" || strings.HasPrefix(part, ".") || (i == 0 && part == "bin") {
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

// PinFolder walks <home>/<rel> (or <SharedRoot>/<rel> when shared) without
// following a symlink and bind-mounts the walked directory at a root-owned pin
// path (#604). The pin is the inode: a symlink swapped in after the walk
// changes the path, not the directory the pin holds, so Docker cannot be
// pointed at a different folder between two starts.
func (m *LinuxUserManager) PinFolder(username string, shared bool, rel string) (string, error) {
	root := protocol.SharedRoot
	if !shared {
		home, _, _, err := m.ResolveHome(username)
		if err != nil {
			return "", err
		}
		root = home
	}
	fd, st, err := walkFolder(root, rel)
	if err != nil {
		return "", err
	}
	defer syscall.Close(fd)
	pin := filepath.Join(folderPinRoot, fmt.Sprintf("%d-%d", st.Dev, st.Ino))
	if err := mountPin(fd, pin, st); err != nil {
		return "", err
	}
	return pin, nil
}

// walkFolder opens root and each level of rel below it with O_NOFOLLOW, one
// level at a time from the previous fd. A file or a symlink on the way returns
// hostagent.ErrNotADirectory. It returns the final fd and its stat, so the
// caller can mount exactly the inode it walked.
func walkFolder(root, rel string) (int, syscall.Stat_t, error) {
	var st syscall.Stat_t
	if !filepath.IsAbs(root) {
		return -1, st, fmt.Errorf("usermgr: root %q is not an absolute path", root)
	}
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return -1, st, fmt.Errorf("%w: %q", hostagent.ErrNotADirectory, root)
		}
		return -1, st, fmt.Errorf("usermgr: open root %q: %w", root, err)
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		// The handler already checked rel (ValidUserFolderPath). Checked again
		// here, because this is the code that runs as root in the home.
		if part == "" || strings.HasPrefix(part, ".") || (i == 0 && part == "bin") {
			syscall.Close(fd)
			return -1, st, fmt.Errorf("usermgr: bad level %q in %q", part, rel)
		}
		next, err := openDirAt(fd, part)
		syscall.Close(fd)
		if err != nil {
			// A missing level is refused too: Docker would create a missing
			// bind source as an empty root-owned directory, and the app would
			// run against a folder that is not the one the user chose.
			if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return -1, st, fmt.Errorf("%w: %q in %q", hostagent.ErrNotADirectory, strings.Join(parts[:i+1], "/"), root)
			}
			return -1, st, fmt.Errorf("usermgr: open %q in %q: %w", part, root, err)
		}
		fd = next
	}
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return -1, st, fmt.Errorf("usermgr: stat %q in %q: %w", rel, root, err)
	}
	return fd, st, nil
}

// mountPin bind-mounts the directory behind fd at pin, then checks that the
// pin really is that directory. The check is what closes the last window: the
// kernel resolves /proc/self/fd/N to a path string, and a swap in that tiny
// window could point it elsewhere. A pin whose device and inode do not match
// the walked fd is unmounted and refused, never handed to Docker.
func mountPin(fd int, pin string, want syscall.Stat_t) error {
	return mountPinAt(fd, pin, want, folderPinRoot)
}

// RestoreFolderPins puts the pins back before Docker starts (#604). A pin is a
// bind mount under /run, so a reboot clears it, and Docker recreates a missing
// bind source as an empty root-owned directory. Restoring first keeps a
// rebooted box from serving an empty folder until the brain reconciles, and
// keeps one-shot jobs from running again against it.
//
// It reads each instance's folder-pins.json, walks the source again, and mounts
// only when the walked device and inode still match the pin's name. A source
// swapped while the box was off is left unpinned and logged; Docker then gets
// an empty pin path, never the real folder, and the brain refuses the start.
// Pins no longer referenced by any record are detached and removed.
func RestoreFolderPins(stateDir, pinRoot string) error {
	recs, err := readPinRecords(stateDir)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	pinBase := filepath.Clean(pinRoot) + string(os.PathSeparator)
	for _, rf := range recs {
		var failure *protocol.FolderPinFailure
		for _, p := range rf.rec.Pins {
			keep[p.Pin] = true
			if !strings.HasPrefix(p.Pin, pinBase) {
				slog.Warn("folder pin: record names a path outside the pin root", "pin", p.Pin, "root", pinRoot)
				failure = firstFailure(failure, p.Path, "the pin path is outside the pin root")
				continue
			}
			root := protocol.SharedRoot
			if !p.Shared {
				home, _, _, err := (&LinuxUserManager{}).ResolveHome(p.Username)
				if err != nil {
					slog.Warn("folder pin: cannot resolve the owner, leaving the pin for the brain", "pin", p.Pin, "username", p.Username, "err", err)
					failure = firstFailure(failure, p.Path, "the owner account is gone")
					continue
				}
				root = home
			}
			fd, st, err := walkFolder(root, p.Path)
			if err != nil {
				slog.Warn("folder pin: the source is not a real folder, leaving the pin for the brain", "pin", p.Pin, "path", p.Path, "err", err)
				failure = firstFailure(failure, p.Path, "the source is not a real folder")
				continue
			}
			// Match the inode, not the device. A data drive remounted after a
			// reboot can come back with a different st_dev while the folder is
			// the same filesystem and inode, and the pin's name is only a
			// label on the mount. The path the record names is the source the
			// walk just resolved, so the inode at that path is the identity
			// that matters. A different inode means the folder changed while
			// the box was off: leave it for the brain to refuse.
			want, ok := pinInode(p.Pin)
			if !ok || st.Ino != want {
				slog.Warn("folder pin: the source inode changed, leaving the pin for the brain", "pin", p.Pin, "path", p.Path, "now", fmt.Sprintf("%d-%d", st.Dev, st.Ino))
				failure = firstFailure(failure, p.Path, "the source inode changed")
				syscall.Close(fd)
				continue
			}
			if err := mountPinAt(fd, p.Pin, st, pinRoot); err != nil {
				slog.Warn("folder pin: restore failed", "pin", p.Pin, "path", p.Path, "err", err)
				failure = firstFailure(failure, p.Path, err.Error())
			}
			syscall.Close(fd)
		}
		writePinFailure(rf.dir, failure)
	}
	prunePins(pinRoot, keep)
	return nil
}

// firstFailure keeps the first pin failure of an instance, so one bad source
// does not get overwritten by a later one before the brain reads it.
func firstFailure(cur *protocol.FolderPinFailure, path, reason string) *protocol.FolderPinFailure {
	if cur != nil {
		return cur
	}
	return &protocol.FolderPinFailure{Path: path, Reason: reason}
}

// pinInode reads the inode from a pin's <dev>-<ino> name, so the restorer can
// compare it with the source it walked.
func pinInode(pin string) (uint64, bool) {
	base := filepath.Base(pin)
	i := strings.LastIndex(base, "-")
	if i < 0 {
		return 0, false
	}
	ino, err := strconv.ParseUint(base[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return ino, true
}

// writePinFailure records, or clears, the instance's pin-failure marker. The
// brain reads it at startup and refuses the app, so a pin the restorer could
// not mount does not leave the app running against Docker's empty directory.
func writePinFailure(dir string, failure *protocol.FolderPinFailure) {
	path := filepath.Join(dir, protocol.FolderPinFailedFile)
	if failure == nil {
		_ = os.Remove(path)
		return
	}
	b, err := json.Marshal(failure)
	if err != nil {
		slog.Warn("folder pin: encode failure marker", "dir", dir, "err", err)
		return
	}
	// Write then rename, so the brain never reads half a marker if this unit is
	// killed.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		slog.Warn("folder pin: write failure marker", "dir", dir, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		slog.Warn("folder pin: install failure marker", "dir", dir, "err", err)
	}
}

// mountPinAt is mountPin with an explicit pin root, so the boot restorer can
// be pointed at a temp dir in tests.
func mountPinAt(fd int, pin string, want syscall.Stat_t, pinRoot string) error {
	// Two pins of the same folder (two starts, or a start and a restamp) would
	// otherwise race: the second one's detach below can drop a pin the first
	// one already handed to Docker, and a bind in that window gets the empty
	// directory under /run. One lock for every pin; a mount is quick.
	pinMu.Lock()
	defer pinMu.Unlock()
	// The restorer runs before host-agent, so /run/moose may not exist yet.
	// Make its parent the mode host-agent's RuntimeDirectory uses, then the pin
	// root itself root-only.
	if err := os.MkdirAll(filepath.Dir(pinRoot), 0o755); err != nil {
		return fmt.Errorf("usermgr: make pin root parent for %q: %w", pinRoot, err)
	}
	if err := os.MkdirAll(pinRoot, 0o700); err != nil {
		return fmt.Errorf("usermgr: make pin root %q: %w", pinRoot, err)
	}
	// MkdirAll is masked by the umask; force the mode. The pin root is for
	// root only: the container reaches the folder through the mount, not by
	// walking this path.
	if err := os.Chmod(pinRoot, 0o700); err != nil {
		return fmt.Errorf("usermgr: mode pin root %q: %w", pinRoot, err)
	}
	// Already pinned to the same inode: keep it. A stale mount of a different
	// inode at this path (device and inode numbers can be reused) is detached
	// and made again.
	var got syscall.Stat_t
	if err := syscall.Stat(pin, &got); err == nil && got.Dev == want.Dev && got.Ino == want.Ino {
		return nil
	}
	_ = syscall.Unmount(pin, syscall.MNT_DETACH)
	if err := os.MkdirAll(pin, 0o700); err != nil {
		return fmt.Errorf("usermgr: make pin %q: %w", pin, err)
	}
	src := fmt.Sprintf("/proc/self/fd/%d", fd)
	if err := syscall.Mount(src, pin, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("usermgr: pin %q: %w", pin, err)
	}
	if err := syscall.Stat(pin, &got); err != nil {
		_ = syscall.Unmount(pin, syscall.MNT_DETACH)
		return fmt.Errorf("usermgr: check pin %q: %w", pin, err)
	}
	if got.Dev != want.Dev || got.Ino != want.Ino {
		_ = syscall.Unmount(pin, syscall.MNT_DETACH)
		return fmt.Errorf("%w: the folder changed while it was pinned", hostagent.ErrNotADirectory)
	}
	return nil
}

// pinMu serializes mountPinAt inside one host-agent. The boot restorer is its
// own process and runs before host-agent starts, so it needs no share of it.
var pinMu sync.Mutex

// pinRecordFile is one instance's folder-pins.json and the dir it came from,
// so the restorer can write that instance's failure marker beside it.
type pinRecordFile struct {
	dir string
	rec protocol.FolderPinRecord
}

// readPinRecords reads every instance's folder-pins.json. A missing instances
// dir (a fresh box) is no records, not an error.
func readPinRecords(stateDir string) ([]pinRecordFile, error) {
	dir := filepath.Join(stateDir, "instances")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read instances: %w", err)
	}
	var out []pinRecordFile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		idDir := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(filepath.Join(idDir, "folder-pins.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			slog.Warn("folder pin: read record", "instance_id", e.Name(), "err", err)
			continue
		}
		var rec protocol.FolderPinRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			slog.Warn("folder pin: parse record", "instance_id", e.Name(), "err", err)
			continue
		}
		out = append(out, pinRecordFile{dir: idDir, rec: rec})
	}
	return out, nil
}

// prunePins detaches and removes every pin no instance references, so an
// uninstalled app or a folder the user moved does not leave a mount behind.
func prunePins(pinRoot string, keep map[string]bool) {
	entries, err := os.ReadDir(pinRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(pinRoot, e.Name())
		if keep[p] {
			continue
		}
		_ = syscall.Unmount(p, syscall.MNT_DETACH)
		_ = os.Remove(p)
	}
}
