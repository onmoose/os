// Package folderlist lists one level of folders for the folder chooser
// (#658, BRAIN_HOST_PROTOCOL.md # Listing folders for the folder chooser). It
// is shared by the fake host-agent, the real host-agent's filter, and the
// brain's own listing of the household shared tree, so the three apply the
// same rules: real directories only, no name that starts with a dot, no
// symlink, sorted, at most protocol.MaxFolderListing names.
package folderlist

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/onmoose/os/internal/protocol"
)

// ErrNotFound means the folder to list, or a level on the way to it, is not
// there.
var ErrNotFound = errors.New("folder not found")

// ErrNotADirectory means a level on the way is a file or a symlink.
var ErrNotADirectory = errors.New("not a directory")

// List returns one level of folders under root/rel. rel is relative to root,
// with "/" between levels, and "" lists root itself; the caller has checked
// its levels. Each level is checked with Lstat before it is entered, inside an
// os.Root, so nothing outside root is ever read and a symlinked level is
// ErrNotADirectory. It is not free of races the way the real host-agent's
// O_NOFOLLOW walk is, so the real host-agent does not use it for a home: in
// the worst case a swapped level shows the names of another folder inside
// root, never its files.
func List(root, rel string) ([]string, bool, error) {
	rt, err := os.OpenRoot(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	defer rt.Close()
	dir := "."
	if rel != "" {
		parts := strings.Split(rel, "/")
		for i := range parts {
			p := strings.Join(parts[:i+1], "/")
			fi, err := rt.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, false, ErrNotFound
			}
			if err != nil {
				return nil, false, err
			}
			if !fi.IsDir() {
				return nil, false, fmt.Errorf("%w: %q", ErrNotADirectory, p)
			}
		}
		dir = rel
	}
	f, err := rt.Open(dir)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, false, err
	}
	names, truncated := Filter(entries)
	return names, truncated, nil
}

// Filter keeps the entries that are real directories (a symlink's type is a
// symlink, never a directory) and whose names do not start with a dot, sorts
// them, and cuts the list at protocol.MaxFolderListing.
func Filter(entries []fs.DirEntry) ([]string, bool) {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type()&fs.ModeType != fs.ModeDir || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) > protocol.MaxFolderListing {
		return names[:protocol.MaxFolderListing], true
	}
	return names, false
}
