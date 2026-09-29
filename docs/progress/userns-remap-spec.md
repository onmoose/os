# userns-remap spec: the three tiers and `root_setup`, #516

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `APP_ISOLATION.md`, `APP_MANIFEST.md`, `APP_LIFECYCLE.md`, `CONTROL_PLANE.md`, `BRAIN_HOST_PROTOCOL.md`, `THREAT_MODEL.md`, `BUILD.md`, `ENVIRONMENT.md`, `DECISIONS.md`, `NEXT.md`; plus status notes in `docs/dev/catalog-import-gaps.md`

This is the spec change that [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md) asked for in What's next item 5, after Step 0 in [userns-remap-pretest.md](userns-remap-pretest.md). It builds on [brain-folder-sources.md](brain-folder-sources.md) (#522), which already changed the folder-source parts of the same specs. Docs only: no Go code and no image change. The build is #523, which also closes #516.

## What was done

### The model, written down

`APP_ISOLATION.md` gains # User-namespace tiers and # The folder-app limit:

- Docker runs with a daemon-wide `userns-remap` on a `moose-remap` range (host ids 1000000 to 1065535).
- Three tiers per container. **default:** remapped, today's sandbox. **caps:** remapped, with `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE` and `FOWNER` back and no `user:` pin. **host:** `userns_mode: host`, today's sandbox, for folder apps, GPU apps, device apps, the socket proxy and the brain.
- The brain picks the host tier, never a manifest. Capabilities never come with the host namespace, in the generator and in the install check.
- Bind dirs are owned by `base+uid` in the default tier and by `base` in the caps tier; managed-service data by `base`. The brain reads `base` from host-agent (`remap_base` on `GET /v1/identity/well-known`, from `/etc/subuid` and `/etc/subgid`) and checks `docker info` for `name=userns`. No env var.
- Without the remap the brain runs today's sandbox and refuses a `root_setup` install. If Docker and host-agent disagree, it refuses app installs.
- The tier is stored on the instance, and an update that would change it is refused.
- The folder-app limit: a folder app stays in the host tier, and a folder app whose image needs root at start must be packaged without it. Per-container ID maps replace "User namespace remap. Breaks too many images" in # Not in v1.

### The manifest field: `root_setup`

`APP_MANIFEST.md` # B gains `root_setup: true`: the image's entrypoint starts as root and must do root work before the app runs (`chown` its data dir, or switch to its own user). The brain maps it to the caps tier. It is a boolean intent, never a capability list or a UID. Admission refuses it with `folders`, `gpu: true`, `devices` or `service_user: true`. It is an optional field with a safe default, so it is additive and `manifest_version` stays 1.

Why this name: it sits next to `service_user`, in the same snake_case boolean style, and says what the image does, not what the brain grants. Names that were weighed:

- `starts_as_root` reads true for nearly every image, since the folderless default already runs as root inside. Authors would set it when they do not need it.
- `drops_privileges` misses the `chown` case, which is poznote's.
- Anything with `capabilities` or `privileged` in it names the grant, which the manifest must not own.

### Other specs

- `CONTROL_PLANE.md` # Locked: control-plane container hardening: the proxy and the brain run with `--userns=host`, always passed. The proxy also gets `--tmpfs /run`, with the reason: haproxy's pid file on image files owned by the remapped root. No capability is given back. Caddy and `moose-ui` stay remapped.
- `APP_LIFECYCLE.md` # Locked: override file contents: the one `cap_add` is the brain's own, for `root_setup`, on a remapped daemon; `userns_mode: host` for the host tier; an author's compose still cannot carry either.
- `BRAIN_HOST_PROTOCOL.md`: `remap_base` on the well-known response.
- `THREAT_MODEL.md` B2: the container escape row now names the shared range, who shares host uid `base` (most folderless apps, Caddy with its certificate store, `moose-ui`), that the caps tier's capabilities stay inside the namespace, and that host-tier containers keep today's exposure. New residual 12.
- `DECISIONS.md` 2026-09-29: turns the remap on, says why not sysbox CE, records the accepted shared-range trade-off, and says it partly flips 2026-05-13 (no `cap_add`).
- `BUILD.md` # User-namespace remap and `ENVIRONMENT.md` # How the profile is realized: `daemon.json`, the `moose-remap` account and range, `SUB_UID_COUNT 0` / `SUB_GID_COUNT 0`, the classic `overlay2` store, both images, fixed for the life of a box, and the rollout order (the `daemon.json` change lands last).

### NEXT.md: closed, not narrowed

`NEXT.md`'s own rule is that an item leaves the doc the moment its design is locked, and that filing the implementation issue is part of the same change. The design is now locked, so the "User-namespace remap for hardcoded-internal-UID app images" item is gone, and #523 was filed. What is left to prove is verification, not design, so it went to #523: proofs 2 and 3 on a provisioned hosted box, proof 5 on a box with a GPU, plunk and formbricks through the brain, an app with `devices`, and a buildkit build only if something needs one.

A smaller item replaces it with the three things that are still open design: images that must run as their own baked user (below), per-container ID maps, and the cross-app data-read product question the old item carried.

### Catalog ledger

`docs/dev/catalog-import-gaps.md` is mutable by design. poznote gets a note, formbricks and plunk move to `planned`, and the two `privilege-drop-denied` entries get a note that the caps tier covers a privilege-dropping folderless app. Each points at the spec and #523, since the `NEXT.md` item they named is gone.

## How it maps to the specs

- Realizes nothing yet. It writes down the model #516 proved, so #523 starts from a spec rather than from a spike branch.
- Keeps "the app declares intent, never a UID" (`DECISIONS.md` 2026-06-10) and "permissions are declared and enforced".
- Keeps admission door-symmetric: an author's compose still cannot carry `cap_add` or `userns_mode`.

## Known gaps & deviations

- **plunk and formbricks are not settled.** The ask was to say they passed in the default tier with plain Docker. The probe shows more precisely that they passed remapped as their own baked user, with **no `user:` pin**. The brain's default tier pins `user:`, so through the brain they would run as another uid and may fail, and the caps tier gives bind dirs to the remapped root, which their user cannot write. The spec says so and lists it as open in `NEXT.md` rather than inventing a fourth tier.
- **`devices` joined the host tier.** The maintainer's model named `folders` and `gpu`. A device node is owned by a real host user or group, the same reason the GPU needs the host tier, so a remapped container could not open it. No app with `devices` was in the proofs; #523 checks one.
- **Two rules are this PR's proposal, not proven:** refusing app installs when Docker and host-agent disagree about the remap, and storing the tier on the instance so an update cannot move it. Both follow from "the owner of the data follows the tier".
- **`remap_base` on the well-known endpoint is this PR's choice.** The spike used an env var. host-agent already owns every host identity number, and the brain container sees no `/etc`, so the endpoint is the natural source. `docker info` alone gives only a data-root path with the base in its name.
- **An OS update must not flip the remap on an existing box.** `BUILD.md` says so. How an image-based update (#486) keeps `daemon.json` per box is not designed here.
- **#461 overlaps.** It proposes a non-root service identity as the folderless default, because root with no capabilities makes images try setup they cannot finish. Under the remap, `root_setup` is another answer for images whose entrypoint needs that root work. This PR does not decide #461.
- **Caddy shares host uid `base`** with most remapped apps, so a container escape from one of them could read Caddy's certificate store. Today the same escape is real root, so this is still better. `THREAT_MODEL.md` names it.
- **The catalog must not offer a `root_setup` app to a box that cannot run it.** An older brain ignores the field and installs the app in the default tier, where it fails. That is a catalog-side filter, a two-repo change, not specced here.

## Review

- **Sonnet (fresh agent): no Block findings.** Two Notes, both fixed. The `service_user` locked decision in `APP_MANIFEST.md` still said "no-userns-remap model"; it now says "in a host-tier container or on a box without the remap". The host row of the tier table said "from the manifest" and "never a manifest field" in one cell; it now names the grants the brain reads.
- **Greptile P2, "Caps-tier escape exposure understated".** Confirmed and fixed. An escape that keeps the caps tier's `SETUID` or `DAC_OVERRIDE` can become any uid in the range, or read past file modes on any range-owned file, so it reaches every remapped container's files, not only its own uid's. `THREAT_MODEL.md` B2, residual 12 and the `DECISIONS.md` entry now say so.
- **Greptile P2, "Catalog references removed item".** Confirmed and fixed. The poznote, formbricks and plunk statuses now name the old item as closed and point at #523.
- **Greptile P2, "Update preservation left undefined".** Confirmed, extended in `BUILD.md`. No update path replaces `daemon.json` today, so nothing breaks yet. The image-based update (#486) must keep the file per box, and #523 must not turn the remap on in both images before that is settled. The mechanism itself belongs to #486, not here.

## What's next

1. #523: build it in small PRs that are inert without the remap, with the image `daemon.json` change last, in both images.
2. The open design topics in the `NEXT.md` item "After the user-namespace remap", the baked-user images first, once #523 has run plunk and formbricks through the brain.
