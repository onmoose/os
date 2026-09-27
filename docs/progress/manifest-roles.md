# Manifest roles, separators and requires

- **Status:** done
- **Date:** 2026-09-26
- **Specs touched:** docs/specs/APP_MANIFEST.md, docs/specs/BRAIN_UI_PROTOCOL.md, docs/specs/INSTALL_SETUP.md, docs/architecture.md

## What was done

The first piece of step 4 of the install setup plan (`INSTALL_SETUP.md` # Suggested order), after [ai-provider-data.md](ai-provider-data.md), which brought the AI provider data onto the box. This piece adds the manifest schema that says what a config field means, and the rule that an app needs at least one of several fields. It follows the 2026-09-26 rows in the `INSTALL_SETUP.md` decisions table. Nothing fills a slot yet.

**Schema** (`internal/manifest/manifest.go`, `internal/manifest/roles.go`). `ConfigField` gains `role` and `separator`, and `Manifest` gains `requires: [{one_of: [...]}]`.

- A role is `<kind>.<protocol>.<attribute>` in lowercase segments. Kind is a closed list (`ai`). Protocol is open, with `openai_compatible` reserved. Attribute is `api_key`, `base_url`, `model.<type>` or `models.<type>`. `ParseRole` parses one and says what is wrong in plain words.
- The model-type list moved here from `internal/catalog`, as `manifest.IsModelType`. The provider data reader now uses it, so a role and a provider's models speak the same types.
- `separator` is a `*string`, so an explicit `""` can be told from absent. `EffectiveSeparator` gives the declared one or `,`.
- A `requires` member is a kind, a slot or an `app_env`. `GroupFields` returns the fields that can meet a group, and `GroupSatisfied` checks a values map (app_env to value). A kind or slot member matches a field whose role starts with it plus `.`; model fields never count. The check needs no vocabulary: a well-formed role with an unknown kind still counts.

**Leniency.** `Parse` never fails over the new keys, because the brain parses every catalog manifest and every stored instance manifest with it.

- `ConfigField` has its own `UnmarshalYAML`: a `role` or `separator` that is not a single scalar (a list, a mapping) is read as absent and noted for the lint, so a type error cannot fail the whole decode.
- `Requirements` decodes node by node and never errors. A shape it cannot read becomes an entry with a problem.
- `FillableRoles` maps app_env to the role the box can fill. A field is left out, and so shown as a plain field, when its role does not parse, its separator is invalid or not on a `models` field, it is not `type: text`, it is a key field that is not `secret`, or its role repeats an earlier field's. The last three go past the brief. They keep piece 3 from filling a key into a field whose value `GET /config` would show back, or a value an enum or bool check would refuse.
- `EffectiveRequires` drops a member that matches no field (including one that matches only model fields), then a group left empty, then an entry it cannot read.
- `RoleDrops` lists what was dropped. The remote catalog logs it at warn, with `manifest_id`, `count` and `dropped` (the keys the AI provider reader already uses), each time it loads an install payload. That is on every install plan and install, not once per snapshot, because manifests are fetched per app.
- The keys survive the installer's `yaml.Marshal` round trip into the instance directory (`TestRolesRoundTrip`).

**Lint** (`internal/manifest/lint.go`, `cmd/moose`). `manifest.Lint(m, opts)` returns errors and warnings. It lives in the manifest package so it can read what the lenient decoder noted.

- Errors: a bad role (shape, kind, attribute, model type) or one that is not a single value; a repeated role; a key without `secret: true`; a role field with a type other than text, or with `options`, `default` or `required: true`; a separator that is invalid or not on `models.<type>`; a slot with only model fields; an `openai_compatible` slot without `base_url`; a `requires` member that matches no field, is a role or is malformed; a repeated member; an empty group; an entry with no `one_of` or another key; a plain field that is both `required: true` and a member; a role-tagged field named by its `app_env`.
- Warnings: a kind group over slots with different model types (a slot with no model field is left out of that comparison, so openclaw's native key-only slots do not trip it); a group with a single plain field; and, with `--ai-providers`, a native protocol no provider offers.
- `moose manifest lint` and `check` list every error at once and exit 1, and print each warning as `warning: ...` on stderr and still exit 0 when there is no error. Both take `--ai-providers <path>` before or after the manifest path. The store's file keeps the list under `providers:`, not `ai_providers:` as the brief said, so the reader takes either key, and a file with neither is an error rather than a warning on every native slot.

**Brain** (`internal/api/appconfig.go`, `internal/api/install_plan.go`).

- `POST /api/v1/apps`: after the answers are resolved, the first unmet group is a 422: `config.fields: pick at least one AI provider` for a group that is exactly `ai`, else `config.fields: fill in at least one of: <titles>`. The check is inside `resolveInstallConfig`. Its caller in `internal/api/api.go` audits `app.install` with `success=false` on that error, like the other rejected elections.
- `PUT /api/v1/apps/{id}/config`: a group met before the edit and unmet after it is a 422, `config.fields: keep at least one AI provider` or `config.fields: keep at least one of these filled in: <titles>`. An already-unmet group does not block. It audits `app.config.update` failure like the other 422s there.
- The install plan and the config DTO gain, additively, per field `role` (only when fillable) and `separator` (only on a fillable `models` field), and a top-level `requires` (the effective groups). The OpenAPI spec and `web-ui/src/generated/openapi.ts` are regenerated. No Vue component changed.

**Checked by hand.** `moose manifest lint` passes on all 58 manifests in the store, with and without `--ai-providers ../store/ai_providers.yml`, with no warning. A copy of openclaw tagged with the seven roles and `requires: [{one_of: [ai]}]` passes lint and check with no warning. A broken copy fails with eight listed errors and one warning.

## How it maps to the specs

Realizes the 2026-09-26 rows of `INSTALL_SETUP.md` on role vocabulary, `separator`, `requires` members and satisfaction, lenient `Parse`, `manifest_version` staying 1, the "no worse" edit rule, and `--ai-providers`. `APP_MANIFEST.md` # D4 has a new # Roles and requires part with the schema and the leniency rule. `BRAIN_UI_PROTOCOL.md` has the new DTO fields, the install 422 and the PUT rule. `INSTALL_SETUP.md` # 1 and # 3 have short as-built notes. `docs/architecture.md` has the new files in the `manifest` row.

## Known gaps & deviations

- **Nothing fills a slot yet.** Provider protocol resolution (which provider can fill a native slot) and slot filling from an account come in piece 3. Until then `role` in the install plan means only "the box can fill this kind of field", not "a provider is there to fill it". The UI still uses its env-name lookup.
- **Known limits from the decisions table.** A field that names the provider (openmuse `MODEL=provider/model`) stays a plain field, and an app gets one slot per protocol, so separate OpenAI-compatible endpoints per job cannot be expressed.
- **No store manifest is tagged yet.** openclaw and hermes-agent get `requires: one_of: [ai]` in a store change. Until then their install is not gated on a provider.
- **Three fillability rules go past the brief**: type text, secret key fields and no repeated role (see Leniency). The lint rejects all three anyway, so a store manifest should never hit them.
- **The install success path is tested at the resolver, not the handler.** A handler test that gets past validation starts an install job, and the API test harness has no Docker driver. The 422 path is tested through the handler.
- **Drop logging is per install payload load**, not once per snapshot. It is also only on the remote catalog source; the disk source is for tests.
- **The first unmet group is reported**, not all of them, the same as `required`.
- **The setup page does not check `requires` before submit yet.** Install stays enabled, and an unmet group shows only as the 422 after the attempt. The setup page reads `requires` in piece 3. No store manifest declares `requires` before then, so no user can hit this. Raised by Greptile on #505.
- **Not tried in a browser.** The UI does not read the new fields yet.

## What's next

- Store: tag the manifests with `config:` with roles, and openclaw and hermes-agent with `requires: one_of: [ai]`, running `moose manifest check --ai-providers`.
- Step 4, next pieces: AI provider accounts, protocol resolution against provider data, slot filling at install and rebind, and the setup page reading `role` and `requires` in place of the env-name lookup.
