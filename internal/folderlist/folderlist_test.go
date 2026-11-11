package folderlist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

func TestList(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"Documents/Taxes", "Documents/Books", "Documents/.cache", "Photos"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "Documents", "a.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "Documents", "Out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Music")); err != nil {
		t.Fatal(err)
	}

	names, truncated, err := List(root, "Documents")
	if err != nil || truncated || !slices.Equal(names, []string{"Books", "Taxes"}) {
		t.Errorf("List(Documents) = %v, %v, %v; want [Books Taxes]", names, truncated, err)
	}
	if names, _, err := List(root, ""); err != nil || !slices.Equal(names, []string{"Documents", "Photos"}) {
		t.Errorf("List(top) = %v, %v", names, err)
	}
	if _, _, err := List(root, "Music"); !errors.Is(err, ErrNotADirectory) {
		t.Errorf("List(symlink) = %v; want ErrNotADirectory", err)
	}
	if _, _, err := List(root, "Documents/a.txt"); !errors.Is(err, ErrNotADirectory) {
		t.Errorf("List(file) = %v; want ErrNotADirectory", err)
	}
	if _, _, err := List(root, "Videos"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List(missing) = %v; want ErrNotFound", err)
	}
	if _, _, err := List(filepath.Join(root, "nope"), ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("List(missing root) = %v; want ErrNotFound", err)
	}
}

func TestList_Truncates(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < protocol.MaxFolderListing+5; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d%05d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	names, truncated, err := List(root, "")
	if err != nil || !truncated || len(names) != protocol.MaxFolderListing || names[0] != "d00000" {
		t.Errorf("List = %d names, truncated %v, %v", len(names), truncated, err)
	}
}
