# Running moose locally

moose's dev model is **two loops**:

- **Inner loop (seconds) — all native, no VM.** The product logic — `moose-brain`
  and the dashboard — runs directly on your machine against the local Docker
  socket. The host-agent is a **fake** that speaks the real protocol but stubs
  host ops. This is where ~90% of development happens.
- **Outer loop (minutes) — a VM.** Only the host-integrated parts (boot
  ordering, LUKS/TPM, systemd, NetworkManager, Avahi) need a booted OS. This is
  QEMU + swtpm per `../specs/TESTING.md`, and is **not wired up yet**.

This guide covers the inner loop.

## Who can contribute to what

moose's dev model splits cleanly along the inner/outer loop boundary, and that
boundary is also the **macOS/Windows ↔ Linux** boundary. The Go code is
deliberately build-tagged so the cross-platform surface compiles and runs
anywhere; the Linux-only parts are the *real host integration*, which by design
only exists on the Debian target.

If you're on **macOS, Windows (WSL2), or any Linux**, you can build and work on
everything in the first list with zero platform-specific setup. The second list
needs a real Linux host (or the CI lanes that provide one).

### Cross-platform — everyone can contribute (no VM, no Linux)

The inner loop and the bulk of the codebase. `GOOS=darwin go build ./...`
compiles clean; Docker Desktop covers Caddy and app containers.

- **The brain** — all of `internal/`: `api`, `lifecycle`, `catalog`,
  `manifest`, `store`, `caddy`, `hostclient`, `events`, `protocol`, `auth`,
  `audit`, `admission`. This is where ~90% of development happens.
- **The dashboard** — all of `web-ui/` (Vue 3 + Vite + npm). Fully OS-agnostic. For its internal code architecture (folders, state model, composables, codegen), see [`web-ui.md`](web-ui.md).
- **The fake host-agent** (`cmd/host-agent`) — speaks the real
  `../specs/BRAIN_HOST_PROTOCOL.md` wire format over a real UNIX socket, with
  host ops stubbed. Lets the entire control-plane spine run on a Mac. Its
  `journal_follow` op streams **real** container output via `docker logs -f`
  (no journald needed), so the per-app Logs tab shows live logs in the inner loop.
- **The full inner-loop stack** — `make dev` (Caddy container + fake host-agent
  + brain + Vite), installing apps, SSE, routing, uninstall.
- **Sample manifests** — none in this repo. App artifacts are authored in `onmoose/store`; use `make dev-app APP=<id> STORE=../store` to boot one against the brain.
- **Unit tests** for all the above, plus the self-contained `make test-health`.

On macOS, cgo automatically selects the stub implementations (`pam_other.go`,
`dbus_other.go`), so you do **not** need `libpam0g-dev` or any C headers.

### Linux-only — requires a real Linux host

The host-integrated subsystems and their test lanes. These talk to
`/etc/shadow`, the system DBus, systemd, and the TPM — there's nothing to fake
them against off-Linux, and they run in CI regardless.

- **`cmd/host-agent-real`** — the production host-agent.
- **`internal/hostagent/pamverifier`** (real PAM via `msteinert/pam`; needs
  `libpam0g-dev` + cgo) and **`usermgr`** (`useradd`/`chpasswd` against
  `/etc/passwd`/`/etc/shadow`).
- **`internal/hostagent/avahipublisher`** (real Avahi DBus publishing).
- **The nspawn fast lane** — `make test-usermgr-nspawn`,
  `make test-boot-chain-nspawn` (need `systemd-nspawn` + `mmdebstrap`).
- **The QEMU medium lane** — `make test-medium-qemu` (needs `mkosi` + `swtpm` +
  `qemu-system-x86` + OVMF).
- **Boot ordering, LUKS/TPM, NetworkManager** — the outer loop generally.

**Bottom line:** if your change is in the brain, the UI, the catalog, or the
protocol contracts, your OS doesn't matter. If it touches host integration, you
need Linux (or you rely on CI to exercise it) — and you can still develop
against the fake host-agent on any platform up to the point where real host
behavior is required.

## Prerequisites

- **Docker** + the `docker compose` plugin (`docker compose version`).
- **Node 20+** (`web-ui/.nvmrc` pins 20).
- **Go 1.23+.** If `go` isn't on your `PATH`, the `Makefile` falls back to
  `~/.local/go/bin/go`.
- **Host port `:80` free.** The dev Caddy binds `:80` (matching production) so
  `<slug>.local` URLs work portless. If something else holds `:80` (another
  web server, a system service), stop it first or `make caddy` will fail to bind.
- **`avahi-daemon` running** (Linux, for the `make dev` loop). `make dev` sets
  `MOOSE_DEV_AVAHI=1`, which publishes each app's `.local` name over the real
  Avahi DBus API so it resolves on the LAN. Without the daemon, the fake
  host-agent still starts but app installs fail at the publish step. Check with
  `systemctl is-active avahi-daemon`; the publisher itself needs no sudo.
- **`libpam0g-dev`** (Linux only). Required to build/test the
  `internal/hostagent/pamverifier` package and `cmd/host-agent-real`. Without
  it, `go test ./...` will fail on those targets with `fatal error:
  security/pam_appl.h: No such file or directory`. The fake binary
  (`cmd/host-agent`) and everything else build without it.

  ```bash
  sudo apt install libpam0g-dev
  ```

  You also need `CGO_CFLAGS=-D_GNU_SOURCE` because `msteinert/pam` v2.1.0
  uses `RTLD_NEXT` (a GNU extension) without setting `_GNU_SOURCE` itself.
  The `Makefile` exports this globally; if you run `go` directly, prefix:

  ```bash
  CGO_CFLAGS=-D_GNU_SOURCE go test ./...
  # or use the Makefile targets:
  make test          # full suite, needs libpam0g-dev
  make test-nopam    # skip pamverifier, no libpam0g-dev needed
  ```

  This is Linux-only — see [Who can contribute to what](#who-can-contribute-to-what)
  above. On macOS/Windows, skip it and use `make test-nopam` or run the fake
  host-agent.

### Ubuntu 24.04: unprivileged user namespaces

Only relevant to the **mkosi image-build lanes** — `make build-cloud-image`, `make test-cloud-qemu`, `make test-medium-qemu`. The inner loop is unaffected.

mkosi (the locked image builder) builds rootless: it unshares a user namespace and drops capabilities inside it. Ubuntu 24.04 ships `kernel.apparmor_restrict_unprivileged_userns=1`, which hands an unconfined process a user namespace with no `CAP_SETPCAP`. mkosi's `PR_CAPBSET_DROP` then returns `EPERM` and the build dies at sandbox bring-up — `acquire_privileges → drop_capabilities` — before doing any work (issue #189). It is not version- or `sudo`-specific: any unprivileged mkosi invocation on a restriction-enabled kernel hits it.

Relax the restriction (it is the system-wide default that gates *all* unprivileged userns, not mkosi-specific):

```bash
# this shell (reverts on reboot):
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
# persist across reboots:
echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/99-mkosi-userns.conf
sudo sysctl --system
```

If you would rather keep the restriction on, the scoped alternative is an AppArmor profile that grants `userns,` to mkosi's interpreter — more setup, but it doesn't open unprivileged userns host-wide. The `dev/cloud/bootstrap.sh` preflight hard-fails with this pointer when the probe trips; `dev/test-qemu/bootstrap.sh` prints it as a warning above mkosi's own output. CI's cloud-image lane (`.github/workflows/ci-cloud-image.yml`) sets the knob automatically — its runner is ephemeral, so relaxing it host-wide is harmless there.

## Start the stack

**One terminal (recommended):**

```bash
make dev          # starts Caddy detached, then backgrounds agent + brain + ui
                  # with their output prefixed [agent]/[brain]/[ui].
                  # Ctrl-C kills all three.
```

No extra tools — pure bash supervisor with a `trap` that kills the process
group on signal.

### Go changes rebuild themselves

Vite watches the UI, and since `dev/dev-go.sh` the Go side is watched too: save a file under `cmd/` or `internal/` and `make dev` rebuilds both binaries and restarts them, printing `[watch]` lines as it goes. Before this, a brain edit only reached the running process on the next `make dev`, and the symptom was never obvious — a route added minutes ago answering 405, a new response field arriving `undefined`.

Three things about how it behaves:

- **The debounce is long: 10 seconds of no further edits before it builds.** Most edits here arrive from a coding agent, which writes a burst of files over several seconds; a short debounce would restart the brain in the middle of one, again and again. Editing by hand? `MOOSE_DEV_DEBOUNCE=2 make dev`. `MOOSE_DEV_POLL` (default 2s) is how often it looks.
- **A failed build leaves the running brain alone.** The build goes to `.dev/next/` first, and only a build that succeeded stops anything. A typo costs you a `[watch] build failed` line, not your stack.
- **A restart drops open SSE streams and kills any install job in flight.** The dashboard reconnects on its own; an install that was running does not resume.

A rebuild is about 7 seconds for a change in `internal/api` (recompile plus linking a 22MB binary), 1–3 seconds for a leaf package, and nothing at all while you are only editing the UI.

The watcher needs GNU `find` (`-newermt`), so it is Linux and WSL. Elsewhere `make dev` prints one line saying Go changes will not rebuild and runs exactly as it did before.

**Four terminals (no extra tools):**

```bash
make caddy        # 1. dev reverse proxy (Caddy container, detached)
make run-agent    # 2. fake host-agent (UNIX socket at .dev/agent.sock)
make run-brain    # 3. moose-brain (:8080, native)
make ui           # 4. dashboard (Vite, :5173)
```

`make help` lists all targets. Either way, open <http://localhost:5173>. On
first run the **Setup** page asks you to create the first admin and shows a
recovery code once — save it. Then install **Whoami** from the catalog and
watch it appear as `running`.

## How the pieces talk

```
browser ──▶ Vite :5173 ──(proxy /api)──▶ brain :8080
                                           │  ├─ docker compose CLI ─▶ Docker
                                           │  ├─ UNIX socket ─────────▶ host-agent (fake)
                                           │  └─ admin API ───────────▶ Caddy :2019
app HTTP:  http://<slug>.local/ ─▶ Caddy :80 ─▶ app container
```

- **Caddy listens on host `:80`** (free it first — see prerequisites) and exposes
  its admin API on `:2019`. App containers join the `moose-ingress` network so
  Caddy reaches them by per-instance alias.
- **`.local` URLs resolve under `make dev`.** `make dev` runs the fake host-agent
  with `MOOSE_DEV_AVAHI=1`, which swaps the in-memory discovery publisher for the
  real Avahi DBus publisher (`internal/hostagent/avahipublisher`) — the same code
  path `host-agent-real` uses. Each installed app's `<slug>.local` is then
  announced on the LAN, reachable by its portless URL from this box and other LAN
  devices. Requires `avahi-daemon` running; the publisher works unprivileged.
  Android browsers don't resolve `.local` (a production limitation too — see
  `DISCOVERY.md`); secure URLs are the Android path.
  ```bash
  curl http://whoami.local/        # under make dev, no Host header needed
  ```
- **The multi-terminal path (`make run-agent`) stays pure-fake** — no Avahi, so
  `.local` won't resolve there. Use the `Host`-header recipe below, or run
  `make dev` for the LAN-faithful loop.

## Where state lives

Everything dev-generated is under `.dev/` (git-ignored):

```
.dev/
├── agent.sock                    # host-agent UNIX socket
├── brain  host-agent             # compiled binaries
└── state/
    ├── moose.db                  # brain SQLite (users, sessions, instances)
    ├── fake-shadow.json          # fake host-agent's passwords + roles (stands in for /etc/shadow)
    └── instances/<id>/           # per-app: manifest, compose, override, .env, data/
```

Override defaults with env vars: `MOOSE_LISTEN`, `MOOSE_STATE_DIR`, `MOOSE_CATALOG_URL`, `MOOSE_CATALOG_CACHE_DIR`, `MOOSE_CATALOG_FILE`, `MOOSE_CATALOG_REFRESH`, `MOOSE_AGENT_SOCK`, `MOOSE_CADDY_ADMIN`, `MOOSE_CADDY_LISTEN`, `MOOSE_TRUSTED_PROXIES`.

Two of those are about the catalog and are easy to mix up. `MOOSE_CATALOG_CACHE_DIR` is where proxied icons and screenshots land — never the catalog itself, which the brain holds in memory and re-fetches (`../specs/APP_STORE.md` # Failure modes). `MOOSE_CATALOG_FILE` is a local snapshot the brain reads once at startup and never writes back, for working without a reachable catalog service; `make dev-app` sets it for you. A real box sets neither by hand.

`MOOSE_TRUSTED_PROXIES` is the one with a security consequence: it lists the proxies (comma-separated IPs or CIDRs) whose `X-Forwarded-For` the brain will believe when deriving the client IP its per-IP throttles key on (`BRAIN_UI_PROTOCOL.md` # Rate limiting & abuse). Leave it unset and it defaults to loopback plus Docker's bridge pool (`127.0.0.0/8`, `::1/128`, `172.16.0.0/12`) — deliberately **not** all the private ranges, because a trusted hop is skipped during the chain walk, so trusting `192.168.0.0/16` or `10.0.0.0/8` would make every LAN client resolve to Caddy's address and share one throttle bucket. Set it to the empty string to ignore the header entirely and key on the peer address. A value that doesn't parse stops the brain at startup rather than silently falling back.

**Why `fake-shadow.json` exists.** The password lives on the host-agent side, never in the brain (`AUTH.md` # Password storage — the brain calls `verify_password` on every login). The *real* host-agent persists it in `/etc/shadow`; the *fake* one used by `make dev` would otherwise keep it in an in-memory map that dies with the process. Because the brain's SQLite persists the user **and** session rows across a restart, that asymmetry produced a confusing bug: restart the stack, clear cookies, log in again, and the password was rejected even though the account still existed (the session cookie had masked it — a kept cookie skips the password re-check). Backing the fake maps with `fake-shadow.json` under `MOOSE_STATE_DIR` makes dev accounts survive a restart, matching the real agent. Set `MOOSE_STATE_DIR` and the fake agent picks it up automatically (the dev stack exports it); leave it unset and the fake stays purely in-memory.

## Reset

```bash
make clean        # stop the dev Caddy, remove moose app containers + networks,
                  # wipe .dev/state
```

## Running host-agent-real

`cmd/host-agent-real` is the production binary: it uses real PAM for
`verify-password` while `set-password`, `set-role`, and `delete-user` remain
in-memory fakes (tracked in `docs/progress/host-agent-pam-verify.md`).

**Prerequisites:**

```bash
apt install libpam0g-dev      # PAM headers for CGO
sudo cp dev/pam/moose /etc/pam.d/moose
```

**Build and run:**

```bash
go build ./cmd/host-agent-real
sudo ./cmd/host-agent-real    # must run as root — pam_unix.so requires privilege
```

Point the brain at it by setting `MOOSE_AGENT_SOCK` to the same path the real
binary listens on. Note: because `set-password` is still fake, dashboard login
will fail until real `useradd`/`passwd` integration lands — use
`cmd/host-agent` (fake) for all normal dev work.

## Verifying routing

moose uses **Host-header-based subdomain routing** — each installed app gets a
virtual host (`<slug>.local`), never a path prefix. This keeps apps in
separate browser origins (same-origin policy enforcement — see `SPEC.md`).

**Dev port:** Caddy listens on `:80` in dev (matching production), so `<slug>.local`
resolves portless. Under `make dev` (real Avahi) the name is announced on the LAN and
works from a browser on this box or another LAN device. With `make run-agent` (no Avahi)
the name won't resolve — fall back to the `Host`-header / `--resolve` recipes below, which
work regardless of mDNS.

**Quick ad-hoc check** (after installing whoami from the catalog):

```bash
# Under make dev — real Avahi, portless, no Host header
curl http://whoami.local/

# Host-header method — works without Avahi (e.g. under make run-agent)
curl -H "Host: whoami.local" http://localhost:80/

# --resolve variant — same effect, avoids quoting issues in scripts
curl --resolve "whoami.local:80:127.0.0.1" http://whoami.local:80/

# Path-based — should NOT return 200 (route does not exist)
curl -s -o /dev/null -w "%{http_code}" http://localhost:80/whoami/
```

**Automated end-to-end test** (installs whoami, exercises positive/negative
routing, then uninstalls — requires `make dev` already running):

```bash
make test-caddy
```

The script (`dev/test-caddy-routing.sh`) boots a `caddytest` admin user on
first run and reuses it on subsequent runs. Reset with `make clean && make dev`.

## Debugging the wire protocols

Both protocols are plain HTTP/JSON and curl-debuggable by design:

```bash
# Brain (UI protocol)
curl -s localhost:8080/api/v1/apps
curl -sN localhost:8080/api/v1/events          # live SSE stream
curl -s localhost:8080/openapi.json            # generated schema

# host-agent (host protocol), over the UNIX socket
curl --unix-socket .dev/agent.sock http://agent/v1/system/status
curl --unix-socket .dev/agent.sock http://agent/v1/discovery/state
```

## CI

Two **path-filtered, PR-only** workflows gate every PR into `dev` or `main`,
mirroring the local pre-PR gate. `dev` is the default branch and where
ordinary feature PRs land; `main` only moves via the dev->main PR the
maintainer opens to cut a release, and it's gated the same way so a release
can't merge with a broken build. Each workflow runs only when its own files
change, so a docs-only PR spins no runner, a Go-only PR skips the web build,
and vice versa. They're PR-only (no `push:`) because every change lands via
PR — a push trigger would just double-run.

- **`.github/workflows/ci-go.yml`** — runs on `**.go` / `go.mod` / `go.sum` /
  `Makefile` changes: installs `libpam0g-dev`, then `go build ./...`,
  `go vet ./...`, and `make test` (full suite incl. `pamverifier`, with
  `CGO_CFLAGS=-D_GNU_SOURCE`). Same as `make check`.
- **`.github/workflows/ci-web.yml`** — runs on `web-ui/**` changes: `npm ci`
  then `npm run build` (Node pinned by `web-ui/.nvmrc`; `build` runs `vue-tsc`
  typecheck before `vite build`). Same as `make check-web`.

Note: if these are ever promoted to *required* status checks, the path filters
mean a PR that doesn't touch a workflow's files won't report that check —
branch protection would then need the [skipped-check workaround](https://docs.github.com/en/actions/managing-workflow-runs-and-deployments/managing-workflow-runs/skipping-workflow-runs). No branch protection is configured today.

The **medium/slow boot lanes** (`make test-boot-chain-nspawn`,
`make test-medium-qemu`) are **not** in CI yet — they need privileged runners
(systemd-nspawn, QEMU+swtpm) and are a coordinated follow-up. See
[`TESTING.md`](../specs/TESTING.md) for the full three-lane model.
