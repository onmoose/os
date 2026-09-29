//go:build linux

package usermgr

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/onmoose/os/internal/hostagent"
)

// The tests run as whoever runs the suite, so they chown to that user's own
// uid and gid: a no-op the kernel allows without privilege. What they check is
// the walk itself: what gets created, and what gets refused.
func self() (int, int) { return os.Getuid(), os.Getgid() }

func TestPrepareUnder_CreatesEveryMissingLevel(t *testing.T) {
	home := t.TempDir()
	uid, gid := self()
	if err := prepareUnder(home, "Documents/Notebooks/2026", uid, gid); err != nil {
		t.Fatalf("prepareUnder: %v", err)
	}
	fi, err := os.Stat(filepath.Join(home, "Documents", "Notebooks", "2026"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("leaf not created as a directory: %v", err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Errorf("leaf owner = %d:%d, want %d:%d", st.Uid, st.Gid, uid, gid)
	}
}

func TestPrepareUnder_IsIdempotent(t *testing.T) {
	home := t.TempDir()
	uid, gid := self()
	for i := 0; i < 2; i++ {
		if err := prepareUnder(home, "Photos", uid, gid); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

func TestPrepareUnder_KeepsWhatIsAlreadyInside(t *testing.T) {
	home := t.TempDir()
	uid, gid := self()
	keep := filepath.Join(home, "Documents", "old.txt")
	if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareUnder(home, "Documents", uid, gid); err != nil {
		t.Fatalf("prepareUnder: %v", err)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "keep" {
		t.Errorf("existing content changed: %q, %v", b, err)
	}
	// An existing level keeps its mode: the op sets the owner, nothing else.
	fi, _ := os.Stat(filepath.Dir(keep))
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("existing folder mode = %v, want 0700 kept", fi.Mode().Perm())
	}
}

// The case the whole walk is built around. The user owns the home, so they can
// point ~/Documents anywhere. A root process that followed it would create and
// chown inside the target. The walk must refuse, and leave the target alone.
func TestPrepareUnder_RefusesASymlinkAtAnyLevel(t *testing.T) {
	uid, gid := self()
	for _, tc := range []struct{ name, link, rel string }{
		{"first level", "Documents", "Documents/Sub"},
		{"deeper level", "Documents/Sub", "Documents/Sub/Leaf"},
		{"last level", "Documents", "Documents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			target := t.TempDir()
			link := filepath.Join(home, filepath.FromSlash(tc.link))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			err := prepareUnder(home, tc.rel, uid, gid)
			if !errors.Is(err, hostagent.ErrNotADirectory) {
				t.Fatalf("err = %v, want ErrNotADirectory", err)
			}
			entries, _ := os.ReadDir(target)
			if len(entries) != 0 {
				t.Errorf("the walk wrote through the symlink: %v", entries)
			}
		})
	}
}

func TestPrepareUnder_RefusesAFileInTheWay(t *testing.T) {
	home := t.TempDir()
	uid, gid := self()
	if err := os.WriteFile(filepath.Join(home, "Music"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareUnder(home, "Music/Playlists", uid, gid); !errors.Is(err, hostagent.ErrNotADirectory) {
		t.Fatalf("err = %v, want ErrNotADirectory", err)
	}
}

func TestPrepareUnder_RefusesBadInput(t *testing.T) {
	uid, gid := self()
	home := t.TempDir()
	if err := prepareUnder("relative/home", "Photos", uid, gid); err == nil {
		t.Error("relative home: want an error")
	}
	for _, rel := range []string{"Photos/../..", "Photos//x", "./Photos", ""} {
		if err := prepareUnder(home, rel, uid, gid); err == nil {
			t.Errorf("rel %q: want an error", rel)
		}
	}
	if err := prepareUnder(filepath.Join(home, "missing"), "Photos", uid, gid); err == nil {
		t.Error("missing home: want an error")
	}
}
