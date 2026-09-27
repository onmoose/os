# Users and groups

> How moose maps dashboard roles to Linux accounts and groups, what privileges each role has on the host, and the rescue path when the dashboard can't help. Companion to `AUTH.md` (credentials, sessions), `STORAGE.md` (group-based file permissions), `FIRST_RUN.md` (account creation), `BRAIN_HOST_PROTOCOL.md` (the `moose` group as IPC channel).

## Principle: UI is the path, SSH is rescue

**Every privileged operation on a moose box has a UI path.** Adding a user, opening an SMB share, changing the network, installing a Tier-2 service, restarting an app — all of it lives in the dashboard. The dashboard talks to host-agent for anything that requires root; the user never types `sudo` in normal use.

**SSH and the shell are rescue tools, not workflows.** A tinkerer can log in and poke around; an admin can `sudo` to unbreak a wedged brain. But if a feature can only be configured from the shell, that's a bug in the spec — file it against the relevant doc, not against this one.

This rule has teeth. It forecloses "we'll ship the daemon now and add the UI later" for any host-mutating feature: no UI, no feature.

## Identity model

User accounts are real Linux users with `/home/<slug>/` home directories. Identity rules (slug derivation, UID range, reserved names, display-name mutability) live in `FIRST_RUN.md` # Identity & display names. Summary for cross-reference:

- **UID range:** ≥ 3000. Below 3000 is reserved for the system and moose internals.
- **Slug is stable, display name is mutable.** Renaming "Cindy" → "Cynthia" doesn't touch the slug, the home directory, or file ownership. `POST /api/v1/users/{id}/name`: self-service for your own name, admin plus the elevation window for somebody else's.
- **The brain derives the slug; no caller supplies one.** `/setup` and `POST /api/v1/users` take a display name only. Part of deriving it is asking the host whether a candidate name already exists, so a derived account never lands on a system or daemon account that set-password would otherwise adopt.
- **Primary group:** the user's own group, created at the same UID/GID.

## Roles map to Linux group membership

Two dashboard roles, two Linux postures.

| Dashboard role | Linux groups                           | `sudo`?     | Shell privileges |
|----------------|----------------------------------------|-------------|------------------|
| **Member**     | own group, `moose-shared`              | No          | Unprivileged user. Reads own files, household shared, that's it. |
| **Admin**      | own group, `moose-shared`, `sudo`      | Yes         | Can become root via `sudo`. Recovery path when the brain is broken. |

**Promote member → admin:** brain calls host-agent → `gpasswd -a <user> sudo`. Demote: `gpasswd -d <user> sudo`. Both operations are reflected in the dashboard's role state and in `/etc/group` atomically (host-agent treats this as one operation; if either side fails, both roll back).

**The first admin** (created in `FIRST_RUN.md` Step 2) is added to `sudo` at account creation. There is no "ungrouped admin" state.

### Why admins get `sudo`

The alternative — "no sudo for anyone, ever" — was rejected because it leaves no recovery path when the brain itself is broken (corrupt SQLite, failed migration, host-agent unreachable from the dashboard). With admins in `sudo`, the rescue path is just "SSH in and fix it." See `DECISIONS.md` 2026-05-15 # Admins get sudo.

The cost is real: a compromised admin SSH session is root. We accept this because (a) the dashboard admin role can already mutate the host through host-agent, so the marginal blast radius is small; (b) SSH is off-by-account-by-default anyway (`AUTH.md` # Device access) — the admin must explicitly opt in.

### Members are unprivileged, full stop

Members cannot `sudo`. There is no opt-in. A member who needs admin power is promoted by an admin in the UI; there is no shell-side equivalent for tinkerers who want a "member with sudo."

If a tinkerer wants a non-moose user account with sudo for shell-only work, they can create one manually (`useradd`, `usermod -aG sudo`). That account won't appear in the dashboard, won't have a brain identity, and won't be a "moose user" — it's outside the system. We don't document this as a supported pattern; we don't break it either.

## Group reference

All groups present on a moose box, why they exist, and who's in them.

| Group           | GID range | Purpose                                                                                   | Members |
|-----------------|-----------|-------------------------------------------------------------------------------------------|---------|
| `<user>`        | ≥ 3000    | A user's own primary group. Created with the account.                                     | The user themselves. |
| `moose-shared`  | fixed     | Read/write access to `/srv/moose/shared/` (household-shared content). Setgid on the dir.  | Every moose user (member or admin). Plus shared service UIDs (e.g., a household Jellyfin). |
| `sudo`          | system    | Standard Debian — members of this group can `sudo`.                                       | Admin users only. |
| `moose`         | fixed     | Access to the host-agent UNIX socket at `/var/run/moose/agent.sock`. Kernel-enforced IPC boundary. | The brain's container runtime UID. **Nothing else.** |

**`moose` vs. `moose-shared` are unrelated, despite the similar name.** `moose` is the IPC channel between brain and host-agent (one member, enforced by a CI test — `AUTH.md` # Authentication between brain and host-agent). `moose-shared` is the household-content permission group (many members, household-wide). Don't conflate them.

## Elevation in the UI

The dashboard role check (`AUTH.md` # Roles) is the primary gate for admin-only operations. On top of that, **destructive or far-reaching operations re-prompt for the password** — the sudo-in-UI pattern, matched to what users expect from macOS System Settings.

- **Trigger:** entering a Settings section that mutates host state (Users, Network, Storage, Updates, Tier-2 services). Not every page; not every action — the destructive/structural ones.
- **Window:** **5 minutes per session.** After re-prompt, the brain marks the session "elevated" with a 5-minute expiry. Subsequent destructive ops in the window don't re-prompt; ops after the window do.
- **Scope:** brain session state, not a real `sudo -v` and not a host-agent capability. This is a UX layer over the role check, nothing more. Host-agent still trusts the brain on every call.
- **What doesn't elevate:** reading state (lists, dashboards, app status), starting/stopping a user's own apps, member-side self-service (changing own password, managing own Tier-3 apps). These rely on the role check alone.

The 5-minute window was chosen over "always re-prompt" because admins doing batch work (set up a new household, configure five users) would otherwise re-type their password every other click. It was chosen over "elevate for the session" because a forgotten open tab shouldn't be a standing admin shell.

## Rescue path: when the brain is broken

If the dashboard is reachable, use it. If it isn't, an admin can:

1. SSH to the box as themselves (assuming SSH is opted-in for that account — `AUTH.md` # Device access).
2. `sudo -i` to become root.
3. Common rescue operations:
   - `systemctl status moose-host-agent` — is host-agent running?
   - `journalctl -u moose-host-agent -n 200` — recent errors.
   - `docker ps -a` — is the brain container up?
   - `docker logs moose-brain --tail 200` — brain logs.
   - `systemctl restart moose-host-agent` — restart the daemon.
   - `docker restart moose-brain` — restart the brain.
4. If the brain's SQLite is corrupt: stop the brain, restore `/var/lib/moose/state/moose.db` **and its `-wal` file** from the newest snapshot, then start it again. Restore both. The brain uses WAL mode, so the newest writes may still sit in the `-wal` file. Copy only the main file and you get a working but *older* database, with no error to warn you (`STORAGE.md` # mount layout). `internal/hostagent/cpupdate` copies both files the same way around a control-plane update. The full snapshot and restore story is its own doc, not yet written.

**Factory reset short of reinstall** is deliberately not documented here. We don't have a clean "reset everything except user content" story yet; treat reinstall + restore from backup as the floor.

## Known gaps

These are tracked in `NEXT.md`; listed here so a reader of this doc isn't surprised.

- **Demotion doesn't kill live sudo capability.** `gpasswd -d <user> sudo` removes the user from the group, but existing SSH sessions retain the capability until they log out (Linux caches group membership at session start). Acceptable for the household trust model; revisit if the threat model changes.
- **TPM-fail-and-admin-forgot-password scenario.** If TPM auto-unlock fails *and* the admin has forgotten their password *and* the recovery code is lost, there's no `root` console password to fall back to (we disable `PermitRootLogin` and don't set one). The LUKS recovery passphrase still gets the box booted; what happens next is unspecified. Honest known gap.
- **Group management UI for `moose-shared`.** Currently every user is in `moose-shared` at creation. Kicking a user off household-shared content (without deleting the account) has no UI surface yet — see `NEXT.md`.

## Locked decisions

- **Dashboard role maps to Linux group membership.** Member = unprivileged. Admin = in `sudo`. Promotion/demotion in the UI flips group membership via host-agent.
- **UI-first, SSH-as-rescue.** Every privileged operation has a UI path. SSH is for unbreaking a broken brain, not for routine admin.
- **5-minute elevation window for destructive UI ops.** Sudo-in-UI pattern, scoped to the brain's session, password re-prompt on entry.
- **Members cannot opt into sudo.** No UI toggle, no documented manual escape hatch. Tinkerers who want shell-root use an admin account or create an out-of-band Linux user.
- **`root` SSH login disabled.** `PermitRootLogin no`. No console root password.
- **The `moose` group has exactly one member** (the brain's runtime UID). CI-enforced — see `AUTH.md` # Authentication between brain and host-agent.

## Open questions

See `NEXT.md` for the live list. Don't add open items here.
