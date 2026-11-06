package api

// App updates (UPDATES.md # 4 and # 6, #631). POST .../update moves one app
// to the catalog's new version as a job, with a rollback when the new version
// fails. PUT .../auto-update turns the box's own updates of the app on or
// off; the nightly window reads it (#632, lifecycle/nightly.go). Both follow the app-control
// gate: the owner, or an admin for a household app.

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
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
}

func appUpdateDTO(i store.Instance) AppUpdateDTO {
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
	}
	if !i.UpdateFailure.At.IsZero() {
		at := i.UpdateFailure.At
		d.FailedAt = &at
	}
	return d
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
	jobCtx := ctx // for the audit inside the job goroutine
	job := s.jobs.run("app-update", func(job *Job) (map[string]any, error) {
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
	return &struct{ Body Job }{Body: job.snapshot()}, nil
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
	dto := s.toDTO(inst, owner.Username, catEntry)
	s.withPublicPaths(&dto)
	return &struct{ Body InstanceDTO }{Body: dto}, nil
}
