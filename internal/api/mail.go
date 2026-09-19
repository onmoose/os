package api

// Outgoing-mail provider management (SERVICE_PROVISIONING.md # BYO outgoing
// mail, SETTINGS.md). Provider CRUD is admin-only and elevation-class; the
// per-app binding endpoint follows the stop/start authorization (a member may
// rebind their own personal instance). Passwords are write-only: requests
// carry them, responses never do.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/mailpreset"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// testMailTimeout bounds the whole synchronous test-send (dial, handshake,
// auth, send) so a black-holed SMTP host returns a clean error, not a hung
// request.
const testMailTimeout = 15 * time.Second

func (s *Server) registerMail(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-mail-providers", Method: "GET", Path: "/api/v1/mail-providers",
		Summary: "List outgoing-mail providers (admin only)",
	}, s.listMailProviders)

	huma.Register(api, huma.Operation{
		OperationID: "create-mail-provider", Method: "POST", Path: "/api/v1/mail-providers",
		Summary: "Register an outgoing-mail provider (admin only)",
	}, s.createMailProvider)

	huma.Register(api, huma.Operation{
		OperationID: "update-mail-provider", Method: "PUT", Path: "/api/v1/mail-providers/{id}",
		Summary: "Update an outgoing-mail provider (admin only)",
	}, s.updateMailProvider)

	huma.Register(api, huma.Operation{
		OperationID: "delete-mail-provider", Method: "DELETE", Path: "/api/v1/mail-providers/{id}",
		Summary: "Delete an outgoing-mail provider (admin only; bound apps fall back to unbound)", DefaultStatus: 204,
	}, s.deleteMailProvider)

	huma.Register(api, huma.Operation{
		OperationID: "verify-mail-provider-config", Method: "POST", Path: "/api/v1/mail-providers/verify",
		Summary: "Check an unsaved provider config by connecting and authenticating (admin only)", DefaultStatus: 204,
	}, s.verifyMailProviderConfig)

	huma.Register(api, huma.Operation{
		OperationID: "test-mail-provider", Method: "POST", Path: "/api/v1/mail-providers/{id}/test",
		Summary: "Send a test email through a provider (admin only)", DefaultStatus: 204,
	}, s.testMailProvider)

	huma.Register(api, huma.Operation{
		OperationID: "list-mail-presets", Method: "GET", Path: "/api/v1/mail-presets",
		Summary: "List built-in outgoing-mail provider presets (admin only)",
	}, s.listMailPresets)

	huma.Register(api, huma.Operation{
		OperationID: "list-mail-provider-options", Method: "GET", Path: "/api/v1/mail-providers/options",
		Summary: "List provider picker options: id, label and provider type (any authenticated user)",
	}, s.listMailProviderOptions)

	huma.Register(api, huma.Operation{
		OperationID: "set-app-mail-binding", Method: "PUT", Path: "/api/v1/apps/{id}/mail-binding",
		Summary: "Bind an app to an outgoing-mail provider (empty provider_id unbinds)",
	}, s.setAppMailBinding)
}

// MailProviderDTO is the read shape of a provider. The password is write-only
// (requests carry it; no response ever echoes it).
type MailProviderDTO struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	FromAddress string `json:"from_address"`
	Encryption  string `json:"encryption" enum:"none,starttls,tls"`
	// ProviderType is the preset the admin picked, or "custom". The UI uses
	// it to label the row and to re-open the right form on edit.
	ProviderType  string `json:"provider_type"`
	ProviderLabel string `json:"provider_label"`
	CreatedAt     int64  `json:"created_at"`
}

func mailProviderDTO(p store.MailProvider) MailProviderDTO {
	return MailProviderDTO{
		ID: p.ID, Label: p.Label, Host: p.Host, Port: p.Port,
		Username: p.Username, FromAddress: p.FromAddress,
		Encryption:   p.Encryption,
		ProviderType: p.ProviderType, ProviderLabel: mailpreset.LabelFor(p.ProviderType),
		CreatedAt: p.CreatedAt.Unix(),
	}
}

// MailProviderBody is the create/update request shape. On update an empty
// password keeps the stored one, so an admin can edit the host or label
// without re-entering the credential.
type MailProviderBody struct {
	Label       string `json:"label"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	FromAddress string `json:"from_address"`
	Encryption  string `json:"encryption" enum:"none,starttls,tls"`
	// ProviderType names the preset the values came from; empty means custom.
	// The server stores what the client sends and does not re-derive host,
	// port or encryption from the preset — re-deriving would silently undo the
	// advanced override (DECISIONS.md 2026-08-27 D3).
	ProviderType string `json:"provider_type,omitempty"`
}

// validateMailProviderBody normalizes and checks the shared create/update
// fields; validation failures are plain 422s and do not audit.
func validateMailProviderBody(b *MailProviderBody) error {
	b.Label = strings.TrimSpace(b.Label)
	b.Host = strings.TrimSpace(b.Host)
	b.FromAddress = strings.TrimSpace(b.FromAddress)
	switch {
	case b.Label == "":
		return huma.Error422UnprocessableEntity("label is required")
	case b.Host == "":
		return huma.Error422UnprocessableEntity("host is required")
	case b.Port < 1 || b.Port > 65535:
		return huma.Error422UnprocessableEntity("port must be 1-65535")
	case b.FromAddress == "" || !strings.Contains(b.FromAddress, "@"):
		return huma.Error422UnprocessableEntity("from_address must be an email address")
	}
	// A newline in any of these reaches a MOOSE_MAIL_* .env line (one field per
	// line) and the test-send's SMTP commands / RFC 5322 headers, so a CRLF would
	// let one field smuggle extra env lines, SMTP commands, or mail headers.
	// Reject at the boundary — none of these fields legitimately span lines.
	for _, f := range []string{b.Label, b.Host, b.Username, b.Password, b.FromAddress} {
		if strings.ContainsAny(f, "\r\n") {
			return huma.Error422UnprocessableEntity("fields must not contain line breaks")
		}
	}
	switch b.Encryption {
	case store.MailEncryptionNone, store.MailEncryptionSTARTTLS, store.MailEncryptionTLS:
	default:
		return huma.Error422UnprocessableEntity("encryption must be none, starttls, or tls")
	}
	if b.ProviderType == "" {
		b.ProviderType = mailpreset.Custom
	}
	if !mailpreset.Valid(b.ProviderType) {
		return huma.Error422UnprocessableEntity("unknown provider_type")
	}
	return nil
}

// MailPresetDTO is the read shape of one built-in provider preset. It mirrors
// mailpreset.Preset rather than serving that type directly, so the wire shape
// stays owned by this layer and the OpenAPI schema names stay specific — a
// bare "Preset" or "Region" would be far too generic for a shared namespace.
//
// Host is empty when Region is set: the chosen region option carries the host.
type MailPresetDTO struct {
	ID              string               `json:"id"`
	Label           string               `json:"label"`
	Host            string               `json:"host"`
	Port            int                  `json:"port"`
	Encryption      string               `json:"encryption" enum:"none,starttls,tls"`
	UsernameMode    string               `json:"username_mode" enum:"user,fixed,same_as_password"`
	UsernameFixed   string               `json:"username_fixed"`
	UsernamePrefill string               `json:"username_prefill"`
	CredentialLabel string               `json:"credential_label"`
	Help            string               `json:"help"`
	DocsURL         string               `json:"docs_url"`
	Region          *MailPresetRegionDTO `json:"region,omitempty"`
}

// MailPresetRegionDTO is the at-most-one variable a preset carries.
type MailPresetRegionDTO struct {
	Label   string                      `json:"label"`
	Default string                      `json:"default"`
	Options []MailPresetRegionOptionDTO `json:"options"`
}

// MailPresetRegionOptionDTO is one region choice and the SMTP host it means.
type MailPresetRegionOptionDTO struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Host  string `json:"host"`
}

func mailPresetDTO(p mailpreset.Preset) MailPresetDTO {
	d := MailPresetDTO{
		ID: p.ID, Label: p.Label, Host: p.Host, Port: p.Port,
		Encryption: p.Encryption, UsernameMode: p.UsernameMode,
		UsernameFixed: p.UsernameFixed, UsernamePrefill: p.UsernamePrefill,
		CredentialLabel: p.CredentialLabel, Help: p.Help, DocsURL: p.DocsURL,
	}
	if p.Region != nil {
		r := &MailPresetRegionDTO{Label: p.Region.Label, Default: p.Region.Default}
		for _, o := range p.Region.Options {
			r.Options = append(r.Options, MailPresetRegionOptionDTO{Value: o.Value, Label: o.Label, Host: o.Host})
		}
		d.Region = r
	}
	return d
}

// listMailPresets serves the built-in provider table so the add-account form
// can offer a picker instead of seven blank fields. Admin-only, matching
// listMailProviders — only an admin can register a provider, so only an admin
// needs the presets.
func (s *Server) listMailPresets(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Presets []MailPresetDTO `json:"presets"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	out := &struct {
		Body struct {
			Presets []MailPresetDTO `json:"presets"`
		}
	}{}
	out.Body.Presets = []MailPresetDTO{}
	for _, p := range mailpreset.List() {
		out.Body.Presets = append(out.Body.Presets, mailPresetDTO(p))
	}
	return out, nil
}

func (s *Server) listMailProviders(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Providers []MailProviderDTO `json:"providers"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	providers, err := s.store.ListMailProviders()
	if err != nil {
		return nil, huma.Error500InternalServerError("list mail providers failed", err)
	}
	out := &struct {
		Body struct {
			Providers []MailProviderDTO `json:"providers"`
		}
	}{}
	out.Body.Providers = []MailProviderDTO{}
	for _, p := range providers {
		out.Body.Providers = append(out.Body.Providers, mailProviderDTO(p))
	}
	return out, nil
}

// listMailProviderOptions is the non-admin sibling of listMailProviders: id,
// label and provider type only, for the rebind picker on an app detail page (a
// member may rebind their own personal instance, so the names must be readable
// without admin).
// The install dialog's picker rides the install plan instead.
func (s *Server) listMailProviderOptions(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Providers []MailProviderOption `json:"providers"`
	}
}, error) {
	if _, ok := auth.FromContext(ctx); !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	providers, err := s.store.ListMailProviders()
	if err != nil {
		return nil, huma.Error500InternalServerError("list mail providers failed", err)
	}
	out := &struct {
		Body struct {
			Providers []MailProviderOption `json:"providers"`
		}
	}{}
	out.Body.Providers = []MailProviderOption{}
	for _, p := range providers {
		out.Body.Providers = append(out.Body.Providers, MailProviderOption{ID: p.ID, Label: p.Label, ProviderType: p.ProviderType})
	}
	return out, nil
}

func (s *Server) createMailProvider(ctx context.Context, in *struct {
	Body MailProviderBody
}) (*struct{ Body MailProviderDTO }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}
	if err := validateMailProviderBody(&in.Body); err != nil {
		return nil, err
	}

	p := store.MailProvider{
		ID: newID(), Label: in.Body.Label, Host: in.Body.Host, Port: in.Body.Port,
		Username: in.Body.Username, Password: in.Body.Password,
		FromAddress: in.Body.FromAddress, Encryption: in.Body.Encryption,
		ProviderType: in.Body.ProviderType,
		CreatedAt:    time.Now(),
	}
	meta := map[string]any{"label": p.Label, "host": p.Host}
	if err := s.store.CreateMailProvider(p); err != nil {
		s.auditor.Record(ctx, audit.ActionMailProviderCreate, audit.Target{Kind: "mail_provider"}, meta, false)
		if errors.Is(err, store.ErrConflict) {
			return nil, huma.Error409Conflict("a provider with that label already exists")
		}
		return nil, huma.Error500InternalServerError("create mail provider failed", err)
	}
	s.auditor.Record(ctx, audit.ActionMailProviderCreate, audit.Target{Kind: "mail_provider", ID: p.ID}, meta, true)
	return &struct{ Body MailProviderDTO }{Body: mailProviderDTO(p)}, nil
}

func (s *Server) updateMailProvider(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body MailProviderBody
}) (*struct{ Body MailProviderDTO }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}
	if err := validateMailProviderBody(&in.Body); err != nil {
		return nil, err
	}

	existing, err := s.store.GetMailProvider(in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such mail provider")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get mail provider failed", err)
	}

	p := existing
	p.Label, p.Host, p.Port = in.Body.Label, in.Body.Host, in.Body.Port
	p.Username, p.FromAddress, p.Encryption = in.Body.Username, in.Body.FromAddress, in.Body.Encryption
	p.ProviderType = in.Body.ProviderType
	if in.Body.Password != "" {
		p.Password = in.Body.Password
	}
	tgt := audit.Target{Kind: "mail_provider", ID: p.ID}
	meta := map[string]any{"label": p.Label, "host": p.Host}
	if err := s.store.UpdateMailProvider(p); err != nil {
		s.auditor.Record(ctx, audit.ActionMailProviderUpdate, tgt, meta, false)
		if errors.Is(err, store.ErrConflict) {
			return nil, huma.Error409Conflict("a provider with that label already exists")
		}
		return nil, huma.Error500InternalServerError("update mail provider failed", err)
	}

	// A provider edit changes what bound apps should be sending with, but their
	// .env still carries the old values until each is re-stamped. Bound apps
	// pick the change up on their next rebind/recreate; v1 accepts that lag.
	s.auditor.Record(ctx, audit.ActionMailProviderUpdate, tgt, meta, true)
	return &struct{ Body MailProviderDTO }{Body: mailProviderDTO(p)}, nil
}

func (s *Server) deleteMailProvider(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}

	tgt := audit.Target{Kind: "mail_provider", ID: in.ID}
	if err := s.store.DeleteMailProvider(in.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("no such mail provider")
		}
		s.auditor.Record(ctx, audit.ActionMailProviderDelete, tgt, nil, false)
		return nil, huma.Error500InternalServerError("delete mail provider failed", err)
	}
	s.auditor.Record(ctx, audit.ActionMailProviderDelete, tgt, nil, true)
	return nil, nil
}

// verifyMailProviderConfig checks a config the admin has not saved yet, so a
// provider that cannot connect is never created in the first place. It takes
// the same body as create rather than an id for exactly that reason.
//
// Admin-only but not elevation-class: it stores nothing and sends nothing. It
// does not audit for the same reason — nothing is mutated and no mail leaves
// the box, which is what separates it from the test-send next door
// (CLAUDE.md: pure reads don't audit).
func (s *Server) verifyMailProviderConfig(ctx context.Context, in *struct {
	Body MailProviderBody
}) (*struct{}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := validateMailProviderBody(&in.Body); err != nil {
		return nil, err
	}

	p := store.MailProvider{
		Label: in.Body.Label, Host: in.Body.Host, Port: in.Body.Port,
		Username: in.Body.Username, Password: in.Body.Password,
		FromAddress: in.Body.FromAddress, Encryption: in.Body.Encryption,
		ProviderType: in.Body.ProviderType,
	}
	checkCtx, cancel := context.WithTimeout(ctx, testMailTimeout)
	defer cancel()
	if err := verifyMailConfig(checkCtx, p); err != nil {
		return nil, huma.Error502BadGateway(fmt.Sprintf("could not connect: %v%s", err, blockedPortHint(s.profile, p, err)))
	}
	return nil, nil
}

func (s *Server) testMailProvider(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		To string `json:"to"`
	}
}) (*struct{}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	to := strings.TrimSpace(in.Body.To)
	if to == "" || !strings.Contains(to, "@") || strings.ContainsAny(to, "\r\n") {
		return nil, huma.Error422UnprocessableEntity("to must be an email address")
	}

	p, err := s.store.GetMailProvider(in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such mail provider")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get mail provider failed", err)
	}

	tgt := audit.Target{Kind: "mail_provider", ID: p.ID}
	meta := map[string]any{"label": p.Label, "host": p.Host}
	sendCtx, cancel := context.WithTimeout(ctx, testMailTimeout)
	defer cancel()
	if err := sendTestMail(sendCtx, p, to); err != nil {
		s.auditor.Record(ctx, audit.ActionMailProviderTest, tgt, meta, false)
		return nil, huma.Error502BadGateway(fmt.Sprintf("test send failed: %v%s", err, blockedPortHint(s.profile, p, err)))
	}
	s.auditor.Record(ctx, audit.ActionMailProviderTest, tgt, meta, true)
	return nil, nil
}

func (s *Server) setAppMailBinding(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		ProviderID string `json:"provider_id"` // empty unbinds
	}
}) (*struct{ Body Job }, error) {
	id := in.ID
	inst, err := s.authorizeAppMutation(ctx, id)
	if err != nil {
		return nil, err
	}
	tgt := audit.Target{Kind: "app", ID: id}
	providerID := in.Body.ProviderID
	meta := map[string]any{"provider_id": providerID}
	if providerID != "" {
		// Reject a binding on a non-mail app synchronously, mirroring the install
		// path — without this a direct API caller (the UI hides the picker for
		// non-mail apps) gets a 200 + job that only fails later in RebindMail. The
		// manifest comes from the INSTANCE's own persisted copy (#434), not the
		// catalog: the app is already installed. An unreadable copy falls through
		// to RebindMail's backstop, same as getApp's picker enrichment.
		if man, err := s.life.InstanceManifest(inst.ID); err == nil && man.Mail == nil {
			s.auditor.Record(ctx, audit.ActionAppMailRebind, tgt, meta, false)
			return nil, huma.Error422UnprocessableEntity("this app does not support outgoing email")
		}
		if _, err := s.store.GetMailProvider(providerID); errors.Is(err, store.ErrNotFound) {
			s.auditor.Record(ctx, audit.ActionAppMailRebind, tgt, meta, false)
			return nil, huma.Error422UnprocessableEntity("no such mail provider")
		} else if err != nil {
			return nil, huma.Error500InternalServerError("get mail provider failed", err)
		}
	}

	jobCtx := ctx
	job := s.jobs.run("app-mail-rebind", func(job *Job) (map[string]any, error) {
		job.setStep("rebinding")
		err := s.life.RebindMail(context.Background(), id, providerID)
		s.auditor.Record(jobCtx, audit.ActionAppMailRebind, tgt, meta, err == nil)
		if err != nil {
			if errors.Is(err, lifecycle.ErrNoMailSupport) {
				return nil, fmt.Errorf("this app does not support outgoing email")
			}
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// dialError marks a failure to establish the connection at all, as opposed to
// a failure once the server is already talking. blockedPortHint is the one
// consumer that has to tell the two apart, which is what earns the type
// (CLAUDE.md: typed errors at boundaries, not everywhere).
type dialError struct{ err error }

// No "connect:" prefix — the type carries that meaning now, and both callers
// already say it in their own words. Prefixing here produced "could not
// connect: connect: dial tcp ...".
func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// blockedPortHint names the likely cause when a hosted box fails to connect on
// a port its network blocks. Hosted reaches 587 and 2525 only — 25 and 465 get
// no SYN-ACK — so a connect failure there surfaces as a bare timeout that says
// nothing about why (SERVICE_PROVISIONING.md # BYO outgoing mail). An
// appliance on a home LAN reaches 465 fine, so the hint is hosted-only.
func blockedPortHint(prof profile.Profile, p store.MailProvider, err error) string {
	if prof != profile.Hosted {
		return ""
	}
	if p.Port != 25 && p.Port != 465 {
		return ""
	}
	// Only a failure to establish the connection points at the port. An auth
	// or recipient rejection means the port was reachable.
	var de *dialError
	if !errors.As(err, &de) {
		return ""
	}
	return fmt.Sprintf(". This box cannot reach port %d. Use port 587 with STARTTLS, "+
		"which every major provider supports.", p.Port)
}

// sendTestMail dials the provider and delivers a short test message to `to`,
// exercising exactly what a bound app would use: TCP (or implicit TLS) to
// host:port, optional STARTTLS, optional AUTH PLAIN, then one DATA round trip.
// connectMail dials the provider and gets as far as an authenticated session:
// TCP (or implicit TLS), optional STARTTLS, optional AUTH PLAIN. Everything a
// provider can be misconfigured about — wrong host, blocked port, wrong
// encryption mode, bad credentials — fails here, which is why the pre-save
// check and the test-send share it rather than approximating each other.
//
// The caller closes the returned client. The connection is closed with it.
func connectMail(ctx context.Context, p store.MailProvider) (*smtp.Client, error) {
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	var conn net.Conn
	var err error
	dialer := &net.Dialer{}
	if p.Encryption == store.MailEncryptionTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, &dialError{err}
	}
	// The smtp.Client has no context support; the connection deadline bounds
	// every subsequent command instead.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	c, err := smtp.NewClient(conn, p.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if p.Encryption == store.MailEncryptionSTARTTLS {
		if err := c.StartTLS(&tls.Config{ServerName: p.Host}); err != nil {
			c.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	if p.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", p.Username, p.Password, p.Host)); err != nil {
			c.Close()
			return nil, fmt.Errorf("auth: %w", err)
		}
	}
	return c, nil
}

// verifyMailConfig checks a provider end to end short of delivering anything:
// it connects and authenticates, then hangs up. This is what the "Test
// configuration" checkbox on the add form runs, and it deliberately sends no
// message — the admin is validating settings, not mailing someone. The
// test-send on a saved provider stays the stronger check, since only a
// delivered message proves the from address is one the provider will accept.
func verifyMailConfig(ctx context.Context, p store.MailProvider) error {
	c, err := connectMail(ctx, p)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Quit()
}

func sendTestMail(ctx context.Context, p store.MailProvider, to string) error {
	c, err := connectMail(ctx, p)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Mail(p.FromAddress); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: moose test email\r\nDate: %s\r\n\r\n"+
		"This is a test email from your moose box. Outgoing-mail provider %q is working.\r\n",
		p.FromAddress, to, time.Now().Format(time.RFC1123Z), p.Label)
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("send body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return c.Quit()
}
