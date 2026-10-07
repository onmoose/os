# Pull an image from a backup source when upstream fails

- **Status:** done
- **Date:** 2026-10-07
- **Specs touched:** `docs/specs/APP_STORE.md`, `docs/specs/APP_LIFECYCLE.md`, `docs/specs/DECISIONS.md`

Follows [pull-rate-limit-retry.md](pull-rate-limit-retry.md) (#586, #587). That entry retries a pull the registry rate-limits, for about 30 seconds. It does not help when the limit lasts, or when upstream deleted the image. The catalog will now publish, per image, an ordered list of backup `sources` that serve the same bytes, and the box uses them only when upstream fails (#588).

## What was done

- **The manifest models `sources`.** `manifest.ImageRef` gains `Sources []ImageSource`, each with a `ref` (a plain repository) and an `auth` field that is only room for later. The field lives in the `images` block of the manifest document the box fetches at install, next to `digest`. The browse payload (`internal/catalog/wire.go`) does not carry `images`, so it and `testdata/snapshot.json` do not change. `manifest.Parse` is lenient, so an older box reads a manifest with `sources` and drops the field.
- **One pull step, `pullImage`,** in `internal/lifecycle/pinning.go`, replaces the body of `pullWithRetry` (which stays as the no-source form for Door-2). Upstream first. On a rate limit it keeps the #587 ladder: wait 2 s, pull upstream again, and if it is still rate-limited, try each source in order; if every source fails, finish the ladder on upstream. Any other error tries the sources at once, except a local one (`isLocalPullError`: daemon or proxy not reachable, disk full, read-only store, no `docker` binary) and a cancelled install. When every source fails, the error is upstream's, and each source failure is logged. Offline mode passes no sources, so `resolveOffline` engages as before.
- **`sourceRefs`** turns sources into `<ref>@<digest>` and skips, with a log line, a source that sets `auth` or whose `ref` has a tag, a digest or nothing.
- **The override names the reference that was pulled,** because Docker refuses to tag a digest reference under another name. `resolveImages` now carries the pulled reference into `servicePin.ref`.
- **The stored pin keeps a source reference.** `instance_images` gains a `ref` column (additive, `DEFAULT ''`). It is set only for a source pull, so every other row reads as before. `reclaimImages` and `inUseImageRefs` use it, so uninstall removes the image under the source's name.
- **Compose no longer pulls app images by itself.** `writeOverride` sets `pull_policy: never` on every pinned service. Every `compose up` after install (`Start`, the two reconcile paths, `recreateRunning`) now goes through `composeUpInstance`, which runs `ensureImages` first: each image the override names is inspected, and a missing one is pulled through `pullImage`, with sources from the instance's own `manifest.yml`. If the bytes come from somewhere else now, the override and the stored pin are rewritten. A present image costs one `docker image inspect`. Install itself is unchanged: `resolveImages` has just pulled.
- **Digest switch.** Nothing on the box compares a stored pin with the published digest today. The only readers of `instance_images` are uninstall's reclaim and its in-use guard. `InstallFootprint` checks presence by the published digest, so an image pinned by its old index digest shows as a download in the install plan: an over-count, never a wrong install. `APP_STORE.md` # Backup image sources says what a future update check must do.
- **Docs.** `APP_STORE.md` gains # Backup image sources and updates the `images` field, # Trust model, # Failure modes and # Locked decisions. `APP_LIFECYCLE.md` # Locked: image digest pinning gives the pull order, the override rule and `pull_policy: never`. `DECISIONS.md` 2026-10-07 records that image mirroring is no longer deferred. `CLAUDE.md` adds the log field `source`. `docs/architecture.md` names the step on the `lifecycle` row.

## Tests

- **Fake driver** (`internal/lifecycle/pinning_sources_test.go`): a table with one row per upstream error kind (unreachable, timeout, 5xx, `manifest unknown`, 401, 403 move; disk full and daemon down do not), checking the pulls in order and the reference returned. The rate-limit path: sources after the first wait, the ladder finished on upstream when they fail, no source when the limit clears. All sources failing reports upstream's error. A cancelled install does not move. `sourceRefs` skips a source with `auth`, a tag, a digest or no ref. The published shape parses. Through `Install`: a source pull writes `<source>@<digest>` into the override with `pull_policy: never`, stores the ref, and uninstall removes that ref; no sources is unchanged; offline mode never tries a source. Through `Start`: a missing image is pulled again, here from upstream, and the override and pin move back; a present image pulls nothing; an image that cannot be pulled fails before `compose up`.
- **Existing tests** pass unchanged, except the override golden files (`testdata/override-golden`, `override-golden-remap`), which each gain the one `pull_policy: never` line per pinned service. The fake driver now remembers images it pulled by digest, so `ensureImages` finds them on a later start.
- **Real Docker** (`internal/lifecycle/sources_live_test.go`, build tag `dockerlive`), run on Docker 28.1.1 with the classic `overlay2` store: a `registry:2` container as the source and an upstream on a refused port. The install pulls from the source, the container runs `<source>@<digest>`, and `RepoDigests` lists `<source>@<digest>` (plus any other name the same image ID already had locally). After `docker rmi` of the image, `Start` pulls it again from the source. Uninstall removes it.
- **Hosted box:** see # Hosted box test.

## Hosted box test

Run on 2026-10-07 on one real hosted box, provisioned through the deployed control plane with the smoke account (Hetzner `cx23`, the current hosted image, Docker 29.8.2, classic `overlay2` store, userns remap on), and deleted at the end: the portal answered 404 for the box, its name stopped resolving, and the Hetzner server list held no server for it.

- **Getting the branch brain on the box.** An owner-created admin account with a password got SSH and `sudo`. The brain image built from this branch was `docker load`ed, and a host-agent drop-in set `MOOSE_BRAIN_IMAGE` to it, `MOOSE_CATALOG_FILE` to a one-app test snapshot, and `MOOSE_CATALOG_URL` to an unused local port so the remote sync could not replace the snapshot. Then `moose-brain` was removed and host-agent restarted, so host-agent launched the branch brain with its normal run spec, behind the normal docker socket proxy.
- **The source** was a `registry:2` container on the box at `127.0.0.1:5000`, holding `traefik/whoami:v1.10.3` under `mirror/traefik/whoami` (pushed digest `sha256:c899…c38d`). The local copies were removed before the install.
- **Upstream** was `registry.invalid/traefik/whoami:v1.10.3`, which cannot resolve.
- **Install** through the brain API (`POST /api/v1/apps`) reached `running`. The brain logged the upstream DNS failure, then `image pulled from backup source`. The override held `image: 127.0.0.1:5000/mirror/traefik/whoami@sha256:c899…` and `pull_policy: never`, and the container ran that reference.
- **`RepoDigests` on the classic store** after the source pull: `["127.0.0.1:5000/mirror/traefik/whoami@sha256:c899…"]`, and `RepoTags` empty. So the image exists only under the source's digest reference, and `docker image inspect <that ref>` finds it, which is what `ensureImages` relies on.
- **Through the socket proxy:** every pull and inspect above went through the brain's `DOCKER_HOST` (the proxy), with no allowlist change.
- **Missing image:** after stop, `docker rm` of the container and `docker rmi` of the image, `POST /api/v1/apps/{id}/start` reached `running`. The brain logged `image missing, pulling`, the upstream failure and the source pull.
- **Uninstall** removed the instance, and the brain logged `reclaimed image` with the source reference. The image was gone from the store afterwards.

## How it maps to the specs

`APP_STORE.md` # Backup image sources is the box-facing contract: the field, the order, `auth` reserved, and the digest switch. `APP_LIFECYCLE.md` # Locked: image digest pinning holds the pull order and the override rules. The install transaction steps are unchanged: a pull that fails everywhere rolls back as before.

## Known gaps & deviations

- **The order on a 429 follows the design note, not the shortest reading of the issue.** After the first 2 s wait the box pulls upstream once more, and moves to the sources only if that pull is still rate-limited. This catches the very short spike seen in #586 without touching a source.
- **A source is tried once,** with no backoff of its own. A source that is itself rate-limited counts as failed.
- **Which errors are local is a short text match.** An unknown error is treated as a registry error and tries the sources: the cost of a wrong guess is one failed pull.
- **Older instances keep compose's default pull policy.** Their override has no `pull_policy` until they are reinstalled. The brain still pulls a missing image before `up`, so compose finds it present.
- **`ensureImages` inspects every pinned image before each start, reconcile `up` and recreate.** It is one `docker image inspect` per distinct image. A pull it needs runs inside the caller's budget, as compose's own pull did before: `Start` and a recreate use the health-wait budget.
- **The reconcile pass at boot pulls a missing image with no backoff wait** (Greptile on #589). It shares one 30-second budget across every app, and the 2, 4, 8 and 16 second waits could use it all on one rate-limited registry, leaving later apps without their routes. On a rate limit it tries the sources at once and then gives up; the app is left for a later `Start`. A pin whose save failed on an earlier start is repaired on the next one, even when nothing is pulled (also Greptile on #589).
- **The hosted run used a source on the box itself** (`127.0.0.1:5000`), not a remote registry. The pull path is the same; only the network distance differs.
- **No login.** A source that needs one (`auth`) is skipped (#588 leaves login for later).
- **Second review on #589, points not acted on:**
  - `ensureImages` treats any `ImageInspect` error as "image missing" and pulls. A Docker or proxy error then shows as `pull image for service ...`. The pull fails at once with Docker's own error, so only the wording is off. The CLI driver's inspect error does not carry Docker's message, so telling the two apart needs a driver change.
  - A pin with an empty digest is not pulled, while the override says `pull_policy: never`. No code path writes an empty digest today (install, offline and Door-2 all store one).
  - The install plan's `imagePresent` checks `repo@digest` only, so an image present only under a source name counts as a download.

## What's next

The catalog side publishes the `amd64` digest and `sources` for each image (store work, outside this repo). Nothing more is needed on the box for public sources.
