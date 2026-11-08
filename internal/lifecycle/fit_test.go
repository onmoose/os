package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/onmoose/os/internal/fit"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

const gibBytes = int64(1) << 30

// withResources adds a resources block to a test manifest.
func withResources(man, block string) string {
	return man + "resources:\n  recommended: " + block + "\n"
}

func TestInstall_RefusesTooBigBeforeAnyWork(t *testing.T) {
	e := newTestEnv(t)
	e.host.resources = protocol.SystemResources{Mem: protocol.MemCounters{TotalBytes: 4 * gibBytes, UsedBytes: gibBytes}, CPU: protocol.CPUCounters{Cores: 2}}
	e.writeCatalogApp(t, "whoami", whoamiCompose, withResources(whoamiManifest(testDigest), "{ memory: 8G, cpu: 1 }"))
	e.docker.digests[testImage] = testDigest

	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	var tooBig *TooBigError
	if !errors.As(err, &tooBig) {
		t.Fatalf("install = %v, want a TooBigError", err)
	}
	if want := "This app can't run on this box as it is. It requires 8.0 GB of memory, and this box only has 4.0 GB."; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("want no instance rows after refusal, got %d", len(insts))
	}
	if got := e.docker.methods(); len(got) != 0 {
		t.Errorf("want no docker calls before the gate, got %v", got)
	}
}

// A tight app installs: the dialog has asked for "Install anyway". So does any
// app when the host cannot report its memory.
func TestInstall_TightAndUnknownInstall(t *testing.T) {
	for name, res := range map[string]protocol.SystemResources{
		"tight":   {Mem: protocol.MemCounters{TotalBytes: 4 * gibBytes, UsedBytes: 3 * gibBytes}},
		"unknown": {},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.host.resources = res
			e.writeCatalogApp(t, "whoami", whoamiCompose, withResources(whoamiManifest(testDigest), "{ memory: 2G }"))
			e.docker.digests[testImage] = testDigest
			if _, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
				Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil); err != nil {
				t.Fatalf("install: %v", err)
			}
		})
	}
}

// FitBox counts a running instance's recommended memory, and stops counting it
// once the instance is stopped.
func TestFitBox_CountsRunningInstancesOnly(t *testing.T) {
	e := newTestEnv(t)
	e.host.resources = protocol.SystemResources{Mem: protocol.MemCounters{TotalBytes: 4 * gibBytes, UsedBytes: gibBytes}, CPU: protocol.CPUCounters{Cores: 4}}
	e.writeCatalogApp(t, "whoami", whoamiCompose, withResources(whoamiManifest(testDigest), "{ memory: 3G }"))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	box, ok := e.m.FitBox(context.Background())
	if !ok || box.Cores != 4 || len(box.Running) != 1 || box.Running[0].RecommendedMemoryBytes != 3*gibBytes || box.Running[0].Name != "Whoami" {
		t.Fatalf("FitBox = %+v, %v; want one running Whoami recommending 3 GiB", box, ok)
	}
	if got := fit.Check(box, 2*gibBytes, 0).State; got != fit.Tight {
		t.Errorf("a 2G app next to it is %s, want tight", got)
	}

	if err := e.store.SetState(inst.ID, "stopped"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	box, _ = e.m.FitBox(context.Background())
	if len(box.Running) != 0 {
		t.Errorf("a stopped instance still counts: %+v", box.Running)
	}
	if got := fit.Check(box, 2*gibBytes, 0).State; got != fit.Fits {
		t.Errorf("a 2G app after the stop is %s, want fits", got)
	}
}

func TestFitBox_HostErrorIsUnknown(t *testing.T) {
	e := newTestEnv(t)
	e.host.resourcesErr = errors.New("host-agent unreachable")
	if _, ok := e.m.FitBox(context.Background()); ok {
		t.Error("FitBox reported a box when the host could not")
	}
}

func TestTooBigErrorCPUText(t *testing.T) {
	err := &TooBigError{Fit: fit.AppFit{TooBigCPU: true, RecommendedCPU: 4, BoxCores: 1}}
	if want := "This app can't run on this box as it is. It requires 4 cores, and this box only has 1 core."; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}
