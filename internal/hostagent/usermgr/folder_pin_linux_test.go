//go:build linux

package usermgr

// Integration test for the folder pin walk and mount (#604). Needs root and
// CAP_SYS_ADMIN; it skips otherwise.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/onmoose/os/internal/hostagent"
)

func TestPinFolder_WalkRefusesALinkAndMountsTheInode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root (bind mount)")
	}
	root := t.TempDir()
	src := filepath.Join(root, "Documents", "Notebooks")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	fd, st, err := walkFolder(root, "Documents/Notebooks")
	if err != nil {
		t.Fatalf("walkFolder: %v", err)
	}
	defer syscall.Close(fd)

	pinRoot := t.TempDir()
	pin := filepath.Join(pinRoot, fmt.Sprintf("%d-%d", st.Dev, st.Ino))
	if err := mountPinAt(fd, pin, st, pinRoot); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("bind mount needs CAP_SYS_ADMIN")
		}
		t.Fatalf("mountPinAt: %v", err)
	}
	defer syscall.Unmount(pin, syscall.MNT_DETACH)

	// The pin is the same inode, and a write through it lands in the source.
	var got syscall.Stat_t
	if err := syscall.Stat(pin, &got); err != nil {
		t.Fatal(err)
	}
	if got.Dev != st.Dev || got.Ino != st.Ino {
		t.Fatalf("pin is %d:%d, want %d:%d", got.Dev, got.Ino, st.Dev, st.Ino)
	}
	if err := os.WriteFile(filepath.Join(pin, "through-pin.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write through the pin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "through-pin.txt")); err != nil {
		t.Errorf("write through the pin did not land in the source: %v", err)
	}

	// A symlink at the source is refused by the next walk.
	if err := os.Rename(src, src+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".ssh"), src); err != nil {
		t.Fatal(err)
	}
	if _, _, err := walkFolder(root, "Documents/Notebooks"); !errors.Is(err, hostagent.ErrNotADirectory) {
		t.Fatalf("walkFolder(link) = %v; want ErrNotADirectory", err)
	}
}
