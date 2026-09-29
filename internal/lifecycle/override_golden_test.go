package lifecycle

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// updateGolden rewrites testdata/override-golden from the current code. The
// files were written by the code on dev before the user-namespace tiers
// existed (#529), so this test proves that on a daemon with no remap the
// override is byte for byte what it was. Never regenerate them to make a
// failure go away: a diff here is a change to every installed app.
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/override-golden")

// goldenShape is one install through the real transaction, with the fakes set
// up the way the shape needs.
type goldenShape struct {
	name    string
	install func(t *testing.T, e *testEnv) store.Instance
}

func goldenCatalogInstall(id, compose, manYAML, scope string, owner Owner, mounts []FolderMount) func(t *testing.T, e *testEnv) store.Instance {
	return func(t *testing.T, e *testEnv) store.Instance {
		t.Helper()
		e.writeCatalogApp(t, id, compose, manYAML)
		e.docker.digests[testImage] = testDigest
		inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, id), owner, scope, mounts, "", nil, nil, nil)
		if err != nil {
			t.Fatalf("install %s: %v", id, err)
		}
		return inst
	}
}

var goldenAdmin = Owner{UserID: "u_admin", Username: "admin"}

func goldenShapes() []goldenShape {
	devicesManifest := strings.Replace(whoamiManifest(testDigest), "  lan: false\n", "  lan: false\n  devices: [/dev/ttyUSB0]\n", 1)
	return []goldenShape{
		{"folderless", goldenCatalogInstall("whoami", whoamiCompose, whoamiManifest(testDigest), store.ScopeHousehold, goldenAdmin, nil)},
		{"jobs", goldenCatalogInstall("jobapp", migrateJobCompose, migrateJobManifest, store.ScopeHousehold, goldenAdmin, nil)},
		{"service-user", goldenCatalogInstall("whoami", whoamiCompose, serviceUserManifest(false), store.ScopeHousehold, goldenAdmin, nil)},
		{"folders-household", goldenCatalogInstall("filesapp", foldersCompose, foldersManifest("write", "whole"), store.ScopeHousehold, goldenAdmin,
			[]FolderMount{{Folder: "documents", Source: sourceShared}})},
		{"folders-personal", goldenCatalogInstall("filesapp", foldersCompose, foldersManifest("read", "pick-subfolder"), store.ScopePersonal, Owner{UserID: "u_alex", Username: "alex"},
			[]FolderMount{{Folder: "documents", Source: sourcePersonal, Subfolder: "Work"}})},
		{"gpu", func(t *testing.T, e *testEnv) store.Instance {
			e.host.gpu = protocol.SystemGPU{Present: true, Vendor: "intel", RenderGID: 104}
			return goldenCatalogInstall("gpuapp", migrateJobCompose, gpuManifest, store.ScopeHousehold, goldenAdmin, nil)(t, e)
		}},
		{"devices", goldenCatalogInstall("whoami", whoamiCompose, devicesManifest, store.ScopeHousehold, goldenAdmin, nil)},
		{"managed-postgres", func(t *testing.T, e *testEnv) store.Instance {
			return installDBApp(t, e, "dbapp")
		}},
		{"custom", func(t *testing.T, e *testEnv) store.Instance {
			e.docker.digests[testImage] = testDigest
			inst, err := e.m.InstallCustom(context.Background(), CustomSpec{
				Name: "My Whoami", Compose: whoamiCompose, MainPort: 80,
				Permissions: manifest.Permissions{Internet: true},
			}, goldenAdmin, store.ScopeHousehold, nil)
			if err != nil {
				t.Fatalf("install custom: %v", err)
			}
			return inst
		}},
		{"custom-folders", func(t *testing.T, e *testEnv) store.Instance {
			e.docker.digests[testImage] = testDigest
			inst, err := e.m.InstallCustom(context.Background(), CustomSpec{
				Name: "My Files", Compose: whoamiCompose, MainPort: 80,
				Permissions: manifest.Permissions{Folders: []manifest.Folder{{Folder: "photos", Mode: "write", Target: "/data/photos"}}},
			}, goldenAdmin, store.ScopeHousehold, nil)
			if err != nil {
				t.Fatalf("install custom: %v", err)
			}
			return inst
		}},
	}
}

// TestOverrideUnchangedWithoutRemap installs every manifest shape the brain
// already supports on a daemon with no userns-remap and compares the generated
// compose.override.yml with the file the code wrote before the tiers existed.
// The parts that differ per run (the instance and manifest ids, the temp dirs, the test
// runner's own uid) are replaced with fixed words first.
func TestOverrideUnchangedWithoutRemap(t *testing.T) {
	for _, sh := range goldenShapes() {
		t.Run(sh.name, func(t *testing.T) {
			e := newTestEnv(t)
			inst := sh.install(t, e)
			got := normalizeOverride(readInstanceFile(t, e, inst.ID, "compose.override.yml"), e, inst)
			path := filepath.Join("testdata", "override-golden", sh.name+".yml")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got != string(want) {
				t.Fatalf("override changed on a daemon with no remap.\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func normalizeOverride(raw string, e *testEnv, inst store.Instance) string {
	// The instance id starts with the manifest id, so it goes first. A Door-2
	// manifest id carries a random suffix, so it is replaced too.
	r := strings.NewReplacer(
		inst.ID, "INSTANCE",
		inst.ManifestID, "MANIFEST",
		e.host.homeRoot, "HOMEROOT",
		e.m.sharedRoot, "SHAREDROOT",
		e.stateDir, "STATEDIR",
		fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()), "EUID:EGID",
	)
	return r.Replace(raw)
}
