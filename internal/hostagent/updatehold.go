package hostagent

import (
	"net/http"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// UpdateHolder is a consumer-side interface for the brain's hold on the OS
// switch (PUT and DELETE /v1/system/update-hold, #632, UPDATES.md # 7
// Reboots). The provider is the update-target loop (updatetarget.Loop) in
// cmd/host-agent-real and a small in-memory hold in the fake binary.
//
// SetHold cuts until to the lease and the cap itself and returns the end it
// kept, or the zero time when the hold is not live after the cuts.
type UpdateHolder interface {
	SetHold(until time.Time) time.Time
	ClearHold()
}

// setUpdateHold answers PUT /v1/system/update-hold: the brain sets or renews
// its hold. 200 with the end host-agent kept ("" when the hold is not live),
// 400 for an until that is not RFC3339, 501 when no holder is wired (a box
// with no update loop: it has no OS switch to hold).
func (a *Agent) setUpdateHold(w http.ResponseWriter, r *http.Request) {
	var req protocol.UpdateHold
	if !decode(w, r, &req) {
		return
	}
	until, err := time.Parse(time.RFC3339, req.Until)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request", "until must be an RFC3339 time")
		return
	}
	if a.UpdateHold == nil {
		writeErr(w, http.StatusNotImplemented, "hold-unsupported", "this host-agent has no update loop to hold")
		return
	}
	out := protocol.UpdateHold{}
	if end := a.UpdateHold.SetHold(until); !end.IsZero() {
		out.Until = end.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// clearUpdateHold answers DELETE /v1/system/update-hold: the brain has no app
// work left tonight. 204, also when there was no hold. 501 when no holder is
// wired.
func (a *Agent) clearUpdateHold(w http.ResponseWriter, r *http.Request) {
	if a.UpdateHold == nil {
		writeErr(w, http.StatusNotImplemented, "hold-unsupported", "this host-agent has no update loop to hold")
		return
	}
	a.UpdateHold.ClearHold()
	w.WriteHeader(http.StatusNoContent)
}

// JobRunning reports whether the job with this id still runs. The
// update-target loop asks it to tell when stream B is done for the night.
func (a *Agent) JobRunning(id string) bool {
	if id == "" {
		return false
	}
	job, ok := a.jobs.get(id)
	return ok && job.Status == protocol.JobStatusRunning
}
