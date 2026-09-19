# Testing the brain

How we test `moose-brain` and its sibling packages. Scoped to the **inner
loop** (the Go code that drives Docker + Caddy + host-agent). The system-
level lanes — `systemd-nspawn`, QEMU+swtpm, ISO end-to-end — live in
[`../specs/TESTING.md`](../specs/TESTING.md) and target boot ordering, LUKS,
TPM, and other host-integrated concerns. The two are complementary; neither
substitutes for the other.

## Stance

The brain is small but already non-trivial: a multi-step install transaction
with rollback, two install doors converging on one core, an admission gate, a
TOFU digest resolver, a reconciler that diffs desired vs. actual Docker state.
Most of what will go wrong is **transaction-ordering bugs and rollback gaps**
— exactly the class of thing that's painful to debug from a failed
end-to-end install but cheap to catch with a fake `DockerDriver`.

The pyramid is the usual shape: fast unit + store tests carry most weight,
lifecycle tests with fakes cover transaction logic, real-Docker integration
is opt-in nightly, and a curl-driven e2e canary codifies the manual
verifications we keep redoing slice after slice.

## The enabling refactor

Today, `exec.Command("docker", …)` is scattered across `internal/lifecycle/`
and `internal/lifecycle/pinning.go`. Before the test plan is worth anything,
extract a typed driver:

```go
// internal/lifecycle/docker.go (new)
type DockerDriver interface {
    Pull(ctx context.Context, image string) error
    ImageInspect(ctx context.Context, image string) (RepoDigests, error)
    ComposeUp(ctx context.Context, dir, project string) (string, error)
    ComposeDown(ctx context.Context, dir, project string) (string, error)
    ComposeStop(ctx context.Context, dir, project string) (string, error)
    Inspect(ctx context.Context, instanceID, mainService string) (Running bool, Health string, Err error)
    NetworkCreate(ctx context.Context, name string, internal bool) error
    NetworkRemove(ctx context.Context, name string) error
    PSManaged(ctx context.Context) (map[string]bool, error)
}
```

Production impl `cliDocker` is what we have today, moved verbatim. Tests use
a `fakeDocker` that records calls and returns scripted results. The same
shape applies to `caddy.Client` and `hostclient.Client` (already typed, just
promote to interfaces).

Without this refactor, every lifecycle test needs a live Docker daemon and
"unit tests" degrade into slow, flaky integration tests. With it, layers 1–4
below run in milliseconds and stay green offline.

## Layer 1 — pure unit (no IO)

Table-driven. Inputs are short strings; assertions are exact.

- **`internal/admission`** — one row per rejection rule (`ports`, `privileged`,
  `cap_add`, `build`, `extends`, host namespaces, abs-path bind, named volume)
  plus the happy case. Each row carries a YAML snippet + expected message
  substring naming the service and field.
- **`internal/manifest`** — `Parse` validation errors (missing required
  fields, wrong `manifest_version`); `Synthesize` (empty name, missing port,
  single-service inference, ambiguous services, unusable slug).
- **`internal/lifecycle` helpers** — `serviceImages`, `repoOf`
  (`ghcr.io:5000/foo/bar:v1`, digest form, bare `redis`, registry-with-port),
  `digestOf`.
- **Override generation** — given `(manifest, compose, pins)`, parse the
  emitted YAML and assert structure: labels per service, networks per
  service, ingress alias on `main_service` only, `image:` pin on each.
  Golden files are fine here; they're regenerable and easy to diff.
- **Slug allocation** — reserved list, conflict fallback to `-2`/`-3`,
  exhaustion error.

## Layer 2 — store (real SQLite on tmpfs)

SQLite is fast and behaves like prod. No mocking. Each test opens a fresh
DB in `t.TempDir()`.

Cover: migration idempotency (running twice doesn't error), `Create` /
`Get` / `List` / `Delete`, `SetInstanceImages` atomic replace, `GetInstanceImages`
ordering, **FK cascade** (delete instance → `instance_images` rows gone),
`SlugTaken`.

## Layer 3 — lifecycle with fakes (the interesting layer)

The core of the test plan. A `lifecycletest` helper wires a `Manager`
against `fakeDocker` + `fakeCaddy` + `fakeHost` + a real temp `store` + a
temp state dir, and returns a builder for scripting fake responses:

```go
m := lifecycletest.New(t).
    WithDigest("traefik/whoami:v1.10.3", "sha256:43a6…").
    Manager()
```

Scenarios (each is its own `t.Run`):

| # | Scenario | Asserts |
|---|----------|---------|
| 1 | Install happy, Door-1 | SQLite state=`running`, override on disk has pin, ordered driver calls (`pull → inspect → up → flip`) |
| 2 | Install happy, Door-2 | Same, manifest synthesized, no catalog verify run |
| 3 | Admission rejection | No SQLite row, no instance dir, no Caddy calls |
| 4 | Digest mismatch (Door-1) | Rollback clean; `compose up` never issued |
| 5 | Unpullable image | Rollback before any network/route work |
| 6 | `compose up` failure | Full rollback per spec |
| 7 | Health timeout | State=`failed`, instance dir kept, splash flipped to `failed`, route still registered |
| 8 | Uninstall | Every resource torn down even when some teardown steps fail |
| 9 | Reconcile drift | Running-but-no-containers → up; stopped-with-containers → stop; orphans → torn down; routes/mDNS re-asserted |

**Assertion discipline.** Each test asserts **end state** + the **one or two
driver calls that actually matter** (e.g. "`compose up` not called"), not the
full call transcript. Brittle assertions across all nine install steps will
rot the moment we refactor.

## Layer 4 — HTTP API (huma + httptest)

Stub `Manager` behind a `LifecycleManager` interface; assert wiring, not
lifecycle logic again:

- Job-shape responses (`kind`, `status`, `step`, `error`).
- Synchronous 422 for custom-compose admission failures (the body is set by
  the handler before the job runs — easy to regress).
- SSE event stream emits on state transitions (`installing → running` etc.).
- OpenAPI schema endpoints remain accurate (cheap snapshot).

## Layer 5 — integration (opt-in, real Docker)

Tag with `//go:build integration`. One canonical happy-path test per door
against `traefik/whoami:v1.10.3` — small, deterministic, already in our
catalog. Real `docker pull`, real `RepoDigests`, real `compose up`, real
teardown. This is the only place we trust that the Docker CLI's output
shape hasn't shifted under us. Nightly, not per-PR.

## Layer 6 — e2e smoke (bash + curl)

The manual verifications we've redone four slices in a row, codified. A
script in `dev/e2e/` that runs against `make run-brain`:

1. Door-1 install whoami → assert override contains `@sha256:`, app reaches
   `http://whoami.local`.
2. Tamper catalog `images:` → reinstall fails at `resolving_digests` with
   mismatch message, no state.
3. Door-2 paste compose → install + reachable.
4. Bogus image → fail at `resolving_digests`, no state.
5. Uninstall → SQLite row, instance dir, Caddy route, mDNS file all gone.

Identical regardless of refactor churn — our "did we regress the
user-visible spine" canary.

## CI shape

| Target | Lanes | When |
|--------|-------|------|
| `make test` | 1 – 4 | Per-PR gate. No Docker required. |
| `make test-integration` | + 5 | Nightly. Needs Docker. |
| `make test-e2e` | 6 | Nightly + on demand. Needs the full dev stack. |

## Discipline notes

- **Keep fakes minimal.** If a fake needs 200 lines to make a test pass,
  production code is wrong, not the test. (`feedback_dont_rabbithole_fixtures`.)
- **No mocks for SQLite or the filesystem.** They're fast and faithful;
  mocking them buys nothing.
- **Golden files where shape is stable** — override YAML, SSE payloads. Easy
  to diff, easy to regenerate when intentional. Don't golden anything with
  timestamps or random IDs without normalizing.
- **Don't re-test the lifecycle from the API tests.** Layer 4 asserts the
  *API* layer; Layer 3 already covers the transaction.
- **Integration tests are not a substitute for layer 3.** They catch driver
  surface drift, not transaction bugs.

## Build order

If we're picking this up incrementally:

1. The `DockerDriver` refactor (no behavior change; pure extraction).
2. Layer 2 (cheapest, immediate value, no refactor dependency).
3. Layer 1 admission + manifest tables.
4. Layer 3 install happy + the rollback scenarios — the highest-leverage
   tests in the whole plan.
5. Layer 6 e2e — stops the manual verification treadmill.
6. Layer 4 + Layer 5 land when they pay for themselves, not on principle.
