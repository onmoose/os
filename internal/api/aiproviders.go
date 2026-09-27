package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/catalog"
)

// aiproviders.go serves the AI provider data the install setup page draws as
// tiles (INSTALL_SETUP.md # 4). The data comes from the synced catalog and is
// served as is, in the order the store authored. Any signed-in user can read
// it, like the catalog browse routes: it is the same public catalog data, and
// every user can install apps.

type aiProvidersOutput struct {
	Body struct {
		Providers []catalog.AIProvider `json:"providers"`
	}
}

func (s *Server) registerAIProviders(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-ai-providers", Method: "GET", Path: "/api/v1/ai-providers",
		Summary: "AI providers and their models, in display order",
	}, s.listAIProviders)
}

// listAIProviders answers 200 with an empty list when there is no data (no
// catalog synced yet, or a catalog without it), so the setup page can fall
// back to plain fields without treating it as an error.
func (s *Server) listAIProviders(ctx context.Context, _ *struct{}) (*aiProvidersOutput, error) {
	providers, err := s.catalog.AIProviders()
	if err != nil {
		return nil, huma.Error500InternalServerError("catalog read failed", err)
	}
	out := &aiProvidersOutput{}
	out.Body.Providers = providers
	if out.Body.Providers == nil {
		out.Body.Providers = []catalog.AIProvider{}
	}
	return out, nil
}

// aiProviderLogo and aiProviderLogoDark serve a provider's logo bytes, proxied
// from the asset origin and cached like an app icon (catalogIcon). Raw, not
// JSON, so they stay out of the OpenAPI surface.
func (s *Server) aiProviderLogo(w http.ResponseWriter, r *http.Request) {
	path, err := s.catalog.AIProviderLogoPath(r.PathValue("id"), false)
	if s.serveAsset(w, r, path, err) {
		http.ServeFile(w, r, path)
	}
}

func (s *Server) aiProviderLogoDark(w http.ResponseWriter, r *http.Request) {
	path, err := s.catalog.AIProviderLogoPath(r.PathValue("id"), true)
	if s.serveAsset(w, r, path, err) {
		http.ServeFile(w, r, path)
	}
}
