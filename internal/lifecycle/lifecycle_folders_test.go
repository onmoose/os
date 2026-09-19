package lifecycle

// Folder-enforcement scenarios: writeOverride/writeEnv stamping user:, bind
// mounts from the elected source, group_add for shared sources, device
// passthrough, and MOOSE_FOLDER_* injection (APP_ISOLATION.md # User content).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
	"gopkg.in/yaml.v3"
)

// foldersManifest builds a single-folder Door-1 manifest. mode/scope let a test
// exercise :ro vs :rw and pick-subfolder.
func foldersManifest(mode, scope string) string {
	man := `
id: filesapp
manifest_version: 1
name: Files App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [filesapp]
permissions:
  internet: false
  lan: false
  folders:
    - folder: documents
      mode: ` + mode + `
      scope: ` + scope + `
`
	return man
}

const foldersCompose = `
services:
  app:
    image: traefik/whoami:v1.10.3
`

// installFolders writes the filesapp catalog entry, scripts the image digest,
// and installs it with the given scope + elected mounts. It returns the parsed
// override service entry for "app" and the raw .env contents.
func installFolders(t *testing.T, e *testEnv, scope string, owner Owner, manYAML string, mounts []FolderMount) (map[string]any, string) {
	t.Helper()
	e.writeCatalogApp(t, "filesapp", foldersCompose, manYAML)
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "filesapp"), owner, scope, mounts, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	dir := filepath.Join(e.stateDir, "instances", inst.ID)
	ov, err := os.ReadFile(filepath.Join(dir, "compose.override.yml"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(ov, &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	return doc.Services["app"], string(env)
}

func TestInstallFolders_HouseholdSharedWrite(t *testing.T) {
	e := newTestEnv(t)
	owner := Owner{UserID: "u_admin", Username: "admin"}
	app, env := installFolders(t, e, store.ScopeHousehold, owner,
		foldersManifest("write", "whole"),
		[]FolderMount{{Folder: "documents", Source: sourceShared}})

	// Household instances run as the moose-app service identity (fake: 2000).
	if got := app["user"]; got != "2000:2000" {
		t.Errorf("user: want 2000:2000, got %v", got)
	}
	wantVol := filepath.Join(e.m.sharedRoot, "Documents") + ":/moose/documents:rw" // write → :rw
	if !hasString(app["volumes"], wantVol) {
		t.Errorf("volumes: want %q, got %v", wantVol, app["volumes"])
	}
	// Shared source → group_add the moose-shared GID (fake: 2001).
	if !hasString(app["group_add"], "2001") {
		t.Errorf("group_add: want 2001, got %v", app["group_add"])
	}
	if !strings.Contains(env, "MOOSE_FOLDER_DOCUMENTS=/moose/documents") {
		t.Errorf("env missing MOOSE_FOLDER_DOCUMENTS, got:\n%s", env)
	}
}

func TestInstallFolders_PersonalSourceReadWithSubfolder(t *testing.T) {
	e := newTestEnv(t)
	owner := Owner{UserID: "u_alex", Username: "alex"}
	app, _ := installFolders(t, e, store.ScopePersonal, owner,
		foldersManifest("read", "pick-subfolder"),
		[]FolderMount{{Folder: "documents", Source: sourcePersonal, Subfolder: "Work"}})

	// Personal instance runs as the owner's UID/GID (fake ResolveHome: 3000).
	if got := app["user"]; got != "3000:3000" {
		t.Errorf("user: want 3000:3000, got %v", got)
	}
	src := filepath.Join(e.host.homeRoot, "alex", "Documents", "Work")
	wantVol := src + ":/moose/documents:ro" // read → :ro, subfolder narrows source
	if !hasString(app["volumes"], wantVol) {
		t.Errorf("volumes: want %q, got %v", wantVol, app["volumes"])
	}
	// No shared source → no group_add.
	if _, ok := app["group_add"]; ok {
		t.Errorf("group_add: want absent for personal source, got %v", app["group_add"])
	}
	// The brain creates the elected personal source (the pick-subfolder subdir)
	// before compose up, so docker can't create it root-owned and the owner-UID
	// container can write user content into it (#147 personal-folder follow-up).
	// Fails before the fix: the subdir was never created by the brain.
	if fi, err := os.Stat(src); err != nil {
		t.Errorf("personal folder source must be created: %v", err)
	} else if !fi.IsDir() {
		t.Errorf("personal folder source %q is not a directory", src)
	}
}

func TestInstallFolders_DeletedOwnerRollsBack(t *testing.T) {
	e := newTestEnv(t)
	e.host.resolveHomeErr = hostclient.ErrUnknownUser
	e.writeCatalogApp(t, "filesapp", foldersCompose, foldersManifest("read", "whole"))
	e.docker.digests[testImage] = testDigest

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "filesapp"),
		Owner{UserID: "u_ghost", Username: "ghost"}, store.ScopePersonal,
		[]FolderMount{{Folder: "documents", Source: sourcePersonal}}, "", nil, nil)
	if err == nil {
		t.Fatal("want install error for deleted owner, got nil")
	}
	// The brain row must be rolled back (brain-commits-first invariant).
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows after rollback, got %d", len(insts))
	}
}

func TestInstallFolders_FolderlessRunsAsBrainIdentity(t *testing.T) {
	// An app with no declared folders still runs as a resolved user: — the
	// brain's own euid/egid, which owns the ./data bind it created — so a
	// cap_drop:ALL container can write its private state. It binds no folders and
	// makes no host identity calls (the brain identity is local).
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	ov, _ := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, "compose.override.yml"))
	wantUser := fmt.Sprintf("user: %d:%d", os.Geteuid(), os.Getegid())
	if !strings.Contains(string(ov), wantUser) {
		t.Errorf("folderless override must run as brain identity %q, got:\n%s", wantUser, ov)
	}
	if strings.Contains(string(ov), "volumes:") || strings.Contains(string(ov), "group_add:") {
		t.Errorf("folderless override must bind no folders, got:\n%s", ov)
	}
	if e.host.called("WellKnownIdentity") || e.host.called("ResolveHome") {
		t.Error("folderless install must not resolve host identity")
	}
	dataDir := filepath.Join(e.stateDir, "instances", inst.ID, "data")
	if fi, err := os.Stat(dataDir); err != nil {
		t.Errorf("data dir must exist: %v", err)
	} else if st, ok := fi.Sys().(*syscall.Stat_t); !ok {
		t.Error("data dir stat: unexpected Stat_t type")
	} else if int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
		t.Errorf("data dir must be owned by brain identity %d:%d, got %d:%d",
			os.Geteuid(), os.Getegid(), st.Uid, st.Gid)
	}
}

// TestInstallCustomFolders_TargetDestination exercises the Door-2 divergence: a
// custom app's folder grant carries an explicit in-container target, so the bind
// lands there (not the fixed /moose/<folder>) and MOOSE_FOLDER_* reflects it. The
// source is scope-derived — no per-folder election — so a household install reads
// the shared tree (DASHBOARD.md # Folder grants carry an explicit destination).
func TestInstallCustomFolders_TargetDestination(t *testing.T) {
	e := newTestEnv(t)
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.InstallCustom(context.Background(), CustomSpec{
		Name: "Photo App", Compose: foldersCompose, MainPort: 8080,
		Permissions: manifest.Permissions{
			Internet: true,
			Folders: []manifest.Folder{
				{Folder: "documents", Mode: "write", Target: "/photoprism/originals"},
			},
		},
	}, Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	dir := filepath.Join(e.stateDir, "instances", inst.ID)
	ov, err := os.ReadFile(filepath.Join(dir, "compose.override.yml"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(ov, &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	app := doc.Services["app"]

	// Household → shared source; write → :rw; bind lands at the typed target.
	wantVol := filepath.Join(e.m.sharedRoot, "Documents") + ":/photoprism/originals:rw"
	if !hasString(app["volumes"], wantVol) {
		t.Errorf("volumes: want %q, got %v", wantVol, app["volumes"])
	}
	env, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(string(env), "MOOSE_FOLDER_DOCUMENTS=/photoprism/originals") {
		t.Errorf("env should map MOOSE_FOLDER_DOCUMENTS to the target, got:\n%s", env)
	}
}

// multiDirCompose binds more than the single top-level ./data: it declares
// several relative bind dirs across two services (Paperless-ngx shape, #142).
// Before the #147 fix the brain created + chowned only ./data, leaving the rest
// for the docker daemon to create root-owned — unwritable to a cap_drop:ALL
// container running as the non-root runtime UID.
const multiDirCompose = `
services:
  app:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data:/data
      - ./data/media:/media
      - ./data/export:/export
      - ./config:/config
  redis:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data/redis:/data
`

func multiDirManifest() string {
	return `
id: multidir
manifest_version: 1
name: Multi Dir
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [multidir]
permissions:
  internet: false
  lan: false
images:
  ` + testImage + `: ` + testDigest + `
`
}

// TestInstallPreparesAllRelativeBindDirs is the #147 regression guard: a
// folderless multi-dir app runs as the brain's own euid, so every declared
// relative bind dir must be created and owned by that runtime identity before
// compose up — not just the top-level data/. Fails before the fix (only data/
// was prepared), passes after.
func TestInstallPreparesAllRelativeBindDirs(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "multidir", multiDirCompose, multiDirManifest())
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "multidir"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	base := filepath.Join(e.stateDir, "instances", inst.ID)
	for _, rel := range []string{"data", "data/media", "data/export", "config", "data/redis"} {
		fi, err := os.Stat(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("bind dir %q must exist: %v", rel, err)
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("stat %q: unexpected Stat_t type", rel)
		}
		if int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
			t.Errorf("bind dir %q owner = %d:%d, want runtime %d:%d",
				rel, st.Uid, st.Gid, os.Geteuid(), os.Getegid())
		}
	}
}

// TestRelativeBindDirs covers the filter directly: only "./"-relative bind
// sources are prepared, deduplicated and sorted. Absolute sources (the use-case
// folder binds the override injects — never created/chowned by the prepare
// step), named volumes, and anonymous volumes are all excluded by construction.
func TestRelativeBindDirs(t *testing.T) {
	compose := `
services:
  app:
    volumes:
      - ./data:/data
      - ./data/media:/media:rw
      - ./data/media:/media2
      - { type: bind, source: ./config, target: /config }
      - /etc/localtime:/etc/localtime:ro
      - { type: bind, source: /var/run/docker.sock, target: /sock }
      - cache-vol:/cache
      - { type: volume, source: data-vol, target: /v }
      - /anon
  sidecar:
    volumes:
      - ./logs:/logs
`
	got, err := relativeBindDirs([]byte(compose))
	if err != nil {
		t.Fatalf("relativeBindDirs: %v", err)
	}
	want := []string{"config", "data", "data/media", "logs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("relativeBindDirs = %v, want %v", got, want)
	}
}

// --- helpers ---

// hasString reports whether v is a []string (or []any of strings) containing s.
func hasString(v any, s string) bool {
	switch xs := v.(type) {
	case []string:
		for _, x := range xs {
			if x == s {
				return true
			}
		}
	case []any:
		for _, x := range xs {
			if str, ok := x.(string); ok && str == s {
				return true
			}
		}
	}
	return false
}
