package hostagent

// The job surface on the host socket (BRAIN_HOST_PROTOCOL.md # Pattern B).
//
// This is deliberately the minimum, not the framework. Pattern B as written
// describes a kind registry with typed attributes, resource-class queueing, a
// cross-class dangerous lock, cancel, and an SSE log stream with replay. There
// is exactly one job kind today (system-update), so building all of that would
// be an abstraction with one consumer, which CLAUDE.md # Go code discipline
// tells us not to do. What is here is the part one dangerous job needs:
//
//   - a job record a caller can poll,
//   - a MaxDuration bound on the run (# Failure semantics A),
//   - one global lock, so two updates can never overlap. That is the
//     `Dangerous: true` rule ("never run two destructive ops concurrently")
//     realized for a single kind. The second caller is refused with 409, not
//     queued: an admin who clicks Update twice wants one update, not two.
//
// The record lives in host-agent's memory, and that is the right side of the
// socket for it. A control-plane update replaces the brain container, so a
// brain-side job record would die halfway through the very operation it tracks.
// host-agent is the process that stays up.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// systemUpdateMaxDuration bounds one control-plane update run
// (BRAIN_HOST_PROTOCOL.md # Failure semantics A gives system-update 30m). Past
// it the run's context is cancelled; cpupdate then rolls the box back on a
// context of its own, so the job ends `failed` with a `job-timeout` code.
//
// A var, not a const, so a test can shrink it and prove the bound fires without
// waiting half an hour.
var systemUpdateMaxDuration = 30 * time.Minute

// SystemUpdater is a consumer-side interface for the control-plane update
// transaction behind POST /v1/jobs/system-update (UPDATES.md # 3). Provider
// packages return concrete types: cpupdate.Runner (real `docker` + HTTP probe)
// for cmd/host-agent-real.
//
// brainRef and uiRef are the target refs; an empty one means "leave that
// component alone". A returned error means the update failed and the box was
// put back — the Result says which step broke and whether the revert worked.
//
// When nil (the fake binary, tests), POST /v1/jobs/system-update returns 501,
// the same degrade as journal_follow: the dev loop has no control plane to
// update, and a silent success would be a lie.
type SystemUpdater interface {
	Update(ctx context.Context, brainRef, uiRef string) (protocol.SystemUpdateResult, error)
}

// errJobRunning is what the global lock returns when a job is already in
// flight. The handler maps it to 409.
var errJobRunning = errors.New("a job is already running")

// ErrJobRunning is errJobRunning for a caller outside this package that has its
// own way of saying "busy", such as the update loop's OS switch, so the apply
// route answers it with the same 409.
var ErrJobRunning = errJobRunning

// errNoUpdater is what StartUpdate returns on a binary with no control-plane
// updater wired (the fake agent, tests). The handler maps it to 501.
var errNoUpdater = errors.New("this host-agent has no control-plane updater")

// jobRegistry holds the in-memory job records and the one global lock.
//
// Records are never evicted. A box runs a handful of control-plane updates in
// its life and each record is a few hundred bytes, so a map that only grows is
// cheaper than an eviction policy nobody can tune. They are lost on host-agent
// restart, which matches "Dangerous: crash mid-flight = no auto-resume".
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*protocol.Job
	// running is the id of the in-flight job, or "" when none. This single
	// field is the whole lock.
	running string
	now     func() time.Time
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{jobs: map[string]*protocol.Job{}, now: time.Now}
}

// start records a new running job, or returns errJobRunning when one is
// already in flight.
func (r *jobRegistry) start(kind string) (protocol.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != "" {
		return protocol.Job{}, fmt.Errorf("%w: %s", errJobRunning, r.running)
	}
	job := &protocol.Job{
		ID:        newJobID(),
		Kind:      kind,
		Status:    protocol.JobStatusRunning,
		StartedAt: r.now().UTC().Format(time.RFC3339),
	}
	r.jobs[job.ID] = job
	r.running = job.ID
	return *job, nil
}

// finish closes a job out and releases the lock. code is empty on success.
func (r *jobRegistry) finish(id string, res protocol.SystemUpdateResult, code, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return
	}
	job.FinishedAt = r.now().UTC().Format(time.RFC3339)
	job.Result = &res
	if code == "" {
		job.Status = protocol.JobStatusCompleted
	} else {
		job.Status = protocol.JobStatusFailed
		job.Error = &protocol.Error{Code: code, Message: message}
	}
	if r.running == id {
		r.running = ""
	}
}

func (r *jobRegistry) get(id string) (protocol.Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return protocol.Job{}, false
	}
	return *job, true
}

func newJobID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "j_" + hex.EncodeToString(b)
}

// startSystemUpdate accepts a control-plane update and runs it in the
// background (202 + a job id, BRAIN_HOST_PROTOCOL.md # Pattern B). The refs are
// in the request body: this endpoint does not know about release manifests or
// the cloud, and the transaction it starts does not either (cpupdate).
func (a *Agent) startSystemUpdate(w http.ResponseWriter, r *http.Request) {
	var req protocol.SystemUpdateRequest
	if !decode(w, r, &req) {
		return
	}
	if a.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update-unsupported", "this host-agent has no control-plane updater")
		return
	}
	if req.BrainImage == "" && req.UIImage == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "brain_image or ui_image is required")
		return
	}

	job, err := a.StartUpdate(req.BrainImage, req.UIImage)
	switch {
	case errors.Is(err, errJobRunning):
		writeErr(w, http.StatusConflict, "job-running", err.Error())
		return
	case errors.Is(err, errNoUpdater):
		// Unreachable behind the nil check above, and matched by its own sentinel
		// rather than as "anything else": a later refusal added inside StartUpdate
		// would otherwise reach an admin as "update not supported", which is a
		// different and wrong thing to tell them.
		writeErr(w, http.StatusNotImplemented, "update-unsupported", err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "update-failed", "could not start the update")
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// StartUpdate begins a control-plane update in the background and returns the
// job record. It is the one way an update starts: the HTTP handler above calls
// it, and so does the update-target loop that applies what the cloud or the
// release manifest names (internal/hostagent/updatetarget).
//
// Sharing it is the point. The job registry holds **one global lock**, so an
// admin clicking Update while the box is already applying its target gets a
// refusal instead of a second concurrent transaction on the same containers. A
// second entry point that reached past this into cpupdate would step around
// that lock, and the two updates would fight over the brain container.
func (a *Agent) StartUpdate(brainRef, uiRef string) (protocol.Job, error) {
	return a.StartUpdateTo(brainRef, uiRef, "")
}

// StartUpdateTo is StartUpdate with the version the target names, for the
// outcome record when the images cannot say it themselves (a target that
// never pulled has no label to read). The update-target loop knows it; the
// explicit-refs route does not.
func (a *Agent) StartUpdateTo(brainRef, uiRef, version string) (protocol.Job, error) {
	if a.Updater == nil {
		return protocol.Job{}, errNoUpdater
	}
	job, err := a.jobs.start(protocol.JobKindSystemUpdate)
	if err != nil {
		slog.Warn("system-update refused: another job is running", "err", err)
		return protocol.Job{}, err
	}
	slog.Info("system-update started", "step", "accepted", "image", brainRef)

	// The run outlives its caller, so it gets its own context — a client hanging
	// up, or a poll tick returning, must not abort an update that is already
	// changing the box. The only bound on it is MaxDuration.
	go a.runSystemUpdate(job, protocol.SystemUpdateRequest{BrainImage: brainRef, UIImage: uiRef}, version)

	return job, nil
}

func (a *Agent) runSystemUpdate(job protocol.Job, req protocol.SystemUpdateRequest, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), systemUpdateMaxDuration)
	defer cancel()

	res, err := a.Updater.Update(ctx, req.BrainImage, req.UIImage)
	if err == nil {
		slog.Info("system-update finished", "step", "committed", "image", req.BrainImage)
		a.saveOutcome(job, res, version, false)
		a.jobs.finish(job.ID, res, "", "")
		return
	}
	// Distinguish "it broke" from "it ran too long and we stopped it": the
	// second is the MaxDuration bound firing, and an operator reading the job
	// needs to know which one happened before deciding to retry.
	code := protocol.JobErrorFailed
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code = protocol.JobErrorTimeout
	}
	slog.Error("system-update failed", "step", res.FailureMode, "err", err)
	a.saveOutcome(job, res, version, true)
	a.jobs.finish(job.ID, res, code, err.Error())
}

// saveOutcome writes the update's outcome where it outlives the brain the
// update replaced (#488). It is written before the job is closed, so a caller
// that sees the job end can already read the outcome.
//
// An update that changed nothing (the box already ran the target) is not an
// update, and leaves the last real outcome in place.
func (a *Agent) saveOutcome(job protocol.Job, res protocol.SystemUpdateResult, version string, failed bool) {
	if a.LastUpdate == nil || (!failed && !res.BrainChanged && !res.UIChanged) {
		return
	}
	o := protocol.ControlPlaneOutcome{
		ID:           job.ID,
		Outcome:      protocol.ControlPlaneOutcomeGood,
		Version:      res.ToVersion,
		From:         res.FromVersion,
		At:           a.jobs.now().UTC().Format(time.RFC3339),
		StartedAt:    job.StartedAt,
		BrainChanged: res.BrainChanged,
		UIChanged:    res.UIChanged,
		FailureMode:  res.FailureMode,
		Reverted:     res.Reverted,
		RevertError:  res.RevertError,
	}
	if o.Version == "" {
		o.Version = version
	}
	if failed {
		o.Outcome = protocol.ControlPlaneOutcomeFailed
	}
	if err := a.LastUpdate.Save(o); err != nil {
		slog.Error("system-update: could not record the outcome; the dashboard will not show it", "job_id", job.ID, "err", err)
	}
}

// ErrNothingToApply is what a TargetApplier returns when the update-target loop
// has no target that differs from what the box runs. The handler maps it to 409.
var ErrNothingToApply = errors.New("there is no update to apply")

// TargetApplier applies the target the update-target loop already fetched and
// checked, now rather than in the window (#488). Consumer-side; the provider is
// the loop (updatetarget.Loop.ApplyNow), adapted in cmd/host-agent-real. It
// returns the job id of the update it started.
type TargetApplier interface {
	ApplyTarget() (jobID string, err error)
}

// LastUpdateSaver keeps the outcome of the last control-plane update on disk
// (controlplane.LastUpdate). Nil means the outcome is not kept.
type LastUpdateSaver interface {
	Save(protocol.ControlPlaneOutcome) error
}

// applyUpdateTarget answers POST /v1/jobs/system-update-target. **No body**:
// the refs come from the loop, which checked them (digest-pinned, expected
// repositories). A route that took refs from the caller would let whoever can
// reach the brain pick the images; this one can only apply what the box's own
// source named.
func (a *Agent) applyUpdateTarget(w http.ResponseWriter, r *http.Request) {
	if a.TargetApplier == nil {
		writeErr(w, http.StatusNotImplemented, "update-unsupported", "this host-agent has no update target to apply")
		return
	}
	id, err := a.TargetApplier.ApplyTarget()
	switch {
	case errors.Is(err, errJobRunning):
		writeErr(w, http.StatusConflict, protocol.ErrCodeJobRunning, err.Error())
		return
	case errors.Is(err, ErrNothingToApply):
		writeErr(w, http.StatusConflict, protocol.ErrCodeNothingToApply, err.Error())
		return
	case errors.Is(err, errNoUpdater):
		writeErr(w, http.StatusNotImplemented, "update-unsupported", err.Error())
		return
	case err != nil:
		slog.Error("system-update-target: could not start the update", "err", err)
		writeErr(w, http.StatusInternalServerError, "update-failed", "could not start the update")
		return
	}
	job, ok := a.jobs.get(id)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "update-failed", "the update started but its job is unknown")
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// jobStatus answers GET /v1/jobs/{id}. 404 for an id this host-agent has never
// seen — including every id from before a host-agent restart, since the records
// are in memory.
func (a *Agent) jobStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := a.jobs.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown-job", "no such job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// IsJobRunning reports whether err is the job lock's refusal: another job is
// already in flight. Callers outside this package (the OS update applier, via
// cmd/host-agent-real) treat it as "wait", not as a failure.
func IsJobRunning(err error) bool { return errors.Is(err, errJobRunning) }

// StartJob runs fn as a job of the given kind under the one global job lock
// (the same lock system-update takes), bounded by maxDuration. It is how the
// OS update's install and switch run (#563): they are dangerous in the same way
// a control-plane update is, so they must never overlap one, or each other.
//
// The record has no Result: that field is the control-plane update's. A
// failure ends the job `failed` with the error text, and a run past
// maxDuration with the `job-timeout` code.
func (a *Agent) StartJob(kind string, maxDuration time.Duration, fn func(ctx context.Context) error) (protocol.Job, error) {
	job, err := a.jobs.start(kind)
	if err != nil {
		return protocol.Job{}, err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), maxDuration)
		defer cancel()
		err := fn(ctx)
		code, msg := "", ""
		if err != nil {
			code, msg = protocol.JobErrorFailed, err.Error()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code = protocol.JobErrorTimeout
			}
		}
		a.jobs.finishPlain(job.ID, code, msg)
	}()
	return job, nil
}

// finishPlain closes a job that carries no Result and releases the lock.
func (r *jobRegistry) finishPlain(id, code, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return
	}
	job.FinishedAt = r.now().UTC().Format(time.RFC3339)
	if code == "" {
		job.Status = protocol.JobStatusCompleted
	} else {
		job.Status = protocol.JobStatusFailed
		job.Error = &protocol.Error{Code: code, Message: message}
	}
	if r.running == id {
		r.running = ""
	}
}
