package lifecycle

// service_user scenarios: a folderless app that opts into a dedicated
// host-allocated runtime identity (APP_ISOLATION.md # Runtime identity & data
// ownership). Covers the pin (override + row), the release at uninstall and at
// install rollback, the admission rejection of service_user+folders, and the
// regression guard that default folderless installs make no identity calls.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/moose/internal/admission"
	"github.com/onmoose/moose/internal/protocol"
	"github.com/onmoose/moose/internal/store"
	"gopkg.in/yaml.v3"
)

// serviceUserManifest is the whoami manifest plus `service_user: true` (and,
// when withFolders is set, a folders grant — the combination admission
// rejects).
func serviceUserManifest(withFolders bool) string {
	folders := ""
	if withFolders {
		folders = `
  folders:
    - folder: documents
      mode: read`
	}
	return `
id: whoami
manifest_version: 1
name: Whoami
version: "1.10"
compose_file: compose.yml
main_service: whoami
main_port: 80
preferred_slugs: [whoami]
service_user: true
permissions:
  internet: false
  lan: false` + folders + `
images:
  ` + testImage + `: ` + testDigest + `
`
}

func TestInstallServiceUser_PinsAllocatedIdentity(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, serviceUserManifest(false))
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// The allocated identity (fake band start: 2100) is pinned in the override…
	ov, err := os.ReadFile(filepath.Join(e.stateDir, "instances", inst.ID, "compose.override.yml"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(ov, &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	if got := doc.Services["whoami"]["user"]; got != "2100:2100" {
		t.Errorf("override user: want 2100:2100, got %v", got)
	}

	// …and persisted on the row — what carries it across container recreations.
	row, err := e.store.Get(inst.ID)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.ServiceUID != protocol.AppServiceUIDMin || row.ServiceGID != protocol.AppServiceUIDMin {
		t.Errorf("row identity: want %d:%d, got %d:%d",
			protocol.AppServiceUIDMin, protocol.AppServiceUIDMin, row.ServiceUID, row.ServiceGID)
	}

	if !e.host.called("AllocateAppServiceIdentity") {
		t.Error("install must allocate an app-service identity")
	}
	// service_user is a folderless path: no folder-identity resolution.
	if e.host.called("WellKnownIdentity") || e.host.called("ResolveHome") {
		t.Error("service_user install must not resolve folder identities")
	}
}

func TestUninstallServiceUser_ReleasesIdentity(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, serviceUserManifest(false))
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(e.host.released) != 1 || e.host.released[0] != protocol.AppServiceUIDMin {
		t.Errorf("released = %v, want [%d]", e.host.released, protocol.AppServiceUIDMin)
	}
}

func TestInstallServiceUser_LateFailureReleasesIdentity(t *testing.T) {
	// compose up fails AFTER the identity was allocated and persisted: the
	// rollback must read the UID back from the row and return it to the band.
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, serviceUserManifest(false))
	e.docker.digests[testImage] = testDigest
	e.docker.composeUpErr = errors.New("compose up exploded")

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatal("want install error, got nil")
	}
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows after rollback, got %d", len(insts))
	}
	if len(e.host.released) != 1 || e.host.released[0] != protocol.AppServiceUIDMin {
		t.Errorf("released = %v, want [%d]", e.host.released, protocol.AppServiceUIDMin)
	}
}

func TestInstallServiceUser_AllocateFailureRollsBack(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, serviceUserManifest(false))
	e.docker.digests[testImage] = testDigest
	e.host.allocErr = errors.New("host-agent unreachable")

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatal("want install error, got nil")
	}
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows after rollback, got %d", len(insts))
	}
	// Nothing was allocated, so nothing may be released.
	if len(e.host.released) != 0 {
		t.Errorf("released = %v, want none", e.host.released)
	}
}

func TestInstallServiceUser_WithFoldersRejectedBeforeState(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, serviceUserManifest(true))
	e.docker.digests[testImage] = testDigest

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold,
		nil, "", nil, nil)
	if err == nil {
		t.Fatal("want admission rejection, got nil")
	}
	var admErr *admission.Error
	if !errors.As(err, &admErr) {
		t.Errorf("want *admission.Error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "service_user") {
		t.Errorf("rejection %q must name service_user", err)
	}
	// Admission writes no state (APP_LIFECYCLE.md # admission policy).
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows after rejection, got %d", len(insts))
	}
	if e.docker.called("Pull") || e.docker.called("ComposeUp") {
		t.Error("rejection must precede any docker work")
	}
}

func TestInstall_DefaultFolderlessAllocatesNoIdentity(t *testing.T) {
	// Regression guard: a folderless app WITHOUT service_user keeps the
	// existing behavior — brain-identity user:, no host identity calls, no
	// row identity.
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if e.host.called("AllocateAppServiceIdentity") {
		t.Error("default folderless install must not allocate an app-service identity")
	}
	row, _ := e.store.Get(inst.ID)
	if row.ServiceUID != 0 || row.ServiceGID != 0 {
		t.Errorf("row identity: want 0:0, got %d:%d", row.ServiceUID, row.ServiceGID)
	}
	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(e.host.released) != 0 {
		t.Errorf("uninstall released %v for a non-service_user instance", e.host.released)
	}
}
