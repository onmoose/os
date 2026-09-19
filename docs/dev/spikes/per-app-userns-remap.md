# Spike: per-app user-namespace remap for hardcoded-internal-UID app images

> **Time-boxed feasibility spike, 2026-06-24.** Investigation-only — no schema field, no admission change, no `lifecycle.go` edit. Anchors to `docs/specs/NEXT.md` # Tier-4 "User-namespace remap for hardcoded-internal-UID app images" and its two gating questions, and to issue #193. Reproduces the gap and tests a fix on the real runtime, then lands one verdict.

## The question

Can moose run **one** app under a private UID remap (so the app's in-container root / `chown` / `setuid` succeed but are powerless on the host) while **every other** app stays under the normal Tier-3 sandbox — on our actual runtime?

## Verdict: **(B) RESHAPE**

Per-app UID remap is **not achievable on the runtime we ship today** (stock `dockerd` + `docker compose`). Classic Docker's `userns-remap` is a **daemon-global** flag — one mapping for *every* container on the daemon (brain, managed Postgres, Caddy, all apps) — and the only per-container knob is `--userns=host`, an opt-**out**. There is no per-container opt-**in**. Proven below on Docker 28.1.1, our target engine.

It becomes achievable only with a **runtime change**: registering **`sysbox-runc`** as a second OCI runtime and selecting it per-service via the compose `runtime:` field (which *is* reachable through our compose-CLI driver, and which admission does not currently police). Sysbox gives each container its own private user namespace with a distinct auto-allocated subuid range, and lets the container run its baked root entrypoint / `chown` / `setuid`-drop while remaining host-powerless. That is a real reshape of both the image/boot pipeline (a new package + daemons + boot ordering) and the per-app sandbox shape (we would *restore* the caps `cap_drop: ALL` strips, made safe by the remap) — so it carries a `THREAT_MODEL.md` delta, which is exactly why `APP_ISOLATION.md` # Not in v1 defers it.

`rootless dockerd` is **not** a path: it still runs one daemon with one subuid mapping for the rootless user — same daemon-global model, just unprivileged — so it does not deliver per-app distinct mappings, and it would reshape how the whole box runs. Per-app distinct mapping straight from the container CLI is a rootless-**Podman** strength (daemonless, per-`run` `--uidmap`), which `docker compose` is not.

A **folderless-first** cut of (B) is viable and covers all the blocked apps; **folder apps stay deferred** because the owner-real-UID model collides with any remap (sub-question 4). This validates `NEXT.md`'s scoping instinct.

---

## Evidence per sub-question

### 1. Does stock `dockerd` + `docker compose` expose any per-container userns opt-IN? — **No.**

- **The driver is the `docker compose` CLI, one shared daemon.** `internal/lifecycle/docker.go:181` `composeRun` shells `docker compose -f compose.yml -f compose.override.yml --env-file .env -p <project> up -d`. There is no daemon-selection or per-app runtime argument anywhere in the driver; every app, the managed services (`ServiceUp`, `docker.go:195`), and the control plane (`ControlPlaneUp`, `docker.go:204`) talk to the same Docker engine. The brain's prod path is one socket-proxied daemon (`CONTROL_PLANE.md`). Target engine is upstream `docker-ce` (`docs/specs/BUILD.md:55`, `:216`).
- **`userns-remap` is daemon-global.** To exercise it I had to launch a *whole separate* `dockerd` with `--userns-remap=andrei`; the flag lives in daemon config (`/etc/docker/daemon.json`), not on a container. `docker info` on that daemon reports `userns` under Security Options; the stock daemon reports none.
- **The per-container flag is opt-OUT only.** On the stock daemon:

  ```
  $ docker run --rm --userns=andrei hello-world
  docker: --userns: invalid USER mode          # arbitrary mapping rejected at parse time
  $ docker run --help | grep -- --userns
        --userns string   User namespace to use # only valid non-empty value is `host`
  ```

  `--userns=host` parses (it is the documented opt-out from a daemon that *has* remap on); a named/custom per-container mapping does not exist. Confirmed Docker **28.1.1**.

**Conclusion:** on stock `dockerd`, "remap app X, leave the rest alone" is impossible. The only way to get *distinct* mappings is *distinct daemons* (what the experiment did) — operationally absurd for an appliance.

### 2. Rootless `dockerd` — per-app distinct mappings? — **No, and costly.**

Rootless `dockerd` runs a single daemon as an unprivileged user with a single subuid range; it is the same daemon-global remap model, not per-app. It also reshapes the whole runtime (socket location, storage/overlay support, `slirp4netns`/RootlessKit networking, and it sits awkwardly against the privileged host-agent that owns LUKS/PAM/Avahi). High cost, and it does not even answer the question. Rejected.

### 3. `runtime:` swap (sysbox-runc / gVisor) — **yes for sysbox, and it is reachable through our driver.**

- **`runtime:` is reachable and currently un-policed.** Compose passes a service's `runtime:` straight to the daemon. `internal/admission/admission.go`'s `rawService` (`:30`) parses `ports/privileged/cap_add/network_mode/pid/ipc/userns_mode/build/volumes/extends/user/deploy` — **no `runtime` field** — and `checkService` (`:109`) never inspects it. So the brain's override generator *could* emit `runtime: sysbox-runc` per-service today and compose would honour it. (Latent gap: a Door-2 paste could also set `runtime:` and slip past admission — harmless today since no alt-runtime is installed, but **if (B) ships, admission must control which runtime an app may select**.)
- **sysbox-runc** is purpose-built for exactly this: per-container private userns, distinct auto-allocated subuid range per container, and the container may run as root / `chown` / `setuid`-drop while being host-unprivileged. It is registered under daemon.json `runtimes:` and selected per-container, so "one app remapped, the rest on `runc`" is native to it. **Cost:** a new package in the image (`sysbox-ce` = `sysbox-runc` + the `sysbox-mgr` and `sysbox-fs` daemons), a daemon.json runtime registration, a boot-ordering dependency (the sysbox daemons must be up before any sysbox app), Trixie-on-`sysbox-ce` package validation, and it must be exercised in the QEMU outer loop (needs a real kernel; ID-mapped mounts want kernel ≥ 5.12, which Trixie's 6.x satisfies).
- **gVisor (`runsc`)** is also a `runtime:` swap with its own userns, but its value is syscall interception/sandboxing with app-compat and perf costs; it is a worse fit than sysbox for the specific "let the image's root entrypoint and chown work" need. Possible alternative, not the recommendation.

**This is the reshape:** (B) means adopting sysbox-runc as an opt-in per-app runtime — not a config tweak to the runtime we have.

### 4. Folder-identity collision, and the folderless exemption — **both confirmed.**

- **Folder apps collide.** A personal-source folder app runs as the **owner's real host UID (≥ 3000)** — `lifecycle.go:531` (`iso.uid, iso.gid, iso.home = rh.UID, rh.GID, rh.HomePath`), per `APP_ISOLATION.md` # Runtime identity & data ownership — precisely so it can read/write `/home/<user>/` natively. A userns remap shifts in-container UIDs into a subordinate range, so a remapped container can no longer act as the real owner on `~/`; every use-case-folder bind would need remap-offset reconciliation. This is `NEXT.md`'s open question 2 and it remains the hard, unsolved part. **Folder apps stay out of scope.**
- **Folderless apps are exempt.** A folderless app (brain-euid default, or `service_user`) binds **no** owner home — it writes only its own `./data` under the instance dir, which the brain creates + chowns (`lifecycle.go:572-584`). Under a remap, moose (or the runtime) targets the *remapped* host uid; **no real host principal is involved**, so there is no collision. A folderless-only first cut is therefore viable while folder apps stay deferred — the key scoping lever, **validated**.
- **The blocked apps are all folderless.** poznote (#90), postiz (#128), formbricks (#182/#193), kimai (#89 secondary) all store state in their own data dir and declare no use-case `folders` grant (`docs/dev/catalog-import-gaps.md` # nonroot-data-ownership). So folderless-first actually covers the apps that justify the spike.

### 5. Bind-chown offset — **depends on the runtime, and sysbox largely removes it.**

Today `lifecycle.go:577` does `os.Chown(dir, iso.uid, iso.gid)` per declared relative bind dir. The math under a remap: an in-container uid `u` appears on the host as `base + u`. The snag is that the image's *internal* uid (poznote's `www-data` = 82) is **unknown to moose** — that opacity is the whole problem — so moose cannot pre-compute `base + 82` itself.

- With **classic daemon-global remap**: moose would have to chown to `base + u` for an unknown `u` → cannot. Another reason classic remap fails here.
- With **sysbox + ID-mapped mounts**: sysbox owns the bind-mount shifting (it ID-maps / chowns the bind into the container's allocated range on mount), so moose can leave the chown targeting the runtime UID and let sysbox shift — likely **no offset math in the brain at all**, pending on-hardware confirmation. If ID-mapped mounts are unavailable, a per-instance subuid *range* would be allocated by host-agent (a sibling of `AllocateAppServiceIdentity`, `docker.go:124`) and the offset logic would live in the `isolation` struct (`lifecycle.go:86`) and the bind-dir loop (`lifecycle.go:572-584`).

---

## Experiment

**Setup.** Docker **28.1.1** (= our `docker-ce` target), registry reachable, no userns-remap on the stock daemon, no rootless, **sysbox not installed**. Subject: `ghcr.io/timothepoznanski/poznote:6` (digest `sha256:75049e…`), a clean hardcoded-internal-UID image. Its `init.sh` runs `chown -R www-data:www-data /var/www/html/data` under `set -e` (line 48); php-fpm pool is `user = www-data` and a supervisord program is `user=www-data`; `www-data` = **uid 82**, and the baked `/var/www/html/data` is owned `82:82`. Plain-Docker mimicry of the moose sandbox, **indicative, not authoritative** (see caveats).

**Baseline — reproduce moose's Tier-3 sandbox; confirm it fails.** `--cap-drop ALL --security-opt no-new-privileges:true`, a writable data bind, in both identity shapes the platform has:

| Run | Flags | Result |
|---|---|---|
| A — `service_user` shape | `--user 2150:2150`, bind chowned to 2150 | **EXIT 1**, `chown: /var/www/html/data: Operation not permitted` |
| B — folderless-root shape | `--user 0:0`, root-owned bind | **EXIT 1**, identical `chown … Operation not permitted` |

Confirms #193 / the ledger exactly: under `cap_drop: ALL` even uid 0 cannot `chown` (no `CAP_CHOWN`), so `init.sh`'s `set -e` aborts boot.

**The fix test — an isolated `dockerd --userns-remap=andrei`** (andrei's subuid range `100000:65536`), image transferred by `docker save | docker load` (the isolated daemon had no network):

| Test | Config | Result |
|---|---|---|
| 1 | userns-remap **+ `cap_drop: ALL`** + `--user 0` | **EXIT 1**, same `chown … Operation not permitted` |
| 2 | userns-remap **+ default caps** + image's own root entrypoint | **boots clean** — nginx, php-fpm, reminder-worker all `RUNNING`, container `running` |

Test 2 host-side `ls -ln` of the data the container wrote:

```
drwxrwxr-x 2 100082 100082 4096 … database
-rw-r--r-- 1 100082 100082  64  … .mcp_token
```

In-container `www-data` (82) lands on the host as uid **100082** = `100000 + 82`; in-container root (0) → host `100000`. Both are unprivileged, non-principal host uids.

**Two load-bearing findings from this matrix:**

1. **Userns-remap does NOT rescue the *existing* sandbox.** Test 1 proves that keeping `cap_drop: ALL` still fails — the missing `CAP_CHOWN` is missing in any namespace. The mechanism that works is **remap + restoring the caps** (Test 2). So (B) is not "add userns to today's sandbox"; it is a different per-app sandbox shape (restore CHOWN/SETUID/SETGID, made safe by the remap). This is the `THREAT_MODEL.md` delta.
2. **The host-powerless property holds.** Remapped in-container root (host 100000), given a bind owned by *real* host root `0:0` at mode `0700`:

   ```
   cat  /mnt/hostroot/secret  → Permission denied
   echo > /mnt/hostroot/owned → Permission denied
   chown 0:0 /mnt/hostroot    → Operation not permitted   (cannot claim real-root ownership)
   ```

   In-container privilege is genuinely powerless on the host — the property the whole approach depends on.

**Caveats (indicative, not authoritative).**

- This was **daemon-global** remap on a throwaway second daemon, *not* the per-app `runtime: sysbox-runc` that (B) actually proposes. It proves the **userns property** (remap → in-container privilege works yet is host-powerless); it does **not** prove sysbox's per-container delivery, its bind-shifting, or coexistence with `runc` apps on one daemon — **sysbox was not installed and was not tested**.
- Plain Docker mimicking the moose sandbox is not moose. Only a real brain boot exercises the override generator, the host-agent identity allocation, and admission together.
- Only poznote's boot was observed, not the full poznote feature path, and not postiz/formbricks/kimai.

---

## Scoped follow-up (verdict B — gated on the runtime decision)

An implementation issue should **not** open until the runtime decision (adopt sysbox-runc?) is taken, because the manifest seam and the override change both depend on it. When it does, scope it **folderless-first**:

- **Runtime.** Add `sysbox-ce` to the image (`BUILD.md`): `sysbox-runc` + `sysbox-mgr` + `sysbox-fs`, registered under daemon.json `runtimes:`; validate the package on Trixie; add the boot-ordering dependency (sysbox daemons before sysbox apps) in `BOOT.md` / a host-agent or systemd unit.
- **Manifest seam.** A new *intent* field (never a UID — `APP_ISOLATION.md` and `admission.go:159` `checkUser` keep that line). It declares "this image hardcodes an internal non-root uid; run it remapped," distinct from `service_user` (which is "adopt the runtime uid I assign"). Folderless-only; reject it alongside a `folders` grant exactly as `CheckManifest` (`admission.go:66`) already rejects `service_user` + folders.
- **Override-generator change** (`writeOverride`, `lifecycle.go:1426`): for a remap-opted service, emit `runtime: sysbox-runc`, **drop the `user:` pin** (let the image's own `USER`/entrypoint run), and **do not** stamp `cap_drop: ALL` (or stamp a reduced set) so the entrypoint's chown/setuid can run inside the userns. This is a per-app branch in the generator — every other app keeps the byte-for-byte Tier-3 envelope.
- **Admission.** Police `runtime:` — only the brain may select an alt-runtime, never a Door-2 paste (close the latent gap from sub-question 3).
- **Bind-chown.** Prefer sysbox + ID-mapped mounts so the brain's existing chown stays as-is and sysbox shifts (confirm on hardware); fall back to a host-agent-allocated per-instance subuid *range* + offset math in the `isolation` struct only if ID-mapped mounts are unavailable.
- **Threat model.** `THREAT_MODEL.md` + `DECISIONS.md` entry for the shape change (cap-restore-under-remap replacing cap_drop:ALL for these apps); update `APP_ISOLATION.md` # Not in v1 / # Runtime identity & data ownership when it lands.
- **Apps unblocked:** poznote (#90), postiz (#128), formbricks (#182/#193), kimai-secondary (#89) — revisit `docs/dev/catalog-import-gaps.md` # nonroot-data-ownership.

---

## What a real moose-lane boot must verify before trusting this

The plain-Docker experiment is indicative; a QEMU medium-lane boot **with `sysbox-ce` installed** must confirm, end-to-end through the brain:

1. The override generator emitting `runtime: sysbox-runc` (+ no `user:` pin, caps restored) actually boots poznote's root entrypoint to healthy.
2. Apps on the **same daemon** that did *not* opt in stay on `runc`, byte-for-byte the current sandbox, unaffected.
3. The app's data bind is writable and its ownership **survives container recreation** (sysbox chown / ID-map behaviour — the spike did not test recreation).
4. Host-side `ls -n` shows the app's files in a **per-instance** subuid range, distinct from other apps and from every real host principal (≥3000 users, the 2000/2001 well-known, system accounts).
5. Admission's bright lines (`privileged`, docker socket, host namespaces, `cap_add`, host ports, numeric `user:`) still hold under the runtime swap, and the new `runtime:` control rejects a Door-2 self-selection.
6. The brain, managed Postgres, and Caddy continue on default `runc` (sysbox is opt-in, never the default runtime).
7. Boot ordering: the sysbox daemons are up before any sysbox app's `compose up`, including after a reboot/reconcile.

---

*Recommendation to the requester: this is a clean (B). If you want to proceed, the next artifact is a **runtime-adoption decision** (sysbox-runc yes/no, with the image/boot cost above), not an implementation issue — the implementation seam only firms up once that's chosen. Say the word and I can also post this verdict as a comment on #193 to keep the deferral record current; I left that as your call rather than acting outward on my own.*
