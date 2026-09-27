// The SSH screen's draft (issue #494): the changes a user has made on
// Settings → SSH and not saved yet, plus the sessionStorage copy that carries
// them across a reload or the hosted owner's portal confirm round-trip.
//
// The draft is a set of changes on top of the box's state, not a copy of it.
// enabled and requirePassword are null while they follow the box, and the key
// list is "these held keys removed, these new keys added". So when the box's
// state moves under an open screen (a refetch on focus, a key added from another
// tab), the screen shows the new state with the user's changes still on top,
// instead of quietly saving an old copy over it.
import type { SSHAccess } from "@/api";

export interface PendingKey {
  // Local only. The brain gives the key its real id when it is saved.
  tempId: string;
  publicKey: string;
  label: string;
}

export interface SSHDraft {
  enabled: boolean | null;
  requirePassword: boolean | null;
  removed: string[];
  added: PendingKey[];
}

export function emptyDraft(): SSHDraft {
  return { enabled: null, requirePassword: null, removed: [], added: [] };
}

export function isDirty(d: SSHDraft): boolean {
  return (
    d.enabled !== null || d.requirePassword !== null || d.removed.length > 0 || d.added.length > 0
  );
}

// keyBlob is the base64 body of a key line, the part that is the key. The brain
// re-serialises what it stores, so the text a user pasted and the line it stored
// can differ in options and spacing; the body is what they share.
export function keyBlob(line: string): string {
  const parts = line.trim().split(/\s+/);
  // A pasted line may start with authorized_keys options; the body follows the
  // key type, which always starts with "ssh-", "ecdsa-" or "sk-".
  const typeAt = parts.findIndex((p) => /^(ssh-|ecdsa-|sk-)/.test(p));
  return typeAt >= 0 ? (parts[typeAt + 1] ?? "") : "";
}

// A private key must never enter the draft, because the draft is written to
// sessionStorage. The brain refuses one too, but only when it is sent, and by
// then it would already be on disk in the browser profile.
export function looksLikePrivateKey(text: string): boolean {
  return text.includes("PRIVATE KEY");
}

// buildSaveBody turns the draft into the whole-state PUT /me/ssh body. Kept keys
// come first, in the order the box holds them, then the new ones: the brain
// reports a bad key by its index in this list, and keptCount is what maps that
// index back to a pending key.
export function buildSaveBody(d: SSHDraft, box: SSHAccess, ownerHasNoPassword: boolean) {
  const removed = new Set(d.removed);
  const kept = (box.keys ?? []).filter((k) => !removed.has(k.id));
  const requirePassword = d.requirePassword ?? box.require_password;
  return {
    keptCount: kept.length,
    body: {
      enabled: d.enabled ?? box.enabled,
      // The hosted owner has no password on this box (sso.go), so asking sshd
      // for one would lock them out. Never send true for them.
      require_password: ownerHasNoPassword ? false : requirePassword,
      keys: [
        ...kept.map((k) => ({ id: k.id })),
        ...d.added.map((a) => ({ public_key: a.publicKey, label: a.label })),
      ],
    },
  };
}

// ── sessionStorage ────────────────────────────────────────────────────────────
// One entry per user id, so two accounts signed in one after the other in the
// same tab never inherit each other's draft. The box state is stored next to
// the draft so a restore can tell the user when it moved in the meantime.

interface Stored {
  v: 1;
  draft: SSHDraft;
  base: { enabled: boolean; requirePassword: boolean; keyIds: string[] };
}

function storageKey(userId: string) {
  return `moose.ssh-draft.${userId}`;
}

// storeDraft writes the draft while it is dirty and removes the entry once it is
// clean, so a visit that changes nothing leaves nothing behind. Storage can be
// full or blocked (private windows); the draft then just does not survive a
// reload, which is how the screen behaved before it had one.
export function storeDraft(userId: string, d: SSHDraft, box: SSHAccess) {
  try {
    if (!isDirty(d)) {
      sessionStorage.removeItem(storageKey(userId));
      return;
    }
    const stored: Stored = {
      v: 1,
      draft: d,
      base: {
        enabled: box.enabled,
        requirePassword: box.require_password,
        keyIds: (box.keys ?? []).map((k) => k.id),
      },
    };
    sessionStorage.setItem(storageKey(userId), JSON.stringify(stored));
  } catch {
    // See above: losing the copy is the old behaviour, not a failure to report.
  }
}

export function clearStoredDraft(userId: string) {
  try {
    sessionStorage.removeItem(storageKey(userId));
  } catch {
    // Nothing to clear if storage is unavailable.
  }
}

// restoreDraft reads a stored draft and fits it to the box's current state:
//   - a removal of a key the box no longer holds is dropped;
//   - a new key the box now holds (saved from another tab) is dropped, so it
//     shows as a normal key rather than as an addition;
//   - an enabled or password choice that now matches the box goes back to
//     following it.
// moved is true when the box's state differs from when the draft was written, so
// the screen can say so. A draft with nothing left after this is cleared, and
// null comes back.
export function restoreDraft(
  userId: string,
  box: SSHAccess,
): { draft: SSHDraft; moved: boolean } | null {
  let stored: Stored;
  try {
    const raw = sessionStorage.getItem(storageKey(userId));
    if (!raw) return null;
    stored = JSON.parse(raw) as Stored;
  } catch {
    return null;
  }
  if (stored?.v !== 1 || !stored.draft || !stored.base) {
    clearStoredDraft(userId);
    return null;
  }

  const boxKeys = box.keys ?? [];
  const boxIds = new Set(boxKeys.map((k) => k.id));
  const boxBlobs = new Set(boxKeys.map((k) => keyBlob(k.public_key)));
  const d = stored.draft;
  const draft: SSHDraft = {
    enabled: d.enabled === null || d.enabled === box.enabled ? null : d.enabled,
    requirePassword:
      d.requirePassword === null || d.requirePassword === box.require_password
        ? null
        : d.requirePassword,
    removed: (d.removed ?? []).filter((id) => boxIds.has(id)),
    added: (d.added ?? []).filter((a) => !boxBlobs.has(keyBlob(a.publicKey))),
  };

  const baseIds = new Set(stored.base.keyIds);
  const moved =
    stored.base.enabled !== box.enabled ||
    stored.base.requirePassword !== box.require_password ||
    baseIds.size !== boxIds.size ||
    [...boxIds].some((id) => !baseIds.has(id));

  if (!isDirty(draft)) {
    clearStoredDraft(userId);
    return null;
  }
  return { draft, moved };
}
