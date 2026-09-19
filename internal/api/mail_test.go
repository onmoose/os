package api

// API-boundary tests for outgoing-mail providers: admin + elevation fences,
// write-only passwords, audit on success AND failure (the elevation-class
// mutation rule), the test-send path against an in-process SMTP sink, and the
// synchronous pre-checks of the install and rebind wiring. The lifecycle
// effects of a binding (env stamping, recreate) are covered in
// lifecycle_mail_test.go; like the stop/start tests, nothing here lets a job
// body reach the harness's nil Docker driver.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/mailpreset"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

// providerBody returns a valid create/update request body.
func providerBody(label string) map[string]any {
	return map[string]any{
		"label": label, "host": "smtp.example.com", "port": 587,
		"username": "box@example.com", "password": "s3cret-pass",
		"from_address": "box@example.com", "encryption": "starttls",
	}
}

// createProvider creates a provider via the API and returns its DTO.
func (h *harness) createProvider(label string) MailProviderDTO {
	h.t.Helper()
	resp := h.do("POST", "/api/v1/mail-providers", providerBody(label))
	if resp.StatusCode != 200 {
		h.t.Fatalf("create provider %q: %d", label, resp.StatusCode)
	}
	return decodeJSON[MailProviderDTO](h.t, resp)
}

// hasAuditEvent reports whether an audit row with the given action, target id
// (any target when empty) and success flag exists.
func (h *harness) hasAuditEvent(action, targetID string, success bool) bool {
	h.t.Helper()
	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		h.t.Fatalf("list audit: %v", err)
	}
	for _, e := range events {
		if e.Action == action && e.Success == success && (targetID == "" || e.TargetID == targetID) {
			return true
		}
	}
	return false
}

// --- provider CRUD ---------------------------------------------------------

func TestMailProvidersAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	// Bodies must pass huma's schema validation, which runs before the
	// handler's admin fence — an off-schema body would 422 instead of 403.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/mail-providers", nil},
		{"POST", "/api/v1/mail-providers", providerBody("x")},
		{"PUT", "/api/v1/mail-providers/mp_x", providerBody("x")},
		{"DELETE", "/api/v1/mail-providers/mp_x", nil},
		{"POST", "/api/v1/mail-providers/mp_x/test", map[string]string{"to": "a@b.c"}},
	} {
		resp := h.do(c.method, c.path, c.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as member = %d; want 403", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestMailProvidersRequireAuth(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	jar, _ := newJar()
	h.jar = jar

	for _, path := range []string{"/api/v1/mail-providers", "/api/v1/mail-providers/options"} {
		resp := h.do("GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d; want 401", path, resp.StatusCode)
		}
	}
}

func TestCreateMailProviderRequiresElevation(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	// No h.elevate — the write must be rejected.

	resp := h.do("POST", "/api/v1/mail-providers", providerBody("fastmail"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create without elevation = %d; want 403", resp.StatusCode)
	}
}

func TestCreateMailProviderHappyPath(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("POST", "/api/v1/mail-providers", providerBody("fastmail"))
	if resp.StatusCode != 200 {
		t.Fatalf("create = %d; want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Passwords are write-only: no response carries the credential.
	if strings.Contains(string(raw), "s3cret-pass") || strings.Contains(string(raw), "password") {
		t.Fatalf("create response leaks the password: %s", raw)
	}

	list := decodeJSON[struct {
		Providers []MailProviderDTO `json:"providers"`
	}](t, h.do("GET", "/api/v1/mail-providers", nil))
	if len(list.Providers) != 1 {
		t.Fatalf("list after create: got %d providers, want 1", len(list.Providers))
	}
	p := list.Providers[0]
	if p.Label != "fastmail" || p.Host != "smtp.example.com" || p.Port != 587 || p.Encryption != "starttls" {
		t.Fatalf("provider round trip = %+v", p)
	}
	// The store kept the credential even though the API never echoes it.
	stored, err := h.st.GetMailProvider(p.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.Password != "s3cret-pass" {
		t.Fatalf("stored password = %q; want s3cret-pass", stored.Password)
	}
	if !h.hasAuditEvent(audit.ActionMailProviderCreate, p.ID, true) {
		t.Fatal("mail.provider.create success audit event not found")
	}
}

func TestCreateMailProviderDuplicateLabel409(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	h.createProvider("fastmail")

	resp := h.do("POST", "/api/v1/mail-providers", providerBody("fastmail"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate label = %d; want 409", resp.StatusCode)
	}
	if !h.hasAuditEvent(audit.ActionMailProviderCreate, "", false) {
		t.Fatal("mail.provider.create failure audit event not found")
	}
}

func TestCreateMailProviderValidation422(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	for name, mutate := range map[string]func(map[string]any){
		"empty label":    func(b map[string]any) { b["label"] = "  " },
		"empty host":     func(b map[string]any) { b["host"] = "" },
		"port zero":      func(b map[string]any) { b["port"] = 0 },
		"port too big":   func(b map[string]any) { b["port"] = 70000 },
		"bad from":       func(b map[string]any) { b["from_address"] = "not-an-email" },
		"bad encryption": func(b map[string]any) { b["encryption"] = "ssl" },
		// CRLF in any field would smuggle extra .env lines / SMTP commands.
		"crlf in from": func(b map[string]any) { b["from_address"] = "ok@example.com\r\nBcc: evil@x.com" },
		"crlf in host": func(b map[string]any) { b["host"] = "smtp.example.com\nINJECT=1" },
		"crlf in user": func(b map[string]any) { b["username"] = "u\rser" },
	} {
		b := providerBody("v")
		mutate(b)
		resp := h.do("POST", "/api/v1/mail-providers", b)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d; want 422", name, resp.StatusCode)
		}
	}
}

func TestUpdateMailProviderKeepsPasswordWhenEmpty(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	p := h.createProvider("fastmail")

	body := providerBody("fastmail")
	body["host"] = "smtp2.example.com"
	body["password"] = "" // edit without re-entering the credential
	resp := h.do("PUT", "/api/v1/mail-providers/"+p.ID, body)
	if resp.StatusCode != 200 {
		t.Fatalf("update = %d; want 200", resp.StatusCode)
	}
	updated := decodeJSON[MailProviderDTO](t, resp)
	if updated.Host != "smtp2.example.com" {
		t.Fatalf("updated host = %q; want smtp2.example.com", updated.Host)
	}
	stored, _ := h.st.GetMailProvider(p.ID)
	if stored.Password != "s3cret-pass" {
		t.Fatalf("empty update password overwrote stored one: %q", stored.Password)
	}
	if !h.hasAuditEvent(audit.ActionMailProviderUpdate, p.ID, true) {
		t.Fatal("mail.provider.update success audit event not found")
	}
}

func TestUpdateMailProviderLabelConflict409(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	h.createProvider("fastmail")
	b := h.createProvider("gmail")

	resp := h.do("PUT", "/api/v1/mail-providers/"+b.ID, providerBody("fastmail"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("label conflict = %d; want 409", resp.StatusCode)
	}
	if !h.hasAuditEvent(audit.ActionMailProviderUpdate, b.ID, false) {
		t.Fatal("mail.provider.update failure audit event not found")
	}
}

func TestUpdateMailProviderNotFound(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("PUT", "/api/v1/mail-providers/mp_ghost", providerBody("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update ghost = %d; want 404", resp.StatusCode)
	}
}

func TestDeleteMailProviderHappyPath(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	p := h.createProvider("fastmail")

	resp := h.do("DELETE", "/api/v1/mail-providers/"+p.ID, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d; want 204", resp.StatusCode)
	}
	if _, err := h.st.GetMailProvider(p.ID); err != store.ErrNotFound {
		t.Fatalf("provider still in store after delete: %v", err)
	}
	if !h.hasAuditEvent(audit.ActionMailProviderDelete, p.ID, true) {
		t.Fatal("mail.provider.delete success audit event not found")
	}
}

func TestDeleteMailProviderNotFound(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	resp := h.do("DELETE", "/api/v1/mail-providers/mp_ghost", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete ghost = %d; want 404", resp.StatusCode)
	}
}

// --- picker options (non-admin) ---------------------------------------------

func TestListMailProviderOptionsMemberVisible(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	h.createProvider("fastmail")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	resp := h.do("GET", "/api/v1/mail-providers/options", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("options as member = %d; want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// id, label and provider type only: no host, no credential.
	if strings.Contains(string(raw), "smtp.example.com") || strings.Contains(string(raw), "s3cret-pass") {
		t.Fatalf("options response leaks provider details: %s", raw)
	}
	if !strings.Contains(string(raw), "fastmail") {
		t.Fatalf("options missing provider label: %s", raw)
	}
}

// --- test-send ---------------------------------------------------------------

// smtpSink is a minimal in-process SMTP server, good for one transaction:
// greet, accept EHLO/MAIL/RCPT/DATA, record the envelope.
type smtpSink struct {
	addr             string
	mu               sync.Mutex
	from, rcpt, data string
}

func startSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sink listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &smtpSink{addr: ln.Addr().String()}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		fmt.Fprintf(conn, "220 sink ESMTP\r\n")
		inData := false
		var data strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					s.mu.Lock()
					s.data = data.String()
					s.mu.Unlock()
					fmt.Fprintf(conn, "250 ok\r\n")
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			switch cmd := strings.ToUpper(line); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				fmt.Fprintf(conn, "250 sink\r\n")
			case strings.HasPrefix(cmd, "MAIL FROM:"):
				s.mu.Lock()
				s.from = line
				s.mu.Unlock()
				fmt.Fprintf(conn, "250 ok\r\n")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				s.mu.Lock()
				s.rcpt = line
				s.mu.Unlock()
				fmt.Fprintf(conn, "250 ok\r\n")
			case cmd == "DATA":
				inData = true
				fmt.Fprintf(conn, "354 go\r\n")
			case cmd == "QUIT":
				fmt.Fprintf(conn, "221 bye\r\n")
				return
			default:
				fmt.Fprintf(conn, "250 ok\r\n")
			}
		}
	}()
	return s
}

func TestTestMailProviderDeliversThroughSink(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	sink := startSMTPSink(t)
	host, portStr, _ := net.SplitHostPort(sink.addr)
	port, _ := strconv.Atoi(portStr)

	resp := h.do("POST", "/api/v1/mail-providers", map[string]any{
		"label": "sink", "host": host, "port": port,
		"from_address": "moose@example.com", "encryption": "none",
	})
	p := decodeJSON[MailProviderDTO](t, resp)

	resp = h.do("POST", "/api/v1/mail-providers/"+p.ID+"/test", map[string]string{
		"to": "admin@example.com",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("test send = %d; want 204", resp.StatusCode)
	}

	sink.mu.Lock()
	from, rcpt, data := sink.from, sink.rcpt, sink.data
	sink.mu.Unlock()
	if !strings.Contains(from, "moose@example.com") {
		t.Errorf("sink MAIL FROM = %q; want moose@example.com", from)
	}
	if !strings.Contains(rcpt, "admin@example.com") {
		t.Errorf("sink RCPT TO = %q; want admin@example.com", rcpt)
	}
	if !strings.Contains(data, "moose test email") {
		t.Errorf("sink DATA missing subject: %q", data)
	}
	if !h.hasAuditEvent(audit.ActionMailProviderTest, p.ID, true) {
		t.Fatal("mail.provider.test success audit event not found")
	}
}

func TestTestMailProviderUnreachable502(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	// A port that was just listening and is now closed: connect refuses fast.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	port, _ := strconv.Atoi(portStr)

	resp := h.do("POST", "/api/v1/mail-providers", map[string]any{
		"label": "dead", "host": host, "port": port,
		"from_address": "moose@example.com", "encryption": "none",
	})
	p := decodeJSON[MailProviderDTO](t, resp)

	resp = h.do("POST", "/api/v1/mail-providers/"+p.ID+"/test", map[string]string{
		"to": "admin@example.com",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("test send to dead host = %d; want 502", resp.StatusCode)
	}
	if !h.hasAuditEvent(audit.ActionMailProviderTest, p.ID, false) {
		t.Fatal("mail.provider.test failure audit event not found")
	}
}

func TestTestMailProviderValidation(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	p := h.createProvider("fastmail")

	resp := h.do("POST", "/api/v1/mail-providers/mp_ghost/test", map[string]string{"to": "a@b.c"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("test ghost provider = %d; want 404", resp.StatusCode)
	}

	resp = h.do("POST", "/api/v1/mail-providers/"+p.ID+"/test", map[string]string{"to": "not-an-email"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("test bad to = %d; want 422", resp.StatusCode)
	}
}

// --- app binding (synchronous fences) ----------------------------------------

func TestSetAppMailBindingRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	jar, _ := newJar()
	h.jar = jar

	resp := h.do("PUT", "/api/v1/apps/i1/mail-binding", map[string]string{"provider_id": "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rebind = %d; want 401", resp.StatusCode)
	}
}

func TestSetAppMailBindingUnknownApp404(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	resp := h.do("PUT", "/api/v1/apps/ghost/mail-binding", map[string]string{"provider_id": ""})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rebind unknown app = %d; want 404", resp.StatusCode)
	}
}

func TestSetAppMailBindingMemberCannotControlHousehold(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")
	h.seedInstance("i1", "whoami", "whoami", "u_admin", store.ScopeHousehold)

	resp := h.do("PUT", "/api/v1/apps/i1/mail-binding", map[string]string{"provider_id": "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member rebind household = %d; want 403", resp.StatusCode)
	}
}

func TestSetAppMailBindingUnknownProvider422(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.seedInstance("i1", "mailer", "mailer", "u_admin", store.ScopeHousehold)

	resp := h.do("PUT", "/api/v1/apps/i1/mail-binding", map[string]string{"provider_id": "mp_ghost"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rebind to ghost provider = %d; want 422", resp.StatusCode)
	}
	if !h.hasAuditEvent(audit.ActionAppMailRebind, "i1", false) {
		t.Fatal("app.mail.rebind failure audit event not found")
	}
}

// A member CAN reach the rebind endpoint for their own personal instance: the
// unknown-provider 422 (not 403/404) proves authorization passed.
func TestSetAppMailBindingMemberOwnPersonal(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")
	h.seedInstance("i1", "mailer", "mailer", "u_bob", store.ScopePersonal)

	resp := h.do("PUT", "/api/v1/apps/i1/mail-binding", map[string]string{"provider_id": "mp_ghost"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("member rebind own personal = %d; want 422", resp.StatusCode)
	}
}

// Rebinding a non-mail app is rejected synchronously (422), mirroring the
// install path — a direct API caller must not get a 200 + a job that only
// fails later inside RebindMail.
func TestSetAppMailBindingNonMailApp422(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.seedInstance("i1", "whoami", "whoami", "u_admin", store.ScopeHousehold)
	// The check reads the INSTANCE's persisted manifest, not the catalog (#434),
	// and the catalog is left empty here to prove it: an app already installed
	// must not need the catalog service to answer this.
	h.seedInstanceManifest("i1", minimalManifestYML)

	resp := h.do("PUT", "/api/v1/apps/i1/mail-binding", map[string]string{"provider_id": "mp_x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rebind non-mail app = %d; want 422", resp.StatusCode)
	}
	if !h.hasAuditEvent(audit.ActionAppMailRebind, "i1", false) {
		t.Fatal("app.mail.rebind failure audit event not found")
	}
}

// --- install pre-checks --------------------------------------------------------

const mailManifestYML = `id: mailer
manifest_version: 1
name: Mailer
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
mail:
  optional: true
`

func TestInstallMailProviderOnNonMailApp422(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "whoami", minimalManifestYML)
	h.setupAdmin("alice", "pass1")

	resp := h.do("POST", "/api/v1/apps", map[string]any{
		"manifest_id": "whoami",
		"config":      map[string]any{"mail_provider_id": "mp_x"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("install non-mail app with provider = %d; want 422", resp.StatusCode)
	}
	if !h.hasAuditEvent(audit.ActionAppInstall, "", false) {
		t.Fatal("app.install failure audit event not found")
	}
}

func TestInstallUnknownMailProvider422(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "mailer", mailManifestYML)
	h.setupAdmin("alice", "pass1")

	resp := h.do("POST", "/api/v1/apps", map[string]any{
		"manifest_id": "mailer",
		"config":      map[string]any{"mail_provider_id": "mp_ghost"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("install with ghost provider = %d; want 422", resp.StatusCode)
	}
	if !h.hasAuditEvent(audit.ActionAppInstall, "", false) {
		t.Fatal("app.install failure audit event not found")
	}
}

// The install plan advertises the mail block + registered providers so the
// install dialog can render the picker without an extra request.
func TestInstallPlanCarriesMailProviders(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "mailer", mailManifestYML)
	writeManifestFixture(t, h.catalogDir, "whoami", minimalManifestYML)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")
	p := h.createProvider("fastmail")

	resp := h.do("GET", "/api/v1/catalog/mailer/install-plan", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("install plan = %d; want 200", resp.StatusCode)
	}
	plan := decodeJSON[InstallPlanDTO](t, resp)
	if plan.Mail == nil {
		t.Fatal("install plan mail block missing for mail-capable app")
	}
	if !plan.Mail.Optional {
		t.Error("install plan mail.optional = false; want true")
	}
	if len(plan.Mail.Providers) != 1 || plan.Mail.Providers[0].ID != p.ID || plan.Mail.Providers[0].Label != "fastmail" {
		t.Errorf("install plan mail.providers = %+v; want [{%s fastmail}]", plan.Mail.Providers, p.ID)
	}

	// A non-mail app's plan omits the block entirely.
	resp = h.do("GET", "/api/v1/catalog/whoami/install-plan", nil)
	plan = decodeJSON[InstallPlanDTO](t, resp)
	if plan.Mail != nil {
		t.Errorf("non-mail app install plan carries mail block: %+v", plan.Mail)
	}
}

// --- provider presets ------------------------------------------------------

func TestListMailPresetsAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	got := decodeJSON[struct {
		Presets []MailPresetDTO `json:"presets"`
	}](t, h.do("GET", "/api/v1/mail-presets", nil))
	if len(got.Presets) == 0 {
		t.Fatal("no presets served")
	}
	var hasCustom bool
	for _, p := range got.Presets {
		if p.ID == mailpreset.Custom {
			hasCustom = true
		}
	}
	if !hasCustom {
		t.Error("custom missing from the preset list")
	}

	// A member cannot register a provider, so a member cannot read the presets.
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")
	resp := h.do("GET", "/api/v1/mail-presets", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member GET /mail-presets = %d; want 403", resp.StatusCode)
	}
}

func TestCreateMailProviderPersistsProviderType(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	b := providerBody("ses account")
	b["provider_type"] = "ses"
	p := decodeJSON[MailProviderDTO](t, h.do("POST", "/api/v1/mail-providers", b))
	if p.ProviderType != "ses" || p.ProviderLabel != "Amazon SES" {
		t.Fatalf("provider_type round trip = %+v", p)
	}
	stored, err := h.st.GetMailProvider(p.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	if stored.ProviderType != "ses" {
		t.Fatalf("stored provider_type = %q; want ses", stored.ProviderType)
	}
	// D3: the server stores what the client sent. It does not re-derive the
	// host from the preset, so an advanced override survives.
	if stored.Host != "smtp.example.com" {
		t.Fatalf("stored host = %q; the server re-derived it from the preset", stored.Host)
	}
}

func TestCreateMailProviderDefaultsProviderTypeToCustom(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	// An older client sends no provider_type at all.
	p := h.createProvider("hand typed")
	if p.ProviderType != mailpreset.Custom {
		t.Fatalf("provider_type = %q; want custom", p.ProviderType)
	}
}

func TestMailProviderUnknownTypeIs422(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	b := providerBody("bogus")
	b["provider_type"] = "not-a-provider"
	resp := h.do("POST", "/api/v1/mail-providers", b)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown provider_type on create = %d; want 422", resp.StatusCode)
	}

	p := h.createProvider("real")
	up := providerBody("real")
	up["provider_type"] = "not-a-provider"
	resp = h.do("PUT", "/api/v1/mail-providers/"+p.ID, up)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown provider_type on update = %d; want 422", resp.StatusCode)
	}
}

// A hosted box cannot reach 465, so the bare connection timeout an admin used
// to see is replaced by an error that names the port. The appliance reaches
// 465 fine and must keep the plain error.
func TestBlockedPortHint(t *testing.T) {
	tls465 := store.MailProvider{Port: 465}
	starttls587 := store.MailProvider{Port: 587}
	connectErr := &dialError{fmt.Errorf("dial tcp 1.2.3.4:465: i/o timeout")}
	authErr := fmt.Errorf("auth: 535 bad credentials")

	if got := blockedPortHint(profile.Hosted, tls465, connectErr); got == "" {
		t.Error("hosted connect failure on 465 got no hint")
	} else if !strings.Contains(got, "587") {
		t.Errorf("hint does not name the working port: %q", got)
	}
	if got := blockedPortHint(profile.Appliance, tls465, connectErr); got != "" {
		t.Errorf("appliance got a hint it should not: %q", got)
	}
	if got := blockedPortHint(profile.Hosted, starttls587, connectErr); got != "" {
		t.Errorf("port 587 got a blocked-port hint: %q", got)
	}
	// The port was reachable — auth failed. Naming the port would mislead.
	if got := blockedPortHint(profile.Hosted, tls465, authErr); got != "" {
		t.Errorf("auth failure got a blocked-port hint: %q", got)
	}
}

// The hint keys off a typed error, so this asserts the live path still produces
// one: a hand-built error in the test above would keep passing even if
// sendTestMail stopped reporting dial failures as dialError, and the hint would
// silently stop firing for the hosted admin it exists for.
func TestSendTestMailReportsDialFailuresAsDialError(t *testing.T) {
	// A port that was just listening and is now closed: connect refuses fast.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	p := store.MailProvider{
		Host: host, Port: port, FromAddress: "box@example.com",
		Encryption: store.MailEncryptionNone,
	}
	ctx, cancel := context.WithTimeout(context.Background(), testMailTimeout)
	defer cancel()
	err = sendTestMail(ctx, p, "admin@example.com")
	if err == nil {
		t.Fatal("send to a closed port succeeded")
	}
	var de *dialError
	if !errors.As(err, &de) {
		t.Fatalf("dial failure = %T (%v); want *dialError, which blockedPortHint keys off", err, err)
	}
}

// --- pre-save config check -------------------------------------------------

func TestVerifyMailProviderConfigConnectsWithoutSending(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	sink := startSMTPSink(t)
	host, portStr, _ := net.SplitHostPort(sink.addr)
	port, _ := strconv.Atoi(portStr)

	resp := h.do("POST", "/api/v1/mail-providers/verify", map[string]any{
		"label": "sink", "host": host, "port": port,
		"from_address": "moose@example.com", "encryption": "none",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("verify = %d; want 204", resp.StatusCode)
	}

	// The whole point of the check: it proves the connection without mailing
	// anyone, so the sink must have taken no message.
	sink.mu.Lock()
	from, rcpt, data := sink.from, sink.rcpt, sink.data
	sink.mu.Unlock()
	if from != "" || rcpt != "" || data != "" {
		t.Fatalf("verify delivered a message: from=%q rcpt=%q data=%q", from, rcpt, data)
	}

	// It stores nothing either — a config that checks out is still not an account.
	list, err := h.st.ListMailProviders()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("verify created %d providers; want 0", len(list))
	}
}

func TestVerifyMailProviderConfigUnreachable502(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	// A port that was just listening and is now closed: connect refuses fast.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	port, _ := strconv.Atoi(portStr)

	resp := h.do("POST", "/api/v1/mail-providers/verify", map[string]any{
		"label": "dead", "host": host, "port": port,
		"from_address": "moose@example.com", "encryption": "none",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("verify against a closed port = %d; want 502", resp.StatusCode)
	}
}

func TestVerifyMailProviderConfigGuards(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	// Same validation as create — a bad body is a 422, not a dial attempt.
	bad := providerBody("v")
	bad["from_address"] = "not-an-email"
	resp := h.do("POST", "/api/v1/mail-providers/verify", bad)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid body = %d; want 422", resp.StatusCode)
	}

	// Admin-only: a member cannot register a provider, so cannot probe with one.
	h.addMember("u_bob", "bob", "bobpass")
	h.loginAs("bob", "bobpass")
	resp = h.do("POST", "/api/v1/mail-providers/verify", providerBody("v"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member verify = %d; want 403", resp.StatusCode)
	}
}

// The mail picker on an installed app's detail page is driven by the INSTANCE's
// own persisted manifest (#434). The catalog is empty here, which is what a box
// looks like before its first sync, or after the app is unpublished: the picker
// must still appear, because the binding it edits still works.
func TestGetAppMailSupportedComesFromTheInstanceManifest(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")

	h.seedInstance("i_mail", "mailer", "mailer", "u_admin", store.ScopeHousehold)
	h.seedInstanceManifest("i_mail", mailManifestYML)
	h.seedInstance("i_plain", "whoami", "whoami", "u_admin", store.ScopeHousehold)
	h.seedInstanceManifest("i_plain", minimalManifestYML)

	for _, tc := range []struct {
		id   string
		want bool
	}{{"i_mail", true}, {"i_plain", false}} {
		resp := h.do("GET", "/api/v1/apps/"+tc.id, nil)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("GET %s = %d; want 200", tc.id, resp.StatusCode)
		}
		var dto InstanceDTO
		if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
			t.Fatalf("decode %s: %v", tc.id, err)
		}
		resp.Body.Close()
		if dto.MailSupported != tc.want {
			t.Errorf("%s mail_supported = %v, want %v (read from the persisted manifest, catalog is empty)",
				tc.id, dto.MailSupported, tc.want)
		}
	}
}
