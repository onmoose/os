# moose docs

The map of all documentation. Three homes:

- **[`specs/`](specs/)** — design source of truth. What moose *is* and the
  locked decisions behind it. Read the relevant spec end-to-end before changing
  behavior in that area.
- **[`progress/`](progress/)** — implementation log. ADR-style entries recording **what was done** and **what's next** for each unit of work.
- **[`dev/`](dev/)** — developer how-to. Running locally, code-level
  architecture, tooling.

Every change ships with documentation (see [`../CLAUDE.md`](../CLAUDE.md) #
Documentation discipline).

**New contributor?** Start with [`dev/contributing.md`](dev/contributing.md) — the
end-to-end loop (orient → pick a task → branch → build → test → document → PR).
Actionable parallel work lives in [GitHub Issues](https://github.com/onmoose/os/issues)
(`gh issue list --label P1`).

## Specs

`specs/` holds the design docs. [`specs/SPEC.md`](specs/SPEC.md) is the entry
point, and [`../CLAUDE.md`](../CLAUDE.md) holds the big decisions the specs
build on. The list below groups every spec in `specs/`. If a doc is in that
folder and not in this list, that is a bug: fix it in the same change. Inside
the specs, cross-references are bare filenames, relative to `specs/`.

Orientation:

- **Start here:** `SPEC.md`, `CONTROL_PLANE.md`, `ENVIRONMENT.md` (the two environment profiles — `appliance` vs moose-operated `hosted` — and every hosted-specific delta).
- **Apps:** `APP_LIFECYCLE.md`, `APP_MANIFEST.md`, `APP_STORE.md`, `APP_ISOLATION.md`, `SERVICE_PROVISIONING.md`, `INSTALL_SETUP.md` (draft: the install page, provider accounts, and role-mapped env vars), `CAPABILITIES.md` (the machine-readable ledger of shipped platform capabilities, so catalog curation stops depending on someone remembering — the manifest itself is [`dev/capabilities.yml`](dev/capabilities.yml)).
- **Protocols:** `BRAIN_UI_PROTOCOL.md`, `BRAIN_HOST_PROTOCOL.md` (Pattern C stream 1 — `journal_follow` per-app log tail — is now implemented; `journal_query` and `journal_export_range` remain deferred).
- **Frontend:** `WEB_UI.md` (stack/deploy), `DASHBOARD.md` (logged-in IA + the owner-scoped apps model + install flows, incl. Door-2 custom-container), `SETTINGS.md` (Settings IA: My-account / Box-settings split, panel inventory, role gating), `FILES.md` (in-dashboard file manager).
- **System:** `STORAGE.md`, `BOOT.md`, `DISCOVERY.md`, `MOOSE_NETWORK.md`, `TIME.md`, `USERS_AND_GROUPS.md`, `AUTH.md`.
- **Operations:** `UPDATES.md`, `RELEASE_MANIFEST.md`, `BUILD.md`, `TESTING.md`, `HEALTH.md`, `LOGGING.md`, `TELEMETRY.md`, `LOCAL_ANALYTICS.md`, `NOTIFICATIONS.md`, `FIRST_RUN.md`.
- **Cross-cutting:** `THREAT_MODEL.md`, `DECISIONS.md` (decision log), `NEXT.md` (open questions).

## Progress

[`progress/README.md`](progress/README.md) is the canonical record: the full
build-ordered index of every entry plus the **Up next** queue (next
implementation slices). It's the single source of order — this map deliberately
doesn't duplicate the list.

## Dev guides

- [`dev/contributing.md`](dev/contributing.md) — the contributor loop: orient,
  pick a task from [GitHub Issues](https://github.com/onmoose/os/issues), branch
  off `dev`, build, test, document, PR into `dev`. Read this first if you're new.
- [`dev/running-locally.md`](dev/running-locally.md) — run the whole stack
  natively (no VM), and the two-loop dev model.
- [`dev/web-ui.md`](dev/web-ui.md) — code-level architecture of the `web-ui/` dashboard: folder layout, the three-tier state model (Query / module-singleton / local), the cross-cutting modules (`api`, `auth`, `useEvents`, `elevate`, `toasts`), routing, styling tokens, the OpenAPI codegen workflow, and add-a-view/query/type recipes. The internal companion to `specs/WEB_UI.md` (design) and `architecture.md` (external contract).
- [`dev/testing-brain.md`](dev/testing-brain.md) — six-layer test plan for
  `moose-brain` (unit → store → lifecycle-with-fakes → API → integration
  → e2e). Companion to `specs/TESTING.md`, which covers boot-level lanes.
- [`dev/hosted-boot-proof.md`](dev/hosted-boot-proof.md) — the "where we are now" runbook for hosted HTTPS bring-up (seed → `EnsureWildcardTLS` → `:443` bind, the two-path cert model) and the `dev/cloud/` boot-proof: the brain-log milestones to grep, a symptom → where-to-look table, and how to run the lane (CI vs local). Read this before chasing a red cloud boot-proof or a "`:443` doesn't bind" report. Operational companion to `specs/TESTING.md` # Hosted cloud variant and `specs/ENVIRONMENT.md` # Networking & discovery.
- [`dev/code-review.md`](dev/code-review.md) — the review standard: what to read before reviewing, 12 lenses (correctness, security, spec fidelity, Go discipline, audit completeness, test coverage, documentation honesty, scope, migration safety, error quality, dependencies, commit hygiene), severity levels, and output format. Used by the pre-PR self-review step and the dedicated review agent.
- [`dev/authoring-apps-with-an-agent.md`](dev/authoring-apps-with-an-agent.md) — a reusable agent prompt that turns an upstream `docker-compose.yml` (or a GitHub repo) into a Door-1 catalog app: rewrites the compose to pass admission, rewires env vars to moose's injected values, resolves image digests, and writes the `apps/<id>/` package into the private `store` repo. The author-side adaptation tool for growing the catalog; the run ends at an open store PR.
- [`dev/catalog-import-gaps.md`](dev/catalog-import-gaps.md) — append-only ledger of platform gaps surfaced during catalog imports (an env var moose can't inject, a secret it can't generate). Import agents capture findings here; a human triages and promotes recurring/severe gaps into `specs/NEXT.md`. Capture, not the design backlog — the two are kept distinct on purpose.
- [`cmd/moose`](../cmd/moose/) — the `moose` author CLI, two `manifest` subcommands runnable on a dev box with no running brain. `lint <path>` validates a manifest against the schema (`APP_MANIFEST.md`) and confirms its sibling compose exists, parses, and declares `main_service` (backs the catalog CI schema-lint step). `resolve <path>` fills the manifest's object-form `images:` map with registry-resolved digests + download/disk sizes via the Docker daemon (`APP_STORE.md` # Catalog schema; backs the CI digest/size-resolution step). Build with `go build ./cmd/moose`.
