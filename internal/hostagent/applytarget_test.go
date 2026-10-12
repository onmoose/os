package hostagent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

// recordingSaver keeps every outcome the agent saved.
type recordingSaver struct {
	mu   sync.Mutex
	got  []protocol.ControlPlaneOutcome
	fail error
}

func (s *recordingSaver) Save(o protocol.ControlPlaneOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, o)
	return s.fail
}

func (s *recordingSaver) last(t *testing.T) protocol.ControlPlaneOutcome {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		t.Fatal("no outcome was saved")
	}
	return s.got[len(s.got)-1]
}

// targetApplierFunc lets a test answer ApplyTarget inline.
type targetApplierFunc func() (string, error)

func (f targetApplierFunc) ApplyTarget() (string, error) { return f() }

func postApply(mux *http.ServeMux) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", protocol.SystemUpdateTargetApplyPath, nil))
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e protocol.Error
	if err := json.NewDecoder(rec.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e.Code
}

// A finished update leaves its outcome on disk, under the job's id, before the
// job is closed: a caller that sees the job end can read the outcome (#488).
func TestSystemUpdate_SavesTheOutcome(t *testing.T) {
	u := newStubUpdater()
	u.res = protocol.SystemUpdateResult{BrainChanged: true, UIChanged: true, FromVersion: "0.17.0", ToVersion: "0.18.0"}
	a, mux := agentWithUpdater(u)
	s := &recordingSaver{}
	a.LastUpdate = s

	job, err := a.StartUpdateTo("brain@sha256:new", "ui@sha256:new", "0.18.0")
	if err != nil {
		t.Fatal(err)
	}
	close(u.release)
	waitForStatus(t, mux, job.ID)

	got := s.last(t)
	if got.ID != job.ID || got.Outcome != protocol.ControlPlaneOutcomeGood || got.Version != "0.18.0" || got.From != "0.17.0" ||
		got.StartedAt != job.StartedAt || got.At == "" || !got.BrainChanged || !got.UIChanged {
		t.Fatalf("outcome = %+v, want a good 0.17.0 -> 0.18.0 under job %s", got, job.ID)
	}
}

// A failed update is recorded as failed with its step, and the version falls
// back to the one the target named when the images could not say it.
func TestSystemUpdate_SavesAFailure(t *testing.T) {
	u := newStubUpdater()
	u.res = protocol.SystemUpdateResult{BrainChanged: true, FailureMode: "health", Reverted: true, FromVersion: "0.17.0"}
	u.err = errors.New("brain never answered")
	a, mux := agentWithUpdater(u)
	s := &recordingSaver{}
	a.LastUpdate = s

	job, _ := a.StartUpdateTo("brain@sha256:new", "", "0.18.0")
	close(u.release)
	waitForStatus(t, mux, job.ID)

	got := s.last(t)
	if got.Outcome != protocol.ControlPlaneOutcomeFailed || got.FailureMode != "health" || !got.Reverted || got.Version != "0.18.0" {
		t.Fatalf("outcome = %+v, want a reverted health failure on the way to 0.18.0", got)
	}
}

// An update that changed nothing is not an outcome worth showing, and must not
// replace the last real one.
func TestSystemUpdate_NoopSavesNothing(t *testing.T) {
	u := newStubUpdater()
	a, mux := agentWithUpdater(u)
	s := &recordingSaver{}
	a.LastUpdate = s
	job, _ := a.StartUpdate("brain@sha256:same", "")
	close(u.release)
	waitForStatus(t, mux, job.ID)
	if len(s.got) != 0 {
		t.Fatalf("a no-op saved %+v, want nothing", s.got)
	}
}

func TestApplyTarget_NoApplier_501(t *testing.T) {
	_, mux := agentWithUpdater(newStubUpdater())
	if rec := postApply(mux); rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
}

func TestApplyTarget_StartsAndReturnsTheJob(t *testing.T) {
	u := newStubUpdater()
	a, mux := agentWithUpdater(u)
	a.TargetApplier = targetApplierFunc(func() (string, error) {
		job, err := a.StartUpdateTo("brain@sha256:new", "ui@sha256:new", "0.18.0")
		return job.ID, err
	})
	rec := postApply(mux)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", rec.Code, rec.Body)
	}
	var job protocol.Job
	if err := json.NewDecoder(rec.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	if job.Kind != protocol.JobKindSystemUpdate || job.Status != protocol.JobStatusRunning {
		t.Fatalf("job = %+v, want a running system-update", job)
	}
	close(u.release)
	waitForStatus(t, mux, job.ID)
}

func TestApplyTarget_Refusals_409(t *testing.T) {
	u := newStubUpdater()
	a, mux := agentWithUpdater(u)

	a.TargetApplier = targetApplierFunc(func() (string, error) { return "", ErrNothingToApply })
	if rec := postApply(mux); rec.Code != http.StatusConflict || errCode(t, rec) != protocol.ErrCodeNothingToApply {
		t.Fatalf("nothing to apply: status %d, want 409 %s", rec.Code, protocol.ErrCodeNothingToApply)
	}

	// The lock is held by another job (here an update; an OS install takes
	// the same lock).
	first, err := a.StartUpdate("brain@sha256:a", "")
	if err != nil {
		t.Fatal(err)
	}
	<-u.started
	a.TargetApplier = targetApplierFunc(func() (string, error) {
		job, err := a.StartUpdate("brain@sha256:b", "")
		return job.ID, err
	})
	if rec := postApply(mux); rec.Code != http.StatusConflict || errCode(t, rec) != protocol.ErrCodeJobRunning {
		t.Fatalf("job running: status %d, want 409 %s", rec.Code, protocol.ErrCodeJobRunning)
	}
	close(u.release)
	waitForStatus(t, mux, first.ID)
}
