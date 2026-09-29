package lifecycle

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// root_setup is modelled and checked, but no tier acts on it yet (#529). Until
// then a root_setup manifest must render exactly the override it rendered
// before the field existed: same user: pin, cap_drop: [ALL], no cap_add.
func TestRootSetupDoesNotChangeTheOverrideYet(t *testing.T) {
	e := newTestEnv(t)
	render := func(id string, rootSetup bool) []byte {
		t.Helper()
		if err := os.MkdirAll(e.m.instanceDir(id), 0o755); err != nil {
			t.Fatal(err)
		}
		man := &manifest.Manifest{ID: "whoami", Name: "Whoami", MainService: "whoami", MainPort: 80, RootSetup: rootSetup}
		if err := e.m.writeOverride(id, man, []byte(whoamiCompose), nil, isolation{uid: 1000, gid: 1000}, store.ResourceLimits{}); err != nil {
			t.Fatalf("writeOverride: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(e.stateDir, "instances", id, "compose.override.yml"))
		if err != nil {
			t.Fatalf("read override: %v", err)
		}
		return raw
	}
	// The same instance id, so any instance-scoped names in the file match.
	without := render("inst-rs", false)
	with := render("inst-rs", true)
	if !bytes.Equal(without, with) {
		t.Fatalf("root_setup changed the override before the tiers exist:\nwithout:\n%s\nwith:\n%s", without, with)
	}
	if bytes.Contains(with, []byte("cap_add")) {
		t.Fatalf("override carries cap_add:\n%s", with)
	}
}
