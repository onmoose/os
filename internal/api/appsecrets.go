package api

// Owner-visible app secrets (#152, SERVICE_PROVISIONING.md # Env-var injection).
// A self-authenticating app declares a bootstrap secret `show: true` in its
// manifest; the owner reads the generated value here to finish first sign-in,
// instead of the manifest shipping a published constant. The read follows the
// app's control authorization (admins for any app, the owner for their own
// personal app) — the same authorizeAppMutation gate as stop/start — so a
// member can't read another user's secret and a household app stays admin-only.
// Reveal is a pure read, so it does not audit (only elevation-class mutations do).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerAppSecrets(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-app-secrets", Method: "GET", Path: "/api/v1/apps/{id}/secrets",
		Summary: "Reveal an app instance's owner-visible setup secrets (owner or admin)",
	}, s.appSecrets)
}

// AppSecretDTO is one revealed secret: the manifest's snake_case name and the
// generated value. The injected env var is MOOSE_SECRET_<NAME> (uppercased).
type AppSecretDTO struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// AppSecretsDTO carries only the owner-visible secrets; internal ones are never
// listed. Empty when the app declares none, so the UI hides the section.
type AppSecretsDTO struct {
	Secrets []AppSecretDTO `json:"secrets"`
}

func (s *Server) appSecrets(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body AppSecretsDTO }, error) {
	if _, err := s.authorizeAppMutation(ctx, in.ID); err != nil {
		return nil, err
	}
	secrets, err := s.life.RevealSecrets(in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("reveal secrets failed", err)
	}
	out := &struct{ Body AppSecretsDTO }{}
	for _, sec := range secrets {
		out.Body.Secrets = append(out.Body.Secrets, AppSecretDTO{Name: sec.Name, Value: sec.Value})
	}
	return out, nil
}
