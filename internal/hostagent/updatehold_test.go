package hostagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// stubHolder records the hold calls and answers SetHold with keep.
type stubHolder struct {
	set     []time.Time
	cleared int
	keep    time.Time
}

func (h *stubHolder) SetHold(until time.Time) time.Time { h.set = append(h.set, until); return h.keep }
func (h *stubHolder) ClearHold()                        { h.cleared++ }

func holdRequest(mux *http.ServeMux, method, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, protocol.UpdateHoldPath, bytes.NewBufferString(body))
	mux.ServeHTTP(rec, req)
	return rec
}

// PUT sets the hold and answers the end the holder kept, which may be cut.
func TestUpdateHold_PutSetsAndAnswersTheKeptEnd(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	kept := time.Date(2026, 11, 7, 2, 55, 0, 0, time.UTC)
	h := &stubHolder{keep: kept}
	a.UpdateHold = h

	rec := holdRequest(mux, http.MethodPut, `{"until":"2026-11-07T03:30:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	var got protocol.UpdateHold
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Until != "2026-11-07T02:55:00Z" {
		t.Fatalf("until = %q, want the kept end", got.Until)
	}
	if len(h.set) != 1 || !h.set[0].Equal(time.Date(2026, 11, 7, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("SetHold calls = %v, want the asked end", h.set)
	}

	// A hold that is not live after the cuts answers an empty until.
	h.keep = time.Time{}
	rec = holdRequest(mux, http.MethodPut, `{"until":"2026-11-07T03:30:00Z"}`)
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil || rec.Code != http.StatusOK || got.Until != "" {
		t.Fatalf("not live: status %d, until %q, err %v; want 200 and an empty until", rec.Code, got.Until, err)
	}
}

func TestUpdateHold_BadUntil_400(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	h := &stubHolder{}
	a.UpdateHold = h
	for _, body := range []string{`{"until":"tonight"}`, `{}`, `not json`} {
		if rec := holdRequest(mux, http.MethodPut, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
	if len(h.set) != 0 {
		t.Fatalf("a bad request reached the holder: %v", h.set)
	}
}

func TestUpdateHold_Delete_204(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	h := &stubHolder{}
	a.UpdateHold = h
	if rec := holdRequest(mux, http.MethodDelete, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	if h.cleared != 1 {
		t.Fatalf("ClearHold calls = %d, want 1", h.cleared)
	}
}

// A host-agent with no update loop has no switch to hold.
func TestUpdateHold_NoHolder_501(t *testing.T) {
	_, mux := newTestAgent(&stubVerifier{})
	if rec := holdRequest(mux, http.MethodPut, `{"until":"2026-11-07T03:30:00Z"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("PUT: status %d, want 501", rec.Code)
	}
	if rec := holdRequest(mux, http.MethodDelete, ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("DELETE: status %d, want 501", rec.Code)
	}
}

// An Update now whose only work is an OS switch that waits for the hold is
// accepted with no job: the caller follows the read.
func TestApplyTarget_SwitchWaitingForTheHold_202WithNoJob(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	a.TargetApplier = targetApplierFunc(func() (string, error) { return "", nil })
	rec := postApply(mux)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", rec.Code, rec.Body)
	}
	var job protocol.Job
	if err := json.NewDecoder(rec.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	if job.ID != "" || job.Kind != protocol.JobKindOSSwitch {
		t.Fatalf("job = %+v, want an os-switch with no id", job)
	}
}

// JobRunning follows the job registry.
func TestJobRunning(t *testing.T) {
	u := newStubUpdater()
	a, mux := agentWithUpdater(u)
	if a.JobRunning("") || a.JobRunning("j_unknown") {
		t.Fatal("an unknown job reads as running")
	}
	job, err := a.StartUpdate("brain@sha256:a", "")
	if err != nil {
		t.Fatal(err)
	}
	<-u.started
	if !a.JobRunning(job.ID) {
		t.Fatal("a started job does not read as running")
	}
	close(u.release)
	waitForStatus(t, mux, job.ID)
	if a.JobRunning(job.ID) {
		t.Fatal("a finished job still reads as running")
	}
}
