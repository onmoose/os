package lifecycle

// BYO outgoing mail (SERVICE_PROVISIONING.md # BYO outgoing mail). The brain
// injects a user's SMTP account into a bound app's .env as
// MOOSE_MAIL_* — writeEnv stamps it at install, RebindMail re-stamps it later.
// No moose-run relay: the app dials the provider itself over its declared
// internet permission.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/onmoose/os/internal/store"
)

// mailEnvLines renders a provider as the MOOSE_MAIL_* env lines: the discrete
// fields plus a Symfony-style DSN, since apps differ in what they consume.
func mailEnvLines(p store.MailProvider) []string {
	return []string{
		"MOOSE_MAIL_HOST=" + p.Host,
		"MOOSE_MAIL_PORT=" + strconv.Itoa(p.Port),
		"MOOSE_MAIL_USER=" + p.Username,
		"MOOSE_MAIL_PASSWORD=" + p.Password,
		"MOOSE_MAIL_FROM=" + p.FromAddress,
		"MOOSE_MAIL_ENCRYPTION=" + p.Encryption,
		// Boolean projections of the encryption mode, for apps that take two
		// separate flags rather than the string or DSN — STARTTLS vs implicit
		// TLS as distinct booleans (e.g. Django's EMAIL_USE_TLS / EMAIL_USE_SSL,
		// which Paperless surfaces). Compose can't derive these from the string.
		"MOOSE_MAIL_USE_TLS=" + strconv.FormatBool(p.Encryption == store.MailEncryptionSTARTTLS),
		"MOOSE_MAIL_USE_SSL=" + strconv.FormatBool(p.Encryption == store.MailEncryptionTLS),
		"MOOSE_MAIL_DSN=" + mailDSN(p),
	}
}

// mailDSN renders a provider as a Symfony-style SMTP URL
// (smtp[s]://user:pass@host:port). Implicit TLS is the smtps scheme; starttls
// and none both stay smtp:// — SMTP URL consumers negotiate STARTTLS
// opportunistically, and apps needing the exact mode read the discrete
// MOOSE_MAIL_ENCRYPTION var instead. url.URL escapes credentials, so a
// password with @ / : survives the round trip.
func mailDSN(p store.MailProvider) string {
	u := url.URL{Scheme: "smtp", Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port))}
	if p.Encryption == store.MailEncryptionTLS {
		u.Scheme = "smtps"
	}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

// RebindMail changes (or, with providerID == "", clears) an instance's
// outgoing-mail binding and re-stamps the MOOSE_MAIL_* lines in its .env. A
// running instance's containers are recreated with `compose up -d` — env is
// read only at container create — so the change takes effect immediately; a
// stopped instance picks it up on its next Start (same op). Brain commits
// first: if the recreate fails the binding and .env already hold the desired
// state and the instance is marked pending-recreate, so the reconcile pass
// brings the container to the new binding — whether it fell over or kept running
// on stale env (the already-up path retries while the marker is set, #268).
func (m *Manager) RebindMail(ctx context.Context, id, providerID string) error {
	defer m.lockInstance(id)()
	inst, err := m.store.Get(id)
	if err != nil {
		return err
	}
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	// Same backstop as install: only mail-declaring apps can be bound (the API
	// validates first; this keeps the transaction owner authoritative).
	if providerID != "" && man.Mail == nil {
		return fmt.Errorf("%w: %s", ErrNoMailSupport, man.ID)
	}

	if providerID == "" {
		if err := m.store.DeleteInstanceMailBinding(id); err != nil {
			return fmt.Errorf("unbind mail provider: %w", err)
		}
	} else if err := m.store.SetInstanceMailBinding(id, providerID); err != nil {
		return fmt.Errorf("bind mail provider: %w", err)
	}
	if err := m.rewriteEnvMail(id); err != nil {
		return fmt.Errorf("rewrite env: %w", err)
	}

	if inst.State != "running" {
		slog.Info("app mail binding updated (applies at next start)",
			"instance_id", id, "name", inst.Name)
		return nil
	}
	if err := m.recreateRunning(ctx, inst); err != nil {
		return err
	}
	slog.Info("app mail binding updated", "instance_id", id, "name", inst.Name)
	return nil
}

// rewriteEnvMail re-stamps only the MOOSE_MAIL_* lines of an instance's .env
// from the current binding, leaving every other line byte-identical — unlike a
// full writeEnv it needs no install-time isolation state, and a stable secret
// can't be re-rolled by accident.
func (m *Manager) rewriteEnvMail(id string) error {
	path := filepath.Join(m.instanceDir(id), ".env")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(l, "MOOSE_MAIL_") {
			kept = append(kept, l)
		}
	}
	mp, err := m.store.GetInstanceMailProvider(id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil {
		kept = append(kept, mailEnvLines(mp)...)
	}
	env := strings.Join(append(kept, ""), "\n")
	return os.WriteFile(path, []byte(env), 0o644)
}
