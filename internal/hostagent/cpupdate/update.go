// Package cpupdate runs the control-plane update transaction: the stream-B
// apply and rollback that UPDATES.md # 3 specifies and # 8 says is shared by
// the appliance and hosted profiles ("the expensive half of the update
// machinery is shared, and we only build it once").
//
// It is deliberately trigger-free. Nothing here knows whether the target came
// from a signed release manifest (appliance, RELEASE_MANIFEST.md), from the
// cloud control plane (hosted, # 8.1), or from an admin typing two refs. A
// caller hands it a target pair; it applies it or puts the box back exactly as
// it was.
//
// The transaction, in order (# 3 step 3, # 8.4 step 3):
//
//  1. Pull every image whose ref moved. Nothing on the box has changed yet, so
//     a pull failure is free.
//  2. If the brain moved: stop it and snapshot its SQLite (snapshot.go).
//  3. Write the declaration — ledger + staged compose — **before** recreating
//     anything. This is the # 8.3 handoff: the brain comes back up and
//     reconciles to the refs already running instead of fighting them back.
//  4. Recreate only what moved — **compose first, brain last**. The brain runs
//     the same compose project at startup, so a brain started before the
//     compose finishes runs a second, concurrent `compose up` on it. See
//     recreate() for what that does to the box.
//  5. Health-check the brain (/healthz) and the UI.
//  6. On any failure from step 2 on: revert **both** refs, restore the
//     snapshot, put the containers back, and report which step failed.
package cpupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/onmoose/os/internal/hostagent/brainlaunch"
	"github.com/onmoose/os/internal/hostagent/controlplane"
	"github.com/onmoose/os/internal/protocol"
)

// controlPlaneProject is the compose project the staged control-plane stack
// runs under. It must match internal/lifecycle's constant of the same name: the
// brain and host-agent both bring this stack up, and two project names would
// give one compose file two independent sets of containers.
const controlPlaneProject = "moose-control-plane"

// brainPort is the port the brain listens on inside its container (cmd/brain's
// MOOSE_LISTEN default). The brain publishes no port, so the health probe dials
// the container's own address on the ingress network.
const brainPort = 8080

// uiPort is the port the moose-ui container serves the dashboard bundle on.
const uiPort = 80

// revertBudget bounds the rollback. It is deliberately generous and wholly
// separate from the apply's budget: the rollback runs on a context that no
// longer answers to the caller (see revert), so this is the only thing keeping
// it from hanging host-agent.
const revertBudget = 5 * time.Minute

// defaultHealthTimeout bounds the wait for both components to answer after a
// recreate — UPDATES.md # 3 step 3d: "wait up to 60s".
const defaultHealthTimeout = 60 * time.Second

// Docker is the slice of the `docker` CLI this transaction needs. Consumer-side
// (CLAUDE.md # Go code discipline): the production implementation is CLIDocker
// in cli.go, tests pass a recording fake.
type Docker interface {
	// Pull fetches an image ref. Production refs are digests (BUILD.md # 6), so
	// this is the step that can fail on a box with no internet.
	Pull(ctx context.Context, ref string) error
	// RemoveContainer stops and removes a container by name, treating "no such
	// container" as success so a retry after a partial failure is safe.
	RemoveContainer(ctx context.Context, name string) error
	// ImageLabel reads one OCI label off an image, or "" when the image does
	// not carry it. The transaction uses it for the brain's protocol-major
	// lockstep check, the same guard brainlaunch applies at boot.
	ImageLabel(ctx context.Context, ref, label string) (string, error)
	// Run starts a detached container from the brain's launch spec.
	Run(ctx context.Context, spec brainlaunch.RunSpec) error
	// ComposeUp reconciles the staged control-plane project (Caddy + moose-ui)
	// to whatever the compose file in dir now declares.
	ComposeUp(ctx context.Context, dir, project string) (string, error)
	// ContainerIP reports a container's address on its network, so the health
	// probe can reach a container that publishes no port.
	ContainerIP(ctx context.Context, name string) (string, error)
	// RemoveImage deletes an image that is past its retention window. Best
	// effort: an image still referenced by a container is left alone.
	RemoveImage(ctx context.Context, ref string) error
}

// Prober answers "is this URL serving?" for the post-recreate health check.
// Separate from Docker because it is the one seam a test must be able to make
// fail on demand — a revert that never runs is a revert nobody has tested.
type Prober interface {
	WaitServing(ctx context.Context, url string) error
}

// Options is one update: where the box's declaration lives, how to launch the
// brain, and the target pair.
type Options struct {
	// ControlPlaneDir holds the staged compose and the ledger.
	ControlPlaneDir string
	// BrainCfg is the brain's launch config: mounts, env, network, container
	// name and state dir, which are what make a recreated brain identical to a
	// first-boot one.
	//
	// Image must be set, and is **not** ignored. On a box that has never
	// updated there is no ledger, so it is the only record of which brain the
	// box is running — the ref a revert has to go back to. Leaving it empty
	// would produce a ledger whose previous pair has no brain, and the failure
	// would land at the moment the rollback is needed.
	BrainCfg brainlaunch.Config
	// SnapshotRoot is where pre-update SQLite snapshots are kept
	// (UPDATES.md # 3 step 3b).
	SnapshotRoot string
	// BrainRef / UIRef are the target refs. An empty ref means "leave this
	// component alone", which is how a UI-only or brain-only ship is expressed
	// (# 3: "the updater recreates only what changed").
	BrainRef string
	UIRef    string
	// UIContainerName is the compose container the UI probe dials. Empty uses
	// the compose file's own container_name.
	UIContainerName string
	// HealthTimeout overrides the 60s default. Tests shrink it.
	HealthTimeout time.Duration
	// Now is the clock, so a test can age a retained generation past the
	// 7-day window without waiting a week.
	Now func() time.Time
}

// Result describes what the transaction did. It is returned on success and on
// failure alike, because "we reverted, and here is what broke" is the answer
// the caller has to surface (# 3 step 4: the UI shows the failure with a
// "rollback succeeded" status).
type Result struct {
	BrainChanged bool
	UIChanged    bool
	// Reverted is true when the box was put back to the pair it was running.
	Reverted bool
	// FailureMode names the step that failed: "pull", "snapshot", "declare",
	// "recreate", or "health". Empty on success.
	FailureMode string
	// RevertErr is set when the revert itself failed — the box is then in a
	// state no automatic path can fix, and this is what tells the operator so.
	RevertErr error
}

// ErrNothingToDo is returned when the target pair is already what the box is
// running. A no-op update is not a failure, but a caller that reports "updated"
// for it would be lying.
var ErrNothingToDo = errors.New("control plane already at the target pair")

// Apply runs the transaction. See the package comment for the ordering and why
// it is that order.
func Apply(ctx context.Context, d Docker, p Prober, o Options) (Result, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	timeout := o.HealthTimeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}

	before, err := currentPair(o)
	if err != nil {
		// Nothing has been declared yet; naming this step "declare" would point
		// an operator at the wrong place.
		return Result{FailureMode: "read"}, err
	}
	target := controlplane.Pair{
		Brain:     firstNonEmpty(o.BrainRef, before.Current.Brain),
		UI:        firstNonEmpty(o.UIRef, before.Current.UI),
		AppliedAt: now().UTC(),
	}
	res := Result{
		BrainChanged: target.Brain != before.Current.Brain,
		UIChanged:    target.UI != before.Current.UI,
	}
	if !res.BrainChanged && !res.UIChanged {
		return res, ErrNothingToDo
	}

	// 1. Pull first: until an image is on the box, nothing else can start, and
	// failing here costs nothing because nothing has been touched.
	for _, ref := range changedRefs(res, target) {
		if err := d.Pull(ctx, ref); err != nil {
			return failed(res, "pull"), fmt.Errorf("pull %s: %w", ref, err)
		}
	}

	// 2. The brain has to stop before its database is copied — a snapshot taken
	// under a live writer restores into a corrupt database (snapshot.go).
	snapDir := snapshotDirFor(o.SnapshotRoot, before.Current.Brain)
	if res.BrainChanged {
		if err := d.RemoveContainer(ctx, o.BrainCfg.ContainerName); err != nil {
			// Part of the snapshot step: the brain is stopped *so that* the
			// database can be copied consistently.
			return failed(res, "snapshot"), fmt.Errorf("remove brain container: %w", err)
		}
		if err := snapshotBrainDB(o.BrainCfg.StateDir, snapDir); err != nil {
			// The brain is down and its image has not changed: put it back on
			// the ref it was already running before giving up. Nothing has been
			// declared yet, so this is the whole recovery — but it runs on the
			// same detached context revert() uses, for the same reason.
			r := failed(res, "snapshot")
			r.Reverted, r.RevertErr = true, restoreBrainContainer(ctx, d, o, before.Current.Brain)
			return r, fmt.Errorf("snapshot brain database: %w", err)
		}
	}

	// 3. Declare before recreating (# 8.3). If the box dies between here and
	// the recreate, the next boot converges to the target rather than to a pair
	// nobody chose.
	if err := declare(o, controlplane.Ledger{Current: target, Previous: &before.Current}, res); err != nil {
		r := revert(ctx, d, p, o, before, res, "declare", snapDir, timeout)
		return r, err
	}

	// 4 + 5. Recreate what moved, then wait for both to answer.
	if err := recreate(ctx, d, o, target, res); err != nil {
		r := revert(ctx, d, p, o, before, res, "recreate", snapDir, timeout)
		return r, err
	}
	if err := waitHealthy(ctx, d, p, o, res, timeout); err != nil {
		r := revert(ctx, d, p, o, before, res, "health", snapDir, timeout)
		return r, err
	}

	slog.Info("control plane updated", "image", target.Brain, "step", "committed")
	gc(ctx, d, o, before, now())
	return res, nil
}

// currentPair reads what the box is running now. The ledger is authoritative
// once an update has landed; before that it does not exist, and the pair is
// assembled from the two places the box's refs actually live — the brain's
// launch config and the staged compose.
func currentPair(o Options) (controlplane.Ledger, error) {
	l, ok, err := controlplane.ReadLedger(o.ControlPlaneDir)
	if err != nil {
		return controlplane.Ledger{}, err
	}
	if ok && l.Current.Brain != "" && l.Current.UI != "" {
		return l, nil
	}
	ui, err := controlplane.ReadUIImage(o.ControlPlaneDir)
	if err != nil {
		return controlplane.Ledger{}, err
	}
	if o.BrainCfg.Image == "" {
		// Fail before anything is touched rather than writing a ledger whose
		// previous pair cannot be rolled back to.
		return controlplane.Ledger{}, errors.New("no current brain image: neither the ledger nor BrainCfg.Image names one")
	}
	return controlplane.Ledger{Current: controlplane.Pair{Brain: o.BrainCfg.Image, UI: ui}}, nil
}

// declare writes both halves of the declaration: the ledger (the brain's ref)
// and the staged compose (the UI's). The compose is only touched when the UI
// moved — rewriting it with the ref it already holds would be a no-op write on
// a file the brain reconciles from, for no gain.
func declare(o Options, l controlplane.Ledger, res Result) error {
	if err := controlplane.WriteLedger(o.ControlPlaneDir, l); err != nil {
		return err
	}
	if res.UIChanged {
		if _, err := controlplane.RewriteUIImage(o.ControlPlaneDir, l.Current.UI); err != nil {
			return err
		}
	}
	return nil
}

// recreate replaces the containers whose refs moved. The brain is recreated
// from the same spec brainlaunch uses at boot, so the only difference between
// an updated brain and a freshly booted one is the image.
//
// **The compose stack goes up first, and the brain is started last.** UPDATES.md
// # 3 step 3c used to say the opposite ("brain first, then UI"), and that order
// is a race: the brain runs `docker compose up -d` on this same
// `moose-control-plane` project at every startup (lifecycle.EnsureControlPlane),
// so a brain started before ComposeUp boots straight into the compose run
// host-agent is in the middle of. Two composes on one project then interleave
// the rename dance compose does to recreate a service, they collide on the
// backup name, and the box is left with **no container named `moose-ui` at
// all** — after which this transaction's own health check cannot resolve the
// UI's address, reports the update unhealthy, and reverts a change that was
// otherwise fine. That is not a theory: it is what the booted-box lane saw on
// its first run (docs/progress/cloud-update-boot-proof.md).
//
// Starting the brain last removes the concurrency instead of narrowing it: the
// compose runs while no brain exists, and the brain then boots into a stack that
// already matches the declaration, so its own reconcile is a no-op. It costs a
// few extra seconds of API downtime, which is inside the coordinated-ship notice
// the spec already promises.
func recreate(ctx context.Context, d Docker, o Options, target controlplane.Pair, res Result) error {
	if res.UIChanged {
		if out, err := d.ComposeUp(ctx, o.ControlPlaneDir, controlPlaneProject); err != nil {
			return fmt.Errorf("control-plane compose up: %w\n%s", err, out)
		}
	}
	if res.BrainChanged {
		// Idempotent by design: step 2 already removed this container to take
		// the snapshot, and RemoveContainer treats an absent container as
		// success. Repeating it keeps this function correct on its own terms
		// instead of depending on what the caller did first.
		if err := d.RemoveContainer(ctx, o.BrainCfg.ContainerName); err != nil {
			return fmt.Errorf("remove brain container: %w", err)
		}
		if err := checkProtocolMajor(ctx, d, target.Brain); err != nil {
			return err
		}
		if err := d.Run(ctx, brainSpec(o, target.Brain)); err != nil {
			return fmt.Errorf("run brain on %s: %w", target.Brain, err)
		}
	}
	return nil
}

// waitHealthy probes the components this transaction actually recreated.
//
// It deliberately does **not** probe a component that did not move. A UI-only
// ship never touches the brain container and a brain-only ship never touches
// the UI, so a failure there says nothing about this update — but it would
// still trigger a revert, undoing a good change because of an outage that
// predates it and that the revert cannot fix. That is the same reasoning that
// makes a 500 count as "serving" (cli.go): the question is whether what we just
// started came up, not whether the whole box is well. Box-wide health has its
// own surface in the brain (HEALTH.md).
func waitHealthy(ctx context.Context, d Docker, p Prober, o Options, res Result, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if res.BrainChanged {
		brainIP, err := d.ContainerIP(ctx, o.BrainCfg.ContainerName)
		if err != nil {
			return fmt.Errorf("resolve brain address: %w", err)
		}
		if err := p.WaitServing(ctx, fmt.Sprintf("http://%s:%d/healthz", brainIP, brainPort)); err != nil {
			return fmt.Errorf("brain health check: %w", err)
		}
	}
	if res.UIChanged {
		uiName := o.UIContainerName
		if uiName == "" {
			uiName = controlplane.UIServiceName
		}
		uiIP, err := d.ContainerIP(ctx, uiName)
		if err != nil {
			return fmt.Errorf("resolve ui address: %w", err)
		}
		if err := p.WaitServing(ctx, fmt.Sprintf("http://%s:%d/", uiIP, uiPort)); err != nil {
			return fmt.Errorf("ui health check: %w", err)
		}
	}
	return nil
}

// revert puts the box back on the pair it was running: the declaration first
// (same order as the apply, for the same reason), then the containers, with the
// SQLite snapshot restored while the brain is down.
//
// **Both refs go back, even if only one moved** (# 3 step 4: "reverts both to
// the previous pair"). A coordinated ship is tested as a pair, so a box left
// running half of one is in a combination nobody has ever run.
func revert(ctx context.Context, d Docker, p Prober, o Options, before controlplane.Ledger, res Result, mode, snapDir string, timeout time.Duration) Result {
	r := failed(res, mode)
	r.Reverted = true
	slog.Warn("control-plane update failed; reverting", "step", mode, "image", before.Current.Brain)

	ctx, cancel := detached(ctx)
	defer cancel()

	if err := declare(o, before, res); err != nil {
		r.RevertErr = fmt.Errorf("restore declaration: %w", err)
		return r
	}
	// Same ordering as the apply, and for the same reason: the compose stack
	// goes back first and the brain is started last, so host-agent's compose can
	// never run at the same time as the brain's own startup reconcile of the
	// same project. The two paths are kept in one order deliberately — this
	// package has already been bitten once by a fix applied to the apply path
	// and missed on the recovery path (see detached()).
	//
	// The brain container is removed BEFORE the compose runs, not after: the
	// database restore below needs the brain stopped, and a brain left running
	// during the compose is the very concurrency this order exists to remove.
	if res.BrainChanged {
		if err := d.RemoveContainer(ctx, o.BrainCfg.ContainerName); err != nil {
			r.RevertErr = fmt.Errorf("remove brain container: %w", err)
			return r
		}
		// The new brain may have migrated the database on startup, which is the
		// case image rollback alone cannot fix (# 4 # Pre-update snapshot makes
		// the same argument for apps).
		if err := restoreBrainDB(snapDir, o.BrainCfg.StateDir); err != nil {
			r.RevertErr = fmt.Errorf("restore brain database: %w", err)
			return r
		}
	}
	if res.UIChanged {
		if out, err := d.ComposeUp(ctx, o.ControlPlaneDir, controlPlaneProject); err != nil {
			r.RevertErr = fmt.Errorf("control-plane compose up: %w\n%s", err, out)
			return r
		}
	}
	if res.BrainChanged {
		if err := d.Run(ctx, brainSpec(o, before.Current.Brain)); err != nil {
			r.RevertErr = fmt.Errorf("run brain on %s: %w", before.Current.Brain, err)
			return r
		}
	}
	// Whether the restored pair answers is worth knowing, but it does not
	// change what happens next: there is no third generation to fall back to.
	if err := waitHealthy(ctx, d, p, o, res, timeout); err != nil {
		slog.Error("reverted control plane is not answering", "err", err)
	}
	return r
}

// gc drops the generation that the retained pair replaced, once it is past the
// 7-day window (# 3: "keep the previous brain/UI image pair and SQLite snapshot
// for 7 days, then GC").
//
// It runs after a successful apply and never fails the update: a box that could
// not delete an old image is a box using more disk than it should, which is not
// a reason to undo a working update. Refs still named by the pair we just kept
// are skipped — the ledger's previous entry is the rollback target.
func gc(ctx context.Context, d Docker, o Options, before controlplane.Ledger, now time.Time) {
	if !before.PreviousExpired(now) {
		return
	}
	old := *before.Previous
	keep := map[string]bool{before.Current.Brain: true, before.Current.UI: true, o.BrainRef: true, o.UIRef: true}
	for _, ref := range []string{old.Brain, old.UI} {
		if ref == "" || keep[ref] {
			continue
		}
		if err := d.RemoveImage(ctx, ref); err != nil {
			slog.Warn("could not remove expired control-plane image", "image", ref, "err", err)
		}
	}
	// The snapshot dir is keyed by brain ref, so when the expired generation and
	// the retained one share a brain ref (the previous update moved only the
	// UI), this path is the snapshot this very transaction just took for the
	// rollback target. Deleting it would leave the kept pair with images and no
	// database — and restoreBrainDB reports a missing snapshot as success, so
	// the future rollback would silently restore nothing.
	if !keep[old.Brain] {
		if err := removeSnapshotDir(snapshotDirFor(o.SnapshotRoot, old.Brain)); err != nil {
			slog.Warn("could not remove expired brain snapshot", "err", err)
		}
	}
}

// checkProtocolMajor refuses to start a brain whose declared wire-protocol
// major this host-agent does not speak — the same guard brainlaunch.Launch
// applies on the first-boot path (BRAIN_HOST_PROTOCOL.md # Versioning).
//
// Skipping it here would be worse than skipping it at boot. The mismatched
// brain **starts and answers /healthz**, because the brain serves HTTP before
// it needs host-agent for anything, so the health check passes and the update
// commits. The box then looks fine until its next reboot, when brainlaunch
// reads the committed ref out of the ledger, applies the guard, and refuses —
// leaving the box with no brain at all, and no failed update to point at.
func checkProtocolMajor(ctx context.Context, d Docker, ref string) error {
	want := strconv.Itoa(protocol.Major)
	got, err := d.ImageLabel(ctx, ref, protocol.ImageProtocolMajorLabel)
	if err != nil {
		return fmt.Errorf("read brain protocol label: %w", err)
	}
	if got != want {
		return fmt.Errorf("%w: brain image %q declares protocol major %q, host-agent speaks %q",
			brainlaunch.ErrProtocolMismatch, ref, got, want)
	}
	return nil
}

// detached returns the context every recovery path runs on: the caller's
// values, none of its cancellation, and a budget of its own.
//
// Recovery must outlive whatever ended the apply. If it inherited the caller's
// context — a job that hit its MaxDuration, a client that hung up, host-agent
// shutting down — every `docker` call in it would fail instantly on a dead
// context, and the box would be left running the new brain against a ledger
// naming the old pair, or with no brain container at all. **A cancelled apply
// is exactly when recovery matters most.**
//
// It is one function so the two recovery paths cannot drift apart. They did:
// revert() was detached in #380 and the snapshot-failure branch was missed,
// which left a real hole once #381 started calling Apply with a cancellable
// context and a Docker that honours it.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), revertBudget)
}

// restoreBrainContainer puts the brain back on ref. It is the whole recovery
// for a failure between "the old brain was removed for the snapshot" and "the
// declaration was written": nothing else on the box has changed yet, so the
// container is the only thing to put back.
func restoreBrainContainer(ctx context.Context, d Docker, o Options, ref string) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	return d.Run(ctx, brainSpec(o, ref))
}

func brainSpec(o Options, ref string) brainlaunch.RunSpec {
	cfg := o.BrainCfg
	cfg.Image = ref
	return brainlaunch.RunSpecFor(cfg)
}

func changedRefs(res Result, target controlplane.Pair) []string {
	var refs []string
	if res.BrainChanged {
		refs = append(refs, target.Brain)
	}
	if res.UIChanged {
		refs = append(refs, target.UI)
	}
	return refs
}

func failed(res Result, mode string) Result {
	res.FailureMode = mode
	return res
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
