package api

// App updates (UPDATES.md # 4 and # 6, #631). POST .../update moves one app
// to the catalog's new version as a job, with a rollback when the new version
// fails. PUT .../auto-update turns the box's own updates of the app on or
// off; the nightly window reads it (#632, lifecycle/nightly.go). Both follow the app-control
// gate: the owner, or an admin for a household app.
//
// An update that asks for more access waits for a yes (#633). POST
// .../update/approve says yes and runs the update at once, POST
// .../update/decline says "Keep current version", and POST
// /me/update-prompt/dismiss records that the person closed the prompt in this
// session. The yes has a stricter gate than an update: only the owner for a
// personal app, and an admin for a household app (approverOK).

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// AppUpdateDTO is what the box knows about an app's update: the new version
// the catalog has and how it was classified, and the last update that did not
// go through. The tile badge and the Update button read it (UPDATES.md # 6).
type AppUpdateDTO struct {
	// AvailableVersion is the catalog's version when it differs from the
	// installed one. Empty when no update is known.
	AvailableVersion string `json:"available_version,omitempty"`
	// Status is the class of the update: available (the box can apply it),
	// needs_approval (it asks for more access, #633), service_major (it moves
	// a managed database to a new major version, #635), needs_newer_moose (a
	// manifest this moose cannot read) or blocked (Reason says why).
	Status string `json:"status,omitempty" enum:"available,needs_approval,service_major,needs_newer_moose,blocked"`
	// Reason is a plain sentence for any status but available.
	Reason string `json:"reason,omitempty"`
	// CanApply is true when POST /apps/{id}/update would start an update: an
	// available update and a running app.
	CanApply bool `json:"can_apply"`
	// The last update that did not go through: the version it tried, the step
	// that broke (pull, snapshot, start, health, refused, snapshot_restore,
	// interrupted), a plain message and when. Cleared by a good update.
	FailedVersion string     `json:"failed_version,omitempty"`
	FailedKind    string     `json:"failed_kind,omitempty"`
	FailedMessage string     `json:"failed_message,omitempty"`
	FailedAt      *time.Time `json:"failed_at,omitempty"`
	// FailedCount is how many updates to FailedVersion failed in a row
	// (#632). A good update and a person's Update set it back to 0.
	FailedCount int `json:"failed_count,omitempty"`
	// Paused is true when the nightly window stopped trying the available
	// version: it failed 3 times in a row (UPDATES.md # 6 Failure
	// signaling). The Update button still works and starts the count again.
	Paused bool `json:"paused"`
	// Wants is the access the new version asks for that the installed one
	// does not have (#633), for every status: the dashboard says each item
	// in the install screen's words. Empty when it asks for no more.
	Wants []AppAccessWant `json:"wants"`
	// Declined is true when the owner said "Keep current version" to this
	// version. The prompt does not ask about it again; the app's settings
	// page still offers it.
	Declined bool `json:"declined"`
	// CanApprove is true when the caller may allow the update now with POST
	// /apps/{id}/update/approve: it waits for an approval, the app is
	// running, and the caller is the owner of a personal app or an admin for
	// a household app. An admin looking at another person's app gets false.
	CanApprove bool `json:"can_approve"`
	// FailedOvernight is true when the nightly window made the update that
	// failed, so the tile can say "last night" (#634). FailedAcked is true
	// once a person closed the tile's banner about the failure.
	FailedOvernight bool `json:"failed_overnight"`
	FailedAcked     bool `json:"failed_acked"`
	// The last good update, while it can be rolled back by hand (#634):
	// the version the app ran before it, when, and whether the nightly
	// window made it. Empty when there is nothing to roll back to.
	UpdatedFrom      string     `json:"updated_from,omitempty"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	UpdatedOvernight bool       `json:"updated_overnight"`
	// RollbackUntil is when the rollback by hand ends: 7 days after the
	// update. CanRollback is true when the caller may roll the app back now
	// with POST /apps/{id}/rollback: there is a version to go back to inside
	// its 7 days, the app is running, stopped or failed, and the caller is
	// the owner, or an admin for a household app.
	RollbackUntil *time.Time `json:"rollback_until,omitempty"`
	CanRollback   bool       `json:"can_rollback"`
}

// AppAccessWant is one piece of access a new version asks for (#633).
// Device is set for kind device; folder, mode (read or write, what the new
// version gets), change (new: a folder it had no access to, write: read
// becomes write, whole: one folder inside it becomes the whole folder) and
// source (personal: the owner's own folder, shared: the household's shared
// folder) for kind folder; path for kind public_path, a path anyone may open
// with no sign-in.
type AppAccessWant struct {
	Kind   string `json:"kind" enum:"internet,lan,gpu,device,folder,public_path"`
	Device string `json:"device,omitempty"`
	Folder string `json:"folder,omitempty"`
	Mode   string `json:"mode,omitempty" enum:"read,write"`
	Change string `json:"change,omitempty" enum:"new,write,whole"`
	Source string `json:"source,omitempty" enum:"personal,shared"`
	Path   string `json:"path,omitempty"`
}

func accessWantDTOs(ws []store.AccessWant) []AppAccessWant {
	out := make([]AppAccessWant, 0, len(ws))
	for _, w := range ws {
		out = append(out, AppAccessWant(w))
	}
	return out
}

// approverOK is the gate for a yes to an update that asks for more access
// (UPDATES.md # Who gets prompted): a personal app is its owner's to allow,
// and an admin cannot allow it for them. A household app is an admin's.
func approverOK(actor auth.Identity, i store.Instance) bool {
	if i.Scope == store.ScopeHousehold {
		return actor.IsAdmin()
	}
	return actor.User.ID == i.OwnerUserID
}

func appUpdateDTO(actor auth.Identity, i store.Instance) AppUpdateDTO {
	d := AppUpdateDTO{
		AvailableVersion: i.Update.Version,
		Status:           i.Update.Status,
		Reason:           i.Update.Reason,
		CanApply:         i.Update.Status == store.UpdateAvailable && i.State == "running" && i.Version != manifest.CustomVersion,
		FailedVersion:    i.UpdateFailure.Version,
		FailedKind:       i.UpdateFailure.Kind,
		FailedMessage:    i.UpdateFailure.Message,
		FailedCount:      i.UpdateFailure.Count,
		Paused:           i.UpdatePaused(),
		Wants:            accessWantDTOs(i.Update.Wants),
		Declined:         i.UpdateDeclined != "" && i.UpdateDeclined == i.Update.Version,
		CanApprove:       i.Update.Status == store.UpdateNeedsApproval && i.State == "running" && approverOK(actor, i),
	}
	if !i.UpdateFailure.At.IsZero() {
		at := i.UpdateFailure.At
		d.FailedAt = &at
	}
	d.FailedOvernight, d.FailedAcked = i.UpdateFailure.ByWindow, i.UpdateFailure.Acked
	if until, ok := lifecycle.RollbackUntil(i, time.Now()); ok {
		at := i.LastUpdate.At
		d.UpdatedFrom, d.UpdatedAt, d.UpdatedOvernight = i.LastUpdate.From, &at, i.LastUpdate.ByWindow
		d.RollbackUntil = &until
		d.CanRollback = lifecycle.RollbackState(i.State) && controlOK(actor, i)
	}
	return d
}

// controlOK is the app-control gate (authorizeAppMutation) for a caller who
// can see the app: a household app is an admin's to control.
func controlOK(actor auth.Identity, i store.Instance) bool {
	return actor.IsAdmin() || (i.Scope != store.ScopeHousehold && actor.User.ID == i.OwnerUserID)
}

func (s *Server) registerAppUpdate(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "update-app", Method: "POST", Path: "/api/v1/apps/{id}/update",
		Summary:       "Update an app to the catalog's new version (job; owner or admin)",
		DefaultStatus: 202,
	}, s.updateApp)
	huma.Register(api, huma.Operation{
		OperationID: "set-app-auto-update", Method: "PUT", Path: "/api/v1/apps/{id}/auto-update",
		Summary: "Turn the automatic update of an app on or off (owner or admin)",
	}, s.setAppAutoUpdate)
	huma.Register(api, huma.Operation{
		OperationID: "approve-app-update", Method: "POST", Path: "/api/v1/apps/{id}/update/approve",
		Summary:       "Allow an update that asks for more access, and run it at once (job; the owner, or an admin for a household app)",
		DefaultStatus: 202,
	}, s.approveAppUpdate)
	huma.Register(api, huma.Operation{
		OperationID: "decline-app-update", Method: "POST", Path: "/api/v1/apps/{id}/update/decline",
		Summary:       "Keep the current version: the update prompt stops asking about this version (the owner, or an admin for a household app)",
		DefaultStatus: 204,
	}, s.declineAppUpdate)
	huma.Register(api, huma.Operation{
		OperationID: "rollback-app", Method: "POST", Path: "/api/v1/apps/{id}/rollback",
		Summary:       "Put an app back on the version it ran before its last update, with the data it had then, for 7 days after the update (job; owner or admin)",
		DefaultStatus: 202,
	}, s.rollbackApp)
	huma.Register(api, huma.Operation{
		OperationID: "acknowledge-app-update-failure", Method: "POST", Path: "/api/v1/apps/{id}/update/acknowledge",
		Summary:       "Close the tile's banner about the app's last failed update (owner or admin)",
		DefaultStatus: 204,
	}, s.acknowledgeAppUpdateFailure)
	huma.Register(api, huma.Operation{
		OperationID: "mark-updates-seen", Method: "POST", Path: "/api/v1/me/updates-seen",
		Summary:       "Record that the person saw the toast about the apps updated overnight, so it shows once",
		DefaultStatus: 204,
	}, s.markUpdatesSeen)
	huma.Register(api, huma.Operation{
		OperationID: "dismiss-update-prompt", Method: "POST", Path: "/api/v1/me/update-prompt/dismiss",
		Summary:       "Close the update prompt for this session; it comes back at the next sign-in",
		DefaultStatus: 204,
	}, s.dismissUpdatePrompt)
}

// UpdateVersionBody names the version the person saw in the prompt or on the
// app's page.
type UpdateVersionBody struct {
	Version string `json:"version" minLength:"1"`
}

func (s *Server) updateApp(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body Job }, error) {
	id := in.ID
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	inst, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get failed", err)
	}
	// Leak guard first, as for every app control: a member gets 404 for an
	// app they cannot see.
	if !canSee(actor, inst) {
		return nil, huma.Error404NotFound("no such app")
	}
	target := audit.Target{Kind: "app", ID: id}
	meta := map[string]any{"version": inst.Version, "new_version": inst.Update.Version}
	if !actor.IsAdmin() && inst.Scope == store.ScopeHousehold {
		s.auditor.Record(ctx, audit.ActionAppUpdate, target, meta, false)
		return nil, huma.Error403Forbidden("only an admin can update a household app")
	}
	refuse := func(reason string) error {
		meta["reason"] = reason
		s.auditor.Record(ctx, audit.ActionAppUpdate, target, meta, false)
		return huma.Error409Conflict(reason)
	}
	switch {
	case inst.Version == manifest.CustomVersion:
		return nil, refuse("This app was added by hand, so moose has no newer version of it.")
	case inst.State != "running":
		return nil, refuse("Start the app before you update it.")
	case inst.Update.Version == "" || inst.Update.Status == "":
		return nil, refuse("There is no update for this app.")
	case inst.Update.Status != store.UpdateAvailable:
		return nil, refuse(inst.Update.Reason)
	}
	job := s.runUpdateJob(ctx, id, target, meta)
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// runUpdateJob runs a person's Update as a job and audits its outcome as
// app.update. Both the Update button and an approval start it.
func (s *Server) runUpdateJob(ctx context.Context, id string, target audit.Target, meta map[string]any) *Job {
	jobCtx := ctx // for the audit inside the job goroutine
	return s.jobs.run("app-update", func(job *Job) (map[string]any, error) {
		// A person's Update starts the count of failures in a row again,
		// so a paused app can be tried (#632).
		err := s.life.UpdateByPerson(context.Background(), id, job.setStep)
		if row, gerr := s.store.Get(id); gerr == nil && err == nil {
			meta["new_version"] = row.Version
		}
		s.auditor.Record(jobCtx, audit.ActionAppUpdate, target, meta, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
}

// loadForApproval loads the app and applies the approver gate. A member
// asking about another person's personal app gets 404, as for every app
// control. Anyone else who is not the approver gets 403, and audit (when it
// is not nil) records the refusal.
func (s *Server) loadForApproval(ctx context.Context, id string, audit func(store.Instance, string)) (auth.Identity, store.Instance, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return actor, store.Instance{}, huma.Error401Unauthorized("unauthenticated")
	}
	inst, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return actor, inst, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return actor, inst, huma.Error500InternalServerError("get failed", err)
	}
	if !canSee(actor, inst) {
		return actor, inst, huma.Error404NotFound("no such app")
	}
	if !approverOK(actor, inst) {
		msg := "Only the app's owner can allow this update."
		if inst.Scope == store.ScopeHousehold {
			msg = "Only an admin can allow an update of a household app."
		}
		if audit != nil {
			audit(inst, msg)
		}
		return actor, inst, huma.Error403Forbidden(msg)
	}
	return actor, inst, nil
}

// approveAppUpdate is the owner's yes to an update that asks for more access
// (UPDATES.md # 6 The permission-expansion prompt). It stores the approval
// and runs the update at once as a job, whether the automatic update is on
// or off. It is elevation-class: the yes, every refusal and every failure
// are audited as app.update_approve, and the update itself as app.update.
func (s *Server) approveAppUpdate(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body UpdateVersionBody
}) (*struct{ Body Job }, error) {
	id := in.ID
	target := audit.Target{Kind: "app", ID: id}
	meta := map[string]any{"new_version": in.Body.Version}
	record := func(ok bool) { s.auditor.Record(ctx, audit.ActionAppUpdateApprove, target, meta, ok) }
	actor, inst, err := s.loadForApproval(ctx, id, func(inst store.Instance, reason string) {
		meta["version"], meta["reason"] = inst.Version, reason
		meta["wants"] = accessWantDTOs(inst.Update.Wants)
		record(false)
	})
	if err != nil {
		return nil, err
	}
	meta["version"] = inst.Version
	meta["wants"] = accessWantDTOs(inst.Update.Wants)
	wants, err := s.life.ApproveUpdate(ctx, id, in.Body.Version, actor.User.ID)
	if err != nil {
		var refusal *lifecycle.UpdateRefusal
		if errors.As(err, &refusal) {
			meta["reason"] = refusal.Error()
			record(false)
			return nil, huma.Error409Conflict(refusal.Error())
		}
		meta["reason"] = "internal error"
		record(false)
		return nil, huma.Error500InternalServerError("moose could not store the approval", err)
	}
	meta["wants"] = accessWantDTOs(wants)
	record(true)
	updMeta := map[string]any{"version": inst.Version, "new_version": in.Body.Version}
	job := s.runUpdateJob(ctx, id, target, updMeta)
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// declineAppUpdate is "Keep current version": the prompt does not ask about
// this version again, and the app stays as it is. The app's settings page
// still offers the update, and a new version that asks for more asks again.
// It grants nothing, so it is not audited.
func (s *Server) declineAppUpdate(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body UpdateVersionBody
}) (*struct{}, error) {
	_, inst, err := s.loadForApproval(ctx, in.ID, nil)
	if err != nil {
		return nil, err
	}
	if inst.Update.Status != store.UpdateNeedsApproval {
		return nil, huma.Error409Conflict(lifecycle.ErrNothingToApprove.Error())
	}
	if inst.Update.Version != in.Body.Version {
		return nil, huma.Error409Conflict(lifecycle.ErrApproveStale.Error())
	}
	if err := s.store.DeclineInstanceUpdate(in.ID, in.Body.Version); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("no such app")
		}
		return nil, huma.Error500InternalServerError("could not save the choice", err)
	}
	if s.bus != nil {
		s.bus.Publish(events.AppUpdateChanged, map[string]any{"instance_id": in.ID})
	}
	return &struct{}{}, nil
}

// dismissUpdatePrompt records that the person closed the update prompt
// without choosing. It is stored on the session, so it holds in every tab and
// the prompt comes back at the next sign-in (a new session), not at the next
// page load.
func (s *Server) dismissUpdatePrompt(ctx context.Context, _ *struct{}) (*struct{}, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	if err := s.store.DismissUpdatePrompt(actor.Session.Token); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error401Unauthorized("unauthenticated")
		}
		return nil, huma.Error500InternalServerError("could not save the choice", err)
	}
	return &struct{}{}, nil
}

// setAppAutoUpdate turns the box's own updates of one app on or off. Off
// means the box never updates the app by itself; the badge still shows and a
// person can still update it. Shaped like set-app-auto-restart, and not
// audited for the same reason: it changes no access and runs no new code by
// itself.
func (s *Server) setAppAutoUpdate(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Enabled bool `json:"enabled"`
	}
}) (*struct{ Body InstanceDTO }, error) {
	inst, err := s.authorizeAppMutation(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetInstanceAutoUpdate(in.ID, in.Body.Enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("no such app")
		}
		return nil, huma.Error500InternalServerError("could not save the automatic update setting", err)
	}
	inst.AutoUpdateOff = !in.Body.Enabled

	owner, err := s.store.GetUser(inst.OwnerUserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error500InternalServerError("owner lookup failed", err)
	}
	var catEntry *catalog.Entry
	if s.catalog != nil {
		if e, err := s.catalog.Entry(inst.ManifestID); err == nil {
			catEntry = &e
		}
	}
	dto := s.toDTO(ctx, inst, owner.Username, catEntry)
	s.withPublicPaths(&dto)
	return &struct{ Body InstanceDTO }{Body: dto}, nil
}

// rollbackApp is the rollback by hand (UPDATES.md # 4, # 6, #634). It puts
// the app back on the version it ran before its last update, on the data it
// had then, as a job. Data written since the update is lost: the dashboard
// says so before it calls this. It has the app-control gate (the owner, or an
// admin for a household app), and it is elevation-class: success, every
// failure and every refusal are audited as app.update_rollback.
func (s *Server) rollbackApp(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body Job }, error) {
	id := in.ID
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	inst, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get failed", err)
	}
	if !canSee(actor, inst) {
		return nil, huma.Error404NotFound("no such app")
	}
	target := audit.Target{Kind: "app", ID: id}
	meta := map[string]any{"version": inst.Version, "new_version": inst.LastUpdate.From}
	if !controlOK(actor, inst) {
		s.auditor.Record(ctx, audit.ActionAppUpdateRollback, target, meta, false)
		return nil, huma.Error403Forbidden("only an admin can roll back a household app")
	}
	refuse := func(reason string) error {
		meta["reason"] = reason
		s.auditor.Record(ctx, audit.ActionAppUpdateRollback, target, meta, false)
		return huma.Error409Conflict(reason)
	}
	if _, ok := lifecycle.RollbackUntil(inst, time.Now()); !ok {
		return nil, refuse("There is no earlier version of this app to go back to.")
	}
	if !lifecycle.RollbackState(inst.State) {
		return nil, refuse("The app is busy. Try again when it has finished.")
	}
	jobCtx := ctx // for the audit inside the job goroutine
	job := s.jobs.run("app-rollback", func(job *Job) (map[string]any, error) {
		err := s.life.Rollback(context.Background(), id, job.setStep)
		s.auditor.Record(jobCtx, audit.ActionAppUpdateRollback, target, meta, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// acknowledgeAppUpdateFailure closes the tile's banner about the last failed
// update (UPDATES.md # 6 Failure signaling: "persistent until acknowledged").
// The failure stays in Settings → Updates and on the app's page. It is a view
// choice that changes no access, so it is not audited.
func (s *Server) acknowledgeAppUpdateFailure(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	if _, err := s.authorizeAppMutation(ctx, in.ID); err != nil {
		return nil, err
	}
	if err := s.store.AckInstanceUpdateFailure(in.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("no such app")
		}
		return nil, huma.Error500InternalServerError("could not save the choice", err)
	}
	if s.bus != nil {
		s.bus.Publish(events.AppUpdateChanged, map[string]any{"instance_id": in.ID})
	}
	return &struct{}{}, nil
}

// markUpdatesSeen records that the person saw the toast about the apps the
// nightly window updated (UPDATES.md # 6 Post-update toast): the dashboard
// shows it once, for the updates after this time. Stored per person, so it
// shows once on any device.
func (s *Server) markUpdatesSeen(ctx context.Context, _ *struct{}) (*struct{}, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	if err := s.store.SetUpdatesSeen(actor.User.ID, time.Now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error401Unauthorized("unauthenticated")
		}
		return nil, huma.Error500InternalServerError("could not save the choice", err)
	}
	return &struct{}{}, nil
}
