package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

const testRemapBase = 1000000

// remapped turns the remap on for both sides of the check: host-agent reports
// the base and Docker lists name=userns.
func (e *testEnv) remapped() {
	b := testRemapBase
	e.host.remapBase = &b
	e.docker.usernsRemap = true
}

// chownRecorder replaces the Manager's chown, so a test run as any user can
// see which owner each path was given.
type chownRecorder struct {
	mu     sync.Mutex
	owners map[string]string // path → "uid:gid"
}

func recordChowns(e *testEnv) *chownRecorder {
	r := &chownRecorder{owners: map[string]string{}}
	e.m.chown = func(p string, uid, gid int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.owners[p] = fmt.Sprintf("%d:%d", uid, gid)
		return nil
	}
	return r
}

func (r *chownRecorder) owner(p string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owners[p]
}

// overrideServices is the part of a service entry the tier decides.
type overrideService struct {
	UsernsMode string   `yaml:"userns_mode"`
	CapAdd     []string `yaml:"cap_add"`
	CapDrop    []string `yaml:"cap_drop"`
	User       string   `yaml:"user"`
}

func parseOverrideServices(t *testing.T, raw string) map[string]overrideService {
	t.Helper()
	var doc struct {
		Services map[string]overrideService `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	return doc.Services
}

// TestUsernsNeverHostWithCaps is #516 proof 9 as a real test. It runs the real
// tier pick and the real override generator over every manifest shape that
// picks a tier, each with and without root_setup, on a daemon with and without
// the remap. For every service it checks: never userns_mode: host together
// with cap_add, cap_drop: [ALL] in every tier, the expected tier, and no user:
// in the caps tier. It also pins which shapes the install refuses: admission
// for root_setup with a host-tier grant, and the tier pick for root_setup with
// no remap.
func TestUsernsNeverHostWithCaps(t *testing.T) {
	const compose = `
services:
  app:
    image: example/app:1
  worker:
    image: example/worker:1
`
	type shape struct {
		name     string
		folders  bool
		gpu      bool
		devices  bool
		svcUser  bool
		hostTier bool // a folders, gpu or devices grant
	}
	shapes := []shape{
		{name: "folderless"},
		{name: "service-user", svcUser: true},
		{name: "folders", folders: true, hostTier: true},
		{name: "gpu", gpu: true, hostTier: true},
		{name: "devices", devices: true, hostTier: true},
		{name: "folders-and-gpu", folders: true, gpu: true, hostTier: true},
	}
	for _, base := range []int{0, testRemapBase} {
		for _, sh := range shapes {
			for _, rootSetup := range []bool{false, true} {
				name := fmt.Sprintf("base-%d/%s/root_setup=%v", base, sh.name, rootSetup)
				t.Run(name, func(t *testing.T) {
					man := &manifest.Manifest{ID: "app-" + sh.name, Name: "App", MainService: "app", MainPort: 80, ServiceUser: sh.svcUser, RootSetup: rootSetup}
					if sh.folders {
						man.Permissions.Folders = []manifest.Folder{{Folder: "documents", Mode: "write"}}
					}
					man.Permissions.GPU = sh.gpu
					if sh.devices {
						man.Permissions.Devices = []string{"/dev/ttyUSB0"}
					}

					// What the install refuses, and where.
					admitErr := admission.CheckManifest(man)
					wantAdmitRefuse := rootSetup && (sh.hostTier || sh.svcUser)
					if (admitErr != nil) != wantAdmitRefuse {
						t.Fatalf("CheckManifest err = %v, want refused=%v", admitErr, wantAdmitRefuse)
					}
					tier, pickErr := pickTier(man, base)
					if rootSetup && base == 0 {
						if !errors.Is(pickErr, ErrRootSetupNeedsRemap) {
							t.Fatalf("pickTier with no remap = %q, %v; want ErrRootSetupNeedsRemap", tier, pickErr)
						}
						return
					}
					if pickErr != nil {
						t.Fatalf("pickTier: %v", pickErr)
					}
					wantTier := store.UsernsTierDefault
					switch {
					case base == 0 || sh.hostTier:
						wantTier = store.UsernsTierHost
					case rootSetup:
						wantTier = store.UsernsTierCaps
					}
					if tier != wantTier {
						t.Fatalf("pickTier = %q, want %q", tier, wantTier)
					}

					// Render the override even for the shapes admission refuses:
					// the generator is the structural guard, and it must hold on
					// its own.
					e := newTestEnv(t)
					id := "inst-" + sh.name
					if err := os.MkdirAll(e.m.instanceDir(id), 0o755); err != nil {
						t.Fatal(err)
					}
					iso := isolation{uid: 2100, gid: 2100, tier: tier, remapBase: base}
					if sh.gpu {
						iso.gpu = protocol.SystemGPU{Present: true, RenderGID: 105}
					}
					if err := e.m.writeOverride(id, man, []byte(compose), nil, iso, store.ResourceLimits{}); err != nil {
						t.Fatalf("writeOverride: %v", err)
					}
					svcs := parseOverrideServices(t, readInstanceFile(t, e, id, "compose.override.yml"))
					if len(svcs) != 2 {
						t.Fatalf("override has %d services, want 2", len(svcs))
					}
					for svc, got := range svcs {
						if got.UsernsMode == "host" && len(got.CapAdd) > 0 {
							t.Fatalf("%s: userns_mode host WITH cap_add %v", svc, got.CapAdd)
						}
						if !slices.Equal(got.CapDrop, []string{"ALL"}) {
							t.Fatalf("%s: cap_drop = %v, want [ALL] in every tier", svc, got.CapDrop)
						}
						if base == 0 && (got.UsernsMode != "" || len(got.CapAdd) > 0) {
							t.Fatalf("%s: no remap but userns_mode %q cap_add %v", svc, got.UsernsMode, got.CapAdd)
						}
						var rendered string
						switch {
						case got.UsernsMode == "host":
							rendered = store.UsernsTierHost
						case len(got.CapAdd) > 0:
							rendered = store.UsernsTierCaps
						case base == 0:
							rendered = store.UsernsTierHost // the whole daemon is in the host namespace
						default:
							rendered = store.UsernsTierDefault
						}
						if rendered != wantTier {
							t.Fatalf("%s: rendered tier %s, want %s", svc, rendered, wantTier)
						}
						switch rendered {
						case store.UsernsTierCaps:
							if got.User != "" {
								t.Fatalf("%s: caps tier keeps user %q, want it removed", svc, got.User)
							}
							if !slices.Equal(got.CapAdd, capsTierCaps) {
								t.Fatalf("%s: cap_add = %v, want %v", svc, got.CapAdd, capsTierCaps)
							}
						default:
							if got.User != "2100:2100" {
								t.Fatalf("%s: user = %q, want 2100:2100", svc, got.User)
							}
						}
					}
				})
			}
		}
	}
}

// applyTier refuses to write the two together even if a caller hands it an
// entry that already carries one of them.
func TestApplyTierRefusesHostWithCaps(t *testing.T) {
	iso := isolation{tier: store.UsernsTierHost, remapBase: testRemapBase}
	if err := iso.applyTier(map[string]any{"cap_add": []string{"CHOWN"}}); err == nil {
		t.Fatal("applyTier wrote userns_mode: host next to cap_add")
	}
	iso = isolation{tier: store.UsernsTierCaps}
	if err := iso.applyTier(map[string]any{}); err == nil {
		t.Fatal("applyTier accepted the caps tier with no remap")
	}
}

func TestBindOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		iso  isolation
		want string
	}{
		{"no remap", isolation{uid: 0, gid: 0, tier: store.UsernsTierHost}, "0:0"},
		{"no remap service user", isolation{uid: 2100, gid: 2100, tier: store.UsernsTierHost}, "2100:2100"},
		{"default root inside", isolation{uid: 0, gid: 0, tier: store.UsernsTierDefault, remapBase: testRemapBase}, "1000000:1000000"},
		{"default service user", isolation{uid: 2101, gid: 2101, tier: store.UsernsTierDefault, remapBase: testRemapBase}, "1002101:1002101"},
		{"caps", isolation{uid: 0, gid: 0, tier: store.UsernsTierCaps, remapBase: testRemapBase}, "1000000:1000000"},
		{"host folder owner", isolation{uid: 3000, gid: 3000, tier: store.UsernsTierHost, remapBase: testRemapBase}, "3000:3000"},
	} {
		uid, gid, err := tc.iso.bindOwner()
		if err != nil || fmt.Sprintf("%d:%d", uid, gid) != tc.want {
			t.Errorf("%s: bindOwner = %d:%d, %v; want %s", tc.name, uid, gid, err, tc.want)
		}
	}
	if _, _, err := (&isolation{uid: 70000, gid: 0, tier: store.UsernsTierDefault, remapBase: testRemapBase}).bindOwner(); err == nil {
		t.Error("bindOwner accepted a uid outside the remap range")
	}
}

func TestUsernsInSecurityOptions(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`["name=seccomp,profile=builtin","name=cgroupns"]`, false},
		{`["name=seccomp,profile=builtin","name=userns","name=cgroupns"]`, true},
		{`["name=apparmor","name=userns"]` + "\n", true},
		{`[]`, false},
		{`null`, false},
	} {
		got, err := usernsInSecurityOptions([]byte(tc.raw))
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
	if _, err := usernsInSecurityOptions([]byte("not json")); err == nil {
		t.Error("parsed a non-JSON answer")
	}
}

// rootSetupManifest is the whoami manifest with root_setup: true.
func rootSetupManifest() string {
	return strings.Replace(whoamiManifest(testDigest), "main_port: 80\n", "main_port: 80\nroot_setup: true\n", 1)
}

func installWhoamiWith(t *testing.T, e *testEnv, manYAML string) (store.Instance, error) {
	t.Helper()
	e.writeCatalogApp(t, "whoami", whoamiCompose, manYAML)
	e.docker.digests[testImage] = testDigest
	return e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), goldenAdmin, store.ScopeHousehold, nil, "", nil, nil, nil)
}

func assertNoInstallState(t *testing.T, e *testEnv) {
	t.Helper()
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows, got %d", len(insts))
	}
	if e.docker.called("Pull") || e.docker.called("ComposeUp") || e.caddy.called("AddSplashRoute") {
		t.Errorf("install did Docker or Caddy work before refusing: %v", e.docker.methods())
	}
}

// On a daemon with no remap a root_setup app is refused before any state,
// with the plain message: there the five capabilities would be real ones.
func TestInstallRootSetupRefusedWithoutRemap(t *testing.T) {
	e := newTestEnv(t)
	_, err := installWhoamiWith(t, e, rootSetupManifest())
	if !errors.Is(err, ErrRootSetupNeedsRemap) {
		t.Fatalf("install = %v, want ErrRootSetupNeedsRemap", err)
	}
	assertNoInstallState(t, e)
}

// Every install is refused while Docker and host-agent disagree, both ways.
func TestInstallRefusedWhenRemapDisagrees(t *testing.T) {
	base := testRemapBase
	for name, set := range map[string]func(e *testEnv){
		"docker remapped, host has no range": func(e *testEnv) { e.docker.usernsRemap = true },
		"host has a range, docker not":       func(e *testEnv) { e.host.remapBase = &base },
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			set(e)
			_, err := installWhoamiWith(t, e, whoamiManifest(testDigest))
			if !errors.Is(err, ErrRemapMismatch) {
				t.Fatalf("install = %v, want ErrRemapMismatch", err)
			}
			assertNoInstallState(t, e)
		})
	}
}

// A failed read on either side refuses the install too, rather than guessing.
func TestInstallRefusedWhenRemapUnreadable(t *testing.T) {
	for name, set := range map[string]func(e *testEnv){
		"docker info fails": func(e *testEnv) { e.docker.usernsRemapErr = errors.New("boom") },
		"well-known fails":  func(e *testEnv) { e.host.wellKnownErr = errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			set(e)
			if _, err := installWhoamiWith(t, e, whoamiManifest(testDigest)); err == nil {
				t.Fatal("install succeeded with an unreadable remap")
			}
			assertNoInstallState(t, e)
		})
	}
}

// With no remap every instance is stored as the host tier.
func TestInstallStoresHostTierWithoutRemap(t *testing.T) {
	e := newTestEnv(t)
	inst, err := installWhoamiWith(t, e, whoamiManifest(testDigest))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.UsernsTier != store.UsernsTierHost {
		t.Fatalf("tier = %q, want host", row.UsernsTier)
	}
}

// A folderless app on a remapped daemon is in the default tier: remapped, a
// pinned user:, and its data dir owned by base+uid.
func TestInstallRemappedFolderlessDefaultTier(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	chowns := recordChowns(e)
	e.writeCatalogApp(t, "whoami", `
services:
  whoami:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data:/data
`, serviceUserManifest(false))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), goldenAdmin, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	row, _ := e.store.Get(inst.ID)
	if row.UsernsTier != store.UsernsTierDefault {
		t.Fatalf("tier = %q, want default", row.UsernsTier)
	}
	svc := parseOverrideServices(t, readInstanceFile(t, e, inst.ID, "compose.override.yml"))["whoami"]
	wantUser := fmt.Sprintf("%d:%d", row.ServiceUID, row.ServiceGID)
	if svc.UsernsMode != "" || len(svc.CapAdd) > 0 || svc.User != wantUser {
		t.Fatalf("default tier service = %+v, want remapped with user %s", svc, wantUser)
	}
	dataDir := filepath.Join(e.m.instanceDir(inst.ID), "data")
	want := fmt.Sprintf("%d:%d", testRemapBase+row.ServiceUID, testRemapBase+row.ServiceGID)
	if got := chowns.owner(dataDir); got != want {
		t.Fatalf("data dir owner = %q, want base+uid %s", got, want)
	}
}

// A root_setup app on a remapped daemon is in the caps tier: the five
// capabilities, no user:, and its data dir owned by base.
func TestInstallRemappedRootSetupCapsTier(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	chowns := recordChowns(e)
	e.writeCatalogApp(t, "whoami", `
services:
  whoami:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data:/var/www/html/data
`, rootSetupManifest())
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), goldenAdmin, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.UsernsTier != store.UsernsTierCaps {
		t.Fatalf("tier = %q, want caps", row.UsernsTier)
	}
	svc := parseOverrideServices(t, readInstanceFile(t, e, inst.ID, "compose.override.yml"))["whoami"]
	if svc.UsernsMode != "" || !slices.Equal(svc.CapAdd, capsTierCaps) || svc.User != "" {
		t.Fatalf("caps tier service = %+v", svc)
	}
	if got := chowns.owner(filepath.Join(e.m.instanceDir(inst.ID), "data")); got != "1000000:1000000" {
		t.Fatalf("data dir owner = %q, want base:base", got)
	}
}

// A folder app on a remapped daemon is in the host tier: userns_mode: host on
// every service, and the real owner ids.
func TestInstallRemappedFolderAppHostTier(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	app, _ := installFolders(t, e, store.ScopeHousehold, goldenAdmin, foldersManifest("write", "whole"),
		[]FolderMount{{Folder: "documents", Source: sourceShared}})
	if app["userns_mode"] != "host" {
		t.Fatalf("folder app userns_mode = %v, want host", app["userns_mode"])
	}
	if _, ok := app["cap_add"]; ok {
		t.Fatalf("folder app has cap_add: %v", app["cap_add"])
	}
	if app["user"] != "2000:2000" {
		t.Fatalf("folder app user = %v, want the real moose-app 2000:2000", app["user"])
	}
	list, _ := e.store.List()
	if len(list) != 1 || list[0].UsernsTier != store.UsernsTierHost {
		t.Fatalf("stored tier = %+v, want host", list)
	}
}

// A managed service's data dir goes to base:base on a remapped daemon, so the
// service image's own entrypoint can take it from there.
func TestManagedServiceDataOwnedByRemapBase(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	chowns := recordChowns(e)
	installDBApp(t, e, "dbapp")
	data := filepath.Join(e.m.serviceDir("postgres", "15"), "data")
	if got := chowns.owner(data); got != "1000000:1000000" {
		t.Fatalf("service data owner = %q, want base:base", got)
	}
}

// Valkey's data dir holds a file the brain writes (users.acl), and it goes to
// base:base too.
func TestManagedValkeyDataOwnedByRemapBase(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	chowns := recordChowns(e)
	installDBAppKind(t, e, "cacheapp", "valkey", "8")
	data := filepath.Join(e.m.serviceDir("valkey", "8"), "data")
	for _, p := range []string{data, filepath.Join(data, "users.acl")} {
		if got := chowns.owner(p); got != "1000000:1000000" {
			t.Fatalf("%s owner = %q, want base:base", p, got)
		}
	}
}

// A chown that fails as root leaves no .env, so the next install sets the
// service dir up again instead of skipping it.
func TestManagedServiceChownFailureLeavesNoEnv(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	e.m.chown = func(string, int, int) error { return errors.New("boom") }
	err := e.m.writeServiceDir("postgres", "15", "pw", testRemapBase)
	if os.Geteuid() == 0 {
		if err == nil {
			t.Fatal("chown failure as root was not returned")
		}
	} else if err != nil {
		t.Fatalf("unprivileged brain should log and go on: %v", err)
	}
	_, envErr := os.Stat(filepath.Join(e.m.serviceDir("postgres", "15"), ".env"))
	if os.Geteuid() == 0 && envErr == nil {
		t.Fatal(".env written after a failed chown")
	}
}

// With no remap a managed service's data dir is left as it was.
func TestManagedServiceDataUntouchedWithoutRemap(t *testing.T) {
	e := newTestEnv(t)
	chowns := recordChowns(e)
	installDBApp(t, e, "dbapp")
	data := filepath.Join(e.m.serviceDir("postgres", "15"), "data")
	if got := chowns.owner(data); got != "" {
		t.Fatalf("service data chowned to %q with no remap", got)
	}
}

// The tier is fixed for the life of an instance: an update whose manifest
// would move it is refused, and one that keeps it is not.
func TestCheckTierKept(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	folderless := &manifest.Manifest{ID: "app"}
	folders := &manifest.Manifest{ID: "app"}
	folders.Permissions.Folders = []manifest.Folder{{Folder: "documents", Mode: "read"}}
	rootSetup := &manifest.Manifest{ID: "app", RootSetup: true}

	for _, tc := range []struct {
		name   string
		stored string
		next   *manifest.Manifest
		refuse bool
	}{
		{"default stays default", store.UsernsTierDefault, folderless, false},
		{"default gains folders", store.UsernsTierDefault, folders, true},
		{"caps drops root_setup", store.UsernsTierCaps, folderless, true},
		{"caps stays caps", store.UsernsTierCaps, rootSetup, false},
		// An instance from before the remap is host tier: its data has real
		// host owners, so a folderless update may not move it into the remap.
		{"old host instance, folderless update", store.UsernsTierHost, folderless, true},
		{"host keeps folders", store.UsernsTierHost, folders, false},
	} {
		err := e.m.checkTierKept(context.Background(), store.Instance{ID: "i", UsernsTier: tc.stored}, tc.next)
		if tc.refuse != errors.Is(err, ErrTierChange) || (!tc.refuse && err != nil) {
			t.Errorf("%s: err = %v, want refused=%v", tc.name, err, tc.refuse)
		}
	}

	// With no remap every instance is host tier, so nothing moves.
	off := newTestEnv(t)
	if err := off.m.checkTierKept(context.Background(), store.Instance{ID: "i", UsernsTier: store.UsernsTierHost}, folders); err != nil {
		t.Errorf("no remap, folder update: %v", err)
	}
	if err := off.m.checkTierKept(context.Background(), store.Instance{ID: "i", UsernsTier: store.UsernsTierHost}, rootSetup); !errors.Is(err, ErrRootSetupNeedsRemap) {
		t.Errorf("no remap, root_setup update = %v, want ErrRootSetupNeedsRemap", err)
	}
}
