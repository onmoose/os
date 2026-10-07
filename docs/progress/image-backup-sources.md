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

To be filled in by the run on a real hosted box.

## How it maps to the specs

`APP_STORE.md` # Backup image sources is the box-facing contract: the field, the order, `auth` reserved, and the digest switch. `APP_LIFECYCLE.md` # Locked: image digest pinning holds the pull order and the override rules. The install transaction steps are unchanged: a pull that fails everywhere rolls back as before.

## Known gaps & deviations

- **The order on a 429 follows the design note, not the shortest reading of the issue.** After the first 2 s wait the box pulls upstream once more, and moves to the sources only if that pull is still rate-limited. This catches the very short spike seen in #586 without touching a source.
- **A source is tried once,** with no backoff of its own. A source that is itself rate-limited counts as failed.
- **Which errors are local is a short text match.** An unknown error is treated as a registry error and tries the sources: the cost of a wrong guess is one failed pull.
- **Older instances keep compose's default pull policy.** Their override has no `pull_policy` until they are reinstalled. The brain still pulls a missing image before `up`, so compose finds it present.
- **`ensureImages` inspects every pinned image before each start, reconcile `up` and recreate.** It is one `docker image inspect` per distinct image.
- **No login.** A source that needs one (`auth`) is skipped (#588 leaves login for later).

## What's next

The catalog side publishes the `amd64` digest and `sources` for each image (store work, outside this repo). Nothing more is needed on the box for public sources.
