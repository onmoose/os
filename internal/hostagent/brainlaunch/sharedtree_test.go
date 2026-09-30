package brainlaunch

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The household shared tree is mounted into the brain read-write at the same
// path, so the brain's prepareSharedSource sees the host tree and the bind it
// writes into an app override names a path that exists on the host (#519).
func TestLaunchRunSpecSharedTreeMount(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.SharedRoot = "/srv/moose/shared"
	if err := Launch(context.Background(), f, cfg); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	s := f.lastRun
	if !hasMount(s.Mounts, "/srv/moose/shared", "/srv/moose/shared") {
		t.Fatalf("missing same-path shared-tree mount: %+v", s.Mounts)
	}
	for _, m := range s.Mounts {
		if m.Source == "/srv/moose/shared" && m.ReadOnly {
			t.Errorf("shared-tree mount must be read-write, the brain creates folders in it: %+v", m)
		}
		// /home stays out of the brain. Personal folders are host-agent's job.
		if m.Source == "/home" || m.Target == "/home" {
			t.Errorf("brain must not mount /home: %+v", m)
		}
	}
	// The update path rebuilds the brain from the same spec, so an updated
	// brain keeps the mount.
	if !hasMount(RunSpecFor(cfg).Mounts, "/srv/moose/shared", "/srv/moose/shared") {
		t.Error("RunSpecFor dropped the shared-tree mount; an updated brain would lose it")
	}
}

// No shared root (make dev, or a box where EnsureSharedTree failed) means no
// mount. A bind of a missing source would make Docker create it root:root.
func TestLaunchRunSpecNoSharedTreeWhenUnset(t *testing.T) {
	f := newFake()
	if err := Launch(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for _, m := range f.lastRun.Mounts {
		if m.Target == "/srv/moose/shared" {
			t.Errorf("unexpected shared-tree mount with SharedRoot empty: %+v", m)
		}
	}
}

func statOf(t *testing.T, p string) (os.FileMode, uint32) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode(), fi.Sys().(*syscall.Stat_t).Gid
}

// The owner is set back too, not only the group: a user who owned the root
// could change its mode again (review, #522). Changing the owner needs root, so
// this runs only as root.
func TestEnsureSharedTree_RepairsOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown to another user")
	}
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 3001, 3001); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedTree(root, 0, 2001); err != nil {
		t.Fatalf("EnsureSharedTree: %v", err)
	}
	fi, _ := os.Lstat(root)
	st := fi.Sys().(*syscall.Stat_t)
	if st.Uid != 0 || st.Gid != 2001 {
		t.Errorf("owner = %d:%d, want 0:2001", st.Uid, st.Gid)
	}
}

// The test chgrps to its own group: a change the kernel allows without root.
func TestEnsureSharedTree_CreatesTreeAndParent(t *testing.T) {
	gid := os.Getegid()
	root := filepath.Join(t.TempDir(), "srv", "moose", "shared")
	if err := EnsureSharedTree(root, os.Getuid(), gid); err != nil {
		t.Fatalf("EnsureSharedTree: %v", err)
	}
	mode, g := statOf(t, root)
	if !mode.IsDir() || mode&os.ModeSetgid == 0 || mode.Perm() != 0o770 {
		t.Errorf("mode = %v, want drwxrws--- (02770)", mode)
	}
	if int(g) != gid {
		t.Errorf("group = %d, want %d", g, gid)
	}
}

// A tree that drifted (made by hand, or root:root 0755 by an old Docker bind)
// is set back to the model, and nothing inside it is touched.
func TestEnsureSharedTree_RepairsExistingRootOnly(t *testing.T) {
	gid := os.Getegid()
	root := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(filepath.Join(root, "Documents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedTree(root, os.Getuid(), gid); err != nil {
		t.Fatalf("EnsureSharedTree: %v", err)
	}
	if mode, _ := statOf(t, root); mode&os.ModeSetgid == 0 || mode.Perm() != 0o770 {
		t.Errorf("root mode = %v, want 02770", mode)
	}
	if mode, _ := statOf(t, filepath.Join(root, "Documents")); mode.Perm() != 0o755 || mode&os.ModeSetgid != 0 {
		t.Errorf("a folder inside the tree was changed: %v", mode)
	}
	// Idempotent.
	if err := EnsureSharedTree(root, os.Getuid(), gid); err != nil {
		t.Fatalf("second EnsureSharedTree: %v", err)
	}
}

func TestEnsureSharedTree_RefusesSymlinkOrFile(t *testing.T) {
	gid := os.Getegid()
	dir := t.TempDir()
	target := t.TempDir()
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "shared")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedTree(link, os.Getuid(), gid); err == nil {
		t.Error("symlink: want an error")
	}
	// Nothing was changed through the link.
	if mode, _ := statOf(t, target); mode.Perm() != 0o700 || mode&os.ModeSetgid != 0 {
		t.Errorf("the symlink target was changed: %v", mode)
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSharedTree(file, os.Getuid(), gid); err == nil {
		t.Error("file: want an error")
	}
}
