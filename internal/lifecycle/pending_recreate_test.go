package lifecycle

// Pending-recreate recovery (#268): an env-restamping edit (config or mail)
// commits the override/.env before its `compose up`, so a failed up strands a
// still-running container on stale env. The edit marks the instance
// pending-recreate; the reconcile pass retries the recreate while the marker is
// set and clears it once a recreate succeeds.

import (
	"context"
	"errors"
	"testing"

	"github.com/onmoose/os/internal/store"
)

// pendingRecreate reads an instance's owed-recreate marker from the store.
func pendingRecreate(t *testing.T, e *testEnv, id string) bool {
	t.Helper()
	row, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	return row.PendingRecreate
}

// editConfig is the minimal running-instance config edit used to drive a
// recreate; the value differs from installConfigApp's so the override changes.
func editConfig(t *testing.T, e *testEnv, id string) {
	t.Helper()
	cfg := []store.InstanceConfig{{AppEnv: "OPENAI_MODEL", Value: "gpt-5"}}
	err := e.m.SetConfig(context.Background(), id, cfg)
	if err == nil {
		t.Fatalf("seed: SetConfig with a failing compose up must return an error")
	}
}

func TestSetConfigFailedRecreateMarksAndClearsPending(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e) // running

	// The follow-up compose up fails: the override is committed but the running
	// container keeps its old env, so the instance is marked pending-recreate.
	e.docker.composeUpErr = errors.New("compose up exploded")
	editConfig(t, e, inst.ID)
	if !pendingRecreate(t, e, inst.ID) {
		t.Fatalf("a failed config recreate must mark the instance pending-recreate")
	}

	// A successful retry applies the env and clears the marker.
	e.docker.composeUpErr = nil
	if err := e.m.SetConfig(context.Background(), inst.ID,
		[]store.InstanceConfig{{AppEnv: "OPENAI_MODEL", Value: "gpt-5"}}); err != nil {
		t.Fatalf("retry SetConfig: %v", err)
	}
	if pendingRecreate(t, e, inst.ID) {
		t.Fatalf("a successful recreate must clear the pending-recreate marker")
	}
}

func TestRebindMailFailedRecreateMarksPending(t *testing.T) {
	e := newTestEnv(t)
	if err := e.createMailProvider(testProvider()); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	second := testProvider()
	second.ID, second.Label, second.Host = "mp_two", "SES", "email-smtp.example.com"
	if err := e.createMailProvider(second); err != nil {
		t.Fatalf("create provider 2: %v", err)
	}
	inst, _ := installMailApp(t, e, "mp_test") // running

	// A rebind whose compose up fails strands the running container on the old
	// binding and marks it pending-recreate.
	e.docker.composeUpErr = errors.New("compose up exploded")
	if err := e.m.RebindMail(context.Background(), inst.ID, "mp_two"); err == nil {
		t.Fatalf("RebindMail with a failing compose up must return an error")
	}
	if !pendingRecreate(t, e, inst.ID) {
		t.Fatalf("a failed mail rebind must mark the instance pending-recreate")
	}
}

func TestReconcileRecreatesPendingRunningInstance(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)

	// Strand the still-running container on stale env via a failed edit.
	e.docker.composeUpErr = errors.New("compose up exploded")
	editConfig(t, e, inst.ID)
	if !pendingRecreate(t, e, inst.ID) {
		t.Fatalf("seed: instance must be pending-recreate")
	}

	// Reconcile with the container still up and compose up healthy: the already-up
	// path retries the recreate and clears the marker.
	e.docker.composeUpErr = nil
	e.docker.psManaged = map[string]bool{inst.ID: true}
	e.docker.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("a pending running instance must be recreated by reconcile: %v", e.docker.methods())
	}
	if pendingRecreate(t, e, inst.ID) {
		t.Fatalf("reconcile must clear the marker after a successful recreate")
	}

	// No churn: the marker is clear and nothing drifted, so a second pass does
	// not recreate the (now converged) instance again.
	e.docker.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (2nd pass): %v", err)
	}
	if methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("a converged instance must not be recreated again: %v", e.docker.methods())
	}
}

func TestReconcileFailedPendingRecreateStaysMarked(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)

	e.docker.composeUpErr = errors.New("compose up exploded")
	editConfig(t, e, inst.ID)

	// The container is still up but compose up keeps failing: reconcile attempts
	// the recreate and, on failure, leaves the marker set so a later pass retries.
	e.docker.psManaged = map[string]bool{inst.ID: true}
	e.docker.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("a pending instance must attempt a recreate: %v", e.docker.methods())
	}
	if !pendingRecreate(t, e, inst.ID) {
		t.Fatalf("a failed reconcile recreate must leave the marker set")
	}
}

func TestReconcileClearsPendingWhenBringingUpDriftedInstance(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)

	e.docker.composeUpErr = errors.New("compose up exploded")
	editConfig(t, e, inst.ID)
	if !pendingRecreate(t, e, inst.ID) {
		t.Fatalf("seed: instance must be pending-recreate")
	}

	// The container is also down (drifted): the no-containers path brings it up
	// against the committed override, which satisfies the owed recreate.
	e.docker.composeUpErr = nil
	e.docker.psManaged = map[string]bool{}
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pendingRecreate(t, e, inst.ID) {
		t.Fatalf("bringing up a drifted pending instance must clear the marker")
	}
}

func TestStartClearsPendingRecreate(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e) // running

	// Strand the running container on stale env via a failed edit, then let it
	// stop normally — the marker is independent of state and survives a Stop.
	e.docker.composeUpErr = errors.New("compose up exploded")
	editConfig(t, e, inst.ID)
	if !pendingRecreate(t, e, inst.ID) {
		t.Fatalf("seed: instance must be pending-recreate")
	}
	e.docker.composeUpErr = nil
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Start applies the committed override/.env via its own ComposeUp, which
	// satisfies the owed recreate just as well as reconcile's would — it must
	// clear the marker itself instead of leaving it for the next reconcile pass
	// to redundantly retry against an already-converged container.
	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if pendingRecreate(t, e, inst.ID) {
		t.Fatalf("a successful Start must clear the pending-recreate marker")
	}

	// No churn: reconcile must not recreate an instance that Start already
	// converged.
	e.docker.psManaged = map[string]bool{inst.ID: true}
	e.docker.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if methodsContainArg(e.docker.Calls(), "ComposeUp", "moose-"+inst.ID) {
		t.Fatalf("reconcile must not recreate an instance Start already converged: %v", e.docker.methods())
	}
}

// A clean install never sets the marker, and an unrelated reconcile pass does
// not invent one — the recovery path stays inert until an edit actually fails.
func TestPendingNotSetOnHealthyPath(t *testing.T) {
	e := newTestEnv(t)
	inst := installConfigApp(t, e)
	if pendingRecreate(t, e, inst.ID) {
		t.Fatalf("a clean install must not be pending-recreate")
	}
	e.docker.psManaged = map[string]bool{inst.ID: true}
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pendingRecreate(t, e, inst.ID) {
		t.Fatalf("reconcile must not set the marker on a healthy instance")
	}
}
