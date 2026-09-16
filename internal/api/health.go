package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/moose/internal/health"
)

// registerHealth wires GET /api/v1/health (HEALTH.md # Display). Admin-only
// in v1 — the dashboard's member-facing transparency variant is wired through
// the SSE event stream once that lands. For now, members get 403 rather than
// a half-built read surface.
func (s *Server) registerHealth(api huma.API) {
	if s.health == nil {
		return // tests that don't need health can skip wiring
	}
	huma.Register(api, huma.Operation{
		OperationID: "list-health-issues",
		Method:      "GET",
		Path:        "/api/v1/health",
		Summary:     "List active health issues",
	}, s.listHealthIssues)
}

type healthListResponse struct {
	Body struct {
		Issues []health.Issue `json:"issues"`
	}
}

func (s *Server) listHealthIssues(ctx context.Context, _ *struct{}) (*healthListResponse, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	out := &healthListResponse{}
	out.Body.Issues = s.health.List()
	return out, nil
}
