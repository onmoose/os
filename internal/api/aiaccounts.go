package api

// AI provider accounts (INSTALL_SETUP.md # 5, SERVICE_PROVISIONING.md # AI
// provider accounts). An account is a saved key for one AI provider. It
// follows email accounts (mail.go): any signed-in user manages their own
// accounts, and only their own. Another user's account id answers 404, the
// same as an id that does not exist, so ids do not leak. Add and edit need no
// password re-prompt; delete keeps it. The key is write-only: requests carry
// it, and no response or log line ever holds it.
//
// An install binds an app's AI slots to the installer's accounts
// (aibindings.go). An edit of the key or base URL, and a delete, reach those
// apps: their values are rewritten or cleared and the running ones restart,
// in one background job whose id the response carries (INSTALL_SETUP.md
// piece 4, DECISIONS.md 2026-09-26).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// Length limits for an AI account. A label is a short name for a picker. A
// key is written into an app's .env later, so it gets a bound; real keys are
// well under it. The base URL limit is the common practical URL limit.
const (
	aiAccountLabelMax   = 100
	aiAccountKeyMax     = 4096
	aiAccountBaseURLMax = 2048
)

// auditTargetAIAccount is the audit target kind for an AI account.
const auditTargetAIAccount = "ai_account"

func (s *Server) registerAIAccounts(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-ai-accounts", Method: "GET", Path: "/api/v1/ai-accounts",
		Summary: "List the caller's own AI provider accounts",
	}, s.listAIAccounts)

	huma.Register(api, huma.Operation{
		OperationID: "create-ai-account", Method: "POST", Path: "/api/v1/ai-accounts",
		Summary: "Add an AI provider account owned by the caller",
	}, s.createAIAccount)

	huma.Register(api, huma.Operation{
		OperationID: "update-ai-account", Method: "PUT", Path: "/api/v1/ai-accounts/{id}",
		Summary: "Update one of the caller's AI provider accounts (an empty api_key keeps the stored one)",
	}, s.updateAIAccount)

	huma.Register(api, huma.Operation{
		OperationID: "delete-ai-account", Method: "DELETE", Path: "/api/v1/ai-accounts/{id}",
		Summary: "Delete one of the caller's AI provider accounts and clear it from the apps that use it (elevation required)", DefaultStatus: 200,
		Responses: map[string]*huma.Response{
			"204": {Description: "Deleted. No app used the account, so nothing else changes."},
		},
		// Listed because the 204 above stops huma adding its default error.
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError},
	}, s.deleteAIAccount)
}

// AIAccountDTO is the read shape of an AI account. The key is never in it:
// KeySet says only whether one is stored.
type AIAccountDTO struct {
	ID         string `json:"id"`
	ProviderID string `json:"provider_id"`
	Label      string `json:"label"`
	// BaseURL is empty when the account uses the provider's own address.
	BaseURL   string `json:"base_url"`
	KeySet    bool   `json:"key_set"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// UsedBy lists the apps that use the account, each once, ordered by
	// name. An edit of the key or base URL restarts them, and a delete clears
	// the account from them, so the UI names them before the user confirms.
	UsedBy []AppUseDTO `json:"used_by"`
}

// AppUseDTO names one app that uses an account.
type AppUseDTO struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
}

func appUseDTOs(uses []store.AppUse) []AppUseDTO {
	out := make([]AppUseDTO, 0, len(uses))
	for _, u := range uses {
		out = append(out, AppUseDTO{InstanceID: u.InstanceID, Name: u.Name})
	}
	return out
}

func aiAccountDTO(a store.AIAccount, uses []store.AppUse) AIAccountDTO {
	return AIAccountDTO{
		ID: a.ID, ProviderID: a.ProviderID, Label: a.Label, BaseURL: a.BaseURL,
		KeySet:    a.APIKey != "",
		CreatedAt: a.CreatedAt.Unix(), UpdatedAt: a.UpdatedAt.Unix(),
		UsedBy: appUseDTOs(uses),
	}
}

// AIAccountSavedDTO is the answer to an edit: the account, plus the id of
// the job that updates and restarts the apps using it. JobID is empty when
// the edit reaches no app (a label change, or an account no app uses).
type AIAccountSavedDTO struct {
	AIAccountDTO
	JobID string `json:"job_id,omitempty"`
}

// AccountDeletedDTO is the answer to deleting an account that apps used: the
// id of the job that updates and restarts them. A delete that reaches no app
// answers 204 with no body.
type AccountDeletedDTO struct {
	JobID string `json:"job_id"`
}

// AIAccountBody is the create and update request shape. On update an empty
// api_key keeps the stored one, so the owner can rename an account without
// typing the key again.
type AIAccountBody struct {
	// ProviderID is an id from GET /api/v1/ai-providers, or
	// "openai_compatible" for a server the user names by its address.
	ProviderID string `json:"provider_id"`
	Label      string `json:"label"`
	APIKey     string `json:"api_key,omitempty"`
	// BaseURL is required for openai_compatible and optional otherwise.
	BaseURL string `json:"base_url,omitempty"`
}

// validateAIAccountBody trims and checks the fields that need no stored state
// or provider data. Validation failures are plain 422s and do not audit.
func validateAIAccountBody(b *AIAccountBody) error {
	b.ProviderID = strings.TrimSpace(b.ProviderID)
	b.Label = strings.TrimSpace(b.Label)
	b.APIKey = strings.TrimSpace(b.APIKey)
	b.BaseURL = strings.TrimSpace(b.BaseURL)

	switch {
	case b.ProviderID == "":
		return huma.Error422UnprocessableEntity("provider_id is required")
	case b.Label == "":
		return huma.Error422UnprocessableEntity("label is required")
	case utf8.RuneCountInString(b.Label) > aiAccountLabelMax:
		return huma.Error422UnprocessableEntity("label is too long: use at most 100 characters")
	case hasControl(b.Label):
		return huma.Error422UnprocessableEntity("label must not contain line breaks or control characters")
	case len(b.APIKey) > aiAccountKeyMax:
		return huma.Error422UnprocessableEntity("api_key is too long: use at most 4096 characters")
	case hasControl(b.APIKey) || strings.ContainsRune(b.APIKey, ' '):
		// The key is written into an app's .env later, one value per line. A
		// line break would let it add a line of its own.
		return huma.Error422UnprocessableEntity("api_key must not contain spaces, line breaks or control characters")
	}
	if b.ProviderID == manifest.ProtocolOpenAICompatible && b.BaseURL == "" {
		return huma.Error422UnprocessableEntity("base_url is required for an OpenAI-compatible server")
	}
	if b.BaseURL != "" {
		if err := validateAIBaseURL(b.BaseURL); err != nil {
			return err
		}
	}
	return nil
}

// validateAIBaseURL accepts an absolute http or https URL with a host. Plain
// http is allowed on purpose: a server on the home network may have no TLS.
// No user name or password in it (the key has its own field), and no query or
// fragment, because apps add their own path to it.
func validateAIBaseURL(raw string) error {
	if len(raw) > aiAccountBaseURLMax {
		return huma.Error422UnprocessableEntity("base_url is too long: use at most 2048 characters")
	}
	if hasControl(raw) || strings.ContainsRune(raw, ' ') {
		return huma.Error422UnprocessableEntity("base_url must not contain spaces, line breaks or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return huma.Error422UnprocessableEntity("base_url must be a full http or https address, like https://example.com/v1")
	}
	if u.User != nil {
		return huma.Error422UnprocessableEntity("base_url must not contain a user name or password")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return huma.Error422UnprocessableEntity("base_url must not contain a query (?) or a fragment (#)")
	}
	return nil
}

// hasControl reports whether s holds a control character, which includes
// line breaks and tabs.
func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// checkAIProvider checks a provider id against the brain's current provider
// data. openai_compatible is always accepted, since it needs no data. When
// the data is empty (the catalog has not loaded, or has none), a listed id
// cannot be checked, so it is refused with its own message.
//
// A failed catalog read is a 500 on a credential mutation, so it is audited
// here as a failure of action. The 422s are validation and are not audited.
func (s *Server) checkAIProvider(ctx context.Context, action string, tgt audit.Target, providerID string) error {
	if providerID == manifest.ProtocolOpenAICompatible {
		return nil
	}
	providers, err := s.catalog.AIProviders()
	if err != nil {
		s.auditor.Record(ctx, action, tgt, map[string]any{"provider_id": providerID}, false)
		return huma.Error500InternalServerError("catalog read failed", err)
	}
	if len(providers) == 0 {
		return huma.Error422UnprocessableEntity("the list of LLM providers is not loaded yet. Try again in a few minutes, or add an OpenAI-compatible server")
	}
	for _, p := range providers {
		if p.ID == providerID {
			return nil
		}
	}
	return huma.Error422UnprocessableEntity("unknown LLM provider")
}

// requireAIKey enforces the key rule once the final key is known: a listed
// provider needs one, an OpenAI-compatible server does not.
func requireAIKey(providerID, key string) error {
	if key == "" && providerID != manifest.ProtocolOpenAICompatible {
		return huma.Error422UnprocessableEntity("api_key is required for this provider")
	}
	return nil
}

// aiAccountMeta is the audit metadata for an account. It never holds the key.
func aiAccountMeta(a store.AIAccount) map[string]any {
	return map[string]any{"label": a.Label, "provider_id": a.ProviderID}
}

// ownAIAccount loads an account the caller owns. Another user's account is
// store.ErrNotFound, the same as a missing one, so a caller cannot learn
// which ids exist.
func (s *Server) ownAIAccount(id auth.Identity, accountID string) (store.AIAccount, error) {
	a, err := s.store.GetAIAccount(accountID)
	if err != nil {
		return store.AIAccount{}, err
	}
	if a.OwnerUserID != id.User.ID {
		return store.AIAccount{}, store.ErrNotFound
	}
	return a, nil
}

func (s *Server) listAIAccounts(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Accounts []AIAccountDTO `json:"accounts"`
	}
}, error) {
	id, err := mailCaller(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := s.store.ListAIAccounts(id.User.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list ai accounts failed", err)
	}
	usage, err := s.store.AIAccountUsage(id.User.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list ai account usage failed", err)
	}
	out := &struct {
		Body struct {
			Accounts []AIAccountDTO `json:"accounts"`
		}
	}{}
	out.Body.Accounts = []AIAccountDTO{}
	for _, a := range accounts {
		out.Body.Accounts = append(out.Body.Accounts, aiAccountDTO(a, usage[a.ID]))
	}
	return out, nil
}

// createAIAccount adds an account owned by the caller. No elevation, for the
// same reason as an email account: it is the caller's own, and the install
// setup page adds one inline.
func (s *Server) createAIAccount(ctx context.Context, in *struct {
	Body AIAccountBody
}) (*struct{ Body AIAccountDTO }, error) {
	id, err := mailCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateAIAccountBody(&in.Body); err != nil {
		return nil, err
	}
	if err := s.checkAIProvider(ctx, audit.ActionAIAccountCreate, audit.Target{Kind: auditTargetAIAccount}, in.Body.ProviderID); err != nil {
		return nil, err
	}
	if err := requireAIKey(in.Body.ProviderID, in.Body.APIKey); err != nil {
		return nil, err
	}

	now := time.Now()
	a := store.AIAccount{
		ID: newID(), OwnerUserID: id.User.ID, ProviderID: in.Body.ProviderID, Label: in.Body.Label,
		APIKey: in.Body.APIKey, BaseURL: in.Body.BaseURL, CreatedAt: now, UpdatedAt: now,
	}
	meta := aiAccountMeta(a)
	if err := s.store.CreateAIAccount(a); err != nil {
		s.auditor.Record(ctx, audit.ActionAIAccountCreate, audit.Target{Kind: auditTargetAIAccount}, meta, false)
		if errors.Is(err, store.ErrConflict) {
			return nil, huma.Error409Conflict("you already have an account with that name")
		}
		return nil, huma.Error500InternalServerError("create ai account failed", err)
	}
	s.auditor.Record(ctx, audit.ActionAIAccountCreate, audit.Target{Kind: auditTargetAIAccount, ID: a.ID}, meta, true)
	return &struct{ Body AIAccountDTO }{Body: aiAccountDTO(a, nil)}, nil
}

// updateAIAccount edits one of the caller's accounts. No elevation, for the
// same reason as create. An empty api_key keeps the stored key.
//
// The provider id is checked against the provider data only when it changes.
// A rename should not fail because the catalog has not loaded, or because the
// store has since dropped the provider the account was made for. It cannot
// change while an app uses the account (409): the app's slot may not fit the
// new provider, so the user adds a new account instead.
//
// When the key or the base URL changes, every app bound to the account gets
// its values again and the running ones restart, in one background job
// (INSTALL_SETUP.md piece 4). The save itself does not wait for it. A label
// change restarts nothing.
func (s *Server) updateAIAccount(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body AIAccountBody
}) (*struct{ Body AIAccountSavedDTO }, error) {
	id, err := mailCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateAIAccountBody(&in.Body); err != nil {
		return nil, err
	}

	tgt := audit.Target{Kind: auditTargetAIAccount, ID: in.ID}
	existing, err := s.ownAIAccount(id, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		// Audited: an edit aimed at someone else's account lands here.
		s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, nil, false)
		return nil, huma.Error404NotFound("no such AI account")
	}
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, nil, false)
		return nil, huma.Error500InternalServerError("get ai account failed", err)
	}

	a := existing
	a.ProviderID, a.Label, a.BaseURL = in.Body.ProviderID, in.Body.Label, in.Body.BaseURL
	if in.Body.APIKey != "" {
		a.APIKey = in.Body.APIKey
	}
	a.UpdatedAt = time.Now()
	meta := aiAccountMeta(a)

	bindings, err := s.store.ListAIBindingsForAccount(a.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, meta, false)
		return nil, huma.Error500InternalServerError("list ai bindings failed", err)
	}
	if a.ProviderID != existing.ProviderID {
		if len(bindings) > 0 {
			s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, meta, false)
			return nil, huma.Error409Conflict("apps use this account, so its provider cannot change. Add a new account for the other provider instead")
		}
		if err := s.checkAIProvider(ctx, audit.ActionAIAccountUpdate, tgt, a.ProviderID); err != nil {
			return nil, err
		}
	}
	if err := requireAIKey(a.ProviderID, a.APIKey); err != nil {
		return nil, err
	}
	if existing.BaseURL != "" && a.BaseURL == "" {
		if err := s.checkBaseURLRemoval(ctx, tgt, meta, a.ProviderID, bindings); err != nil {
			return nil, err
		}
	}

	// Send the key only when the request carries one. An empty key tells the
	// store to keep the stored one, so a concurrent key change is not undone.
	upd := a
	upd.APIKey = in.Body.APIKey
	if err := s.store.UpdateAIAccount(upd); err != nil {
		s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, meta, false)
		if errors.Is(err, store.ErrConflict) {
			return nil, huma.Error409Conflict("you already have an account with that name")
		}
		if errors.Is(err, store.ErrNotFound) {
			// Deleted between the read and the write.
			return nil, huma.Error404NotFound("no such AI account")
		}
		return nil, huma.Error500InternalServerError("update ai account failed", err)
	}
	s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, meta, true)

	usage, err := s.store.AIAccountUsage(id.User.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list ai account usage failed", err)
	}
	out := &struct{ Body AIAccountSavedDTO }{Body: AIAccountSavedDTO{AIAccountDTO: aiAccountDTO(a, usage[a.ID])}}
	reaches := a.APIKey != existing.APIKey || a.BaseURL != existing.BaseURL
	if reaches && len(bindings) > 0 {
		var ids []string
		for _, b := range bindings {
			if len(ids) == 0 || ids[len(ids)-1] != b.InstanceID {
				ids = append(ids, b.InstanceID)
			}
		}
		// A removed base URL is passed on, so an app that still holds it is
		// not left on it when the provider data cannot fill the slot.
		var removedURL string
		if a.BaseURL == "" {
			removedURL = existing.BaseURL
		}
		out.Body.JobID = s.restampAIAccountJob(a.ID, ids, removedURL).ID
	}
	return out, nil
}

// checkBaseURLRemoval refuses an edit that removes the account's own base URL
// while an app's OpenAI-compatible slot has nothing to take in its place: the
// provider data is not loaded, or it has no openai_base_url for the provider.
// Saving it would leave the app on the removed address, or fail the app's
// whole re-stamp, so a key changed in the same edit would never reach it. The
// user can still change the key on its own.
func (s *Server) checkBaseURLRemoval(ctx context.Context, tgt audit.Target, meta map[string]any, providerID string, bindings []store.AIBinding) error {
	compatible := false
	for _, b := range bindings {
		if strings.HasSuffix(b.Slot, "."+manifest.ProtocolOpenAICompatible) {
			compatible = true
			break
		}
	}
	if !compatible {
		return nil
	}
	providers, err := s.catalog.AIProviders()
	if err != nil || len(providers) == 0 {
		if err != nil {
			slog.Warn("read ai providers for base URL removal failed", "err", err)
		}
		s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, meta, false)
		return huma.Error409Conflict("the list of LLM providers is not loaded yet, so this account's base URL cannot be removed now. Try again in a few minutes. You can still change the key on its own")
	}
	if providerBaseURL(providers, providerID) == "" {
		s.auditor.Record(ctx, audit.ActionAIAccountUpdate, tgt, meta, false)
		return huma.Error409Conflict("an app uses this account's base URL as its OpenAI-compatible address, and the provider has no standard one to use instead. Keep the base URL, or pick another account in that app's settings first")
	}
	return nil
}

// restampAIAccountJob starts the job that gives every app bound to an
// account its values again and restarts the running ones. It carries only
// the account id and the app ids: each app's values are resolved under its
// lock from the account as it is then (accountSlotResolver), so two quick
// edits end on the newer one whichever job finishes last. One app failing
// does not stop the others.
func (s *Server) restampAIAccountJob(accountID string, ids []string, removedURL string) *Job {
	return s.jobs.run("ai-account-restamp", func(job *Job) (map[string]any, error) {
		job.setStep("updating_apps")
		// The provider data is only a fallback here (restampSlot), so a read
		// error is logged and the re-stamp goes on without it.
		providers, err := s.catalog.AIProviders()
		if err != nil {
			slog.Warn("read ai providers for account re-stamp failed", "err", err)
			providers = nil
		}
		resolve := accountSlotResolver(s.store.GetAIAccount, providers, removedURL)
		if err := s.life.RestampAIAccount(context.Background(), accountID, ids, resolve); err != nil {
			return nil, err
		}
		return map[string]any{"account_id": accountID}, nil
	})
}

// bindingSlotFields names the app_env fields of one AI slot from the app's
// manifest copy. A delete that clears an account's values from apps
// (store.DeleteAIAccountAndValues, store.DeleteUserAndAIValues) calls it
// only for a binding from before bindings recorded their fields. If the copy
// cannot be read, or no longer has the slot, it fails and the delete is
// refused: a deleted key must never stay in an app.
func (s *Server) bindingSlotFields(instanceID, slot string) ([]string, error) {
	man, err := s.life.InstanceManifest(instanceID)
	if err != nil {
		return nil, err
	}
	fields := fillableSlots(man)[slot]
	if len(fields) == 0 {
		return nil, fmt.Errorf("the manifest has no slot %s", slot)
	}
	envs := make([]string, 0, len(fields))
	for _, sf := range fields {
		envs = append(envs, sf.field.AppEnv)
	}
	return envs, nil
}

// slotFieldsRefused is the answer to a delete that bindingSlotFields
// refused. what names the thing that was not deleted ("account", "user").
func (s *Server) slotFieldsRefused(sfe *store.SlotFieldsError, what string) error {
	name := sfe.InstanceID
	if inst, err := s.store.Get(sfe.InstanceID); err == nil {
		name = inst.Name
	}
	slog.Error("delete refused: cannot name the AI fields to clear",
		"instance_id", sfe.InstanceID, "name", name, "target_kind", what, "err", sfe.Err)
	return huma.Error500InternalServerError(fmt.Sprintf(
		"the LLM provider settings of %s could not be found, so its key could not be removed. The %s was not deleted. Try again, or uninstall %s first", name, what, name))
}

// deleteAIAccount removes one of the caller's accounts. It keeps the password
// re-prompt, like an email account: a delete cannot be undone.
//
// The values the account gave each app are removed in the same store
// transaction as the account, so a deleted key never stays in the brain's
// state. Then a background job rewrites those apps and restarts the running
// ones; its id is the answer. A delete that reaches no app answers 204.
func (s *Server) deleteAIAccount(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Status int
	Body   *AccountDeletedDTO
}, error) {
	id, err := mailCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}

	tgt := audit.Target{Kind: auditTargetAIAccount, ID: in.ID}
	if _, err := s.ownAIAccount(id, in.ID); err != nil {
		s.auditor.Record(ctx, audit.ActionAIAccountDelete, tgt, nil, false)
		if errors.Is(err, store.ErrNotFound) {
			// Audited: a delete aimed at someone else's account lands here.
			return nil, huma.Error404NotFound("no such AI account")
		}
		return nil, huma.Error500InternalServerError("get ai account failed", err)
	}
	// The bindings are read inside the delete transaction, so a slot rebound
	// to another account a moment ago keeps its new values.
	ids, err := s.store.DeleteAIAccountAndValues(in.ID, id.User.ID, s.bindingSlotFields)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAIAccountDelete, tgt, nil, false)
		var sfe *store.SlotFieldsError
		switch {
		case errors.Is(err, store.ErrNotFound):
			// Deleted between the read and the write.
			return nil, huma.Error404NotFound("no such AI account")
		case errors.As(err, &sfe):
			return nil, s.slotFieldsRefused(sfe, "account")
		}
		return nil, huma.Error500InternalServerError("delete ai account failed", err)
	}
	s.auditor.Record(ctx, audit.ActionAIAccountDelete, tgt, nil, true)

	out := &struct {
		Status int
		Body   *AccountDeletedDTO
	}{Status: http.StatusNoContent}
	if len(ids) == 0 {
		return out, nil
	}
	job := s.jobs.run("ai-account-delete", func(job *Job) (map[string]any, error) {
		job.setStep("updating_apps")
		if err := s.life.RestampConfig(context.Background(), ids); err != nil {
			return nil, err
		}
		return map[string]any{"account_id": in.ID}, nil
	})
	out.Status = http.StatusOK
	out.Body = &AccountDeletedDTO{JobID: job.ID}
	return out, nil
}
