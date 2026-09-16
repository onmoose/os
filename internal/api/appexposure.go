package api

// Per-app access mode (ENVIRONMENT.md #306, hosted): an app is either
// "restricted" (owner-only, gated by the box forward-auth) or "public"
// (anonymous). This PUT is the backend for the dashboard's Only-me / Public
// toggle (#307). Hosted-only — the appliance has no public app subdomains, so the
// endpoint 404s there. Owner-or-admin gated (authorizeAppMutation, the same gate
// as stop/start/config) and elevation-class, so it audits success and failure.

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/moose/internal/audit"
	"github.com/onmoose/moose/internal/catalog"
	"github.com/onmoose/moose/internal/profile"
	"github.com/onmoose/moose/internal/store"
)

func (s *Server) registerAppExposure(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "set-app-exposure", Method: "PUT", Path: "/api/v1/apps/{id}/exposure",
		Summary: "Set an app's access mode — restricted (owner-only) or public (hosted; owner or admin)",
	}, s.setAppExposure)
}

func (s *Server) setAppExposure(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Exposure string `json:"exposure" enum:"restricted,public"`
	}
}) (*struct{ Body InstanceDTO }, error) {
	// Hosted-only: exposure is a public-subdomain concept the appliance doesn't
	// have. 404 rather than 403 so the capability simply doesn't exist off hosted.
	if s.profile != profile.Hosted {
		return nil, huma.Error404NotFound("per-app access mode is a hosted-only setting")
	}
	id := in.ID
	inst, err := s.authorizeAppMutation(ctx, id)
	if err != nil {
		return nil, err
	}
	// Defense-in-depth for the huma enum tag (which a direct handler call in tests
	// bypasses): the lifecycle/store also reject an out-of-range value.
	exposure := in.Body.Exposure
	if exposure != store.ExposurePublic && exposure != store.ExposureRestricted {
		return nil, huma.Error422UnprocessableEntity(`exposure must be "restricted" or "public"`)
	}
	tgt := audit.Target{Kind: "app", ID: id}
	if err := s.life.SetExposure(ctx, id, exposure); err != nil {
		s.auditor.Record(ctx, audit.ActionAppExposureSet, tgt, nil, false)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("no such app")
		}
		return nil, huma.Error500InternalServerError("set exposure failed", err)
	}
	s.auditor.Record(ctx, audit.ActionAppExposureSet, tgt, nil, true)

	// Echo the updated app so the dashboard reflects the new mode without a refetch.
	inst.Exposure = exposure
	owner, err := s.store.GetUser(inst.OwnerUserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error500InternalServerError("owner lookup failed", err)
	}
	var catEntry *catalog.Entry
	if e, err := s.catalog.Entry(inst.ManifestID); err == nil {
		catEntry = &e
	}
	dto := s.toDTO(inst, owner.Username, catEntry)
	// The access label is exposure + declared public paths together, so a toggle
	// response that carried only the exposure would let the dashboard render
	// "Only me" for an app with open paths until the next refetch.
	s.withPublicPaths(&dto)
	return &struct{ Body InstanceDTO }{Body: dto}, nil
}
