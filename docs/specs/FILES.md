# moose in-dashboard file manager

> Working spec for the **Files** destination in the dashboard — the in-product surface for browsing, uploading, downloading, and organizing a user's own content and the household-shared tree. Companion to `STORAGE.md` (the on-disk layout, use-case folders, and `0750`/`02770` permissions this surface renders), `DASHBOARD.md` (the dock destination this fills), `AUTH.md` (session + per-user scoping), `BRAIN_UI_PROTOCOL.md` (the `/api/v1/files/*` surface), `BRAIN_HOST_PROTOCOL.md` (the `/v1/files/*` host-agent family that does the actual filesystem work), `APP_ISOLATION.md` (how apps see the same folders), and `DISCOVERY.md` (whose "the dashboard is the browse surface" claim this makes good on).

## Stance

"Files are first-class, apps are windows" is a load-bearing differentiator (`STORAGE.md`). It is true on disk from day one — user content lives in `/home/<user>/Photos/`, not inside an app's opaque library. But until this surface exists, the *only* specced way a user reaches their own files is **SMB + a desktop OS file manager**. For the north-star audience — the Plex/Synology user who will not mount an SMB share — that makes the differentiator invisible in the product.

The Files destination is the zero-setup answer: open the dashboard, see your folders, upload and download from any device with a browser. It is the in-product half of a pair — SMB (`STORAGE.md` # Cross-device access) is the native bulk path; this is the no-configuration path. **Neither owns the files.** Both are windows on the same `0750`/`02770` tree on disk, exactly as apps are.

This is Synology's flagship surface (File Station) and a core Umbrel module. It is table stakes for the audience, not a power-user feature.

## Scope: two roots, nothing else

The file manager exposes exactly two top-level scopes:

| Scope | Maps to | Who sees it |
|---|---|---|
| **My files** | `/home/<user>/` | The signed-in user, their own home only. |
| **Shared** | `/srv/moose/shared/` | Every household user (the `moose-shared` group, `STORAGE.md` # Permissions). |

Within each root the user browses freely. **The use-case folders are not hardcoded here.** `STORAGE.md` lets users rename, delete, and add folders under their home, so the file manager lists *whatever exists* in `/home/<user>/` rather than a fixed `Photos/Music/...` menu. A renamed `Photos/` → `Pictures/` simply shows as `Pictures/`; a user-created `Receipts/` shows up with no special-casing.

**Out of scope, by construction:**

- **App state** (`/var/lib/moose/state/instances/<id>/`, managed-service data). This is moose's bookkeeping, "never user-facing" (`STORAGE.md`). The file manager is for user *content*, not the plumbing apps write underneath it.
- **Other users' homes.** See # Authorization.
- **The rest of the host filesystem** (`/etc`, `/var`, another `/home/<other>`). The two roots are the entire navigable surface. This is a UX boundary; the security boundary is the UID drop in # Execution.
- **`~/Shared`** as a *separate* entry. `/home/<user>/Shared` is a symlink to `/srv/moose/shared/` (`STORAGE.md`); the file manager presents the shared tree once, as the **Shared** root, and does not also descend into it through the symlink (avoids a confusing double-listing).

**Hidden files** (dotfiles like `~/.config`, `~/.ssh`) are the user's own data but are hidden by default with a "show hidden files" toggle — a Finder-style convenience, not a security boundary (the UID drop already means a user only ever reaches what they could reach on the host anyway).

## Authorization — own + Shared, for everyone

Every user — **admin included** — sees only their own **My files** and the **Shared** root. There is no admin "browse all homes" view.

This is not only a privacy preference; it is *required* by decisions already locked elsewhere:

- `STORAGE.md` # Future: per-user encryption — "No admin-keyed cross-user search or indexing. If a feature needs to read every user's files, it doesn't ship until each user's instance does it for themselves." An admin file-browser over every home is exactly the admin-keyed cross-user read that rule forbids.
- `APP_ISOLATION.md` # Privacy ceiling — "v1 features that touch user data are designed *as if* fscrypt were already on." Under fscrypt, an admin physically cannot read another user's files without that user's key; the file manager is built to that contract now, so the future migration is data-only.

An admin's existing reach into another user's files is unchanged and lives where it already does: **SSH + `sudo` as a rescue path** (`USERS_AND_GROUPS.md` # Rescue path). A deliberate, audited in-dashboard "access this user's files" break-glass surface is a *possible* future addition — but it belongs in the user-management surface as an administrative action with consequences (and would audit, unlike normal file ops), **not** as a tab in the everyday Files view. Tracked as an open item in `NEXT.md`, deliberately out of v1.

**Enforcement is two-layer, kernel-backed:**

1. **Brain (policy).** Resolves the session → user identity; accepts only the two logical roots; rejects path traversal (`..`, absolute escapes) before forwarding. The role check and root containment are the user-visible layer.
2. **host-agent (mechanism), running as the user's UID.** The real backstop — see # Execution. Even if the brain's path check were buggy, a member's operations run as their own UID and the kernel denies `0750` access to another user's home outright.

## Execution — host-agent, as the requesting user's UID

This is the load-bearing architectural decision.

The brain runs in a container behind the docker-socket-proxy and **cannot touch `/home`** — touching the host's root filesystem is host-agent's job by definition (`BRAIN_HOST_PROTOCOL.md` # Scope). So:

> **All file operations execute inside host-agent, with host-agent dropping to the requesting user's Linux UID/GID for the duration of each operation.** The brain is the policy/validation layer and a transparent byte-pipe for transfers; it never reads or writes user content itself.

Privilege-drop per operation (`setresuid`/`setresgid` to the user's moose UID in the 3000+ range, or a forked child that does so) is what makes the whole model safe and correct:

- **POSIX permissions become the kernel-enforced backstop.** `0750` on `/home/<user>/` and `02770` on the shared tree do the real enforcement, identical to how every app already reaches user content (`APP_ISOLATION.md` # User content). Authorization bugs in the brain degrade to "denied," not "leaked."
- **File ownership is correct natively.** An uploaded or newly created file is owned by the user's UID — the same contract the compose `user:` directive gives app instances. No `chown` dance, no `PUID`/`PGID` convention.
- **Symlink attacks are contained for free.** An op running as the user's UID can only follow a symlink to something that UID could already read. A symlink to `/etc/passwd` reads world-readable bytes; a symlink to another user's home is denied by the kernel. No special symlink-resolution guard needed.
- **It is the shape fscrypt wants.** When per-user encryption lands (`STORAGE.md`), host-agent acting as the user with their key loaded is exactly right. Building it this way now keeps that migration data-only.

**host-agent owns path resolution.** The brain passes `(username, root, relative-path)`; host-agent resolves the logical root to an absolute path (`My files` → the user's home via the existing `/v1/users/{username}/home` lookup; `Shared` → `/srv/moose/shared/`), re-validates containment, and executes as the UID. The brain stays free of host-path knowledge, mirroring how app installs resolve bind-mount sources through host-agent rather than hardcoding `/home`.

### Transfers — a streaming binary body

Metadata operations (list, stat, mkdir, move, copy, delete) are small request/response calls. **Uploads and downloads are not** — length-prefixed JSON is the wrong frame for a multi-gigabyte video.

A new wire shape on the brain↔host-agent boundary carries file *content* as a streamed binary body (`application/octet-stream`), with the brain piping bytes between the dashboard's HTTP request/response and host-agent without buffering whole files in memory. Download: host-agent streams the file (as the UID) → brain → dashboard response. Upload: dashboard body → brain → host-agent writes (as the UID).

This is a **deliberate exception to the ">5s = job" rule** (`BRAIN_HOST_PROTOCOL.md`, `BRAIN_UI_PROTOCOL.md`): a transfer can take minutes, but it is pure I/O streaming with transport-native progress (the browser's own upload/download progress), with no server-side job state to poll — the same reasoning that exempts SSE log tails from the job pattern. The exact framing is specified in `BRAIN_HOST_PROTOCOL.md` # files; the dashboard-facing endpoints in `BRAIN_UI_PROTOCOL.md` # files.

## v1 operation set

| Operation | Notes |
|---|---|
| **List / browse** | Directory entries: name, type (file/dir), size, mtime, is-hidden. |
| **Download** | Single file, streamed. |
| **Upload** | One or more files into the current directory, streamed. Browser-native progress. |
| **New folder** | `mkdir` within a root. |
| **Rename** | A move within the same directory. |
| **Move** | Within or across the two roots (a move from **My files** to **Shared** is a real cross-tree move; host-agent does it as the UID, so it only succeeds where the user has write access on both ends — the `moose-shared` group grants the shared side). |
| **Copy** | Same scope rules as move. |
| **Delete** | Permanent (no trash in v1 — see deferred). The user's own data; confirmed in the UI, not elevation-gated (see # Audit & elevation). |

**Deferred to `NEXT.md` (not v1):**

- **Thumbnails / inline preview.** Generic file-type icons + download-to-view in v1; the rich Photos-grid experience is an app's job (Immich), consistent with "apps are windows." A thumbnail pipeline (UID-scoped generation, caching, video frames) is real cost, pinned as deferred.
- **Search** across a tree.
- **Folder / multi-file download as a zip.**
- **Trash + undo** (delete is permanent in v1).
- **In-place editing** (text/office files).
- **Resumable / chunked uploads.** v1 is basic streaming upload; an interrupted large upload restarts.
- **Sharing links** (intentionally exposing one user's file to another, or off-box). v1 sharing is "drop it in Shared/," the same ceiling as `APP_ISOLATION.md` # Routing per instance.

## Dashboard API

New `/api/v1/files/*` surface in `BRAIN_UI_PROTOCOL.md`, public-API-posture consistent with the rest of the brain↔UI contract:

- Metadata ops (list, stat, mkdir, move, copy, delete) are **Pattern A** (sync request/response): `path` is `<root>/<relative>`, errors are the standard `{code, message, details?}`.
- Content ops (download, upload) are **streaming endpoints**, the deliberate non-job exception above.

Authentication is the same `moose_session` cookie (`AUTH.md`); no separate auth path. The full route list, request/response shapes, and the streaming-body contract are specified in `BRAIN_UI_PROTOCOL.md` # files (and its `BRAIN_HOST_PROTOCOL.md` # files counterpart) rather than duplicated here — this doc owns the *product surface and policy*, those docs own the *wire*.

## Relationship to SMB

The file manager and SMB (`STORAGE.md` # Cross-device access) are complementary, not redundant:

- **Same files on disk.** Both operate on `/home/<user>/` and `/srv/moose/shared/` with the same `0750`/`02770` permissions and the same identity (the user's moose account). A file uploaded via the dashboard appears in the SMB share and vice versa, instantly — there is no separate store.
- **Different audiences.** SMB is the native, bulk, "mount it as a drive and drag thousands of photos" path for users who set it up. The file manager is the zero-setup, works-from-any-browser, works-on-the-phone path for users who never will — explicitly the north-star audience.
- **The file manager needs no SMB.** SMB is off-by-account-by-default (`AUTH.md` # Device access); the Files destination works regardless, because it goes through host-agent, not Samba.

## Data import

Folds in the former `NEXT.md` Tier-4 "data-import flows" item.

- **v1 import path: browser upload.** "Add files" from the device you're on, into the current folder. This is the in-product import story for the audience.
- **Deferred: bulk import from a USB stick or a network share** plugged into the box ("copy everything off this drive into ~/Photos"). This rides removable-drive auto-mount (`STORAGE.md`, itself deferred in `NEXT.md`) and is a distinct flow; pinned in `NEXT.md`, not built in v1. The v1 answer for bulk import remains SMB.

## Audit & elevation

- **File operations are not written to the audit log.** The audit log records elevation-class mutations to *principals and apps* — create/delete/role-change a user, install/uninstall/permission-change an app (`CLAUDE.md` # Go code discipline, `LOGGING.md`). A user reading, uploading, or deleting their *own content* is ordinary use, the file-manager equivalent of a read; auditing it would drown the Activity view in noise and make it less useful for the question it exists to answer ("did someone unauthorized try to mutate X?"). (A future admin break-glass into another user's files *would* audit — but that's the deferred item above, not a v1 file op.)
- **File operations do not trigger the 5-minute elevation re-prompt.** Elevation (`USERS_AND_GROUPS.md` # Elevation in the UI) guards destructive *host/Settings* operations — users, network, storage, updates. Deleting your own file is normal use, like deleting in Finder; it is confirmed inline (a "Delete N items?" dialog) but not password-gated.

## Failure modes

- **Data drive missing.** When the data drive is enrolled but absent, `/home` and `/srv/moose` writes are blocked and the box runs degraded (`STORAGE.md` # Data drive enrollment marker, `HEALTH.md`). The Files view surfaces the existing `data-drive-missing` issue (reusing `blocks_writes`) rather than presenting an empty or half-populated tree as if normal — the same degraded-state treatment the rest of the dashboard uses.
- **Disk full on upload.** A write that would exhaust the drive fails with `507 Insufficient Storage`, linked to the `disk-full` health issue (`HEALTH.md`) so the user is routed to "what's eating my disk" rather than a bare error.
- **Permission denied.** Within a user's own two roots this should not occur (the UID owns its home and is in `moose-shared`); if it does (e.g. an app left a file owned by a service UID in a shared folder), it surfaces as a plain per-item error, not a whole-view failure.
- **Large-file streaming.** Transfers are streamed end-to-end (# Transfers); the brain does not buffer whole files, so a 4 GB download does not pin brain memory.

## Cross-references

- `STORAGE.md` — the on-disk layout, use-case folders, `0750`/`02770` permissions, `Shared/`, and the SMB pairing this surface mirrors.
- `DASHBOARD.md` — the **Files** dock destination (# global navigation) this doc fills.
- `AUTH.md` — `moose_session` cookie; per-user scoping; SSH/SMB as the other access paths.
- `BRAIN_UI_PROTOCOL.md` — `/api/v1/files/*` (metadata Pattern A + streaming content).
- `BRAIN_HOST_PROTOCOL.md` — `/v1/files/*` host-agent family, executed as the user UID; the streaming-body wire shape.
- `APP_ISOLATION.md` — apps reach the same folders by bind mount; the UID-as-user and "design as if fscrypt is on" contracts this surface follows.
- `DISCOVERY.md` — the "browse experience is the dashboard, not the OS file manager" claim this makes good on.
- `HEALTH.md` — `data-drive-missing` / `disk-full` issues the Files view honors.

## Open

Tracked in `NEXT.md`. The notable ones:

- **Admin break-glass into another user's files** — a deliberate, audited, user-management-surface action; out of v1, shape to pin before public release.
- **Thumbnails / inline preview**, **search**, **zip download**, **trash + undo**, **in-place editing**, **resumable uploads**, **sharing links** — the deferred v1 cut above.
- **Bulk import from USB / network share** — rides removable-drive auto-mount.
