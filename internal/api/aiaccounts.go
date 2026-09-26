package api

// AI provider accounts (INSTALL_SETUP.md # 5, SERVICE_PROVISIONING.md # AI
// provider accounts). An account is a saved key for one AI provider. It
// follows email accounts (mail.go): any signed-in user manages their own
// accounts, and only their own. Another user's account id answers 404, the
// same as an id that does not exist, so ids do not leak. Add and edit need no
// password re-prompt; delete keeps it. The key is write-only: requests carry
// it, and no response or log line ever holds it.
//
// Nothing binds an app to an account yet. That comes with slot filling.

import (
	"context"
	"errors"
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
		Summary: "Delete one of the caller's AI provider accounts (elevation required)", DefaultStatus: 204,
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
}

func aiAccountDTO(a store.AIAccount) AIAccountDTO {
	return AIAccountDTO{
		ID: a.ID, ProviderID: a.ProviderID, Label: a.Label, BaseURL: a.BaseURL,
		KeySet:    a.APIKey != "",
		CreatedAt: a.CreatedAt.Unix(), UpdatedAt: a.UpdatedAt.Unix(),
	}
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
		return huma.Error422UnprocessableEntity("the list of AI providers is not loaded yet. Try again in a few minutes, or add an OpenAI-compatible server")
	}
	for _, p := range providers {
		if p.ID == providerID {
			return nil
		}
	}
	return huma.Error422UnprocessableEntity("unknown AI provider")
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
	out := &struct {
		Body struct {
			Accounts []AIAccountDTO `json:"accounts"`
		}
	}{}
	out.Body.Accounts = []AIAccountDTO{}
	for _, a := range accounts {
		out.Body.Accounts = append(out.Body.Accounts, aiAccountDTO(a))
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
	return &struct{ Body AIAccountDTO }{Body: aiAccountDTO(a)}, nil
}

// updateAIAccount edits one of the caller's accounts. No elevation, for the
// same reason as create. An empty api_key keeps the stored key.
//
// The provider id is checked against the provider data only when it changes.
// A rename should not fail because the catalog has not loaded, or because the
// store has since dropped the provider the account was made for.
func (s *Server) updateAIAccount(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body AIAccountBody
}) (*struct{ Body AIAccountDTO }, error) {
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
	if a.ProviderID != existing.ProviderID {
		if err := s.checkAIProvider(ctx, audit.ActionAIAccountUpdate, tgt, a.ProviderID); err != nil {
			return nil, err
		}
	}
	if err := requireAIKey(a.ProviderID, a.APIKey); err != nil {
		return nil, err
	}

	meta := aiAccountMeta(a)
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
	return &struct{ Body AIAccountDTO }{Body: aiAccountDTO(a)}, nil
}

// deleteAIAccount removes one of the caller's accounts. It keeps the password
// re-prompt, like an email account: a delete cannot be undone.
func (s *Server) deleteAIAccount(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	id, err := mailCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}

	tgt := audit.Target{Kind: auditTargetAIAccount, ID: in.ID}
	if err := s.store.DeleteAIAccount(in.ID, id.User.ID); err != nil {
		s.auditor.Record(ctx, audit.ActionAIAccountDelete, tgt, nil, false)
		if errors.Is(err, store.ErrNotFound) {
			// Audited: a delete aimed at someone else's account lands here.
			return nil, huma.Error404NotFound("no such AI account")
		}
		return nil, huma.Error500InternalServerError("delete ai account failed", err)
	}
	s.auditor.Record(ctx, audit.ActionAIAccountDelete, tgt, nil, true)
	return nil, nil
}
