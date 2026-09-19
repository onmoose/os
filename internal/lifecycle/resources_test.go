package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
	"gopkg.in/yaml.v3"
)

// ovDeploy decodes the main service's `deploy.resources.limits` stanza from an
// instance's compose.override.yml. Deploy is a pointer so a test can tell an
// absent stanza (uncapped) from a present one.
type ovDeploy struct {
	Services map[string]struct {
		Deploy *struct {
			Resources struct {
				Limits struct {
					Memory int64  `yaml:"memory"`
					CPUs   string `yaml:"cpus"`
				} `yaml:"limits"`
			} `yaml:"resources"`
		} `yaml:"deploy"`
	} `yaml:"services"`
}

// readDeploy returns the rendered deploy stanza for an instance's service, or
// nil when the override omits one.
func readDeploy(t *testing.T, e *testEnv, id, service string) *struct {
	Resources struct {
		Limits struct {
			Memory int64  `yaml:"memory"`
			CPUs   string `yaml:"cpus"`
		} `yaml:"limits"`
	} `yaml:"resources"`
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "instances", id, "compose.override.yml"))
	if err != nil {
		t.Fatalf("read override: %v", err)
	}
	var doc ovDeploy
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	return doc.Services[service].Deploy
}

// installedWhoami installs the minimal whoami app and returns the env + row,
// the common starting point for the policy-application tests.
func installedWhoami(t *testing.T) (*testEnv, store.Instance) {
	t.Helper()
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(""))
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return e, inst
}

// --- resourceLimitsStanza (pure rendering) ------------------------------

func TestResourceLimitsStanza(t *testing.T) {
	limitsOf := func(stanza map[string]any) map[string]any {
		if stanza == nil {
			return nil
		}
		res, _ := stanza["resources"].(map[string]any)
		lim, _ := res["limits"].(map[string]any)
		return lim
	}
	const oneCPU = 1_000_000_000

	t.Run("appliance renders memory only", func(t *testing.T) {
		lim := limitsOf(resourceLimitsStanza(profile.Appliance, store.ResourceLimits{MemoryBytes: 100}))
		if lim["memory"] != int64(100) {
			t.Fatalf("memory = %v, want 100", lim["memory"])
		}
		if _, ok := lim["cpus"]; ok {
			t.Fatalf("appliance must not render cpus, got %v", lim["cpus"])
		}
	})

	t.Run("appliance drops a stray cpu value", func(t *testing.T) {
		// CPU is never capped on the appliance (APP_ISOLATION.md): a set NanoCPUs
		// is dropped, not rendered.
		lim := limitsOf(resourceLimitsStanza(profile.Appliance, store.ResourceLimits{MemoryBytes: 100, NanoCPUs: oneCPU}))
		if _, ok := lim["cpus"]; ok {
			t.Fatalf("appliance must drop cpus, got %v", lim["cpus"])
		}
	})

	t.Run("appliance cpu-only is uncapped", func(t *testing.T) {
		// Only a CPU value set, which the appliance drops → nothing to render.
		if s := resourceLimitsStanza(profile.Appliance, store.ResourceLimits{NanoCPUs: oneCPU}); s != nil {
			t.Fatalf("appliance cpu-only must be nil, got %v", s)
		}
	})

	t.Run("hosted renders memory and cpus", func(t *testing.T) {
		lim := limitsOf(resourceLimitsStanza(profile.Hosted, store.ResourceLimits{MemoryBytes: 100, NanoCPUs: 1_500_000_000}))
		if lim["memory"] != int64(100) {
			t.Fatalf("memory = %v, want 100", lim["memory"])
		}
		if lim["cpus"] != "1.5" {
			t.Fatalf("cpus = %v, want 1.5", lim["cpus"])
		}
	})

	t.Run("hosted cpu-only renders cpus", func(t *testing.T) {
		lim := limitsOf(resourceLimitsStanza(profile.Hosted, store.ResourceLimits{NanoCPUs: oneCPU}))
		if _, ok := lim["memory"]; ok {
			t.Fatalf("memory must be absent, got %v", lim["memory"])
		}
		if lim["cpus"] != "1" {
			t.Fatalf("cpus = %v, want 1", lim["cpus"])
		}
	})

	t.Run("zero policy is nil in both profiles", func(t *testing.T) {
		for _, p := range []profile.Profile{profile.Appliance, profile.Hosted} {
			if s := resourceLimitsStanza(p, store.ResourceLimits{}); s != nil {
				t.Fatalf("profile %s: zero policy must be nil, got %v", p, s)
			}
		}
	})
}

func TestFormatCPUs(t *testing.T) {
	for _, tc := range []struct {
		nano int64
		want string
	}{
		{1_000_000_000, "1"},
		{1_500_000_000, "1.5"},
		{500_000_000, "0.5"},
		{2_000_000_000, "2"},
		{250_000_000, "0.25"},
	} {
		if got := formatCPUs(tc.nano); got != tc.want {
			t.Errorf("formatCPUs(%d) = %q, want %q", tc.nano, got, tc.want)
		}
	}
}

// --- Manager.SetResourceLimits (the policy seam) ------------------------

func TestManagerSetResourceLimits(t *testing.T) {
	e, inst := installedWhoami(t)

	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{MemoryBytes: -1}); err == nil {
		t.Fatal("negative memory must be rejected")
	}
	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{NanoCPUs: -1}); err == nil {
		t.Fatal("negative cpus must be rejected")
	}
	if err := e.m.SetResourceLimits("ghost", store.ResourceLimits{MemoryBytes: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown instance: err = %v, want ErrNotFound", err)
	}

	want := store.ResourceLimits{MemoryBytes: 64 << 20, NanoCPUs: 2_000_000_000}
	if err := e.m.SetResourceLimits(inst.ID, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := e.store.GetResourceLimits(inst.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != want {
		t.Fatalf("persisted = %+v, want %+v", got, want)
	}
}

// --- writeOverride renders the stanza onto the main service --------------

func TestWriteOverrideRendersResourceLimits(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "")
	id := "inst-wo"
	if err := os.MkdirAll(e.m.instanceDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	man := &manifest.Manifest{ID: "whoami", Name: "Whoami", MainService: "whoami", MainPort: 80}
	iso := isolation{uid: 1000, gid: 1000}
	lim := store.ResourceLimits{MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000}
	if err := e.m.writeOverride(id, man, []byte(whoamiCompose), nil, iso, lim); err != nil {
		t.Fatalf("writeOverride: %v", err)
	}
	dep := readDeploy(t, e, id, "whoami")
	if dep == nil {
		t.Fatal("main service deploy stanza missing")
	}
	if dep.Resources.Limits.Memory != 512<<20 {
		t.Fatalf("memory = %d, want %d", dep.Resources.Limits.Memory, 512<<20)
	}
	if dep.Resources.Limits.CPUs != "1.5" {
		t.Fatalf("cpus = %q, want 1.5", dep.Resources.Limits.CPUs)
	}
}

// --- reconcile applies a changed policy without a reinstall --------------

func TestReconcileAppliesChangedResourceLimits(t *testing.T) {
	e, inst := installedWhoami(t)
	// Fresh install carries no policy → no deploy stanza in the override.
	if dep := readDeploy(t, e, inst.ID, "whoami"); dep != nil {
		t.Fatalf("fresh install must not render a deploy stanza, got %+v", dep)
	}

	// Set a memory cap, then reconcile with containers already up.
	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{MemoryBytes: 256 << 20}); err != nil {
		t.Fatalf("set limits: %v", err)
	}
	e.docker.psManaged = map[string]bool{inst.ID: true}
	e.docker.calls = nil

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The override now carries the cap, and the app was recreated to apply it.
	dep := readDeploy(t, e, inst.ID, "whoami")
	if dep == nil || dep.Resources.Limits.Memory != 256<<20 {
		t.Fatalf("override deploy = %+v, want memory %d", dep, 256<<20)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("changed policy must recreate the app: %v", e.docker.methods())
	}
}

func TestReconcileUnchangedResourceLimitsDoesNotRecreate(t *testing.T) {
	e, inst := installedWhoami(t)
	// No policy set: the override has no deploy stanza, and reconcile of an
	// up instance must be a no-op for resource limits (no recreate).
	e.docker.psManaged = map[string]bool{inst.ID: true}
	e.docker.calls = nil

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("unchanged policy must not recreate the app: %v", e.docker.methods())
	}
}

func TestReconcileDriftedInstanceAppliesPolicyBeforeUp(t *testing.T) {
	e, inst := installedWhoami(t)
	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{MemoryBytes: 128 << 20}); err != nil {
		t.Fatalf("set limits: %v", err)
	}
	// Drifted: SQLite says running, Docker has no containers.
	e.docker.psManaged = map[string]bool{}
	e.docker.calls = nil

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dep := readDeploy(t, e, inst.ID, "whoami")
	if dep == nil || dep.Resources.Limits.Memory != 128<<20 {
		t.Fatalf("drifted instance came up without its cap: deploy = %+v", dep)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("drifted instance must be brought up: %v", e.docker.methods())
	}
}

// Clearing a policy (set back to zero) must drop the deploy stanza and recreate
// the app, so an un-capped instance bursts freely again.
func TestReconcileClearingResourceLimitsRemovesStanza(t *testing.T) {
	e, inst := installedWhoami(t)
	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{MemoryBytes: 256 << 20}); err != nil {
		t.Fatalf("set: %v", err)
	}
	e.docker.psManaged = map[string]bool{inst.ID: true}
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (apply): %v", err)
	}
	if dep := readDeploy(t, e, inst.ID, "whoami"); dep == nil {
		t.Fatal("stanza missing after setting a cap")
	}

	// Clear the cap, reconcile again.
	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	e.docker.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (clear): %v", err)
	}
	if dep := readDeploy(t, e, inst.ID, "whoami"); dep != nil {
		t.Fatalf("stanza must be removed after clearing, got %+v", dep)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("clearing a cap must recreate the app: %v", e.docker.methods())
	}
}

// A failed ComposeUp after the override is patched must not strand the instance
// on its old limits: the override is rewound so the next reconcile re-detects
// the change and retries, instead of seeing a patched-but-unapplied file as
// already converged.
func TestReconcileFailedRecreateRewindsAndRetries(t *testing.T) {
	e, inst := installedWhoami(t)
	if err := e.m.SetResourceLimits(inst.ID, store.ResourceLimits{MemoryBytes: 256 << 20}); err != nil {
		t.Fatalf("set limits: %v", err)
	}
	e.docker.psManaged = map[string]bool{inst.ID: true}

	// First pass: ComposeUp fails. The override must be rewound to its pre-patch
	// (uncapped) state so the policy is not falsely marked applied.
	e.docker.composeUpErr = errors.New("compose up exploded")
	e.docker.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (failing up): %v", err)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("a changed policy must attempt a recreate: %v", e.docker.methods())
	}
	if dep := readDeploy(t, e, inst.ID, "whoami"); dep != nil {
		t.Fatalf("override must be rewound after a failed up, got %+v", dep)
	}

	// Second pass: ComposeUp succeeds. The change is re-detected and applied,
	// proving the failure left a retryable state rather than a stranded one.
	e.docker.composeUpErr = nil
	e.docker.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (retry): %v", err)
	}
	dep := readDeploy(t, e, inst.ID, "whoami")
	if dep == nil || dep.Resources.Limits.Memory != 256<<20 {
		t.Fatalf("retry must apply the cap, override deploy = %+v", dep)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("retry must recreate the app: %v", e.docker.methods())
	}
}

func TestReapplyResourceLimitsErrors(t *testing.T) {
	overridePath := func(e *testEnv, id string) string {
		return filepath.Join(e.stateDir, "instances", id, "compose.override.yml")
	}

	t.Run("missing override", func(t *testing.T) {
		e, inst := installedWhoami(t)
		if err := os.Remove(overridePath(e, inst.ID)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.m.reapplyResourceLimits(inst.ID); err == nil {
			t.Fatal("want read-override error")
		}
	})

	t.Run("unparseable override", func(t *testing.T) {
		e, inst := installedWhoami(t)
		if err := os.WriteFile(overridePath(e, inst.ID), []byte("\tnot: [valid"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.m.reapplyResourceLimits(inst.ID); err == nil {
			t.Fatal("want parse-override error")
		}
	})

	t.Run("override missing main service", func(t *testing.T) {
		e, inst := installedWhoami(t)
		if err := os.WriteFile(overridePath(e, inst.ID), []byte("services:\n  other: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.m.reapplyResourceLimits(inst.ID); err == nil {
			t.Fatal("want missing-main-service error")
		}
	})

	t.Run("manifest without main_service", func(t *testing.T) {
		e, inst := installedWhoami(t)
		manPath := filepath.Join(e.stateDir, "instances", inst.ID, "manifest.yml")
		if err := os.WriteFile(manPath, []byte("id: whoami\nname: Whoami\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.m.reapplyResourceLimits(inst.ID); err == nil {
			t.Fatal("want main-service resolution error")
		}
	})
}

func TestMainServiceErrors(t *testing.T) {
	e := newTestEnv(t)

	t.Run("missing manifest", func(t *testing.T) {
		id := "no-manifest"
		if err := os.MkdirAll(e.m.instanceDir(id), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := e.m.mainService(id); err == nil {
			t.Fatal("want read error")
		}
	})

	t.Run("unparseable manifest", func(t *testing.T) {
		id := "bad-manifest"
		if err := os.MkdirAll(e.m.instanceDir(id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.m.instanceDir(id), "manifest.yml"), []byte("\tnope: ["), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := e.m.mainService(id); err == nil {
			t.Fatal("want parse error")
		}
	})
}
