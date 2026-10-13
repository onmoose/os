# Boxes made before #608 move to the Caddy admin socket

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `UPDATES.md` # 8.3 and Locked decisions; `CONTROL_PLANE.md` # Locked: Caddy is moose substrate; `THREAT_MODEL.md` B2 and the list of gaps; `BUILD.md` # As built (copy once); `DECISIONS.md` 2026-10-13; `docs/architecture.md`; `docs/dev/hosted-boot-proof.md`

This closes #612, the first gap [apps-off-ingress.md](apps-off-ingress.md) left. #608 put Caddy's admin API on a unix socket, but only on boxes made from an image that has it: the staged `caddy.json` and `compose.yml` are copied to a box once, at first boot, and an OS update never replaces them. So a box made before #608 kept `"admin": {"listen": "0.0.0.0:2019"}` and no bind for `caddy/`. Its apps are off `moose-ingress`, but Caddy is on each app's network, so an app could still reach the unauthenticated admin API on `moose-caddy:2019` and rewrite routes, including the owner-only gate.

## What was done

- **The brain moves the box itself** (`Manager.MigrateCaddyAdminSocket`, `internal/lifecycle/caddysocket.go`). It runs once at brain startup, after Docker answers and before `EnsureControlPlane`. In order:
  1. Read `admin.listen` from the staged `caddy.json`. A socket means nothing to do: a new box, or one already moved, returns at once.
  2. Work out both edits in memory before writing anything. `caddy.json`: the JSON string of the old address is replaced in place, and the address must appear exactly once. `compose.yml`: one line, `- ./caddy:/run/moose-caddy`, after the last item of the `caddy` service's `volumes` list, at the items' indent. No marshal, so every comment stays. Each result is parsed back and must equal the original with only that change. A compose that already has the bind (a crash between the two writes) is left as it is.
  3. Read the remap base from host-agent, checked against `docker info` (`hostIdentity`, the same check every install makes), and make `caddy/` mode `0700`, owned by that base, or 0 with no remap. It is the same rule as host-agent's `brainlaunch.EnsureCaddySocketDir`, copied because the brain may not import host-side packages. A host-agent from before #608 never makes this dir.
  4. Keep copies of both files as `caddy.json.tcp-admin` and `compose.yml.tcp-admin`, then write `compose.yml`, then `caddy.json`, each atomically. `caddy.json` goes last because it is the file the brain reads to pick its address.
  5. Run compose up, which recreates Caddy because its mounts changed, and wait up to 20s for the admin API on the socket.
  6. If compose up or the wait fails, put `caddy.json` back, then `compose.yml`, and run compose up again with its own 60s budget, so Caddy is recreated on TCP. The brain logs a warning and stays on TCP, and the next brain start tries again.
- **`cmd/brain`** calls it with a 60s budget. On success it points a new Caddy client at the socket (`Manager.SetCaddy`) before anything has used the old one, and logs `caddy admin` with the socket address. The rest of startup then writes every route, the catch-all and the TLS policy, as on any start.
- **The two decisions** the issue left open, both settled by the maintainer and recorded in `DECISIONS.md` 2026-10-13 and `UPDATES.md` # 8.3:
  - **The brain writes one line of `compose.yml`**, a narrow exception to "host-agent is the only writer". host-agent doing it was rejected because it races the brain's startup compose at boot.
  - **The move happens at once.** The issue's step 2 (wait until the ledger has no previous pair or `PreviousExpired`) is dropped. A revert to a brain from before #608 on a moved box gets a brain that cannot change routes: current apps keep their routes, but new installs get none until the next update.
- **Tests** (`internal/lifecycle/caddysocket_test.go`). The fixtures in `testdata/controlplane-pre608/` are the staged files from just before #608 (`git show 48266cd^:dev/control-plane/...`). The move turns them into documents equal to today's `dev/control-plane/` files, with every comment kept, makes the dir for the remap base, keeps the copies, runs compose up once and waits on the socket address. A second run does nothing. A new box does nothing. A socket that does not answer, or a failed compose up, puts both files back byte for byte (and runs compose up again in the first case). A remap mismatch, or a file where `caddy/` goes, writes nothing and runs no compose. The line edit is checked on both list indents, a list at the end of the file, a comment after the list, the bind already there, and refuses a flow list, no `volumes`, and no `caddy` service. The JSON edit refuses an address that appears twice.
- **Cloud lane, `access` boot, new step 6g** (`dev/cloud/cloud-assertions.sh`), after the brain restart of 6e and before the uninstall of 6f. It stages the pre-#608 files (`sed` on the live ones) and brings Caddy up on them, and checks the admin API answers on TCP `:2019`.
  - **6g-1, the move fails.** A non-empty, root-owned dir where the socket goes, which Caddy's remapped root cannot remove. After a brain restart: the brain logs the failed move, both files are back to TCP, whoami answers through Caddy, Caddy answers on TCP, and `moose-caddy:2019` is reachable from inside whoami's netns (the box really stays on TCP).
  - **6g-2, the move.** The blocker and the whole `caddy/` dir removed, as on a box whose host-agent predates #608. After a brain restart: the brain logs the move, `caddy.json` names the socket, the compose binds `caddy/`, `caddy.json.tcp-admin` is kept, `caddy/` is `700` owned by the remap base, the admin API answers on the socket and not on TCP, whoami answers through Caddy, and from inside whoami's netns `moose-caddy:80` answers and `moose-caddy:2019` refuses. The uninstall in 6f then runs through the socket.
  - `VERDICT_TIMEOUT` for the access boot goes from 1290 to 1590.

## How it maps to the specs

- `UPDATES.md` # 8.3 names the exception to the single-writer rule, why it is safe (a different line, only in the brain's startup compose) and the revert cost the maintainer accepted. The Locked decisions bullet points at it.
- `CONTROL_PLANE.md` # Locked: Caddy is moose substrate gains a bullet for the move.
- `THREAT_MODEL.md` B2: the residual is now "until a box takes this control-plane release, or while the move keeps failing", not "every box made before #608".
- `BUILD.md` # As built (copy once) says the brain edits the state copies of these two files once.

## Known gaps & deviations

- **The issue's second "Done when" proof is dropped**, with its step 2: "with a fresh previous pair in the ledger, nothing migrates". The maintainer decided the move happens at once, so there is no ledger check to prove.
- **A revert to a brain from before #608 on a moved box cannot change routes** until the next control-plane update. Accepted by the maintainer (`DECISIONS.md` 2026-10-13). Not proved on the lane: no brain from before #608 is available to it.
- **A crash between the last write and the socket check** (the brain killed in that window of seconds) leaves a box that says socket. If Caddy then cannot make its socket, the next start does not put the files back, because the files already say socket. The copies in `*.tcp-admin` are there for a person to restore by hand. The dir owner is set before any write, so the only known cause of a failed socket is covered.
- **The updater's revert and this step can overlap** if a new brain is still moving the box when host-agent's 60s health check runs out. Both write atomically, but one could undo the other's line. The next brain start puts the bind back either way, and the UI line is written again by the next update.
- **The move costs a Caddy recreate at the first start on this release**, a few seconds with no routes until the brain writes them again in the same startup. A failed move costs two recreates and up to about 20s more.
- **Appliance boxes take the same path**, but the appliance lane (local-only) was not run. Nothing in the code is hosted-only.

## What's next

- #613: write Caddy's config again when it was recreated while the brain runs.
- #610 can put its probe socket in the same `caddy/` dir; this change binds the whole dir and assumes nothing else about its contents.
