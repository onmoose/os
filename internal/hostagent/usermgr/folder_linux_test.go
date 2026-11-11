//go:build linux

package usermgr

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	if _, err := prepareUnder(home, "Documents/Notebooks/2026", uid, gid); err != nil {
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
		if _, err := prepareUnder(home, "Photos", uid, gid); err != nil {
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
	if _, err := prepareUnder(home, "Documents", uid, gid); err != nil {
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
			_, err := prepareUnder(home, tc.rel, uid, gid)
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
	if _, err := prepareUnder(home, "Music/Playlists", uid, gid); !errors.Is(err, hostagent.ErrNotADirectory) {
		t.Fatalf("err = %v, want ErrNotADirectory", err)
	}
}

func TestPrepareUnder_RefusesBadInput(t *testing.T) {
	uid, gid := self()
	home := t.TempDir()
	if _, err := prepareUnder("relative/home", "Photos", uid, gid); err == nil {
		t.Error("relative home: want an error")
	}
	for _, rel := range []string{"Photos/../..", "Photos//x", "./Photos", "", ".ssh", "Projects/.git", "bin", "bin/x"} {
		if _, err := prepareUnder(home, rel, uid, gid); err == nil {
			t.Errorf("rel %q: want an error", rel)
		}
	}
	if _, err := prepareUnder(filepath.Join(home, "missing"), "Photos", uid, gid); err == nil {
		t.Error("missing home: want an error")
	}
}

// prepareUnder says whether it made the first level (#658): the brain
// records a new folder only when host-agent made it.
func TestPrepareUnder_ReportsWhetherItMadeTheTop(t *testing.T) {
	home := t.TempDir()
	uid, gid := self()
	if made, err := prepareUnder(home, "Recipes/Cakes", uid, gid); err != nil || !made {
		t.Fatalf("first call: made = %v, %v; want true", made, err)
	}
	if made, err := prepareUnder(home, "Recipes/Bread", uid, gid); err != nil || made {
		t.Fatalf("second call: made = %v, %v; want false", made, err)
	}
	if err := os.Mkdir(filepath.Join(home, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if made, err := prepareUnder(home, "scripts", uid, gid); err != nil || made {
		t.Fatalf("hand-made folder: made = %v, %v; want false", made, err)
	}
}

// listUnder lists real directories only, sorted, with no dot name, no file
// and no symlink, and never walks through a symlink (#658).
func TestListUnder(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{"Documents/Taxes", "Documents/Books", "Documents/.git", "Photos"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "Documents", "note.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "Documents", "Linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "Music")); err != nil {
		t.Fatal(err)
	}

	names, truncated, err := listUnder(home, "Documents")
	if err != nil || truncated {
		t.Fatalf("listUnder(Documents) = %v, %v, %v", names, truncated, err)
	}
	if want := []string{"Books", "Taxes"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if names, _, err := listUnder(home, ""); err != nil || !slices.Equal(names, []string{"Documents", "Photos"}) {
		t.Errorf("listUnder(top) = %v, %v; want [Documents Photos]", names, err)
	}
	if _, _, err := listUnder(home, "Music"); !errors.Is(err, hostagent.ErrNotADirectory) {
		t.Errorf("listUnder(symlinked level) = %v; want ErrNotADirectory", err)
	}
	if _, _, err := listUnder(home, "Documents/Linked"); !errors.Is(err, hostagent.ErrNotADirectory) {
		t.Errorf("listUnder(symlinked deeper level) = %v; want ErrNotADirectory", err)
	}
	if _, _, err := listUnder(home, "Documents/note.txt"); !errors.Is(err, hostagent.ErrNotADirectory) {
		t.Errorf("listUnder(file) = %v; want ErrNotADirectory", err)
	}
	if _, _, err := listUnder(home, "Videos"); !errors.Is(err, hostagent.ErrFolderNotFound) {
		t.Errorf("listUnder(missing) = %v; want ErrFolderNotFound", err)
	}
	if _, _, err := listUnder(home, "Documents/.git"); err == nil {
		t.Errorf("listUnder(dot level) succeeded; want an error")
	}
}
