# Mail enum value maps: manifest-declared `mail.env`, brain-resolved tokens

- **Status:** done
- **Date:** 2026-11-12
- **Specs touched:** `APP_MANIFEST.md` (# D3: new `mail.env` subsection), `SERVICE_PROVISIONING.md` (# BYO outgoing mail: enum-config paragraph), `DECISIONS.md` (2026-11-12 entry for the unbound rule), `docs/dev/catalog-import-gaps.md` (the `mail-enable-enum` entry for twenty flipped to implemented), `docs/dev/capabilities.yml` (new id `mail-enable-enum`, version 2→3)

Closes #302, extending [byo-outgoing-mail.md](byo-outgoing-mail.md) (#122). BYO mail injects moose-canonical `MOOSE_MAIL_*` and relies on the app's compose to adapt them. Compose `environment:` interpolation can only substitute, default (`:-`), or test presence (`:+`). It cannot **remap one enum's tokens onto another's**. Two shipped apps are degraded purely by this: twenty's `EMAIL_DRIVER` (`logger|smtp`, which boot-rejects the empty string the `${MOOSE_MAIL_HOST:+smtp}` gate expands to on an unbound box) and vaultwarden's `SMTP_SECURITY` (`off|starttls|force_tls`, a value remap off moose's `none|starttls|tls`). This generalizes the projection the brain already hand-computes for `MOOSE_MAIL_USE_TLS`/`USE_SSL`/`DSN` (which `mail.go` does in Go because compose cannot derive these from the string) into a manifest-declared value map. The change to the unbound rule is recorded in `DECISIONS.md` 2026-11-12.

## What was done

### Manifest (`internal/manifest/manifest.go`)

- `Mail` gains `Env map[string]MailEnvMap`; new `MailEnvMap{From, Map}`. Each key is the app's own env-var name. `from` names a mail-state domain and `map` gives the app's token for each domain value.
- Two domains: `encryption` (`none|starttls|tls`, the bound provider's mode, treated as `none` when unbound) and the synthetic `bound` (`bound|unbound`). Domain names and values are exported constants (`MailFromEncryption`, `MailFromBound`, `MailBound`, `MailUnbound`) so lifecycle resolution references them, not string literals. The `encryption` values stay literals here, not imported from `store`, so the manifest layer keeps no `store` dependency; a lifecycle test guards the two vocabularies against drift.
- `validateMail` extended. Each env name is validated exactly like a config `app_env`: it reuses `configEnvName` plus the `MOOSE_` and `reservedConfigEnv` guards, because the token lands in `.env` under the app's own name. It also rejects a name that is already a config `app_env`, since the config value lands in the compose override and would silently win over the `.env` line. `from` must be a known domain, and `map` must cover that domain **exactly** (right count, every key present, a non-empty single-word token) so resolution never writes an empty or broken env line. A token is letters, digits, or `_ . : + -`, so `.env` and compose substitution pass it through unchanged.

### Lifecycle (`internal/lifecycle/mail.go`, `lifecycle.go`)

- New `mailAppEnvLines(mail, bound)` resolves each declared var to its token for the current state and renders `APP_VAR=token`, with keys sorted for a byte-stable `.env`. `bound == nil` (unbound) resolves the `none`/`unbound` tokens. An unknown `from`, a missing token, or an empty token is skipped, never stamped as `VAR=`.
- **Emitted in both states**, which is the load-bearing change. Unlike `MOOSE_MAIL_*` (bound-only), the declared vars are stamped whether or not a provider is bound, so an enum-driver app is present and valid unbound instead of boot-rejecting on an empty value. `writeEnv` (install, update, folder change) and `rewriteEnvMail` (rebind) take the manifest's `Mail` block and append `mailAppEnvLines` after the `MOOSE_MAIL_*` block.
- `rewriteEnvMail` now strips the declared vars **by name** (not just the `MOOSE_MAIL_` prefix) before re-resolving, so a rebind re-stamps cleanly with no duplicate lines. That includes an unbind, which drops `MOOSE_MAIL_*` but keeps the enum vars with their unbound tokens. A declared name that resolves to no token keeps its previous line, so a stale manifest or a future state never leaves the app's pass-through variable empty. The account-delete restamp loads the manifest when it can. When it cannot, the provider credentials still leave the app and the declared vars keep their values (`TestRestampMailWithoutManifest`).

## Verification

- `internal/manifest`: `mail.env` maps parse for both domains. `validateMail` rejects unknown `from`, an incomplete domain, an extra key, a misspelled key at the right count, an empty token, a token with a line break, a `$`, or a space, a `MOOSE_`-prefixed name, a lowercase name, a reserved runtime name (`PATH`), and a name that is also a config `app_env`.
- `internal/lifecycle`: install unbound stamps `EMAIL_DRIVER=logger` + `SMTP_SECURITY=off` and no `MOOSE_MAIL_*`; install bound stamps `smtp`/`force_tls` alongside the `MOOSE_MAIL_*` family; rebind bound→unbound→bound re-stamps the right tokens each way with exactly one line per var (no duplicates); a unit test drives `mailAppEnvLines` across all three encryption modes, guards the manifest-literal to `store.MailEncryption*` alignment, and checks that an empty `mail.env`, an unknown `from`, a missing map, and an empty token yield no lines; a declared name with no resolvable token keeps its old line (`TestRewriteEnvMailKeepsUnresolvedDeclaredVar`); a restamp with no manifest still strips the provider lines and keeps the declared var (`TestRestampMailWithoutManifest`).
- `make check` green.

## Known gaps & deviations

- **Catalog/store manifest wiring is a coordinated follow-up, not in this repo.** The `catalog/` tree was cut over to the store repo (`07aac3a`), so the store repo's `apps/twenty` (`EMAIL_DRIVER`) and `apps/vaultwarden` (`SMTP_SECURITY`, its `outbound-mail-enum-mismatch` limitation), plus the issue's live send checks for both, ship separately against this brain mechanism. The ledger entry is flipped to `implemented` per the "mechanism shipped" convention and names both apps for the revisit.
- **Validation of `mail.env` keys is slightly beyond the issue's literal list**, which named only the domain-coverage and unknown-`from` rules. Kept deliberately: these vars land in the same `.env` as `MOOSE_MAIL_*`, so a `MOOSE_`-prefixed or loader-var key could clobber a brain-owned or process-critical value, and a name shared with a config field would be silently overridden. The guard reuses the existing config `app_env` validator.
- **Compose still needs a pass-through line** (`EMAIL_DRIVER: ${EMAIL_DRIVER}`) in the app's compose. The remap happens in Go; the compose just forwards the resolved value. That line is part of the store-repo manifest work above.

## What's next

- Wire the store repo's `apps/twenty` (`EMAIL_DRIVER` via `from: bound`) and `apps/vaultwarden` (`SMTP_SECURITY` via `from: encryption`), flip vaultwarden's `outbound-mail-enum-mismatch` and twenty's row toward `full` after a live send check.
- The `capabilities.yml` id `mail-enable-enum` now fires the mechanical re-screen of any app that recorded a wait on that gap class (`CAPABILITIES.md`).
