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
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/onmoose/os/internal/manifest"
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

// mailAppEnvLines resolves each manifest-declared mail.env var to its token for
// the box's current mail state and renders it as `APP_VAR=token` (#302). Unlike
// the MOOSE_MAIL_* family, injected only when bound, these are emitted in both
// states, so an app whose mail switch is a boot-validated enum is present and
// valid even unbound (bound == nil): `encryption` reads none, `bound` reads
// unbound. Keys are sorted so a rewrite produces a byte-stable .env.
func mailAppEnvLines(mail *manifest.Mail, bound *store.MailProvider) []string {
	if mail == nil || len(mail.Env) == 0 {
		return nil
	}
	names := make([]string, 0, len(mail.Env))
	for name := range mail.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		em := mail.Env[name]
		var domainValue string
		switch em.From {
		case manifest.MailFromEncryption:
			domainValue = store.MailEncryptionNone // unbound ⇒ no encryption
			if bound != nil {
				domainValue = bound.Encryption
			}
		case manifest.MailFromBound:
			domainValue = manifest.MailUnbound
			if bound != nil {
				domainValue = manifest.MailBound
			}
		default:
			// validateMail rejects an unknown from at parse, so this is only
			// reachable for a hand-built struct. Skip it rather than stamping
			// VAR= (the empty value this mechanism exists to prevent).
			continue
		}
		token, ok := em.Map[domainValue]
		if !ok || token == "" {
			// Parse validated every map, so this only happens to a manifest
			// edited on disk. Leave the name out of the fresh lines; its old
			// line is kept by rewriteEnvMail rather than replaced with VAR=.
			continue
		}
		lines = append(lines, name+"="+token)
	}
	return lines
}

// MailEnvChanged reports whether two versions of an account give an app
// different MOOSE_MAIL_* lines. The API uses it to decide whether an edit must
// re-stamp the apps bound to the account: a label or preset change does not.
func MailEnvChanged(a, b store.MailProvider) bool {
	return !slices.Equal(mailEnvLines(a), mailEnvLines(b))
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
	if err := m.rewriteEnvMail(id, man.Mail); err != nil {
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

// instanceMail returns the mail block of the manifest the installer persisted,
// or nil when that copy cannot be read. A nil block still lets rewriteEnvMail
// strip the provider credentials, which an account delete must do even when the
// manifest is gone; the app's own declared vars keep their values.
func (m *Manager) instanceMail(id string) *manifest.Mail {
	man, err := m.loadInstanceManifest(id)
	if err != nil {
		slog.Warn("mail lines rewritten without the manifest; declared mail vars keep their values",
			"instance_id", id, "err", err)
		return nil
	}
	return man.Mail
}

// rewriteEnvMail re-stamps only the mail-owned lines of an instance's .env from
// the current binding: the MOOSE_MAIL_* family plus any manifest-declared
// mail.env vars (#302). Every other line stays byte-identical, so unlike a full
// writeEnv it needs no install-time isolation state and a stable secret cannot
// be re-rolled by accident. The declared vars are stamped under the app's own
// names, so they are stripped by name (not the MOOSE_MAIL_ prefix) and
// re-resolved for the new state, including unbound. Unbound drops the
// MOOSE_MAIL_* family but keeps the declared vars with their unbound tokens. A
// declared name with no resolvable token keeps its previous line, so a manifest
// edited on disk never leaves the app's pass-through variable empty.
func (m *Manager) rewriteEnvMail(id string, mail *manifest.Mail) error {
	path := filepath.Join(m.instanceDir(id), ".env")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	declared := map[string]bool{}
	var declaredNames []string
	if mail != nil {
		for name := range mail.Env {
			declared[name] = true
			declaredNames = append(declaredNames, name)
		}
		sort.Strings(declaredNames)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	oldDeclared := map[string]string{}
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "MOOSE_MAIL_") {
			continue
		}
		if name, _, ok := strings.Cut(l, "="); ok && declared[name] {
			oldDeclared[name] = l
			continue
		}
		kept = append(kept, l)
	}
	mp, err := m.store.GetInstanceMailProvider(id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var bound *store.MailProvider
	if err == nil {
		bound = &mp
		kept = append(kept, mailEnvLines(mp)...)
	}
	fresh := mailAppEnvLines(mail, bound)
	resolved := make(map[string]bool, len(fresh))
	for _, l := range fresh {
		if name, _, ok := strings.Cut(l, "="); ok {
			resolved[name] = true
		}
	}
	// A declared name with no fresh line (a manifest edited on disk, or a
	// future state the resolver does not know) keeps the line it already had.
	// Dropping it would leave the app's pass-through variable empty, the boot
	// reject this mechanism exists to prevent.
	for _, name := range declaredNames {
		if !resolved[name] {
			if old, ok := oldDeclared[name]; ok {
				kept = append(kept, old)
			}
		}
	}
	kept = append(kept, fresh...)
	env := strings.Join(append(kept, ""), "\n")
	return os.WriteFile(path, []byte(env), 0o644)
}
