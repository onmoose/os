package api

// User-supplied app configuration (APP_MANIFEST.md # D4). A manifest config:
// field declares a value only the user can provide (an API token, a connection
// string, a model selector); the brain injects it under the app's own env-var
// name. GET reads the form schema + current state (secret values masked to a
// "set" flag, never returned); PUT applies a partial update, rewrites the
// override, and restarts the app. Both are owner-or-admin gated (same
// authorizeAppMutation as stop/start and the secret reveal). The mutation is
// elevation-class, so PUT audits success and failure.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

func (s *Server) registerAppConfig(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-app-config", Method: "GET", Path: "/api/v1/apps/{id}/config",
		Summary: "Read an app instance's user-supplied configuration (owner or admin)",
	}, s.getAppConfig)
	huma.Register(api, huma.Operation{
		OperationID: "update-app-config", Method: "PUT", Path: "/api/v1/apps/{id}/config",
		Summary: "Update an app instance's user-supplied configuration (owner or admin)",
	}, s.updateAppConfig)
}

// AppConfigFieldDTO is one config field's form schema plus its current state.
// For a secret field Value is always empty and Set reports whether a value is
// stored — the value itself is never returned. For a non-secret field that has
// never been set, Value pre-fills the manifest default and Set is false.
type AppConfigFieldDTO struct {
	AppEnv      string   `json:"app_env"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Secret      bool     `json:"secret"`
	Required    bool     `json:"required"`
	Type        string   `json:"type"`
	Options     []string `json:"options,omitempty"`
	Default     string   `json:"default,omitempty"`
	Value       string   `json:"value"`
	Set         bool     `json:"set"`
}

// AppConfigDTO is the config editor's view: the field list in manifest order.
// Empty when the app declares no config: block (the UI hides the section).
type AppConfigDTO struct {
	Fields []AppConfigFieldDTO `json:"fields"`
}

func (s *Server) getAppConfig(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body AppConfigDTO }, error) {
	if _, err := s.authorizeAppMutation(ctx, in.ID); err != nil {
		return nil, err
	}
	man, err := s.life.InstanceManifest(in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("load manifest failed", err)
	}
	stored, err := s.store.GetInstanceConfig(in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("read config failed", err)
	}
	valueByEnv := make(map[string]string, len(stored))
	setByEnv := make(map[string]bool, len(stored))
	for _, c := range stored {
		valueByEnv[c.AppEnv] = c.Value
		setByEnv[c.AppEnv] = true
	}
	out := &struct{ Body AppConfigDTO }{}
	out.Body.Fields = make([]AppConfigFieldDTO, 0, len(man.Config))
	for _, f := range man.Config {
		dto := AppConfigFieldDTO{
			AppEnv: f.AppEnv, Title: f.Title, Description: f.Description,
			Secret: f.Secret, Required: f.Required, Type: f.Type,
			Options: f.Options, Default: f.Default, Set: setByEnv[f.AppEnv],
		}
		// A secret value is never returned — only whether one is set. A non-secret
		// field shows its stored value, or the manifest default if never set.
		if !f.Secret {
			if setByEnv[f.AppEnv] {
				dto.Value = valueByEnv[f.AppEnv]
			} else {
				dto.Value = f.Default
			}
		}
		out.Body.Fields = append(out.Body.Fields, dto)
	}
	return out, nil
}

func (s *Server) updateAppConfig(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Fields map[string]string `json:"fields"`
	}
}) (*struct{ Body Job }, error) {
	id := in.ID
	if _, err := s.authorizeAppMutation(ctx, id); err != nil {
		return nil, err
	}
	tgt := audit.Target{Kind: "app", ID: id}
	man, err := s.life.InstanceManifest(id)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAppConfigUpdate, tgt, nil, false)
		return nil, huma.Error500InternalServerError("load manifest failed", err)
	}
	current, err := s.store.GetInstanceConfig(id)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAppConfigUpdate, tgt, nil, false)
		return nil, huma.Error500InternalServerError("read config failed", err)
	}
	resolved, err := resolvePutConfig(man, current, in.Body.Fields)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAppConfigUpdate, tgt, nil, false)
		return nil, err
	}
	jobCtx := ctx
	job := s.jobs.run("app-config-update", func(job *Job) (map[string]any, error) {
		job.setStep("updating_config")
		err := s.life.SetConfig(context.Background(), id, resolved)
		s.auditor.Record(jobCtx, audit.ActionAppConfigUpdate, tgt, nil, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// validateConfigValue checks a single user-supplied value against its field's
// type constraint (APP_MANIFEST.md # D4): an enum value must be one of the
// declared options, a bool must be "true" or "false". text is unconstrained.
func validateConfigValue(f manifest.ConfigField, value string) error {
	switch f.Type {
	case "enum":
		if !slices.Contains(f.Options, value) {
			return huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %s must be one of: %s", f.AppEnv, strings.Join(f.Options, ", ")))
		}
	case "bool":
		if value != "true" && value != "false" {
			return huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %s must be true or false", f.AppEnv))
		}
	}
	return nil
}

// configFieldsByEnv indexes a manifest's config fields and rejects any request
// key that names no declared field — shared by the install and PUT resolvers.
func configFieldsByEnv(man *manifest.Manifest, fields map[string]string) (map[string]manifest.ConfigField, error) {
	byEnv := make(map[string]manifest.ConfigField, len(man.Config))
	for _, f := range man.Config {
		byEnv[f.AppEnv] = f
	}
	for k := range fields {
		if _, ok := byEnv[k]; !ok {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %q is not a configurable value for this app", k))
		}
	}
	return byEnv, nil
}

// resolveInstallConfig validates the full set of install-time config answers
// against the manifest and returns the values to persist+inject (APP_MANIFEST.md
// # D4). A required field must be present and non-empty; an optional field left
// blank is omitted (injects nothing — the app keeps its own default).
func resolveInstallConfig(man *manifest.Manifest, fields map[string]string) ([]store.InstanceConfig, error) {
	if _, err := configFieldsByEnv(man, fields); err != nil {
		return nil, err
	}
	var out []store.InstanceConfig
	for _, f := range man.Config {
		v := fields[f.AppEnv]
		if v == "" {
			if f.Required {
				return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %s is required", f.AppEnv))
			}
			continue
		}
		if err := validateConfigValue(f, v); err != nil {
			return nil, err
		}
		out = append(out, store.InstanceConfig{AppEnv: f.AppEnv, Value: v, Secret: f.Secret})
	}
	return out, nil
}

// resolvePutConfig applies a partial post-install update to the current stored
// values and returns the full resolved set (APP_MANIFEST.md # D4). An absent key
// keeps the current value; a non-empty value sets it; an explicit "" clears it
// (optional fields only). required is validated against the resulting state, so
// an already-stored value satisfies it and a required secret can be replaced but
// never blanked.
func resolvePutConfig(man *manifest.Manifest, current []store.InstanceConfig, fields map[string]string) ([]store.InstanceConfig, error) {
	byEnv, err := configFieldsByEnv(man, fields)
	if err != nil {
		return nil, err
	}
	curVal := make(map[string]string, len(current))
	for _, c := range current {
		curVal[c.AppEnv] = c.Value
	}
	for appEnv, v := range fields {
		f := byEnv[appEnv]
		if v == "" {
			if f.Required {
				return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %s is required and cannot be cleared", appEnv))
			}
			delete(curVal, appEnv)
			continue
		}
		if err := validateConfigValue(f, v); err != nil {
			return nil, err
		}
		curVal[appEnv] = v
	}
	var out []store.InstanceConfig
	for _, f := range man.Config {
		v, ok := curVal[f.AppEnv]
		if !ok {
			if f.Required {
				return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %s is required", f.AppEnv))
			}
			continue
		}
		out = append(out, store.InstanceConfig{AppEnv: f.AppEnv, Value: v, Secret: f.Secret})
	}
	return out, nil
}
