# moose Testing

> Working spec for moose's test infrastructure — what runs in CI, at which speed lane, and what each lane catches. Companion to `BOOT.md` (the system-under-test for the bulk of these tests), `BUILD.md` (the image pipeline that feeds them).

## Stance

moose's correctness story isn't unit tests in isolation. Most of what we get wrong is **boot ordering, mount race conditions, TPM behavior, and failure modes that only surface during real boots.** The test strategy is built around that reality.

Three lanes, fastest to slowest. Each catches a different class of bug. Together they convert "boot ordering is hard to test" into "boot ordering is among the most-tested parts of moose."

## Fast lane — `systemd-nspawn` (per-PR, ~1 minute)

Runs systemd userspace in a Linux namespace. No initramfs, no kernel boot, no TPM.

**What it catches:**
- Unit dependency errors (`Requires=` / `After=` ordering)
- Drop-in overrides applying correctly
- Synthetic targets activating in the right shape
- Service-level integration: brain ↔ host-agent ↔ Caddy contracts
- nftables rule generation (in a netns-isolated container)

**What it misses:**
- Initramfs / LUKS / TPM unsealing
- Real kernel + driver behavior
- Hardware-level race conditions

Implementation: a `nspawn` test harness that mounts a built moose rootfs, boots it with synthetic disks (loopback files), and runs assertion scripts inside. Each test is a separate nspawn instance, run in parallel where possible.

**PAM verify coverage:** real `verifyPassword` test coverage (via `PAMVerifier` in `host-agent-real`) requires `/etc/pam.d/moose` installed in the nspawn rootfs plus a provisioned test user (`useradd moose-pamtest && chpasswd`). The `pam_linux_test.go` skeleton (`-tags pamtest`) is the entry point; it lands with this lane's full build-out, not earlier.

## Medium lane — QEMU + swtpm (per-PR or per-merge, ~10 minutes)

Full VM boot with a software TPM (`swtpm`). Mature stack — `systemd`'s own CI runs this way.

**What it catches:**
- Initramfs behavior (LUKS unlock at boot)
- TPM2 unseal and PCR sealing semantics
- Real boot ordering across the kernel-userspace boundary
- Disk hotplug scenarios via QEMU `device_add` / `device_del`
- Failure injection: kill Docker mid-boot, simulate clock skew, detach the data drive
- End-to-end first-run flow
- NetworkManager-integrated discovery: per-LAN-interface mDNS announcement, avahi-daemon.conf allowlist sync, interface-removal rewrite, IP-change replay (realized — rides the second boot of the LUKS cycle against a multi-NIC VM; see `dev/test-qemu/`)

**Core test matrix** (in priority order — these are the ones we should not ship without):

| Test | Setup | Assertion |
|---|---|---|
| Happy-path boot | Two virtio disks (OS + data), enrolled vTPM | Boot completes; dashboard reachable; canary file matches |
| No data drive | OS drive only, no enrollment marker | Level-0 boot; moose userspace starts on OS-drive paths |
| Data drive missing (enrolled, absent) | OS drive + marker, no data disk attached | Recovery mode reached; `smbd` not serving; `host-agent` not started |
| Data drive detached mid-write | Boot, write file, `device_del` data drive | host-agent stops; no writes to OS-drive `/home` |
| Canary mismatch | Tamper with `/srv/moose/.canary` between boots | `moose-storage-verify.service` fails; recovery mode reached |
| Ordering inversion | Remove `After=srv-moose.mount` from a bind mount | Verify oneshot catches it; box does not expose empty `/home` |
| TPM2 unseal happy path | Boot with sealed TPM, no policy change | Auto-unlock succeeds; no passphrase prompt |
| Clock skew | `-rtc base=1970-01-01`, no NTP | Caddy delays ACME until `time-sync.target` reached |
| host-agent crashloop | Ship a known-broken host-agent in a test variant | `moose-recovery.target` activates; recovery page reachable on port 80 |
| nftables coexistence with Docker | Boot, capture `nft list ruleset`, restart Docker, diff | moose's `inet moose` table is unchanged across Docker restarts |

Implementation: built on `mkosi qemu` (`mkosi` is the locked image builder — `BUILD.md` # 2, `DECISIONS.md` 2026-06-16). Each test boots a VM, runs an assertion script via SSH or serial console, tears down.

Note on "reboot + unseal" scenarios (the TPM2 unseal happy path, and the slow lane's TPM rot simulation): a reboot that must *withhold* the recovery passphrase on the second boot is realized as **two sequential QEMU processes** sharing one disk overlay + OVMF vars + swtpm state dir (a faithful TPM power cycle — PCRs re-measure identically, SRK/NVRAM persist), not an in-guest `systemctl reboot`. The recovery passphrase is delivered as an SMBIOS type-11 credential fixed at QEMU launch and can't be withheld partway through a single long-lived process, so proving *unattended* unseal (no passphrase, TPM2-only) requires a fresh process with the credential omitted. Realized in slice 0023; see `docs/progress/luks-tpm-enrollment.md`.

### Full-stack control-plane integration

Everything above boots the **host-OS layer** — storage assembly, LUKS/TPM, NetworkManager/Avahi — but deliberately stubs the moose *application* out of the VM: `host-agent-real` is replaced with `/bin/true`, and no brain, UI, Caddy, or Docker app runs. The matrix proves the box reaches "storage ready," not "dashboard reachable with an app installed."

This sub-lane closes that gap. It is the **bridge between the inner loop** (native brain, fake host-agent — `docs/dev/running-locally.md`) **and the boot/storage assertions above**: the first time the whole production topology runs together on real Debian. It is the same QEMU+swtpm harness (`dev/test-qemu/`), not a new rig — it un-stubs the parts the boot matrix mocks.

What it adds on top of a happy-path boot:

- **The control-plane images are baked in, not pulled.** `moose-brain` and `moose-ui` are built on the host, `docker save`'d to tarballs, and sourced into the image; the VM `docker load`s them at first boot. This is the same offline-first mechanism `BUILD.md` # First-boot brain bootstrap specifies for the ISO, applied to the test image — hermetic, no registry, no network dependency in CI.
- **The real launch chain runs.** `host-agent.service` runs the real `host-agent-real` (not `/bin/true`); host-agent launches the brain (`CONTROL_PLANE.md` # Locked: host-agent launches the brain container); the brain launches Caddy, `moose-ui`, and the docker-socket-proxy. The production topology `host-agent → moose-brain → (moose-caddy + moose-ui)` is exercised end-to-end.
- **A real, headless first-run.** `/setup` creates the admin account through the real PAM/`useradd`/`chpasswd` path (`FIRST_RUN.md`, `USERS_AND_GROUPS.md`), driven over SSH/serial with no interactive UI — the start of the headless-automation path the slow-lane ISO test (# Slow lane) completes.
- **A real app install.** A catalog app (`whoami`) installs end-to-end: real `docker compose up`, a real Caddy route inserted via the admin API, a real per-app `.local` record published via Avahi DBus, and real bind mounts into the use-case folders.

Assertions (in priority order):

| Test | Assertion |
|---|---|
| Dashboard reachable | After full boot, `GET /api/v1/...` via the LAN Caddy returns 200; the SPA loads from `moose-ui` |
| Real PAM login | The first-run admin authenticates against `/etc/shadow` through host-agent `verify-password` — no brain-side password hash |
| App install end-to-end | `whoami` installs; its container runs; `whoami.local` resolves; the app's route returns its page through Caddy |
| Content survives uninstall | A file written under a use-case folder is still present after the app is uninstalled (`STORAGE.md` # Files are first-class) |
| Socket-proxy boundary | The brain reaches Docker only via the proxy allowlist; the raw `/var/run/docker.sock` is not mounted into the brain container (`CONTROL_PLANE.md` # Locked: Docker socket exposure) |

This sub-lane runs in the medium budget when the image is cached; the first build (two extra images) costs more. It is the foundation the slow-lane ISO end-to-end test builds on — same assertions, driven from a real installer flow instead of a pre-baked image.

#### Hosted cloud variant — `dev/cloud/` (C2 #205, seed/gate C3a #220, wiring promoted #242)

The hosted profile (`ENVIRONMENT.md`) has its own full-stack lane (`dev/cloud/run-cloud-tests.sh`, `make test-cloud-qemu`): the same baked-control-plane mechanism, but minus swtpm/LUKS/installer ("the disk IS the installed system") and minus SSH (hosted ships none), so the in-VM self-check (`cloud-assertions.sh`) writes its verdict to the **serial console** and the driver greps it. As of #242 the first-boot runtime wiring (networkd DHCP, host-agent + control-plane bundle, the seed materializer, the moose identities) lives in the **production** image (`dev/cloud/`, staged by `dev/cloud/stage-control-plane.sh` + enabled by `dev/cloud/mkosi.postinst.chroot`); the test lane (`dev/cloud/test/`, `Include=..`) adds only the serial self-check on top, so it boots the *exact* image `make build-cloud-image` ships rather than a test-only superset. On top of the C2 control-plane-up proof, the C3a slice exercises the **first-boot provisioning seed + portal-to-box SSO admin-bootstrap gate** (#275 replaced the appliance's `/setup` secret with the SSO handshake; `/setup` is now disabled on hosted). The seed is delivered the cloud-init way — a systemd credential over **SMBIOS type 11** (`io.systemd.credential.binary:moose.seed=<base64 JSON>`, the same channel the medium lane uses for the LUKS passphrase; a real cloud uses cloud-init `write_files`, both materializing the same `/var/lib/moose/seed.json`) — landed by a first-boot `moose-seed.service` before host-agent launches the brain. A **3-boot sequence over one persisted qcow2 overlay** (so the brain's box-id + first admin survive boot→boot) asserts the gate's box-side properties: **un-seeded** ⇒ `GET /_moose/sso` **503** (gate armed but closed — no seed, no verification key) and `POST /setup` **403** (disabled on hosted); **seeded** ⇒ a bad/unsigned token on `/_moose/sso` **401** (the seed's key loaded, the verifier is armed), `/setup` still **403**, and the brain having logged `provisioning seed ingested`; and a **frozen-identity reboot** — a *different* seed re-delivered on a later boot is ignored (the box-id is frozen in the brain's SQLite), so the dashboard + `/api` still serve under the original box-id. Verifying the **real production portal** round-trip stays the joint on-ramp acceptance (the cloud repo holds the production signing key). But the box's own per-app access mechanism is proven here without that portal by a fourth **`access` boot** (#308), on its own fresh overlay + box-id, where the harness **plays a test portal**: it generates a throwaway Ed25519 keypair, seeds the box with the public key (exactly what a real portal hands over) and mints a valid owner assertion with the private key (which never enters the VM), so the box establishes a real owner session with **zero brain changes** (it verifies a genuine assertion through the same `assertion.Verify` + `ssoLanding` path). `cloud-assertions.sh` then installs `whoami` air-gapped (a test-lane-only image tar + `mkcatalog` snapshot + an offline-install host-agent drop-in, staged into `dev/cloud/test/` only — never the lean image) and drives every per-app forward-auth access mode end-to-end through the box's own Caddy on `:80`: **restricted** (the hosted default) — an unauthenticated GET 302s to the box login, while the owner's `moose_forward_auth` cookie proxies through with no second login and the app upstream sees `X-Moose-User` but **never a `Cookie` header** (the #306 whole-header strip) — and **public** (after `PUT /api/v1/apps/{id}/exposure`) — reachable with no session, and *still* cookie-stripped so a public app can't leak the Domain-scoped cookie to replay against the owner's restricted apps. This is a deliberate, scoped extension of the box-only lane's prior no-portal boundary; the trust model is unchanged (the box only ever sees a public key). The per-boot scenario is selected via a second SMBIOS credential (`moose.assert`, plus `moose.sso_token` carrying the minted assertion for the `access` boot); on PASS the guest powers off cleanly so SQLite flushes before the next boot. Before any of that it asserts the **two-cookie safety model** on the wire, from the real `Set-Cookie` headers the SSO landing returns: `moose_session` carries **no `Domain`** (host-only — a `Domain` here would hand the admin session to every app subdomain, where an app could replay it as the owner) and `moose_forward_auth` carries `Domain=<box-id>.onmoose.io` (deliberately domain-wide, which is why the app route must strip it). The `access` boot runs in the default `make test-cloud-qemu` run **and in the `CI / Cloud image` boot gate** (`MOOSE_CLOUD_BOOTS: "unseeded seeded bios access update"`): the per-app forward-auth gate is on by default for every hosted app (`DECISIONS.md` 2026-07-08 flipped the default to `restricted` ahead of this proof), so this lane is the only real-Caddy net under a live security invariant and a box that leaks its forward-auth cookie to an app upstream must fail the gate rather than publish. (The `frozen` boot stays out of the gate — it is orthogonal and has a known-flaky verdict.) **Prerequisites** beyond the other boots' mkosi + `/dev/kvm`: the `access` boot mints its test-portal owner assertion **on the host** (`dev/cloud/mkassertion`, so the private key never enters the VM), so it needs **`go` on the host**; the lane runs under `sudo -E` and sudo's `secure_path` resets `PATH`, so pass `GO=$(command -v go)` if `go` is not at a standard location (the script falls back to `~/.local/go/bin/go` and `/usr/local/go/bin/go`, and fails loudly rather than skipping the boot if it finds none). This is the serial-driven analogue of the appliance lane's SSH-driven two-process reboot (# Medium lane, the LUKS unseal note). The seeded boot additionally carries a **complete acme-dns `enrollment`** block, so it asserts the brain reaches and **applies** its wildcard-TLS pass (`EnsureWildcardTLS` — Caddy's acme-dns DNS-01 issuer for `*.<box-id>.onmoose.io` plus the `:443` listener bound); real Let's Encrypt issuance can't run air-gapped (`restrict=on`, no reach to acme-dns/ACME), so the lane proves *application of the config and the `:443` bind*, not a real cert (the cert itself is the cloud on-ramp's job). This closes the gap that let a hosted box fail to bind `:443` / obtain its wildcard cert pass CI (#278): the prior seed carried no enrollment, so `EnsureWildcardTLS` was never exercised. A fifth **`update` boot** (#382), also on its own fresh overlay + box-id and also seeded with the harness's test-portal key (the update trigger is admin-only, so the box needs a real owner session first), is where the **control-plane updater** meets reality: everything under `POST /api/v1/system/update` — the ledger, the transaction, the trigger — is otherwise proven against a fake Docker. Because the lane is air-gapped and a production box **pulls by digest** (`BUILD.md` # 6), the guest runs **its own registry** on `127.0.0.1:5000` (a test-lane-only image tar under `/var/lib/moose/test-images/`, never the lean image; Docker treats a localhost registry as insecure by default, so no daemon config is involved): `cloud-assertions.sh` derives a new brain + UI with `docker commit` from the images the box is running, pushes both, **deletes both locally and asserts they are gone**, so the updater's pull has to fetch them back. The happy path asserts the step that had never once run — host-agent recreating the brain **while the brain is serving the request that asked for it**: the brain container id must change, the running brain must carry a marker label only the new image has, both files of the `UPDATES.md` # 8.3 declaration (`images.json` + the staged compose) must name the new pair, `/healthz` must answer on the new container, and `GET /api/v1/system/version` must report the new UI ref. The revert is then driven for real: an update to a brain that starts, truncates the brain's SQLite database, drops a marker file and never serves ⇒ the job is `failed` with `failure_mode: health` and `reverted: true` and no `revert_error`, the marker proves the bad brain really ran (so the proof is not vacuous), the ledger is back on the good ref and no longer names the failed one, `moose.db` is a valid SQLite file again (the snapshot was restored **over** the damage), and the box answers an authenticated request on the same session. It is in the publish gate (`MOOSE_CLOUD_BOOTS: "unseeded seeded bios access update"`): an image whose updater cannot replace the brain, or cannot put the box back when the new brain does not serve, must never ship. What it does **not** prove: the derived images are `commit`-made, so no real schema migration, no changed version stamp and no protocol-major refusal; only the `health` failure mode is exercised; the 7-day GC never runs.

## Slow lane — Soak + ISO end-to-end (nightly on `main`)

**What it catches:**
- Race conditions that fail intermittently (Docker readiness races, network-online timing)
- Full ISO → installer → first boot → wizard → bootstrapped state
- TPM rot simulation across simulated kernel updates

**Soak runs:**
- 100 boots of the happy-path test in a loop. Assert zero failures. Race conditions don't fail deterministically — soak catches them; single boots do not.
- Boot with throttled I/O (`iops=200`) and constrained CPU shares to surface Docker-readiness races.

**ISO end-to-end:**
- Build full ISO from the current branch.
- Boot in QEMU with no pre-installed system.
- Run installer (kiosk web UI) headless via Playwright (or equivalent).
- Reboot; run first-run wizard.
- Assert: dashboard reachable; admin account created; bootstrap marker written; recovery passphrase shown exactly once.

**TPM rot simulation:**
- Boot, capture sealed state.
- Replace `vmlinuz` with a different version (simulates a kernel security update).
- Reboot, assert TPM unseal still succeeds — because we sealed against PCR 7 only, not PCR 11 (see `STORAGE.md` # Encryption posture).
- This is the canary for the silent-rot risk over the appliance's lifetime.

## Where each lane runs

| Lane | Trigger | Budget | Blocking? |
|---|---|---|---|
| Fast (`nspawn`) | Every PR push | ~1 min | Yes — blocks merge |
| Medium (QEMU+swtpm) | Every PR push (after fast lane passes) | ~10 min | Yes — blocks merge |
| Slow (soak + ISO) | Nightly on `main` | ~1–2 hours | No — surfaces an alert; does not block PRs |

## Why this shape

- **Fast lane** validates the *spec* — that units declare the right dependencies, drop-ins are wired, synthetic targets exist.
- **Medium lane** validates the *implementation* — that the units, when actually booted, produce the expected sequence and recover from injected failures.
- **Slow lane** validates *time* — that races we couldn't trigger deterministically don't bite under load, and that the appliance ages gracefully (kernel updates, drift, clock skew).

Each lane runs strictly cheaper than the next. PR feedback comes from fast + medium; expensive coverage runs nightly.

## Tooling — locked

- **`swtpm`** (software TPM emulator) for vTPM in QEMU. Mature, widely deployed, supports TPM2 with PCRs.
- **QEMU** for VM boot, with virtio block devices for disks and a virtual UEFI firmware that supports Secure Boot (so PCR 7 sealing tests are honest).
- **Drop-in compatibility with `mkosi`'s test mode** (`mkosi qemu`). This test-story weight helped decide the `live-build` vs. `mkosi` call in `BUILD.md` in mkosi's favor (`DECISIONS.md` 2026-06-16) — the same builder now produces the production ISO, the cloud image, and this test lane.

## Tooling — open

- **Assertion harness language.** Go (matches the rest of the codebase, single language) or Python (mature QEMU/swtpm tooling, larger ecosystem of test helpers). Either works. Tracked in `NEXT.md`.

## What this doc deliberately doesn't pin

- Unit-test conventions inside the brain Go codebase (table tests, mocking philosophy, etc.) — that's an implementation-time call once the codebase exists.
- Frontend test strategy for the dashboard (`WEB_UI.md`) — separate concern, lighter stakes, follows whatever the chosen stack idiomatically uses (`vitest` + `@testing-library/vue` is the obvious default).
- Performance / load testing. Out of scope for the household appliance threat model in v1.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
