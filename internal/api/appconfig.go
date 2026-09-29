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
	"log/slog"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/lifecycle"
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
	// Role and Separator are as on InstallPlanConfigField.
	Role      string `json:"role,omitempty"`
	Separator string `json:"separator,omitempty"`
}

// AppConfigDTO is the config editor's view: the field list in manifest order.
// Empty when the app declares no config: block (the UI hides the section).
// Requires is as on InstallPlanDTO.
type AppConfigDTO struct {
	Fields   []AppConfigFieldDTO `json:"fields"`
	Requires []RequiresGroupDTO  `json:"requires,omitempty"`
	// AIBindings are the app's AI slots filled from an account, ordered by
	// slot. A slot with values but no binding (typed by hand) is not here.
	AIBindings []AppAIBindingDTO `json:"ai_bindings"`
	// NeedsSetup is true when a required field has no value or a requires
	// group is unmet. Missing says what, one plain sentence per item.
	NeedsSetup bool     `json:"needs_setup"`
	Missing    []string `json:"missing"`
}

// AppAIBindingDTO is one AI slot of an installed app and the account that
// fills it (INSTALL_SETUP.md piece 4).
type AppAIBindingDTO struct {
	// Slot is kind.protocol from the manifest, e.g. ai.anthropic.
	Slot      string `json:"slot"`
	AccountID string `json:"account_id"`
	// Mine is true when the account is the caller's. Another user's account
	// comes without its label, and the UI calls it "Someone else's account"
	// until the caller picks one of their own.
	Mine         bool   `json:"mine"`
	AccountLabel string `json:"account_label,omitempty"`
	// ProviderID is the account's provider, or openai_compatible.
	ProviderID string `json:"provider_id"`
	// Models are the model ids the app was given, by model setting
	// (model.chat, models.embedding).
	Models map[string][]string `json:"models,omitempty"`
}

// RequiresGroupDTO is one "at least one of" group the brain checks
// (INSTALL_SETUP.md # 3): members are a kind (ai), a slot (ai.anthropic) or a
// plain field's app_env. Only the groups the box can use are sent: a member
// that matches no field, and a group left empty, are dropped (manifest
// EffectiveRequires).
type RequiresGroupDTO struct {
	OneOf []string `json:"one_of"`
}

// requiresDTO projects the manifest's effective requires groups.
func requiresDTO(man *manifest.Manifest) []RequiresGroupDTO {
	var out []RequiresGroupDTO
	for _, g := range man.EffectiveRequires() {
		out = append(out, RequiresGroupDTO{OneOf: g})
	}
	return out
}

// fieldRole returns the role and separator a DTO shows for one field: the role
// only when the box can fill it, and the separator only on a models field.
func fieldRole(roles map[string]manifest.Role, f *manifest.ConfigField) (role, separator string) {
	r, ok := roles[f.AppEnv]
	if !ok {
		return "", ""
	}
	if r.Attribute == manifest.AttrModels {
		separator = f.EffectiveSeparator()
	}
	return r.String(), separator
}

func (s *Server) getAppConfig(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body AppConfigDTO }, error) {
	if _, err := s.authorizeAppMutation(ctx, in.ID); err != nil {
		return nil, err
	}
	caller, _ := auth.FromContext(ctx) // authorizeAppMutation already required it
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
	out.Body.Requires = requiresDTO(man)
	roles := man.FillableRoles()
	for _, f := range man.Config {
		dto := AppConfigFieldDTO{
			AppEnv: f.AppEnv, Title: f.Title, Description: f.Description,
			Secret: f.Secret, Required: f.Required, Type: f.Type,
			Options: f.Options, Default: f.Default, Set: setByEnv[f.AppEnv],
		}
		dto.Role, dto.Separator = fieldRole(roles, &f)
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
	out.Body.Missing = setupMissing(man, valueByEnv)
	out.Body.NeedsSetup = len(out.Body.Missing) > 0
	bindings, err := s.appAIBindings(caller, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("read ai bindings failed", err)
	}
	out.Body.AIBindings = bindings
	return out, nil
}

// appAIBindings projects an app's stored bindings for the caller. The label of
// another user's account is left out, so the answer does not show what other
// users call their accounts.
func (s *Server) appAIBindings(caller auth.Identity, instanceID string) ([]AppAIBindingDTO, error) {
	stored, err := s.store.ListInstanceAIBindings(instanceID)
	if err != nil {
		return nil, err
	}
	out := make([]AppAIBindingDTO, 0, len(stored))
	for _, b := range stored {
		acct, err := s.store.GetAIAccount(b.AccountID)
		if err != nil {
			// The foreign key keeps a binding's account alive, so a miss here
			// is a real read error.
			return nil, err
		}
		dto := AppAIBindingDTO{Slot: b.Slot, AccountID: b.AccountID, ProviderID: acct.ProviderID, Models: b.Models}
		if acct.OwnerUserID == caller.User.ID {
			dto.Mine = true
			dto.AccountLabel = acct.Label
		}
		out = append(out, dto)
	}
	return out, nil
}

// setupMissing lists what an installed app still needs, one plain sentence
// per item: each required field with no value, and each unmet requires group
// (INSTALL_SETUP.md piece 4, "needs setup"). The wording follows the install
// 422s. A group made only of AI kinds and slots is "an LLM provider", which is
// what the user reads on the app's page. values maps app_env to value.
func setupMissing(man *manifest.Manifest, values map[string]string) []string {
	out := []string{}
	for _, f := range man.Config {
		if f.Required && values[f.AppEnv] == "" {
			out = append(out, "Fill in "+f.Title+".")
		}
	}
	for _, g := range man.EffectiveRequires() {
		if man.GroupSatisfied(g, values) {
			continue
		}
		if isAIGroup(g) {
			out = append(out, "Pick at least one LLM provider.")
			continue
		}
		out = append(out, "Fill in at least one of: "+groupTitles(man, g)+".")
	}
	return out
}

// isAIGroup reports whether every member of a requires group is the ai kind
// or an ai slot, so the group reads as "an LLM provider".
func isAIGroup(group []string) bool {
	for _, m := range group {
		if m != "ai" && !strings.HasPrefix(m, "ai.") {
			return false
		}
	}
	return len(group) > 0
}

// instanceNeedsSetup is setupMissing for the app lists: true when the app's
// own manifest copy says something is missing from its stored values. An app
// whose manifest copy cannot be read is not flagged: the list must still
// render, as for withPublicPaths.
func (s *Server) instanceNeedsSetup(id string) bool {
	man, err := s.life.InstanceManifest(id)
	if err != nil || (len(man.Config) == 0 && len(man.Requires) == 0) {
		return false
	}
	stored, err := s.store.GetInstanceConfig(id)
	if err != nil {
		slog.Warn("read config for needs-setup failed", "instance_id", id, "err", err)
		return false
	}
	return len(setupMissing(man, configValues(stored))) > 0
}

// AppConfigUpdateBody is the PUT /apps/{id}/config request. Fields is a
// partial update of typed values. AIBindings changes AI slots in the install
// shape: a listed slot is replaced, a slot with an empty account_id is
// cleared, and a slot not listed is left alone (INSTALL_SETUP.md piece 4).
type AppConfigUpdateBody struct {
	Fields     map[string]string `json:"fields,omitempty"`
	AIBindings []AIBindingBody   `json:"ai_bindings,omitempty"`
}

func (s *Server) updateAppConfig(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body AppConfigUpdateBody
}) (*struct{ Body Job }, error) {
	id := in.ID
	if _, err := s.authorizeAppMutation(ctx, id); err != nil {
		return nil, err
	}
	caller, _ := auth.FromContext(ctx) // authorizeAppMutation already required it
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
	currentBindings, err := s.store.ListInstanceAIBindings(id)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAppConfigUpdate, tgt, nil, false)
		return nil, huma.Error500InternalServerError("read ai bindings failed", err)
	}
	// The provider data is read only when the edit carries bindings, as at
	// install, so a plain field edit never waits on the catalog.
	var providers []catalog.AIProvider
	if len(in.Body.AIBindings) > 0 {
		if providers, err = s.catalog.AIProviders(); err != nil {
			s.auditor.Record(ctx, audit.ActionAppConfigUpdate, tgt, nil, false)
			return nil, huma.Error500InternalServerError("catalog read failed", err)
		}
	}
	account := func(accountID string) (store.AIAccount, error) { return s.ownAIAccount(caller, accountID) }
	// Resolve now for the 422s the user can act on. The job resolves again
	// under the app's lock, from the values, bindings and account rows as they
	// are at commit time, so an account edited or deleted in between cannot
	// bring back an old key, and another save to the same app in between is
	// not undone.
	fields, bindings := in.Body.Fields, in.Body.AIBindings
	if _, err := resolvePutWithAI(man, current, currentBindings, fields, bindings, account, providers); err != nil {
		s.auditor.Record(ctx, audit.ActionAppConfigUpdate, tgt, nil, false)
		return nil, err
	}
	resolve := configResolver(fields, bindings, account, providers)
	jobCtx := ctx
	job := s.jobs.run("app-config-update", func(job *Job) (map[string]any, error) {
		job.setStep("updating_config")
		err := s.life.UpdateConfig(context.Background(), id, resolve)
		s.auditor.Record(jobCtx, audit.ActionAppConfigUpdate, tgt, nil, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// configResolver is the lifecycle.ConfigResolver of a config PUT. Lifecycle
// calls it under the app's lock with the app's values and bindings as they
// are then, and account reads the account rows then too. So the edit is
// applied to the current state, and an account edited or deleted since the
// request was checked cannot bring back an old key. A resolver error (for
// example the account was deleted) fails the job with its plain message.
func configResolver(fields map[string]string, bindings []AIBindingBody, account func(id string) (store.AIAccount, error), providers []catalog.AIProvider) lifecycle.ConfigResolver {
	return func(man *manifest.Manifest, current []store.InstanceConfig, bound []store.AIBinding) (lifecycle.ConfigChange, error) {
		res, err := resolvePutWithAI(man, current, bound, fields, bindings, account, providers)
		if err != nil {
			// A huma error's text is its plain message, so it reads well as
			// the job's error.
			return lifecycle.ConfigChange{}, err
		}
		return lifecycle.ConfigChange{Values: res.cfg, Slots: res.slots, Bindings: res.bindings}, nil
	}
}

// validateConfigValue checks a single user-supplied value against its field's
// type constraint (APP_MANIFEST.md # D4): an enum value must be one of the
// declared options, a bool must be "true" or "false". text is unconstrained.
func validateConfigValue(f manifest.ConfigField, value string) error {
	switch f.Type {
	case "enum":
		if !slices.Contains(f.Options, value) {
			return configError("config.fields."+f.AppEnv, fmt.Sprintf("config.fields: %s must be one of: %s", f.AppEnv, strings.Join(f.Options, ", ")))
		}
	case "bool":
		if value != "true" && value != "false" {
			return configError("config.fields."+f.AppEnv, fmt.Sprintf("config.fields: %s must be true or false", f.AppEnv))
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
			return nil, configError("config.fields."+k, fmt.Sprintf("config.fields: %q is not a configurable value for this app", k))
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
				return nil, configError("config.fields."+f.AppEnv, fmt.Sprintf("config.fields: %s is required", f.AppEnv))
			}
			continue
		}
		if err := validateConfigValue(f, v); err != nil {
			return nil, err
		}
		out = append(out, store.InstanceConfig{AppEnv: f.AppEnv, Value: v, Secret: f.Secret})
	}
	values := configValues(out)
	for i, g := range man.EffectiveRequires() {
		if !man.GroupSatisfied(g, values) {
			if isKindGroup(g, "ai") {
				return nil, configError(fmt.Sprintf("config.requires[%d]", i), "config.fields: pick at least one LLM provider")
			}
			return nil, configError(fmt.Sprintf("config.requires[%d]", i), "config.fields: fill in at least one of: "+groupTitles(man, g))
		}
	}
	return out, nil
}

// configError is a 422 that names the part of the request it blames, in the
// problem body's errors[0].location: config.fields.<APP_ENV>,
// config.ai_bindings.<slot>, config.requires[<i>] (an index into the
// install plan's requires), config.mail_provider_id or config.folders.<name>.
// The install flow routes the error to the page that owns that part
// (INSTALL_STEPS.md # 2, Errors); the message is only the text it shows.
func configError(location, message string) error {
	return huma.Error422UnprocessableEntity(message, &huma.ErrorDetail{Location: location, Message: message})
}

// configValues maps app_env to value, the shape the requires check reads.
func configValues(cfg []store.InstanceConfig) map[string]string {
	out := make(map[string]string, len(cfg))
	for _, c := range cfg {
		out[c.AppEnv] = c.Value
	}
	return out
}

// isKindGroup reports whether a requires group is exactly one kind member, so
// the 422 can name the kind ("an LLM provider") rather than list its fields.
func isKindGroup(group []string, kind string) bool {
	return len(group) == 1 && group[0] == kind
}

// groupTitles lists the titles of the fields that can satisfy a group, for a
// 422 the user can act on.
func groupTitles(man *manifest.Manifest, group []string) string {
	var titles []string
	for _, f := range man.GroupFields(group) {
		titles = append(titles, f.Title)
	}
	return strings.Join(titles, ", ")
}

// resolvePutConfig applies a partial post-install update to the current stored
// values and returns the full resolved set (APP_MANIFEST.md # D4). An absent key
// keeps the current value; a non-empty value sets it; an explicit "" clears it
// (optional fields only). required is validated against the resulting state, so
// an already-stored value satisfies it and a required secret can be replaced but
// never blanked.
//
// The requires groups are checked as "no worse": an edit may not leave a group
// unmet that was met before it. A group that was already unmet (an app
// installed before requires existed) does not block an unrelated edit.
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
				return nil, configError("config.fields."+appEnv, fmt.Sprintf("config.fields: %s is required and cannot be cleared", appEnv))
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
				return nil, configError("config.fields."+f.AppEnv, fmt.Sprintf("config.fields: %s is required", f.AppEnv))
			}
			continue
		}
		out = append(out, store.InstanceConfig{AppEnv: f.AppEnv, Value: v, Secret: f.Secret})
	}
	before, after := configValues(current), configValues(out)
	for i, g := range man.EffectiveRequires() {
		if man.GroupSatisfied(g, before) && !man.GroupSatisfied(g, after) {
			if isKindGroup(g, "ai") {
				return nil, configError(fmt.Sprintf("config.requires[%d]", i), "config.fields: keep at least one LLM provider")
			}
			return nil, configError(fmt.Sprintf("config.requires[%d]", i), "config.fields: keep at least one of these filled in: "+groupTitles(man, g))
		}
	}
	return out, nil
}
