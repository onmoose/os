package brainlaunch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Caddy's admin socket dir (#608). The tests run unprivileged, so the owner is
// the test's own uid; on a box it is the remap base or 0.

func TestEnsureCaddySocketDirCreates(t *testing.T) {
	cp := t.TempDir()
	if err := EnsureCaddySocketDir(cp, os.Getuid()); err != nil {
		t.Fatalf("EnsureCaddySocketDir: %v", err)
	}
	fi, err := os.Stat(filepath.Join(cp, "caddy"))
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("got %v, want a 0700 dir", fi.Mode())
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() {
		t.Fatalf("owner uid %d, want %d", st.Uid, os.Getuid())
	}
}

func TestEnsureCaddySocketDirRepairsMode(t *testing.T) {
	cp := t.TempDir()
	dir := filepath.Join(cp, "caddy")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A socket an earlier Caddy left behind stays: Caddy replaces it itself.
	keep := filepath.Join(dir, "admin.sock")
	if err := os.WriteFile(keep, nil, 0o200); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCaddySocketDir(cp, os.Getuid()); err != nil {
		t.Fatalf("EnsureCaddySocketDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700", fi.Mode().Perm())
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Fatalf("contents touched: %v", err)
	}
}

func TestEnsureCaddySocketDirRefusesNonDir(t *testing.T) {
	for name, create := range map[string]func(string) error{
		"file":    func(p string) error { return os.WriteFile(p, nil, 0o644) },
		"symlink": func(p string) error { return os.Symlink(t.TempDir(), p) },
	} {
		t.Run(name, func(t *testing.T) {
			cp := t.TempDir()
			if err := create(filepath.Join(cp, "caddy")); err != nil {
				t.Fatal(err)
			}
			if err := EnsureCaddySocketDir(cp, os.Getuid()); err == nil {
				t.Fatalf("want an error for a %s in the way", name)
			}
		})
	}
}
