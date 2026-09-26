package api

// Filling an app's AI slots from the caller's AI accounts at install
// (INSTALL_SETUP.md # 1 and # 5). The setup page sends one binding per slot:
// which account, and which model ids for each model setting the slot declares.
// The brain turns each binding into the slot's app_env values here, because
// only the brain can read the key. The values then go through the same checks
// as typed values (resolveInstallConfig), so `required` and `requires` see the
// filled slot.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// AIBindingBody fills one AI slot of the app from one of the caller's AI
// accounts.
type AIBindingBody struct {
	// Slot is kind.protocol from the manifest's roles, e.g. ai.anthropic or
	// ai.openai_compatible.
	Slot string `json:"slot"`
	// AccountID is one of the caller's accounts from GET /api/v1/ai-accounts.
	AccountID string `json:"account_id"`
	// Models maps a model setting the slot declares (model.chat,
	// models.embedding) to model ids: exactly one for model.<type>, one or
	// more for models.<type>. A setting left out gets the provider's default
	// for that type. An id that is not in the provider data is accepted.
	Models map[string][]string `json:"models,omitempty"`
}

// slotField is one fillable field of a slot, with its parsed role.
type slotField struct {
	field manifest.ConfigField
	role  manifest.Role
}

// modelKey is how a binding names a model setting: model.chat, models.embedding.
func modelKey(r manifest.Role) string { return r.Attribute + "." + r.ModelType }

// fillableSlots groups the manifest's fillable fields by slot, in manifest
// order.
func fillableSlots(man *manifest.Manifest) map[string][]slotField {
	roles := man.FillableRoles()
	out := map[string][]slotField{}
	for _, f := range man.Config {
		if r, ok := roles[f.AppEnv]; ok {
			out[r.Slot()] = append(out[r.Slot()], slotField{field: f, role: r})
		}
	}
	return out
}

// aiResolution is what the bindings of one install resolved to.
type aiResolution struct {
	// values maps app_env to the value a binding filled. A field the binding
	// leaves empty (no key on a keyless account, no native base URL) is absent.
	values map[string]string
	// claimed is every role field of a bound slot, filled or not. The request
	// may not also type a value for one of them.
	claimed map[string]bool
	// bindings are the rows to store, with the model ids the app was given.
	bindings []store.AIBinding
}

// resolveAIBindings checks each binding and resolves it into app_env values
// (INSTALL_SETUP.md decisions, 2026-09-26). account loads one of the caller's
// accounts: store.ErrNotFound for a missing id and for another user's, so the
// answer does not say whether the id exists. providers is the provider data.
func resolveAIBindings(man *manifest.Manifest, bindings []AIBindingBody, account func(id string) (store.AIAccount, error), providers []catalog.AIProvider) (aiResolution, error) {
	res := aiResolution{values: map[string]string{}, claimed: map[string]bool{}}
	slots := fillableSlots(man)
	seen := map[string]bool{}
	for _, b := range bindings {
		slot := strings.TrimSpace(b.Slot)
		fields, ok := slots[slot]
		if !ok {
			return aiResolution{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: this app has no AI slot %q", slot))
		}
		if seen[slot] {
			return aiResolution{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: slot %s is given more than once", slot))
		}
		seen[slot] = true
		accountID := strings.TrimSpace(b.AccountID)
		if accountID == "" {
			return aiResolution{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: pick an AI account for slot %s", slot))
		}
		acct, err := account(accountID)
		if errors.Is(err, store.ErrNotFound) {
			return aiResolution{}, huma.Error422UnprocessableEntity("config.ai_bindings: no such AI account")
		}
		if err != nil {
			return aiResolution{}, huma.Error500InternalServerError("ai account lookup failed", err)
		}
		bound, err := resolveSlot(slot, fields, b.Models, acct, providers)
		if err != nil {
			return aiResolution{}, err
		}
		for env, v := range bound.values {
			res.values[env] = v
		}
		envs := make([]string, 0, len(fields))
		for _, sf := range fields {
			res.claimed[sf.field.AppEnv] = true
			envs = append(envs, sf.field.AppEnv)
		}
		// Envs records every field of the slot, so an account delete can
		// clear them without the manifest.
		res.bindings = append(res.bindings, store.AIBinding{Slot: slot, AccountID: acct.ID, Models: bound.models, Envs: envs})
	}
	return res, nil
}

// boundSlot is one slot's resolved values and model ids.
type boundSlot struct {
	values map[string]string
	models map[string][]string
}

// resolveSlot fills one slot from one account. The account's provider must fit
// the slot: a native slot takes a provider with that native_protocol, and the
// compatible slot takes an openai_compatible account or a provider with an
// openai_base_url.
func resolveSlot(slot string, fields []slotField, chosen map[string][]string, acct store.AIAccount, providers []catalog.AIProvider) (boundSlot, error) {
	protocol := fields[0].role.Protocol
	compatible := protocol == manifest.ProtocolOpenAICompatible
	notFit := huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: the account %q does not work with this app's %s slot", acct.Label, slot))

	var prov *catalog.AIProvider
	for i := range providers {
		if providers[i].ID == acct.ProviderID {
			prov = &providers[i]
			break
		}
	}

	// baseURL is what a base_url field gets. Empty leaves the field out, so the
	// app uses its own default.
	var baseURL string
	switch {
	case acct.ProviderID == manifest.ProtocolOpenAICompatible:
		if !compatible {
			return boundSlot{}, notFit
		}
		baseURL = acct.BaseURL
	case prov == nil:
		// The provider has left the provider data since the account was made.
		// A native slot cannot confirm the protocol without it. The compatible
		// slot still works when the account has its own address.
		if !compatible {
			return boundSlot{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: the provider of the account %q is not in the provider list now, so it cannot fill the %s slot", acct.Label, slot))
		}
		if acct.BaseURL == "" {
			return boundSlot{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: the provider of the account %q is not in the provider list now. Give the account its own base URL, or pick another account", acct.Label))
		}
		baseURL = acct.BaseURL
	case compatible:
		if prov.OpenAIBaseURL == "" {
			return boundSlot{}, notFit
		}
		baseURL = acct.BaseURL
		if baseURL == "" {
			baseURL = prov.OpenAIBaseURL
		}
	default:
		if prov.NativeProtocol != protocol {
			return boundSlot{}, notFit
		}
		baseURL = acct.BaseURL
	}

	// Every model setting the request names must be one the slot declares.
	declared := map[string]bool{}
	for _, sf := range fields {
		if sf.role.IsModel() {
			declared[modelKey(sf.role)] = true
		}
	}
	keys := make([]string, 0, len(chosen))
	for k := range chosen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !declared[k] {
			return boundSlot{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: the %s slot of this app does not take %s", slot, k))
		}
	}

	out := boundSlot{values: map[string]string{}, models: map[string][]string{}}
	for _, sf := range fields {
		env := sf.field.AppEnv
		switch sf.role.Attribute {
		case manifest.AttrAPIKey:
			// A keyless openai_compatible account injects nothing.
			if acct.APIKey != "" {
				out.values[env] = acct.APIKey
			}
		case manifest.AttrBaseURL:
			if baseURL != "" {
				out.values[env] = baseURL
			}
		case manifest.AttrModel, manifest.AttrModels:
			ids, err := slotModels(sf, chosen, prov)
			if err != nil {
				return boundSlot{}, err
			}
			out.values[env] = strings.Join(ids, sf.field.EffectiveSeparator())
			out.models[modelKey(sf.role)] = ids
		}
	}
	return out, nil
}

// slotModels returns the model ids for one model field: the chosen ones, or
// else the provider's default for the field's type, checked the same way.
// Only a listed provider has defaults; an openai_compatible account, or a
// provider that has left the data, has none.
func slotModels(sf slotField, chosen map[string][]string, prov *catalog.AIProvider) ([]string, error) {
	title := sf.field.Title
	key := modelKey(sf.role)
	list := sf.role.Attribute == manifest.AttrModels
	raw, given := chosen[key]
	if !given {
		if prov == nil || prov.Defaults[sf.role.ModelType] == "" {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: pick a model for %s", title))
		}
		// The default goes through the same checks as a chosen id, so a default
		// that holds the app's separator is refused, not split into a list.
		raw = []string{prov.Defaults[sf.role.ModelType]}
	}
	sep := sf.field.EffectiveSeparator()
	ids := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		switch {
		case id == "":
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: a model name for %s is empty", title))
		case hasControl(id):
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: a model name for %s must not contain line breaks or control characters", title))
		case slices.Contains(ids, id):
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: model %q is picked twice for %s", id, title))
		case list && strings.Contains(id, sep):
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: model %q for %s contains %q, which this app uses to separate models", id, title, sep))
		}
		ids = append(ids, id)
	}
	if !list && len(ids) != 1 {
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: pick exactly one model for %s", title))
	}
	if list && len(ids) == 0 {
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: pick at least one model for %s", title))
	}
	return ids, nil
}

// resolveInstallWithAI resolves the bindings, refuses a typed value for a
// field a binding fills, and runs the merged answers through
// resolveInstallConfig, so `required` and `requires` see the filled slots. It
// returns the config values to store and the bindings to record.
func resolveInstallWithAI(man *manifest.Manifest, fields map[string]string, bindings []AIBindingBody, account func(id string) (store.AIAccount, error), providers []catalog.AIProvider) ([]store.InstanceConfig, []store.AIBinding, error) {
	res, err := resolveAIBindings(man, bindings, account, providers)
	if err != nil {
		return nil, nil, err
	}
	merged := make(map[string]string, len(fields)+len(res.values))
	envs := make([]string, 0, len(fields))
	for env := range fields {
		envs = append(envs, env)
	}
	slices.Sort(envs)
	for _, env := range envs {
		v := fields[env]
		if v != "" && res.claimed[env] {
			return nil, nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %s is filled from an AI account, so do not also send a value for it", env))
		}
		merged[env] = v
	}
	for env, v := range res.values {
		merged[env] = v
	}
	cfg, err := resolveInstallConfig(man, merged)
	if err != nil {
		return nil, nil, err
	}
	return cfg, res.bindings, nil
}

// resolveInstallAnswers is the install handler's entry to the config answers.
// Without bindings it is resolveInstallConfig alone, and the catalog is not
// read. With bindings it loads the provider data and looks accounts up as the
// caller.
func (s *Server) resolveInstallAnswers(ctx context.Context, man *manifest.Manifest, fields map[string]string, bindings []AIBindingBody) ([]store.InstanceConfig, []store.AIBinding, error) {
	if len(bindings) == 0 {
		cfg, err := resolveInstallConfig(man, fields)
		return cfg, nil, err
	}
	caller, ok := auth.FromContext(ctx)
	if !ok {
		return nil, nil, huma.Error401Unauthorized("unauthenticated")
	}
	providers, err := s.catalog.AIProviders()
	if err != nil {
		return nil, nil, huma.Error500InternalServerError("catalog read failed", err)
	}
	account := func(id string) (store.AIAccount, error) { return s.ownAIAccount(caller, id) }
	return resolveInstallWithAI(man, fields, bindings, account, providers)
}

// putAIResolution is what a config edit with bindings resolved to.
type putAIResolution struct {
	// cfg is the app's full set of config values after the edit.
	cfg []store.InstanceConfig
	// slots are the slots the edit replaced or cleared, in request order.
	slots []string
	// bindings are the new binding rows for the replaced slots. A cleared
	// slot has none.
	bindings []store.AIBinding
}

// resolvePutWithAI applies a config edit that may carry AI bindings
// (INSTALL_SETUP.md piece 4). A listed slot is replaced: its binding with the
// new one and its fields with the values the new binding gives. A slot sent
// with an empty account_id is cleared: its binding and its values go. A slot
// not listed is left alone. The typed fields and the slots are checked
// together by resolvePutConfig, so `required` and the "no worse" requires
// rule see the result of the whole edit.
//
// A typed value for a field of a listed slot is a 422, as at install. So is a
// typed value for a field of a slot that is bound now and not listed: the
// user changes such a slot by picking another account or clearing it. An
// empty typed value for either is ignored, like at install.
func resolvePutWithAI(man *manifest.Manifest, current []store.InstanceConfig, currentBindings []store.AIBinding, fields map[string]string, bindings []AIBindingBody, account func(id string) (store.AIAccount, error), providers []catalog.AIProvider) (putAIResolution, error) {
	slots := fillableSlots(man)
	listed := map[string]bool{}
	var order []string
	var sets []AIBindingBody
	for _, b := range bindings {
		slot := strings.TrimSpace(b.Slot)
		if _, ok := slots[slot]; !ok {
			return putAIResolution{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: this app has no AI slot %q", slot))
		}
		if listed[slot] {
			return putAIResolution{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.ai_bindings: slot %s is given more than once", slot))
		}
		listed[slot] = true
		order = append(order, slot)
		if strings.TrimSpace(b.AccountID) == "" {
			continue // a clear
		}
		b.Slot = slot
		sets = append(sets, b)
	}
	res, err := resolveAIBindings(man, sets, account, providers)
	if err != nil {
		return putAIResolution{}, err
	}

	claimed := map[string]bool{}
	for slot := range listed {
		for _, sf := range slots[slot] {
			claimed[sf.field.AppEnv] = true
		}
	}
	for _, cb := range currentBindings {
		if listed[cb.Slot] {
			continue
		}
		for _, sf := range slots[cb.Slot] {
			claimed[sf.field.AppEnv] = true
		}
	}

	put := make(map[string]string, len(fields))
	envs := make([]string, 0, len(fields))
	for env := range fields {
		envs = append(envs, env)
	}
	slices.Sort(envs)
	for _, env := range envs {
		v := fields[env]
		if !claimed[env] {
			put[env] = v
			continue
		}
		if v != "" {
			return putAIResolution{}, huma.Error422UnprocessableEntity(fmt.Sprintf("config.fields: %s is filled from an AI account, so do not also send a value for it", env))
		}
	}
	// Every field of a listed slot gets the value its new binding gives, or is
	// cleared when the binding gives none (or the slot is being cleared).
	for slot := range listed {
		for _, sf := range slots[slot] {
			put[sf.field.AppEnv] = res.values[sf.field.AppEnv]
		}
	}
	cfg, err := resolvePutConfig(man, current, put)
	if err != nil {
		return putAIResolution{}, err
	}
	return putAIResolution{cfg: cfg, slots: order, bindings: res.bindings}, nil
}

// accountSlotResolver is the lifecycle.SlotResolver for an account edit
// (INSTALL_SETUP.md piece 4). Lifecycle calls it under each app's lock, and
// it reads the account row then, through account, so the values come from
// the account as it is at commit time, not as it was when the edit started.
//
// It fills the slot from what the edit can change, and needs no provider
// data for that, so a key change still reaches the apps right after a boot
// or while the catalog service is down:
//   - api_key gets the account's key (nothing for a keyless account).
//   - base_url gets the account's base URL when it has one. Without one, a
//     native slot's base URL is left blank (the decision row), and the
//     compatible slot takes the provider's openai_base_url from the data if
//     the data has it, else keeps the value it has now.
//   - a model field gets the stored model ids, joined with the field's
//     separator, else keeps the value it has now.
//
// The provider does not have to fit the slot again: the binding was checked
// when it was made, and the provider id of a bound account cannot change.
func accountSlotResolver(account func(id string) (store.AIAccount, error), providers []catalog.AIProvider) lifecycle.SlotResolver {
	return func(man *manifest.Manifest, b store.AIBinding, current map[string]string) (map[string]string, error) {
		acct, err := account(b.AccountID)
		if err != nil {
			return nil, fmt.Errorf("read account: %w", err)
		}
		fields, ok := fillableSlots(man)[b.Slot]
		if !ok {
			return nil, fmt.Errorf("the app has no LLM provider setting %s now", b.Slot)
		}
		return restampSlot(fields, b, acct, providers, current), nil
	}
}

// restampSlot is the values of one bound slot after its account changed.
// See accountSlotResolver for the rules.
func restampSlot(fields []slotField, b store.AIBinding, acct store.AIAccount, providers []catalog.AIProvider, current map[string]string) map[string]string {
	out := map[string]string{}
	for _, sf := range fields {
		env := sf.field.AppEnv
		switch sf.role.Attribute {
		case manifest.AttrAPIKey:
			if acct.APIKey != "" {
				out[env] = acct.APIKey
			}
		case manifest.AttrBaseURL:
			switch {
			case acct.BaseURL != "":
				out[env] = acct.BaseURL
			case sf.role.Protocol != manifest.ProtocolOpenAICompatible:
				// A native base URL comes only from the account.
			default:
				if u := providerBaseURL(providers, acct.ProviderID); u != "" {
					out[env] = u
				} else if v := current[env]; v != "" {
					out[env] = v
				}
			}
		case manifest.AttrModel, manifest.AttrModels:
			if ids := b.Models[modelKey(sf.role)]; len(ids) > 0 {
				out[env] = strings.Join(ids, sf.field.EffectiveSeparator())
			} else if v := current[env]; v != "" {
				out[env] = v
			}
		}
	}
	return out
}

// providerBaseURL is a listed provider's OpenAI-compatible endpoint, or ""
// when the provider is not in the data (or the data is empty).
func providerBaseURL(providers []catalog.AIProvider, id string) string {
	for _, p := range providers {
		if p.ID == id {
			return p.OpenAIBaseURL
		}
	}
	return ""
}
