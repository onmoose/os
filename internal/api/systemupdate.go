package api

// The admin trigger for a control-plane update (UPDATES.md # 3, issue #381).
//
// The brain does not run the update — it cannot, because it is one of the two
// containers being replaced. host-agent runs it as a job (BRAIN_HOST_PROTOCOL.md
// # Pattern B); this endpoint starts that job and hands back its id.
//
// **The job id is host-agent's, not the brain's.** The brain has its own job
// registry (jobs.go) and deliberately does not wrap this in one: a brain-side
// record would die the moment the update recreates the brain, halfway through
// the operation it was tracking. Polling goes to host-agent, which stays up, so
// the status read still works after the brain has been replaced — as long as
// the caller kept the id.
//
// **The target is two explicit image refs.** There is no release-manifest poll
// and no cloud call: the box↔cloud credential is still undesigned (NEXT.md,
// Tier 1). The whole updater is testable and shippable without it.

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/protocol"
)

func (s *Server) registerSystemUpdate(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "start-system-update", Method: "POST", Path: "/api/v1/system/update",
		Summary:       "Move the control plane to the given brain/UI image pair (admin only)",
		DefaultStatus: 202,
	}, s.startSystemUpdate)
	huma.Register(api, huma.Operation{
		OperationID: "get-system-update", Method: "GET", Path: "/api/v1/system/update/{job_id}",
		Summary: "Status of a control-plane update job (admin only)",
	}, s.getSystemUpdate)
	huma.Register(api, huma.Operation{
		OperationID: "get-system-update-target", Method: "GET", Path: "/api/v1/system/update-target",
		Summary: "What this box could be running, and why it is not (admin only)",
	}, s.getSystemUpdateTarget)
}

// UpdateTargetOfferDTO is a target the box's source named: the version for
// display, and the two references that would be pulled.
//
// Read State first. On "current" and "available" this is a validated,
// digest-pinned pair. On "refused" it is the answer the box **rejected**, and
// it may well name a tag — naming one is a way to get refused. It is carried
// there so an operator can see what a broken source is serving; nothing acted
// on it.
type UpdateTargetOfferDTO struct {
	Version     string `json:"version,omitempty"`
	BrainImage  string `json:"brain_image"`
	UIImage     string `json:"ui_image"`
	PublishedAt string `json:"published_at,omitempty"`
}

// ControlPlanePairDTO is the brain + UI image pair the box is running, read
// from the box's own declaration.
type ControlPlanePairDTO struct {
	Brain string `json:"brain,omitempty"`
	UI    string `json:"ui,omitempty"`
}

// UpdateTargetDTO is the GET /api/v1/system/update-target body: what the box's
// update-target loop last decided (UPDATES.md # 8.4).
//
// State is the whole answer in one word, and the seven values stay apart on
// purpose:
//
//   - "current" — the target is what the box already runs. The healthy answer.
//   - "available" — a different control plane is on offer. On hosted it applies
//     itself in the window; on appliance it waits for an admin (# 8.2).
//   - "none" — the source has nothing to offer. Normal, not a failure: most
//     boxes are in this state.
//   - "unreachable" — the box could not complete a check. It keeps running what
//     it runs; it never degrades because it could not ask.
//   - "refused" — the source answered and the box rejected the answer (a tag,
//     the wrong repository, half an answer). Nothing was pulled. A source stuck
//     on a bad answer is a fleet problem; a source that is down is not.
//   - "disabled" — this box has no update loop at all, because its configured
//     target is unusable. The only state here that needs a human.
//   - "unknown" — nothing has been measured yet. Never "you are up to date".
//
// Detail is a diagnostic, not UI copy: it carries the box's underlying error
// text, which is what makes a broken source fixable. The dashboard writes its
// own sentence from State and shows Detail as the technical reason underneath.
//
// From and WindowFrom are two different settings whose values only look alike.
// From is where the box's update-target URL came from ("seed", "env",
// "default"), and is empty on an appliance, which has no such URL. WindowFrom
// is where the update window came from ("answer", "env", "default") — a window
// can come from the source's answer, a URL never can.
type UpdateTargetDTO struct {
	// The enum tag is what makes the generated client a union of the seven
	// values rather than a bare string, so a dashboard that forgets one of them
	// fails to typecheck instead of falling through at runtime.
	State      string                `json:"state" enum:"current,available,none,unreachable,refused,disabled,unknown"`
	Running    ControlPlanePairDTO   `json:"running"`
	Target     *UpdateTargetOfferDTO `json:"target,omitempty"`
	CheckedAt  string                `json:"checked_at,omitempty"`
	Detail     string                `json:"detail,omitempty"`
	From       string                `json:"from,omitempty"`
	Window     string                `json:"window,omitempty"`
	WindowFrom string                `json:"window_from,omitempty"`
	AutoApply  bool                  `json:"auto_apply"`
	Profile    string                `json:"profile,omitempty"`
}

// getSystemUpdateTarget reports what the box could be running. A pure read, so
// it does not audit, and admin-only for the same reason the update trigger is:
// what a box is being moved to is admin business.
//
// A host-agent that cannot be reached is a 502. That is not the posture of
// systemVersion, which degrades to a partial answer, because there is no
// partial answer here — every field comes from the host, and an empty payload
// would render as "nothing to offer", which is a different and much calmer
// claim than "we could not ask".
func (s *Server) getSystemUpdateTarget(ctx context.Context, _ *struct{}) (*struct {
	Body UpdateTargetDTO
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	t, err := s.host.SystemUpdateTarget(ctx)
	if err != nil {
		slog.Error("system-update: host update-target read failed", "err", err)
		return nil, huma.Error502BadGateway("could not read what this box could be running")
	}
	out := UpdateTargetDTO{
		State:      t.State,
		Running:    ControlPlanePairDTO{Brain: t.Running.Brain, UI: t.Running.UI},
		CheckedAt:  t.CheckedAt,
		Detail:     t.Detail,
		From:       t.From,
		Window:     t.Window,
		WindowFrom: t.WindowFrom,
		AutoApply:  t.AutoApply,
		Profile:    t.Profile,
	}
	if t.Target != nil {
		out.Target = &UpdateTargetOfferDTO{
			Version:     t.Target.Version,
			BrainImage:  t.Target.BrainImage,
			UIImage:     t.Target.UIImage,
			PublishedAt: t.Target.PublishedAt,
		}
	}
	return &struct{ Body UpdateTargetDTO }{Body: out}, nil
}

// SystemUpdateRequestDTO is the POST body: the target pair. An empty ref means
// "leave that component alone", which is how a UI-only or brain-only ship is
// expressed. Both empty is refused.
type SystemUpdateRequestDTO struct {
	BrainImage string `json:"brain_image,omitempty"`
	UIImage    string `json:"ui_image,omitempty"`
}

// SystemUpdateResultDTO is what a finished job did. Present on failure too:
// "we reverted, and here is what broke" is what the admin needs to see
// (UPDATES.md # 3 step 4).
type SystemUpdateResultDTO struct {
	BrainChanged bool   `json:"brain_changed"`
	UIChanged    bool   `json:"ui_changed"`
	Reverted     bool   `json:"reverted"`
	FailureMode  string `json:"failure_mode,omitempty"`
	RevertError  string `json:"revert_error,omitempty"`
}

// SystemUpdateErrorDTO is the failure code and message of a failed job.
type SystemUpdateErrorDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SystemUpdateJobDTO is one host-agent job as the dashboard sees it. Status is
// running, completed, or failed.
type SystemUpdateJobDTO struct {
	JobID      string                 `json:"job_id"`
	Kind       string                 `json:"kind"`
	Status     string                 `json:"status"`
	StartedAt  string                 `json:"started_at"`
	FinishedAt string                 `json:"finished_at,omitempty"`
	Error      *SystemUpdateErrorDTO  `json:"error,omitempty"`
	Result     *SystemUpdateResultDTO `json:"result,omitempty"`
}

// startSystemUpdate starts the update job. Elevation-class per CLAUDE.md, so it
// audits the start **and** the refusals that carry information about who tried
// what: the 403, the 409, and a host failure. A member trying to update the box
// is exactly the kind of thing the Activity view exists to show. The two
// validation 422s do not audit — CLAUDE.md exempts them.
//
// success=true here means "the update started", not "the update worked". The
// brain cannot audit the outcome, because the brain is what the update
// replaces; the job record on host-agent is where the outcome lives.
func (s *Server) startSystemUpdate(ctx context.Context, in *struct {
	Body SystemUpdateRequestDTO
}) (*struct{ Body SystemUpdateJobDTO }, error) {
	brainRef := strings.TrimSpace(in.Body.BrainImage)
	uiRef := strings.TrimSpace(in.Body.UIImage)
	tgt := audit.Target{Kind: "system", ID: "control-plane"}
	meta := map[string]any{"brain_image": brainRef, "ui_image": uiRef}
	fail := func() { s.auditor.Record(ctx, audit.ActionSystemUpdate, tgt, meta, false) }

	if err := requireAdmin(ctx); err != nil {
		fail()
		return nil, err
	}
	// The two 422s below do not audit. CLAUDE.md # Go code discipline:
	// "Pure reads and validation 422s don't audit." A malformed ref from an
	// admin who is already allowed to do this is not the question the audit
	// trail answers ("did someone unauthorized try to mutate this box").
	if brainRef == "" && uiRef == "" {
		return nil, huma.Error422UnprocessableEntity("brain_image or ui_image is required")
	}
	// Refs travel into a compose file and onto a `docker pull` argument list.
	// The arguments are exec'd without a shell and the compose rewrite verifies
	// what it wrote, so this is not the only guard — but a ref with a newline in
	// it is a mistake in every case, and refusing it here keeps the mistake out
	// of the box's declaration entirely.
	for _, ref := range []string{brainRef, uiRef} {
		if strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			return nil, huma.Error422UnprocessableEntity("image refs may not contain whitespace or control characters")
		}
	}

	job, err := s.host.StartSystemUpdate(ctx, brainRef, uiRef)
	if err != nil {
		fail()
		if errors.Is(err, hostclient.ErrUpdateInProgress) {
			return nil, huma.Error409Conflict("a control-plane update is already running")
		}
		slog.Error("system-update: host refused the job", "image", brainRef, "err", err)
		return nil, huma.Error502BadGateway("could not start the update")
	}
	s.auditor.Record(ctx, audit.ActionSystemUpdate, tgt, meta, true)
	slog.Info("system-update started", "image", brainRef, "step", "accepted")
	return &struct{ Body SystemUpdateJobDTO }{Body: toSystemUpdateJobDTO(job)}, nil
}

// getSystemUpdate polls the job. A pure read, so it does not audit. Admin-only,
// same as the start: what a box is being moved to is admin business.
func (s *Server) getSystemUpdate(ctx context.Context, in *struct {
	JobID string `path:"job_id"`
}) (*struct{ Body SystemUpdateJobDTO }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	job, err := s.host.Job(ctx, in.JobID)
	if err != nil {
		if errors.Is(err, hostclient.ErrJobNotFound) {
			return nil, huma.Error404NotFound("no such update job")
		}
		slog.Error("system-update: host job read failed", "err", err)
		return nil, huma.Error502BadGateway("could not read the update job")
	}
	return &struct{ Body SystemUpdateJobDTO }{Body: toSystemUpdateJobDTO(job)}, nil
}

func toSystemUpdateJobDTO(j protocol.Job) SystemUpdateJobDTO {
	out := SystemUpdateJobDTO{
		JobID:      j.ID,
		Kind:       j.Kind,
		Status:     j.Status,
		StartedAt:  j.StartedAt,
		FinishedAt: j.FinishedAt,
	}
	if j.Error != nil {
		out.Error = &SystemUpdateErrorDTO{Code: j.Error.Code, Message: j.Error.Message}
	}
	if j.Result != nil {
		out.Result = &SystemUpdateResultDTO{
			BrainChanged: j.Result.BrainChanged,
			UIChanged:    j.Result.UIChanged,
			Reverted:     j.Result.Reverted,
			FailureMode:  j.Result.FailureMode,
			RevertError:  j.Result.RevertError,
		}
	}
	return out
}
