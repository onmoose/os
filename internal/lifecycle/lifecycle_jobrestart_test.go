package lifecycle

// One-shot-job restart handling (#92): the override force-stamps
// `restart: unless-stopped` on every long-running service, but must NOT do so
// for services the author designed to terminate — a restarted one-shot job
// never reaches the "completed" state a `service_completed_successfully` gate
// waits on, hanging `compose up -d` forever. Layer 1 is the detection; layer 2
// is the bounded `compose up` that contains any case layer 1 misses.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"

	"gopkg.in/yaml.v3"
)

// --- pure parsing / detection -------------------------------------------

func TestParseComposeServices(t *testing.T) {
	got, err := parseComposeServices([]byte(`
services:
  migrate:
    image: x
    restart: "no"
  web:
    image: y
    restart: unless-stopped
    depends_on:
      migrate:
        condition: service_completed_successfully
  worker:
    image: z
    depends_on: [web]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]composeService{
		"migrate": {Restart: "no"},
		"web":     {Restart: "unless-stopped", DependsOn: map[string]string{"migrate": "service_completed_successfully"}},
		// short-form depends_on carries no conditions.
		"worker": {Restart: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseComposeServices =\n%#v\nwant\n%#v", got, want)
	}
}

func TestCompletionGateTargets(t *testing.T) {
	svcs := map[string]composeService{
		"migrate": {Restart: "no"},
		"seed":    {},
		"web": {DependsOn: map[string]string{
			"migrate": "service_completed_successfully",
			"seed":    "service_started", // not a completion gate
		}},
	}
	got := completionGateTargets(svcs)
	if !reflect.DeepEqual(got, map[string]bool{"migrate": true}) {
		t.Fatalf("completionGateTargets = %v, want {migrate:true}", got)
	}
}

func TestIsTerminatingJob(t *testing.T) {
	cases := []struct {
		name       string
		svc        composeService
		gateTarget bool
		want       bool
	}{
		{"restart no", composeService{Restart: "no"}, false, true},
		{"restart on-failure", composeService{Restart: "on-failure"}, false, true},
		{"restart on-failure with count", composeService{Restart: "on-failure:5"}, false, true},
		{"gate target only", composeService{}, true, true},
		{"long-running, not a target", composeService{Restart: "unless-stopped"}, false, false},
		{"no restart, not a target", composeService{}, false, false},
		{"always restart", composeService{Restart: "always"}, false, false},
	}
	for _, tc := range cases {
		if got := isTerminatingJob(tc.svc, tc.gateTarget); got != tc.want {
			t.Errorf("%s: isTerminatingJob = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- end-to-end override generation -------------------------------------

// migrateJobManifest/Compose model the kan shape: a one-shot `migrate` job
// (restart: "no") that `web` (main_service) gates on via
// service_completed_successfully. Both services share the test image so the
// fake's digest map needs only one entry.
const migrateJobManifest = `
id: jobapp
manifest_version: 1
name: Job App
version: "1.0"
compose_file: compose.yml
main_service: web
main_port: 3000
preferred_slugs: [jobapp]
permissions:
  internet: false
  lan: false
`

const migrateJobCompose = `
services:
  migrate:
    image: traefik/whoami:v1.10.3
    restart: "no"
  seed:
    image: traefik/whoami:v1.10.3
    depends_on:
      migrate:
        condition: service_completed_successfully
  web:
    image: traefik/whoami:v1.10.3
    depends_on:
      seed:
        condition: service_completed_successfully
`

// overrideRestart returns the `restart` value the override stamped on a service,
// and whether the key was present at all (absent = author's value preserved).
func overrideRestart(t *testing.T, e *testEnv, instID, service string) (string, bool) {
	t.Helper()
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readInstanceFile(t, e, instID, "compose.override.yml")), &doc); err != nil {
		t.Fatalf("parse override: %v", err)
	}
	svc, ok := doc.Services[service]
	if !ok {
		t.Fatalf("override has no service %q", service)
	}
	v, present := svc["restart"]
	if !present {
		return "", false
	}
	return v.(string), true
}

func TestOverrideExemptsTerminatingJobs(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "jobapp", migrateJobCompose, migrateJobManifest)
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "jobapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// migrate: terminating via signal (a) restart: "no" → no forced restart.
	if v, present := overrideRestart(t, e, inst.ID, "migrate"); present {
		t.Fatalf("migrate restart = %q, want omitted (author value preserved)", v)
	}
	// seed: terminating via signal (b) — it's a completion-gate target but
	// declares no restart of its own → no forced restart.
	if v, present := overrideRestart(t, e, inst.ID, "seed"); present {
		t.Fatalf("seed restart = %q, want omitted (completion-gate target)", v)
	}
	// web: main_service, a long-running gate source → forced unless-stopped.
	if v, present := overrideRestart(t, e, inst.ID, "web"); !present || v != "unless-stopped" {
		t.Fatalf("web restart = %q present=%v, want unless-stopped", v, present)
	}
}

func TestOverrideMainServiceAlwaysForced(t *testing.T) {
	e := newTestEnv(t)
	// A paranoid/buggy author sets restart: "no" on the main service itself.
	// main_service must still be forced long-running.
	compose := `
services:
  web:
    image: traefik/whoami:v1.10.3
    restart: "no"
`
	e.writeCatalogApp(t, "jobapp", compose, migrateJobManifest)
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "jobapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if v, present := overrideRestart(t, e, inst.ID, "web"); !present || v != "unless-stopped" {
		t.Fatalf("main_service restart = %q present=%v, want forced unless-stopped", v, present)
	}
}

func TestOverrideForcesOrdinaryService(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest

	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if v, present := overrideRestart(t, e, inst.ID, "whoami"); !present || v != "unless-stopped" {
		t.Fatalf("ordinary service restart = %q present=%v, want unless-stopped", v, present)
	}
}

// --- layer 2: bounded compose up ----------------------------------------

// TestComposeUpBoundedByHealthWait proves that a `compose up -d` that never
// returns (a pathological completion gate) fails the install within the
// health-wait budget instead of wedging the brain. The fake blocks until its
// context is cancelled; the install's bounded context (m.healthWait, 200ms in
// tests) must cancel it.
func TestComposeUpBoundedByHealthWait(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.digests[testImage] = testDigest
	e.docker.composeUp = func(ctx context.Context, dir, project string) (string, error) {
		<-ctx.Done() // hang until the bounded context fires
		return "", ctx.Err()
	}

	start := time.Now()
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err == nil {
		t.Fatalf("want install failure from a hung compose up")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("install took %s — compose up was not bounded", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a context deadline failure", err)
	}

	// Clean rollback: no row, no instance dir survives the wedge.
	list, _ := e.store.List()
	if len(list) != 0 {
		t.Fatalf("instance row must be rolled back, got %v", list)
	}
}
