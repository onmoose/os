package lifecycle

// Shared folder-source preparation (#156): the household shared tree is
// root:moose-shared, mode 02770 setgid (STORAGE.md # user content), so the
// brain creates an elected shared <Folder>[/<subfolder>] owning each NEW level
// to the moose-shared group with the setgid bit — never chowning to a runtime
// UID, never re-owning a pre-existing parent. Writing the shared tree needs
// root, so the install loop runs it only under euid 0 and skips it (warn) under
// the unprivileged dev brain; the creation logic itself is exercised here
// directly with a temp root + the test process's own GID (a group it can
// chgrp/chmod to without privilege).

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/onmoose/os/internal/store"
)

func statDir(t *testing.T, dir string) (os.FileInfo, *syscall.Stat_t) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %q: %v", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %q: unexpected Stat_t type", dir)
	}
	return fi, st
}

func TestPrepareSharedSource_CreatesSetgidGroupOwnedLevels(t *testing.T) {
	root := t.TempDir()
	gid := os.Getegid()
	src := filepath.Join(root, "Documents", "Notebooks")

	if err := prepareSharedSource(root, src, gid); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// Both the elected folder and its subfolder are created, each with the
	// shared-tree mode (group rwx + setgid) and the moose-shared group.
	for _, d := range []string{filepath.Join(root, "Documents"), src} {
		fi, st := statDir(t, d)
		if fi.Mode().Perm() != 0o770 {
			t.Errorf("%q perm = %o, want 770", d, fi.Mode().Perm())
		}
		if fi.Mode()&os.ModeSetgid == 0 {
			t.Errorf("%q missing setgid bit (mode %v)", d, fi.Mode())
		}
		if int(st.Gid) != gid {
			t.Errorf("%q gid = %d, want moose-shared %d", d, st.Gid, gid)
		}
	}
}

func TestPrepareSharedSource_Idempotent(t *testing.T) {
	root := t.TempDir()
	gid := os.Getegid()
	src := filepath.Join(root, "Movies", "Family")

	if err := prepareSharedSource(root, src, gid); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if err := prepareSharedSource(root, src, gid); err != nil {
		t.Fatalf("second prepare (must be a no-op): %v", err)
	}
	if fi, _ := statDir(t, src); fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm() != 0o770 {
		t.Errorf("source mode after re-prepare = %v, want setgid + 770", fi.Mode())
	}
}

func TestPrepareSharedSource_NeverReownsPreexistingParent(t *testing.T) {
	root := t.TempDir()
	gid := os.Getegid()
	// A shared <Folder> that already exists (storage setup created it) with a
	// deliberately non-shared mode must be left exactly as-is; only the missing
	// leaf below it is created with the shared mode.
	parent := filepath.Join(root, "Music")
	if err := os.Mkdir(parent, 0o701); err != nil {
		t.Fatal(err)
	}
	if err := prepareSharedSource(root, filepath.Join(parent, "Playlists"), gid); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if fi, _ := statDir(t, parent); fi.Mode().Perm() != 0o701 {
		t.Errorf("pre-existing parent re-moded: perm = %o, want 701 (untouched)", fi.Mode().Perm())
	}
	if fi, _ := statDir(t, filepath.Join(parent, "Playlists")); fi.Mode()&os.ModeSetgid == 0 {
		t.Error("new leaf missing setgid bit")
	}
}

func TestPrepareSharedSource_Rejects(t *testing.T) {
	gid := os.Getegid()

	// src outside the shared root — a programming error, never silently created.
	if err := prepareSharedSource(t.TempDir(), "/etc/moose-bogus", gid); err == nil {
		t.Error("want error for a source outside the shared root")
	}
	// shared root itself absent — a storage-setup fault, not ours to create.
	missing := filepath.Join(t.TempDir(), "nope")
	if err := prepareSharedSource(missing, filepath.Join(missing, "Documents"), gid); err == nil {
		t.Error("want error when the shared root does not exist")
	}
	// shared root is a regular file, not a directory.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareSharedSource(file, filepath.Join(file, "x"), gid); err == nil {
		t.Error("want error when the shared root is not a directory")
	}
	// A path component is an existing file: creating a child under it must
	// surface the OS error, not be mistaken for a missing directory to create.
	froot := t.TempDir()
	if err := os.WriteFile(filepath.Join(froot, "Documents"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareSharedSource(froot, filepath.Join(froot, "Documents", "sub"), gid); err == nil {
		t.Error("want error when a path component is a file")
	}
	// The remaining failure paths need an unprivileged process — root ignores
	// directory write bits and can chgrp to any group.
	if os.Geteuid() == 0 {
		return
	}
	// mkdir failure: a missing leaf under a read-only parent.
	roRoot := t.TempDir()
	ro := filepath.Join(roRoot, "Music")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := prepareSharedSource(roRoot, filepath.Join(ro, "leaf"), gid); err == nil {
		t.Error("want mkdir error under a read-only parent")
	}
	// chown failure: group 0 (root), which an unprivileged process is not a
	// member of, so the chgrp to the moose-shared GID is rejected.
	cgRoot := t.TempDir()
	if err := prepareSharedSource(cgRoot, filepath.Join(cgRoot, "Notes"), 0); err == nil {
		t.Error("want chown error for a non-member gid")
	}
}

// TestInstallFolders_SharedSkippedUnderUnprivilegedBrain pins the dev-seam
// decision: under the unprivileged native dev brain (euid != 0, the suite's own
// posture) a household shared-folder install does NOT hard-fail and does NOT
// create the shared source — preparation is skipped (out-of-inner-loop, #156),
// leaving the override bind for the real (root) brain to back. Guards against a
// regression that would make `make dev` installs of household folder apps fail.
func TestInstallFolders_SharedSkippedUnderUnprivilegedBrain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("euid 0: shared-source prep runs for real; this guards the unprivileged skip path")
	}
	e := newTestEnv(t)
	app, _ := installFolders(t, e, store.ScopeHousehold,
		Owner{UserID: "u_admin", Username: "admin"},
		foldersManifest("write", "pick-subfolder"),
		[]FolderMount{{Folder: "documents", Source: sourceShared, Subfolder: "Shared"}})

	// Install succeeded (installFolders fatals otherwise) and the override still
	// binds the shared source the root brain would have prepared...
	wantVol := filepath.Join(e.m.sharedRoot, "Documents", "Shared") + ":/moose/documents:rw"
	if !hasString(app["volumes"], wantVol) {
		t.Errorf("volumes: want %q, got %v", wantVol, app["volumes"])
	}
	// ...but the unprivileged brain created nothing under the shared root.
	if _, err := os.Stat(filepath.Join(e.m.sharedRoot, "Documents")); !os.IsNotExist(err) {
		t.Errorf("shared source must not be created under the unprivileged brain, stat err = %v", err)
	}
}
