package lifecycle

// GPU runtime wiring (#67): a `gpu: true` manifest gets, on the main service
// only, a /dev/dri device bind + a group_add of the host's render GID from
// the GET /v1/system/gpu capability query; a no-GPU host is a hard install
// refusal at capacity-check time, before the instance row and any Docker
// work (APP_ISOLATION.md # GPU).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/onmoose/moose/internal/protocol"
	"github.com/onmoose/moose/internal/store"

	"gopkg.in/yaml.v3"
)

// gpuManifest is migrateJobManifest with gpu: true — the multi-service shape
// (migrate/seed sidecars + web main_service) makes main-service-only stanza
// placement assertable.
const gpuManifest = `
id: gpuapp
manifest_version: 1
name: GPU App
version: "1.0"
compose_file: compose.yml
main_service: web
main_port: 3000
preferred_slugs: [gpuapp]
permissions:
  internet: false
  lan: false
  gpu: true
`

func TestInstallGPU_OverrideBindsDRIAndRenderGroup(t *testing.T) {
	e := newTestEnv(t)
	e.host.gpu = protocol.SystemGPU{Present: true, Vendor: "intel", RenderGID: 104}
	e.writeCatalogApp(t, "gpuapp", migrateJobCompose, gpuManifest)
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "gpuapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readInstanceFile(t, e, inst.ID, "compose.override.yml")), &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	web := doc.Services["web"]
	if !hasString(web["devices"], "/dev/dri:/dev/dri") {
		t.Errorf("main service devices: want /dev/dri bind, got %v", web["devices"])
	}
	if !hasString(web["group_add"], "104") {
		t.Errorf("main service group_add: want render GID 104, got %v", web["group_add"])
	}
	// Main service only (APP_ISOLATION.md # GPU) — sidecars get no stanza.
	for _, svc := range []string{"migrate", "seed"} {
		if v, ok := doc.Services[svc]["devices"]; ok {
			t.Errorf("%s devices: want absent, got %v", svc, v)
		}
		if v, ok := doc.Services[svc]["group_add"]; ok {
			t.Errorf("%s group_add: want absent, got %v", svc, v)
		}
	}
}

func TestInstallGPU_RefusedWhenHostHasNoGPU(t *testing.T) {
	e := newTestEnv(t)
	// newFakeHost's zero-value gpu reports no usable GPU.
	e.writeCatalogApp(t, "gpuapp", migrateJobCompose, gpuManifest)
	e.docker.digests[testImage] = testDigest

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "gpuapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if !errors.Is(err, ErrNoGPU) {
		t.Fatalf("install = %v, want ErrNoGPU", err)
	}
	// The gate fires at capacity-check time: before the instance row exists
	// and before any Docker work, not a late compose-up failure.
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows after refusal, got %d", len(insts))
	}
	if got := e.docker.methods(); len(got) != 0 {
		t.Errorf("want no docker calls before the gate, got %v", got)
	}
}

func TestInstallGPU_HostErrorFailsInstall(t *testing.T) {
	e := newTestEnv(t)
	e.host.gpuErr = errors.New("host-agent unreachable")
	e.writeCatalogApp(t, "gpuapp", migrateJobCompose, gpuManifest)
	e.docker.digests[testImage] = testDigest

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "gpuapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatal("want install failure on GPU capability host error, got nil")
	}
	if errors.Is(err, ErrNoGPU) {
		t.Fatalf("host error must not masquerade as the no-GPU refusal: %v", err)
	}
}

// A present:true report with no render group is a malformed host answer: the
// gate must fail the install rather than group_add GID 0 (the root group) onto
// the cap_drop:ALL container. It is a host fault, not the no-GPU refusal.
func TestInstallGPU_PresentButNoRenderGroup_FailsInstall(t *testing.T) {
	e := newTestEnv(t)
	e.host.gpu = protocol.SystemGPU{Present: true, Vendor: "intel", RenderGID: 0}
	e.writeCatalogApp(t, "gpuapp", migrateJobCompose, gpuManifest)
	e.docker.digests[testImage] = testDigest

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "gpuapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatal("want install failure on present GPU with no render group, got nil")
	}
	if errors.Is(err, ErrNoGPU) {
		t.Fatalf("malformed present-GPU report must not surface as the no-GPU refusal: %v", err)
	}
	// Fails at the gate, before the instance row and any Docker work.
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows after host-fault refusal, got %d", len(insts))
	}
	if got := e.docker.methods(); len(got) != 0 {
		t.Errorf("want no docker calls before the gate, got %v", got)
	}
}

func TestInstallNoGPUPermission_NoStanzaNoQuery(t *testing.T) {
	e := newTestEnv(t)
	// Even on a GPU-present host, an app that doesn't declare gpu: true gets
	// no /dev/dri and no render-group membership — and the brain never asks.
	e.host.gpu = protocol.SystemGPU{Present: true, Vendor: "intel", RenderGID: 104}
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	ov := readInstanceFile(t, e, inst.ID, "compose.override.yml")
	if strings.Contains(ov, "/dev/dri") || strings.Contains(ov, "group_add") {
		t.Errorf("no-gpu app must get no GPU stanza, got:\n%s", ov)
	}
	if e.host.called("SystemGPU") {
		t.Error("install without gpu: true must not query host GPU capability")
	}
}
