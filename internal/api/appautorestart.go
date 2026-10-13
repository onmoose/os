package api

// Per-app automatic restart (#598, HEALTH.md # Automatic restart). The brain
// restarts an app whose app-unresponsive issue stays raised. This PUT turns
// that off for one app, for an owner debugging it by hand, and back on. Both
// profiles. Owner-or-admin gated (authorizeAppMutation, the same gate as
// stop/start/exposure). It changes no access and no principal, so it is not
// audited; the restarts themselves are (app.restart.auto).

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/store"
)

func (s *Server) registerAppAutoRestart(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "set-app-auto-restart", Method: "PUT", Path: "/api/v1/apps/{id}/auto-restart",
		Summary: "Turn the automatic restart of an unresponsive app on or off (owner or admin)",
	}, s.setAppAutoRestart)
}

func (s *Server) setAppAutoRestart(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Enabled bool `json:"enabled"`
	}
}) (*struct{ Body InstanceDTO }, error) {
	inst, err := s.authorizeAppMutation(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetInstanceAutoRestart(in.ID, in.Body.Enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("no such app")
		}
		return nil, huma.Error500InternalServerError("could not save the automatic restart setting", err)
	}
	inst.AutoRestartOff = !in.Body.Enabled

	// Echo the updated app so the dashboard shows the new setting without a refetch.
	owner, err := s.store.GetUser(inst.OwnerUserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error500InternalServerError("owner lookup failed", err)
	}
	var catEntry *catalog.Entry
	if e, err := s.catalog.Entry(inst.ManifestID); err == nil {
		catEntry = &e
	}
	dto := s.toDTO(inst, owner.Username, catEntry)
	s.withPublicPaths(&dto)
	return &struct{ Body InstanceDTO }{Body: dto}, nil
}
