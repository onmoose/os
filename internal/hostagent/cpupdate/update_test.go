package cpupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/hostagent/brainlaunch"
	"github.com/onmoose/os/internal/hostagent/controlplane"
	"github.com/onmoose/os/internal/protocol"
)

const (
	oldBrain = "moose-brain:latest"
	oldUI    = "moose-ui:dev"
	newBrain = "ghcr.io/onmoose/os-brain@sha256:new"
	newUI    = "ghcr.io/onmoose/os-ui@sha256:new"
)

// fakeDocker records every call in order. The order is the point: this
// transaction's central claim is that the declaration is written *before* any
// container is recreated (UPDATES.md # 8.3), and only a recording of the
// sequence can hold that claim.
type fakeDocker struct {
	calls []string
	// failures, keyed by "verb:arg". A test sets one to make that step break.
	failOn map[string]error
	ips    map[string]string
	labels map[string]string
	// onRun lets a test simulate what a started container does to the box —
	// notably a new brain migrating the SQLite database on boot, which is the
	// case image rollback alone cannot undo.
	onRun func(image string)
	// beforeCall runs before each recorded call, so a test can observe the box's
	// on-disk state at the exact moment a container is touched.
	beforeCall func(call string)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{failOn: map[string]error{}, ips: map[string]string{}, labels: map[string]string{}}
}

// record fails on a dead context, the way exec.CommandContext does. Without
// this the fake would happily "run docker" after cancellation and the
// revert-survives-cancellation test would prove nothing.
func (f *fakeDocker) record(ctx context.Context, call string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", call, err)
	}
	if f.beforeCall != nil {
		f.beforeCall(call)
	}
	f.calls = append(f.calls, call)
	return f.failOn[call]
}

func (f *fakeDocker) Pull(ctx context.Context, ref string) error { return f.record(ctx, "pull:"+ref) }
func (f *fakeDocker) RemoveContainer(ctx context.Context, name string) error {
	return f.record(ctx, "rm:"+name)
}

// ImageLabel answers with the protocol major this host-agent speaks unless a
// test overrides it, so the lockstep guard is satisfied by default and can be
// made to fail on demand.
func (f *fakeDocker) ImageLabel(ctx context.Context, ref, _ string) (string, error) {
	if err := f.record(ctx, "label:"+ref); err != nil {
		return "", err
	}
	if v, ok := f.labels[ref]; ok {
		return v, nil
	}
	return strconv.Itoa(protocol.Major), nil
}

func (f *fakeDocker) Run(ctx context.Context, spec brainlaunch.RunSpec) error {
	if f.onRun != nil {
		f.onRun(spec.Image)
	}
	return f.record(ctx, "run:"+spec.Image)
}
func (f *fakeDocker) ComposeUp(ctx context.Context, dir, project string) (string, error) {
	return "", f.record(ctx, "compose:"+project)
}
func (f *fakeDocker) ContainerIP(ctx context.Context, name string) (string, error) {
	if err := f.record(ctx, "ip:"+name); err != nil {
		return "", err
	}
	if ip, ok := f.ips[name]; ok {
		return ip, nil
	}
	return "10.0.0.1", nil
}
func (f *fakeDocker) RemoveImage(ctx context.Context, ref string) error {
	return f.record(ctx, "rmi:"+ref)
}

func (f *fakeDocker) indexOf(t *testing.T, call string) int {
	t.Helper()
	for i, c := range f.calls {
		if c == call {
			return i
		}
	}
	t.Fatalf("call %q never happened; calls were %v", call, f.calls)
	return -1
}

// lastIndexOf is indexOf's mirror, for a call that happens on both the apply
// and the revert path: "compose:" appears once when the update runs and again
// when it is undone, and only the second one says anything about the revert's
// ordering.
func (f *fakeDocker) lastIndexOf(t *testing.T, call string) int {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i] == call {
			return i
		}
	}
	t.Fatalf("call %q never happened; calls were %v", call, f.calls)
	return -1
}

func (f *fakeDocker) has(call string) bool {
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

// fakeProber fails a configurable number of times, then succeeds.
type fakeProber struct {
	failURLs map[string]error
	seen     []string
}

func (p *fakeProber) WaitServing(ctx context.Context, url string) error {
	p.seen = append(p.seen, url)
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.failURLs[url]
}

// setup stages a control-plane dir with the real committed compose plus a brain
// state dir holding a SQLite file and its WAL sidecar.
func setup(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join("..", "..", "..", "dev", "control-plane", "compose.yml")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read committed compose: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, controlplane.ComposeFile), b, 0o644); err != nil {
		t.Fatalf("stage compose: %v", err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	for name, content := range map[string]string{
		"moose.db":     "OLD-DATABASE",
		"moose.db-wal": "OLD-WAL",
	} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return Options{
		ControlPlaneDir: dir,
		SnapshotRoot:    filepath.Join(t.TempDir(), "brain-snapshots"),
		BrainCfg: brainlaunch.Config{
			Image:         oldBrain,
			ContainerName: "moose-brain",
			StateDir:      stateDir,
			DataDir:       filepath.Dir(stateDir),
			SocketPath:    "/run/moose/agent.sock",
			Network:       "moose-ingress",
		},
		UIContainerName: "moose-ui",
		HealthTimeout:   time.Second,
	}
}

func ledgerOf(t *testing.T, o Options) controlplane.Ledger {
	t.Helper()
	l, ok, err := controlplane.ReadLedger(o.ControlPlaneDir)
	if err != nil || !ok {
		t.Fatalf("ReadLedger: ok=%v err=%v", ok, err)
	}
	return l
}

func uiRefOf(t *testing.T, o Options) string {
	t.Helper()
	ref, err := controlplane.ReadUIImage(o.ControlPlaneDir)
	if err != nil {
		t.Fatalf("ReadUIImage: %v", err)
	}
	return ref
}

// A coordinated ship: both refs move, both containers are recreated, and the
// declaration is written before either is touched.
func TestApplyBothChanged(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d, p := newFakeDocker(), &fakeProber{failURLs: map[string]error{}}

	res, err := Apply(context.Background(), d, p, o)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.BrainChanged || !res.UIChanged || res.Reverted {
		t.Fatalf("result = %+v, want both changed and no revert", res)
	}

	// The load-bearing ordering claim from # 8.3.
	if got := ledgerOf(t, o); got.Current.Brain != newBrain || got.Current.UI != newUI {
		t.Errorf("ledger current = %+v, want the new pair", got.Current)
	}
	if got := uiRefOf(t, o); got != newUI {
		t.Errorf("compose UI ref = %q, want %q", got, newUI)
	}
	if prev := ledgerOf(t, o).Previous; prev == nil || prev.Brain != oldBrain || prev.UI != oldUI {
		t.Errorf("ledger previous = %+v, want the old pair — a revert has nothing to aim at without it", prev)
	}
	if d.indexOf(t, "run:"+newBrain) < d.indexOf(t, "pull:"+newBrain) {
		t.Error("the brain was started before its image was pulled")
	}
}

// The # 8.3 handoff, asserted directly: at the moment the first container is
// *started*, both halves of the declaration on disk already name the new pair.
// That ordering is what lets the brain restart and reconcile *to* the running
// versions instead of reverting them — and it is what makes a box that dies
// mid-update come back converging on the target rather than on a pair nobody
// chose. Reading the files from inside the Docker fake is the only way to
// observe the sequence rather than infer it.
func TestDeclarationIsOnDiskBeforeTheFirstRecreate(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d := newFakeDocker()

	var ledgerAt, uiAt string
	d.beforeCall = func(call string) {
		// Only *starts* count. Stopping the old brain deliberately happens
		// before the declaration is written, because its database cannot be
		// snapshotted under a live writer — and a crash in that window is
		// consistent anyway: the ledger still names the old pair, so the next
		// boot brings the old brain back. What must never happen is a container
		// running an image the declaration does not name.
		if ledgerAt != "" || !(strings.HasPrefix(call, "run:") || strings.HasPrefix(call, "compose:")) {
			return
		}
		l, ok, err := controlplane.ReadLedger(o.ControlPlaneDir)
		if err != nil || !ok {
			ledgerAt = "MISSING"
		} else {
			ledgerAt = l.Current.Brain
		}
		uiAt, _ = controlplane.ReadUIImage(o.ControlPlaneDir)
	}

	if _, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ledgerAt != newBrain {
		t.Errorf("ledger named %q when the first container was touched, want %q", ledgerAt, newBrain)
	}
	if uiAt != newUI {
		t.Errorf("compose named %q when the first container was touched, want %q", uiAt, newUI)
	}
}

// A failure after the declaration is written must put both files back, or the
// box converges on a pair it is not running.
func TestApplyRestoresTheDeclarationWhenARecreateFails(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d := newFakeDocker()
	d.failOn["run:"+newBrain] = errors.New("boom")

	res, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o)
	if err == nil {
		t.Fatal("Apply succeeded, want the seeded run failure")
	}
	if res.FailureMode != "recreate" || !res.Reverted {
		t.Fatalf("result = %+v, want a reverted recreate failure", res)
	}
	// The recreate failed *after* the declaration was written, and the revert
	// then had to put the old pair back — which is only possible because the
	// ledger recorded it.
	if got := ledgerOf(t, o).Current; got.Brain != oldBrain || got.UI != oldUI {
		t.Errorf("ledger current = %+v after revert, want the old pair", got)
	}
	if got := uiRefOf(t, o); got != oldUI {
		t.Errorf("compose UI ref = %q after revert, want %q", got, oldUI)
	}
}

// A UI-only ship must not touch the brain at all: no pull, no removal, no
// restart. A brain restart is ~5–10s of API downtime (# 3 # Update window) and
// spending it for a change that did not involve the brain is a real cost.
func TestApplyUIOnlyLeavesTheBrainAlone(t *testing.T) {
	o := setup(t)
	o.UIRef = newUI
	d := newFakeDocker()

	res, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.BrainChanged {
		t.Error("BrainChanged = true for a UI-only target")
	}
	for _, call := range []string{"pull:" + oldBrain, "rm:moose-brain", "run:" + oldBrain} {
		if d.has(call) {
			t.Errorf("UI-only update made brain call %q; calls were %v", call, d.calls)
		}
	}
	if !d.has("compose:" + controlPlaneProject) {
		t.Error("the UI was not recreated")
	}
	// The ledger still records the whole pair — half a pair is not something a
	// revert can act on.
	if got := ledgerOf(t, o).Current; got.Brain != oldBrain || got.UI != newUI {
		t.Errorf("ledger current = %+v, want the unchanged brain beside the new UI", got)
	}
}

// A brain-only ship recreates the brain and leaves the compose file untouched.
func TestApplyBrainOnlyLeavesTheComposeAlone(t *testing.T) {
	o := setup(t)
	o.BrainRef = newBrain
	before, err := os.ReadFile(filepath.Join(o.ControlPlaneDir, controlplane.ComposeFile))
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	d := newFakeDocker()

	res, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.BrainChanged || res.UIChanged {
		t.Fatalf("result = %+v, want brain-only", res)
	}
	after, _ := os.ReadFile(filepath.Join(o.ControlPlaneDir, controlplane.ComposeFile))
	if string(after) != string(before) {
		t.Error("the compose was rewritten for a brain-only update")
	}
	if d.has("compose:" + controlPlaneProject) {
		t.Error("compose up ran for a brain-only update")
	}
}

// The health check is what the whole transaction turns on. When the brain does
// not answer, both refs go back, the containers are recreated on the old pair,
// and the SQLite snapshot is restored — the last of those is the part image
// rollback alone cannot do (# 3 step 4).
func TestApplyRevertsBothWhenTheBrainIsUnhealthy(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d := newFakeDocker()
	p := &fakeProber{failURLs: map[string]error{"http://10.0.0.1:8080/healthz": errors.New("connection refused")}}

	// The new brain migrates the database as it boots — the exact case that
	// makes an image-only rollback insufficient, since reverting the image
	// would leave old code reading data the new code rewrote.
	dbPath := filepath.Join(o.BrainCfg.StateDir, "moose.db")
	d.onRun = func(image string) {
		if image == newBrain {
			if err := os.WriteFile(dbPath, []byte("MIGRATED-BY-NEW-BRAIN"), 0o600); err != nil {
				t.Fatalf("simulate migration: %v", err)
			}
		}
	}

	res, err := Apply(context.Background(), d, p, o)
	if err == nil {
		t.Fatal("Apply succeeded, want the health-check failure")
	}
	if res.FailureMode != "health" || !res.Reverted || res.RevertErr != nil {
		t.Fatalf("result = %+v, want a clean reverted health failure", res)
	}
	if got := ledgerOf(t, o).Current; got.Brain != oldBrain || got.UI != oldUI {
		t.Errorf("ledger current = %+v, want the old pair restored", got)
	}
	if got := uiRefOf(t, o); got != oldUI {
		t.Errorf("compose UI ref = %q, want the old UI restored", got)
	}
	if !d.has("run:" + oldBrain) {
		t.Errorf("the old brain was not started again; calls were %v", d.calls)
	}
	// Both were reverted, not just the one that failed.
	if !d.has("compose:" + controlPlaneProject) {
		t.Error("the UI was not put back")
	}
	if b, err := os.ReadFile(dbPath); err != nil || string(b) != "OLD-DATABASE" {
		t.Errorf("database = %q (err %v), want the snapshot restored", b, err)
	}
}

// A pull failure must leave the box completely untouched: no container removed,
// no declaration written, nothing to revert.
func TestApplyPullFailureChangesNothing(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d := newFakeDocker()
	d.failOn["pull:"+newBrain] = errors.New("no route to host")
	composeBefore, _ := os.ReadFile(filepath.Join(o.ControlPlaneDir, controlplane.ComposeFile))

	res, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o)
	if err == nil {
		t.Fatal("Apply succeeded, want the pull failure")
	}
	if res.FailureMode != "pull" || res.Reverted {
		t.Fatalf("result = %+v, want an unreverted pull failure (nothing to revert)", res)
	}
	if _, ok, _ := controlplane.ReadLedger(o.ControlPlaneDir); ok {
		t.Error("a ledger was written despite the pull failing")
	}
	after, _ := os.ReadFile(filepath.Join(o.ControlPlaneDir, controlplane.ComposeFile))
	if string(after) != string(composeBefore) {
		t.Error("the compose changed despite the pull failing")
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "rm:") || strings.HasPrefix(c, "run:") || strings.HasPrefix(c, "compose:") {
			t.Errorf("container call %q ran after a failed pull; calls were %v", c, d.calls)
		}
	}
}

// Re-applying the pair the box already runs is a no-op, not an update. Without
// this the caller would report "updated" for a transaction that recreated
// containers for no reason.
func TestApplyNoopWhenAlreadyAtTarget(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = oldBrain, oldUI
	d := newFakeDocker()

	_, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o)
	if !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("err = %v, want ErrNothingToDo", err)
	}
	if len(d.calls) != 0 {
		t.Errorf("a no-op update made Docker calls: %v", d.calls)
	}
}

// The 7-day window (# 3): the generation the retained pair replaced is dropped
// once it expires, and the pair still on the box is never touched.
func TestGCDropsOnlyTheExpiredGeneration(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	now := time.Now()
	o.Now = func() time.Time { return now }

	// The box has already updated once: it runs the old pair and retains an
	// ancient one behind it.
	if err := controlplane.WriteLedger(o.ControlPlaneDir, controlplane.Ledger{
		Current:  controlplane.Pair{Brain: oldBrain, UI: oldUI, AppliedAt: now.Add(-9 * 24 * time.Hour)},
		Previous: &controlplane.Pair{Brain: "moose-brain:ancient", UI: "moose-ui:ancient", AppliedAt: now.Add(-9 * 24 * time.Hour)},
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	d := newFakeDocker()

	if _, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, ref := range []string{"moose-brain:ancient", "moose-ui:ancient"} {
		if !d.has("rmi:" + ref) {
			t.Errorf("expired image %s was not collected; calls were %v", ref, d.calls)
		}
	}
	for _, ref := range []string{oldBrain, oldUI, newBrain, newUI} {
		if d.has("rmi:" + ref) {
			t.Errorf("image %s was collected, but it is the current or rollback pair", ref)
		}
	}
}

// A generation still inside the window is kept — "moose broke since last night"
// is the realistic complaint, and a rollback whose target was already deleted
// is not a rollback.
func TestGCKeepsAGenerationInsideTheWindow(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	now := time.Now()
	o.Now = func() time.Time { return now }
	if err := controlplane.WriteLedger(o.ControlPlaneDir, controlplane.Ledger{
		Current:  controlplane.Pair{Brain: oldBrain, UI: oldUI, AppliedAt: now.Add(-2 * 24 * time.Hour)},
		Previous: &controlplane.Pair{Brain: "moose-brain:recent", UI: "moose-ui:recent", AppliedAt: now.Add(-2 * 24 * time.Hour)},
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	d := newFakeDocker()

	if _, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "rmi:") {
			t.Errorf("collected %q, but nothing is past the 7-day window", c)
		}
	}
}

// On a box that has never updated there is no ledger, so the "current" pair has
// to be assembled from the two places the refs actually live: the brain's
// launch config and the staged compose. Getting this wrong would record a
// previous pair that the box was never running.
func TestApplyDerivesTheCurrentPairWithoutALedger(t *testing.T) {
	o := setup(t)
	o.BrainRef = newBrain
	d := newFakeDocker()

	if _, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	prev := ledgerOf(t, o).Previous
	if prev == nil || prev.Brain != oldBrain || prev.UI != oldUI {
		t.Errorf("previous = %+v, want the baked brain ref and the compose's UI ref", prev)
	}
}

// A brain image declaring a protocol major this host-agent does not speak must
// never be committed. Skipping this guard is worse than skipping it at boot:
// the mismatched brain starts and answers /healthz (it serves HTTP before it
// needs host-agent), so the update commits and the box looks fine — until the
// next reboot, when brainlaunch reads that ref out of the ledger, applies the
// guard, and refuses, leaving the box with no brain and no failed update to
// point at.
func TestApplyRefusesABrainWithTheWrongProtocolMajor(t *testing.T) {
	o := setup(t)
	o.BrainRef = newBrain
	d := newFakeDocker()
	d.labels[newBrain] = "99"

	res, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o)
	if err == nil {
		t.Fatal("Apply succeeded on a protocol-major mismatch")
	}
	if !errors.Is(err, brainlaunch.ErrProtocolMismatch) {
		t.Errorf("err = %v, want it to wrap ErrProtocolMismatch", err)
	}
	if res.FailureMode != "recreate" || !res.Reverted {
		t.Fatalf("result = %+v, want a reverted recreate failure", res)
	}
	if d.has("run:" + newBrain) {
		t.Error("the mismatched brain was started")
	}
	if got := ledgerOf(t, o).Current.Brain; got != oldBrain {
		t.Errorf("ledger brain = %q after revert, want %q", got, oldBrain)
	}
}

// The revert has to survive whatever ended the apply. If it inherited the
// caller's context — a job past its deadline, a client that hung up, host-agent
// shutting down — every docker call in the rollback would fail instantly and
// the box would be left on the new brain with a ledger naming the old pair.
func TestRevertRunsEvenWhenTheCallersContextIsCancelled(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d := newFakeDocker()
	p := &fakeProber{failURLs: map[string]error{}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the moment the new brain starts, so the health check and
	// everything after it runs under a dead context.
	d.onRun = func(image string) {
		if image == newBrain {
			cancel()
		}
	}
	defer cancel()
	// Fail the health check too, so the revert is what we are watching rather
	// than the cancellation racing a success.
	p.failURLs["http://10.0.0.1:8080/healthz"] = errors.New("connection refused")

	res, err := Apply(ctx, d, p, o)
	if err == nil {
		t.Fatal("Apply succeeded, want a failure")
	}
	if !res.Reverted {
		t.Fatalf("result = %+v, want a revert", res)
	}
	if res.RevertErr != nil {
		t.Errorf("RevertErr = %v, want the revert to complete despite the cancelled context", res.RevertErr)
	}
	if !d.has("run:" + oldBrain) {
		t.Errorf("the old brain was not restarted; calls were %v", d.calls)
	}
	if got := ledgerOf(t, o).Current; got.Brain != oldBrain || got.UI != oldUI {
		t.Errorf("ledger current = %+v, want the old pair restored", got)
	}
}

// A component that did not move is not probed. Probing it would revert a good
// update because of an outage that predates it and that the revert cannot fix.
func TestApplyDoesNotProbeAComponentThatDidNotMove(t *testing.T) {
	o := setup(t)
	o.BrainRef = newBrain // brain-only
	d := newFakeDocker()
	// The UI has been down since before this update started.
	d.failOn["ip:moose-ui"] = errors.New("no such container")

	res, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o)
	if err != nil {
		t.Fatalf("Apply: %v — a pre-existing UI outage must not fail a brain-only update", err)
	}
	if res.Reverted {
		t.Error("a brain-only update was reverted because of the UI")
	}
	if d.has("ip:moose-ui") {
		t.Error("the UI was probed although it did not move")
	}
}

// The snapshot kept for the rollback target must survive GC. When the previous
// generation moved only the UI, the expired generation and the retained one
// share a brain ref — and so share a snapshot directory. Deleting it would
// leave the kept pair with images and no database, and restoreBrainDB reports a
// missing snapshot as success, so the rollback would silently restore nothing.
func TestGCKeepsTheSnapshotTheRollbackTargetNeeds(t *testing.T) {
	o := setup(t)
	o.BrainRef = newBrain
	now := time.Now()
	o.Now = func() time.Time { return now }
	// Last update moved only the UI, so both generations name the same brain.
	if err := controlplane.WriteLedger(o.ControlPlaneDir, controlplane.Ledger{
		Current:  controlplane.Pair{Brain: oldBrain, UI: oldUI, AppliedAt: now.Add(-9 * 24 * time.Hour)},
		Previous: &controlplane.Pair{Brain: oldBrain, UI: "moose-ui:ancient", AppliedAt: now.Add(-9 * 24 * time.Hour)},
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	d := newFakeDocker()

	if _, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	snap := filepath.Join(snapshotDirFor(o.SnapshotRoot, oldBrain), "moose.db")
	if _, err := os.Stat(snap); err != nil {
		t.Errorf("the retained pair's snapshot was collected: %v", err)
	}
	if d.has("rmi:" + oldBrain) {
		t.Error("the brain image the rollback target needs was collected")
	}
}

// The compose stack must be up BEFORE the brain is started, on a coordinated
// ship. This is not a style preference — it is the fix for a real failure the
// booted-box lane caught on its first run (#382).
//
// The brain runs `docker compose up -d` on this same project at every startup
// (lifecycle.EnsureControlPlane). Started first, it boots straight into the
// compose run host-agent is in the middle of; the two interleave the rename
// dance compose does to recreate a service, collide on the backup name, and
// leave the box with **no container named moose-ui at all**. The health check
// then cannot resolve the UI's address and reverts a perfectly good update.
//
// UPDATES.md # 3 step 3c used to specify the racing order ("brain first, then
// UI") and was corrected with this change.
func TestComposeIsUpBeforeTheBrainIsStarted(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d := newFakeDocker()

	if _, err := Apply(context.Background(), d, &fakeProber{failURLs: map[string]error{}}, o); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	compose := d.indexOf(t, "compose:"+controlPlaneProject)
	brain := d.indexOf(t, "run:"+newBrain)
	if compose > brain {
		t.Errorf("the brain was started before the compose stack was up (compose at %d, run at %d); calls were %v — the brain's own startup reconcile then runs a second compose on the same project", compose, brain, d.calls)
	}
}

// Same ordering on the revert path. It is asserted separately, and not assumed
// from the apply path, because these two functions have already drifted apart
// once in this package: #380 detached the revert's context and missed the
// snapshot-failure branch, and #381 is where that became a real hole.
func TestRevertBringsComposeUpBeforeStartingTheOldBrain(t *testing.T) {
	o := setup(t)
	o.BrainRef, o.UIRef = newBrain, newUI
	d := newFakeDocker()
	p := &fakeProber{failURLs: map[string]error{"http://10.0.0.1:8080/healthz": errors.New("connection refused")}}

	res, err := Apply(context.Background(), d, p, o)
	if err == nil {
		t.Fatal("Apply succeeded, want the health-check failure that triggers the revert")
	}
	if !res.Reverted {
		t.Fatalf("result = %+v, want a revert", res)
	}
	// The revert's compose is the LAST one; the apply's came earlier.
	compose := d.lastIndexOf(t, "compose:"+controlPlaneProject)
	brain := d.indexOf(t, "run:"+oldBrain) // only the revert starts the old brain
	if compose > brain {
		t.Errorf("the revert started the old brain before putting the compose stack back (compose at %d, run at %d); calls were %v", compose, brain, d.calls)
	}
}
