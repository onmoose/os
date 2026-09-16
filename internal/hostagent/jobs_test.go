package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// stubUpdater is a SystemUpdater a test drives by hand: it records the refs it
// was called with, blocks until release is closed (so the "already running"
// window is deterministic, not timing-dependent), and then returns whatever the
// test set.
type stubUpdater struct {
	release chan struct{}
	// started is closed on the first call, so a test can wait for the run to be
	// in flight rather than sleeping.
	started   chan struct{}
	startOnce sync.Once

	mu              sync.Mutex
	calls           int
	brainRef, uiRef string
	// ctxErr records the run context's state at return time — the evidence for
	// "the run does not inherit the request context".
	ctxErr error

	res protocol.SystemUpdateResult
	err error
}

func newStubUpdater() *stubUpdater {
	return &stubUpdater{release: make(chan struct{}), started: make(chan struct{})}
}

func (s *stubUpdater) Update(ctx context.Context, brainRef, uiRef string) (protocol.SystemUpdateResult, error) {
	s.mu.Lock()
	s.calls++
	s.brainRef, s.uiRef = brainRef, uiRef
	s.mu.Unlock()
	s.startOnce.Do(func() { close(s.started) })
	// Honour the context the way a real docker-driven transaction does: a run
	// whose context dies must stop. A fake that ignored ctx would let the
	// MaxDuration test pass with the bound removed.
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	s.mu.Lock()
	s.ctxErr = ctx.Err()
	s.mu.Unlock()
	return s.res, s.err
}

// seen reports the refs of the most recent call, the call count, and the run
// context's state at return.
func (s *stubUpdater) seen() (brainRef, uiRef string, calls int, ctxErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.brainRef, s.uiRef, s.calls, s.ctxErr
}

func agentWithUpdater(u SystemUpdater) (*Agent, *http.ServeMux) {
	a := New(nil, NewFakePublisher(".local"))
	a.Updater = u
	mux := http.NewServeMux()
	a.Mount(mux)
	return a, mux
}

func postUpdate(mux *http.ServeMux, req protocol.SystemUpdateRequest) *httptest.ResponseRecorder {
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/jobs/system-update", bytes.NewReader(body)))
	return rec
}

func getJob(t *testing.T, mux *http.ServeMux, id string) (protocol.Job, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs/"+id, nil))
	var job protocol.Job
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	return job, rec.Code
}

// waitForStatus polls the job until it leaves "running". The run itself is a
// goroutine, so there is no synchronous moment to assert on.
func waitForStatus(t *testing.T, mux *http.ServeMux, id string) protocol.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, code := getJob(t, mux, id)
		if code != http.StatusOK {
			t.Fatalf("GET /v1/jobs/%s: want 200, got %d", id, code)
		}
		if job.Status != protocol.JobStatusRunning {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job %s never left running", id)
	return protocol.Job{}
}

// TestSystemUpdate_NoUpdater_501: the fake binary wires no updater. The route
// must say so rather than reporting a success the box never had — the same
// degrade as journal_follow.
func TestSystemUpdate_NoUpdater_501(t *testing.T) {
	_, mux := agentWithUpdater(nil)
	rec := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v2"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestSystemUpdate_NoRefs_400: an update with no target is a caller bug, not an
// empty no-op run.
func TestSystemUpdate_NoRefs_400(t *testing.T) {
	u := newStubUpdater()
	_, mux := agentWithUpdater(u)
	rec := postUpdate(mux, protocol.SystemUpdateRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body)
	}
	select {
	case <-u.started:
		t.Fatal("refused request still started an update")
	default:
	}
}

// TestSystemUpdate_AcceptsAndCompletes: 202 with a job id, the refs reach the
// updater unchanged, and the finished record carries the result.
func TestSystemUpdate_AcceptsAndCompletes(t *testing.T) {
	u := newStubUpdater()
	u.res = protocol.SystemUpdateResult{BrainChanged: true, UIChanged: true}
	_, mux := agentWithUpdater(u)

	rec := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v2", UIImage: "ui:v2"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (%s)", rec.Code, rec.Body)
	}
	var accepted protocol.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}
	if accepted.ID == "" || accepted.Kind != protocol.JobKindSystemUpdate || accepted.Status != protocol.JobStatusRunning {
		t.Fatalf("202 body: got %+v", accepted)
	}

	<-u.started
	close(u.release)
	job := waitForStatus(t, mux, accepted.ID)
	if job.Status != protocol.JobStatusCompleted {
		t.Fatalf("want completed, got %+v", job)
	}
	if job.Result == nil || !job.Result.BrainChanged || !job.Result.UIChanged {
		t.Fatalf("result not carried: %+v", job.Result)
	}
	if job.FinishedAt == "" {
		t.Error("finished job has no finished_at")
	}
	brainRef, uiRef, calls, _ := u.seen()
	if brainRef != "brain:v2" || uiRef != "ui:v2" || calls != 1 {
		t.Errorf("updater saw %d call(s) with refs %q / %q; want 1 call on brain:v2 + ui:v2", calls, brainRef, uiRef)
	}
}

// TestSystemUpdate_Failure_ReportsMode: a failed transaction ends `failed`, and
// the record still carries what the transaction did — which step broke and that
// the box was put back. That pair is the whole point of the status read.
func TestSystemUpdate_Failure_ReportsMode(t *testing.T) {
	u := newStubUpdater()
	u.res = protocol.SystemUpdateResult{BrainChanged: true, Reverted: true, FailureMode: "health"}
	u.err = errors.New("brain health check: connection refused")
	_, mux := agentWithUpdater(u)

	rec := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v2"})
	var accepted protocol.Job
	_ = json.Unmarshal(rec.Body.Bytes(), &accepted)
	<-u.started
	close(u.release)

	job := waitForStatus(t, mux, accepted.ID)
	if job.Status != protocol.JobStatusFailed {
		t.Fatalf("want failed, got %+v", job)
	}
	if job.Error == nil || job.Error.Code != protocol.JobErrorFailed {
		t.Fatalf("want a job-failed error, got %+v", job.Error)
	}
	if job.Result == nil || job.Result.FailureMode != "health" || !job.Result.Reverted {
		t.Fatalf("failure detail lost: %+v", job.Result)
	}
}

// TestSystemUpdate_SecondIsRefused: the one global lock. Two overlapping
// updates would interleave two container recreates on the same box, which is
// the thing Dangerous: true exists to prevent. The second caller is refused
// with 409, not queued — an admin who clicks twice wants one update.
func TestSystemUpdate_SecondIsRefused(t *testing.T) {
	u := newStubUpdater()
	_, mux := agentWithUpdater(u)

	first := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v2"})
	if first.Code != http.StatusAccepted {
		t.Fatalf("first update: want 202, got %d", first.Code)
	}
	<-u.started // the run is genuinely in flight

	second := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v3"})
	if second.Code != http.StatusConflict {
		t.Fatalf("second update while one runs: want 409, got %d (%s)", second.Code, second.Body)
	}
	var e protocol.Error
	_ = json.Unmarshal(second.Body.Bytes(), &e)
	if e.Code != "job-running" {
		t.Errorf("409 code: got %q", e.Code)
	}
	ref, _, calls, _ := u.seen()
	if calls != 1 || ref != "brain:v2" {
		t.Errorf("refused update reached the updater: %d call(s), last ref %q", calls, ref)
	}
}

// TestSystemUpdate_LockReleasedAfterFinish: the lock is a lock, not a
// one-shot. A box that could never update twice would be worse than one that
// could not update at all — the second update is the fix for the first.
func TestSystemUpdate_LockReleasedAfterFinish(t *testing.T) {
	first := newStubUpdater()
	a, mux := agentWithUpdater(first)

	rec := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v2"})
	var accepted protocol.Job
	_ = json.Unmarshal(rec.Body.Bytes(), &accepted)
	<-first.started
	close(first.release)
	waitForStatus(t, mux, accepted.ID)

	second := newStubUpdater()
	close(second.release)
	a.Updater = second
	rec2 := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v3"})
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("update after the first finished: want 202, got %d (%s)", rec2.Code, rec2.Body)
	}
}

// TestSystemUpdate_MaxDuration: the MaxDuration bound (# Failure semantics A).
// A run that never returns must not hold the lock forever, and the job has to
// say *why* it ended — "it ran too long and we stopped it" is a different
// operator decision from "it broke".
func TestSystemUpdate_MaxDuration(t *testing.T) {
	prev := systemUpdateMaxDuration
	systemUpdateMaxDuration = 20 * time.Millisecond
	defer func() { systemUpdateMaxDuration = prev }()

	u := newStubUpdater() // never released: only the deadline can end this run
	u.err = context.DeadlineExceeded
	a, mux := agentWithUpdater(u)

	rec := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v2"})
	var accepted protocol.Job
	_ = json.Unmarshal(rec.Body.Bytes(), &accepted)

	job := waitForStatus(t, mux, accepted.ID)
	if job.Status != protocol.JobStatusFailed {
		t.Fatalf("want failed, got %+v", job)
	}
	if job.Error == nil || job.Error.Code != protocol.JobErrorTimeout {
		t.Fatalf("want a job-timeout error, got %+v", job.Error)
	}
	// And the lock is free again, so an admin can retry.
	u2 := newStubUpdater()
	close(u2.release)
	a.Updater = u2
	rec2 := postUpdate(mux, protocol.SystemUpdateRequest{BrainImage: "brain:v3"})
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("retry after a timed-out update: want 202, got %d", rec2.Code)
	}
	// Let the retry finish before the deferred restore of the shared bound —
	// otherwise the test writes it while the second run is still reading it.
	var retry protocol.Job
	_ = json.Unmarshal(rec2.Body.Bytes(), &retry)
	waitForStatus(t, mux, retry.ID)
}

// TestSystemUpdate_SurvivesClientHangup: the run gets its own context. If it
// inherited the request's, a brain that disconnected — or that was itself
// replaced by this very update — would abort a transaction already changing the
// box, mid-flight.
func TestSystemUpdate_SurvivesClientHangup(t *testing.T) {
	u := newStubUpdater()
	_, mux := agentWithUpdater(u)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(protocol.SystemUpdateRequest{BrainImage: "brain:v2"})
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/jobs/system-update", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var accepted protocol.Job
	_ = json.NewDecoder(resp.Body).Decode(&accepted)
	resp.Body.Close()

	<-u.started
	cancel() // the caller goes away while the update is in flight

	// Give a context inherited from the request time to trip.
	time.Sleep(20 * time.Millisecond)
	close(u.release)
	job := waitForStatus(t, mux, accepted.ID)
	if job.Status != protocol.JobStatusCompleted {
		t.Fatalf("client hangup ended the update: %+v", job)
	}
	if _, _, _, ctxErr := u.seen(); ctxErr != nil {
		t.Fatalf("run context died with the request: %v", ctxErr)
	}
}

// TestJobStatus_Unknown_404: an id this host-agent never issued — including
// every id from before a restart, since the records are in memory.
func TestJobStatus_Unknown_404(t *testing.T) {
	_, mux := agentWithUpdater(newStubUpdater())
	_, code := getJob(t, mux, "j_deadbeef")
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
}
