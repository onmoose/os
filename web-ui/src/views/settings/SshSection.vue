<script setup lang="ts">
// Settings → SSH: the signed-in user's own shell access (AUTH.md # Device
// access, issues #482 and #494). Every user manages their own, and no admin
// manages it for them, which is why the route sits under /me and this screen has
// no role gate.
//
// AUTH.md calls this panel "Device access (SSH + SMB)". SMB has no API yet, so
// the screen is named SSH and carries SSH alone. When file shares land they join
// this screen and the name goes back to Device access.
//
// Shape: one switch that turns SSH on, then an "Authentication methods" pair.
// The two methods are styled the same on purpose, because they are the same kind
// of thing: a lock this account must pass. The one the box makes mandatory shows
// an on switch that cannot be moved; the other one is the account's own choice.
// Both on means sshd demands both, so the pair is an AND and never a menu.
//
// The screen is a draft (#494). Nothing reaches the box until Save, which sends
// the whole desired state in one PUT, asks for elevation once, and is checked by
// the brain as one change. That is what lets a user rotate their only key:
// remove it, paste the new one, Save. The draft lives in sshDraft.ts, along with
// the sessionStorage copy that carries it across a reload or the hosted owner's
// portal confirm round-trip.
//
// The profile rule is NOT decided here. The brain answers it in key_required:
// true on hosted, where a public key is the mandatory method, and false on the
// appliance, where the password is. The server enforces both rows whatever this
// screen does, so reading the flag keeps the two from drifting.
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { onBeforeRouteLeave } from "vue-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/vue-query";
import { SwitchRoot, SwitchThumb } from "reka-ui";
import { KeyRound, Lock, Trash2, Undo2, Upload } from "lucide-vue-next";
import { api, ApiError, type SSHAccess } from "@/api";
import { isLeavingForConfirm, withElevation } from "@/elevate";
import { isHosted, isBoxOwner, useAuth } from "@/auth";
import {
  buildSaveBody,
  clearStoredDraft,
  emptyDraft,
  isDirty,
  keyBlob,
  looksLikePrivateKey,
  restoreDraft,
  storeDraft,
  type SSHDraft,
} from "@/sshDraft";
import Button from "@/components/ui/Button.vue";

const qc = useQueryClient();
const { currentUser } = useAuth();

const ssh = useQuery({
  queryKey: ["ssh"],
  queryFn: () => api.get<SSHAccess>("/me/ssh"),
});

const box = computed(() => ssh.data.value);
// keys is nullable on the wire: an account with none serialises as null.
const heldKeys = computed(() => box.value?.keys ?? []);
const keyRequired = computed(() => box.value?.key_required ?? false);

// ── the draft ──────────────────────────────────────────────────────────────────
const draft = ref<SSHDraft>(emptyDraft());

const enabled = computed(() => draft.value.enabled ?? box.value?.enabled ?? false);
const removedIds = computed(() => new Set(draft.value.removed));
// The key set the account would hold after Save. Keys the box no longer holds
// drop out of it by themselves, so a removal made elsewhere cannot count twice.
const finalKeyCount = computed(
  () =>
    heldKeys.value.filter((k) => !removedIds.value.has(k.id)).length + draft.value.added.length,
);
const keysChanged = computed(() => draft.value.removed.length > 0 || draft.value.added.length > 0);

// The hosted box owner cannot use the password method. Their box password was
// generated during the portal handshake and thrown away (internal/api/sso.go),
// so nobody knows it and sshd would demand a string they cannot type. The brain
// does not guard this, and should not: it cannot tell an owner asking for a
// second lock from anybody else asking for one. This screen is the only place
// the case is handled, which is why it is written down here.
const ownerHasNoPassword = computed(() => isHosted() && isBoxOwner());

// What the Password switch shows. Appliance: on and fixed, it is the mandatory
// factor. Hosted owner: off and fixed, there is no password to ask for. Anyone
// else on hosted: their own choice.
const passwordOn = computed(() => {
  if (!keyRequired.value) return true;
  if (ownerHasNoPassword.value) return false;
  return draft.value.requirePassword ?? box.value?.require_password ?? false;
});

// Hosted refuses an enable with no key, so the top switch stays off until the
// draft holds one. The appliance always has the password behind it.
const needsKeyFirst = computed(() => keyRequired.value && finalKeyCount.value === 0);

// The one state Save refuses: hosted, SSH on, and no key left. Reachable by
// removing every key from an account that has SSH on. Save is hidden while it
// holds, and the reason is shown on the key card, which is the control that can
// fix it.
const invalidReason = computed(() =>
  keyRequired.value && enabled.value && finalKeyCount.value === 0
    ? "Keep at least one key while SSH is on. This box needs a key to let you in."
    : "",
);

// Hidden while SSH is off, because a lock on a door nobody can open is noise.
// Two exceptions: a hosted box with no key yet, where the key card is the only
// way to reach the first key, and a draft with key changes in it, which must stay
// in view until it is saved or cancelled.
const showMethods = computed(() => enabled.value || needsKeyFirst.value || keysChanged.value);

// The add form. Its text is part of the draft for Save and for "unsaved
// changes", but it is not stored: a half-typed paste is not worth the risk of
// writing whatever it is to sessionStorage.
const showAddKey = ref(false);
const keyText = ref("");
const keyLabel = ref("");
const fileInput = ref<HTMLInputElement | null>(null);

// A key is "in use" when the account would hold one. There is no separate flag
// for it on the wire, and there should not be: a key set of zero IS the method
// being off. On hosted the box requires the method whatever the key list says,
// so the switch reads on and cannot be moved. The open add form counts, so the
// switch moves the moment it is clicked.
const keysInUse = computed(
  () => keyRequired.value || finalKeyCount.value > 0 || showAddKey.value,
);

const dirty = computed(() => isDirty(draft.value) || keyText.value.trim() !== "");

// ── errors ─────────────────────────────────────────────────────────────────────
const addKeyError = ref("");
const saveError = ref("");
// A refused new key, shown under that key's row. Keyed by the pending key's
// tempId, from the index the brain names in its error location.
const pendingKeyErrors = ref<Record<string, string>>({});

function errorMessage(e: unknown): string {
  // A dismissed confirm prompt is a deliberate no-op, not a failure.
  if (e instanceof ApiError && e.code === "elevation_cancelled") return "";
  return e instanceof ApiError ? e.message : "Something went wrong.";
}

function clearErrors() {
  addKeyError.value = "";
  saveError.value = "";
  pendingKeyErrors.value = {};
}

// ── editing ────────────────────────────────────────────────────────────────────
// A switch set back to the box's value stops being a change, so turning something
// on and off again leaves a clean draft and Save and Cancel go away again.
function setEnabled(on: boolean) {
  draft.value.enabled = on === box.value?.enabled ? null : on;
}

function setPassword(on: boolean) {
  draft.value.requirePassword = on === box.value?.require_password ? null : on;
}

function markRemoved(id: string) {
  if (!draft.value.removed.includes(id)) draft.value.removed.push(id);
}

function undoRemove(id: string) {
  draft.value.removed = draft.value.removed.filter((r) => r !== id);
}

function dropPending(tempId: string) {
  draft.value.added = draft.value.added.filter((a) => a.tempId !== tempId);
  delete pendingKeyErrors.value[tempId];
}

// Turning the key method on opens the add form, because a method with no key is
// not on yet. Turning it off marks every key for removal. Nothing is lost until
// Save, and each key can be brought back with Undo, so this no longer asks first.
// Only reachable on the appliance: on hosted the switch is fixed on.
function toggleKeys(on: boolean) {
  addKeyError.value = "";
  if (on) {
    showAddKey.value = true;
    return;
  }
  cancelAddKey();
  draft.value.added = [];
  pendingKeyErrors.value = {};
  for (const k of heldKeys.value) markRemoved(k.id);
}

// A .pub file is read here and dropped into the same box the user could have
// pasted into, so both paths go through stageKey. The brain re-serialises and
// fingerprints the key, so nothing about a key is parsed in the browser beyond
// the checks in stageKey.
async function pickFile(event: Event) {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  input.value = "";
  if (!file) return;
  addKeyError.value = "";
  keyText.value = (await file.text()).trim();
  if (!keyLabel.value) keyLabel.value = file.name.replace(/\.pub$/, "");
}

// stageKey moves the add form's key into the draft. It returns false when the key
// is refused, so Save can stop there. Two checks happen here and not only in the
// brain: a private key must never reach the draft, because the draft is written
// to sessionStorage; and a key the list already shows is caught before it turns
// into a refused Save.
function stageKey(): boolean {
  const text = keyText.value.trim();
  if (!text) return true;
  if (looksLikePrivateKey(text)) {
    addKeyError.value =
      "That is a private key. Keep it secret and paste the matching .pub file instead.";
    return false;
  }
  const blob = keyBlob(text);
  if (blob) {
    const held = heldKeys.value.find((k) => keyBlob(k.public_key) === blob);
    if (held && removedIds.value.has(held.id)) {
      // Pasting back a key marked for removal just keeps it.
      undoRemove(held.id);
      cancelAddKey();
      return true;
    }
    if (held || draft.value.added.some((a) => keyBlob(a.publicKey) === blob)) {
      addKeyError.value = "That key is already on your account.";
      return false;
    }
  }
  draft.value.added.push({
    tempId: newTempId(),
    publicKey: text,
    label: keyLabel.value.trim(),
  });
  cancelAddKey();
  return true;
}

// Not crypto.randomUUID: it only exists in a secure context, and the appliance
// dashboard is plain http on .local. The id only has to be unique within one
// draft, including one restored from storage, so the time plus a counter is
// enough.
let tempSeq = 0;
function newTempId(): string {
  tempSeq += 1;
  return `new-${Date.now()}-${tempSeq}`;
}

function cancelAddKey() {
  showAddKey.value = false;
  keyText.value = "";
  keyLabel.value = "";
  addKeyError.value = "";
}

// ── keeping the draft ──────────────────────────────────────────────────────────
// Written to sessionStorage on every change while dirty, for every profile. The
// hosted owner's portal confirm is the case that needs it most, but one path for
// everyone is the path that keeps working, and it covers an accidental reload.
const restored = ref(false);
const restoredMoved = ref(false);
// No write before the restore has run: the empty draft on first load would
// otherwise clear the stored one before it could be read.
const restoreChecked = ref(false);

watch(
  box,
  (b) => {
    if (!b || restoreChecked.value) return;
    restoreChecked.value = true;
    const userId = currentUser.value?.id;
    if (!userId) return;
    const r = restoreDraft(userId, b);
    if (r) {
      draft.value = r.draft;
      restored.value = true;
      restoredMoved.value = r.moved;
    }
  },
  { immediate: true },
);

// When the box's state moves under an open screen (a refetch on focus, a change
// from another tab), drop the parts of the draft it made moot. Otherwise a removal
// of a key that is already gone, or a switch now matching the box, would keep
// the draft dirty with nothing on screen to show for it.
watch(box, (b) => {
  if (!b) return;
  const d = draft.value;
  const ids = new Set((b.keys ?? []).map((k) => k.id));
  if (d.removed.some((id) => !ids.has(id))) d.removed = d.removed.filter((id) => ids.has(id));
  if (d.enabled === b.enabled) d.enabled = null;
  if (d.requirePassword === b.require_password) d.requirePassword = null;
});

function persist() {
  const userId = currentUser.value?.id;
  if (!restoreChecked.value || !userId || !box.value) return;
  storeDraft(userId, draft.value, box.value);
}

watch([draft, box], persist, { deep: true });

function resetDraft() {
  draft.value = emptyDraft();
  cancelAddKey();
  clearErrors();
  restored.value = false;
  restoredMoved.value = false;
  const userId = currentUser.value?.id;
  if (userId) clearStoredDraft(userId);
}

// Cancel and the restore line's Discard do the same thing: back to what the box
// holds, and the stored copy goes too.
function discard() {
  resetDraft();
}

// ── saving ─────────────────────────────────────────────────────────────────────
const save = useMutation({
  mutationFn: (body: ReturnType<typeof buildSaveBody>["body"]) =>
    withElevation(() => api.put<SSHAccess>("/me/ssh", body)),
  onSuccess: (next) => {
    qc.setQueryData(["ssh"], next);
    resetDraft();
  },
});

// keptCount of the body being saved, so a refusal naming keys[N] can be mapped
// back to the pending key at fault.
let savingKeptCount = 0;

function submit() {
  clearErrors();
  if (!box.value || !stageKey() || invalidReason.value) return;
  persist();
  const { body, keptCount } = buildSaveBody(draft.value, box.value, ownerHasNoPassword.value);
  savingKeptCount = keptCount;
  save.mutate(body, {
    onError: (e) => {
      const message = errorMessage(e);
      if (!message) return;
      const at = e instanceof ApiError ? e.location?.match(/^body\.keys\[(\d+)\]/) : null;
      const pending = at ? draft.value.added[Number(at[1]) - savingKeptCount] : undefined;
      if (pending) {
        pendingKeyErrors.value = { [pending.tempId]: message };
        return;
      }
      saveError.value = message;
      // A 409 without a key named means the key list moved under the draft
      // (another tab or device). Refetch, so the screen shows what is there now.
      if (e instanceof ApiError && e.status === 409) qc.invalidateQueries({ queryKey: ["ssh"] });
    },
  });
}

// ── leaving with unsaved changes ───────────────────────────────────────────────
// Leaving through the app asks first. Saying yes is an explicit discard, so the
// stored copy goes with it.
onBeforeRouteLeave(() => {
  if (!dirty.value) return true;
  const leave = window.confirm("You have unsaved SSH changes. Leave without saving them?");
  if (leave) resetDraft();
  return leave;
});

// Closing or reloading the tab gets the browser's own warning. Not on the portal
// confirm redirect: that leave is part of saving, and the draft is already stored.
function onBeforeUnload(e: BeforeUnloadEvent) {
  if (!dirty.value || isLeavingForConfirm()) return;
  e.preventDefault();
  e.returnValue = "";
}
onMounted(() => window.addEventListener("beforeunload", onBeforeUnload));
onBeforeUnmount(() => window.removeEventListener("beforeunload", onBeforeUnload));

// ── display helpers ────────────────────────────────────────────────────────────
function addedOn(seconds: number): string {
  return new Date(seconds * 1000).toLocaleDateString();
}

// A pending key has no fingerprint yet (the brain makes it on save), so its row
// shows the key type and the comment, the two parts a person recognises.
function pendingSummary(publicKey: string): string {
  const parts = publicKey.trim().split(/\s+/);
  const typeAt = parts.findIndex((p) => /^(ssh-|ecdsa-|sk-)/.test(p));
  if (typeAt < 0) return publicKey.slice(0, 40);
  const comment = parts.slice(typeAt + 2).join(" ");
  return comment ? `${parts[typeAt]} … ${comment}` : `${parts[typeAt]} …`;
}

const saving = computed(() => save.isPending.value);
</script>

<template>
  <div class="space-y-6">
    <section class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">SSH</h2>
      <p class="text-sm text-muted-foreground">
        SSH gives you a command line on this box from another computer. It is off until you turn it
        on, and it only ever covers your own account.
      </p>

      <p v-if="restored" class="text-sm">
        Unsaved changes restored.
        <template v-if="restoredMoved">
          Your SSH settings also changed since then, so check them before you save.
        </template>
        <button type="button" class="text-accent underline-offset-2 hover:underline" @click="discard">
          Discard
        </button>
      </p>

      <p v-if="ssh.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
      <p v-else-if="ssh.isError.value" class="text-sm text-destructive">
        Could not read your SSH settings. {{ (ssh.error.value as Error)?.message }}
      </p>

      <div
        v-else
        class="flex items-center justify-between gap-4 rounded-xl border border-border bg-card px-4 py-3"
      >
        <div class="min-w-0">
          <div class="text-sm font-medium">Enable SSH for my account</div>
          <div class="text-xs text-muted-foreground">
            <template v-if="needsKeyFirst && !enabled">
              Add a public key below first. This box needs a key, and a password on its own is not
              enough.
            </template>
            <template v-else-if="enabled && box?.enabled">
              You can sign in from another computer as
              <span class="font-mono">{{ currentUser?.username }}</span
              >.
            </template>
            <template v-else-if="enabled">
              Once you save, you can sign in from another computer as
              <span class="font-mono">{{ currentUser?.username }}</span
              >.
            </template>
            <template v-else-if="box?.enabled">SSH turns off for your account when you save.</template>
            <template v-else>Nobody can reach your account over SSH while this is off.</template>
          </div>
        </div>
        <SwitchRoot
          :model-value="enabled"
          :disabled="saving || (needsKeyFirst && !enabled)"
          aria-label="Enable SSH for my account"
          class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors disabled:cursor-default disabled:opacity-60 data-[state=checked]:border-accent data-[state=checked]:bg-accent"
          @update:model-value="setEnabled"
        >
          <SwitchThumb
            class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
          />
        </SwitchRoot>
      </div>
    </section>

    <!-- The two methods, styled alike. The one this box makes mandatory is on and
         cannot be switched off; the other is the account's own choice. -->
    <section
      v-if="!ssh.isLoading.value && !ssh.isError.value && showMethods"
      class="space-y-3"
    >
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">
        Authentication methods
      </h2>
      <p class="text-sm text-muted-foreground">
        What you have to prove to get in. With both on you need both, so the second one is an extra
        lock and never another way in.
      </p>

      <!-- Password. Never called anything but "Password" here: there is one
           password for everything (AUTH.md # Device access), not an SSH one. -->
      <div class="space-y-3 rounded-xl border border-border bg-card px-4 py-3">
        <div class="flex items-center justify-between gap-4">
          <div class="flex min-w-0 items-center gap-3">
            <Lock class="size-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0">
              <div class="text-sm font-medium">Password</div>
              <div class="text-xs text-muted-foreground">
                <template v-if="!keyRequired">
                  Required by this box. It is the same password you sign in to the dashboard with.
                </template>
                <template v-else-if="ownerHasNoPassword">
                  This box has no password for your account. You sign in through your moose account
                  instead, so there is nothing here to type. Your key is what gets you in.
                </template>
                <template v-else>
                  The same password you sign in to this box with. With this on, SSH asks for your key
                  and then your password. Two locks, not two ways in.
                </template>
              </div>
            </div>
          </div>
          <SwitchRoot
            :model-value="passwordOn"
            :disabled="saving || !keyRequired || ownerHasNoPassword"
            aria-label="Ask for my password"
            class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors disabled:cursor-default disabled:opacity-60 data-[state=checked]:border-accent data-[state=checked]:bg-accent"
            @update:model-value="setPassword"
          >
            <SwitchThumb
              class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
            />
          </SwitchRoot>
        </div>
      </div>

      <!-- Public key, same card, with the key list and the add form inside it. -->
      <div class="space-y-3 rounded-xl border border-border bg-card px-4 py-3">
        <div class="flex items-center justify-between gap-4">
          <div class="flex min-w-0 items-center gap-3">
            <KeyRound class="size-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0">
              <div class="text-sm font-medium">Public key</div>
              <div class="text-xs text-muted-foreground">
                <template v-if="needsKeyFirst">
                  Required by this box. Add your first key to turn SSH on.
                </template>
                <template v-else-if="keyRequired">
                  Required by this box. A key is a file your computer holds.
                </template>
                <template v-else>
                  A key is a file your computer holds, and an extra lock on top of your password.
                </template>
              </div>
            </div>
          </div>
          <SwitchRoot
            :model-value="keysInUse"
            :disabled="saving || keyRequired"
            aria-label="Ask for a public key"
            class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors disabled:cursor-default disabled:opacity-60 data-[state=checked]:border-accent data-[state=checked]:bg-accent"
            @update:model-value="toggleKeys"
          >
            <SwitchThumb
              class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
            />
          </SwitchRoot>
        </div>

        <div v-if="keysInUse || keysChanged" class="space-y-3 border-t border-border pt-3">
          <p class="text-xs text-muted-foreground">
            Add the public half of your key, the file ending in
            <span class="font-mono">.pub</span>. Never share the other one. Several keys is normal,
            one for each computer you use.
          </p>

          <ul v-if="heldKeys.length || draft.added.length" class="space-y-2">
            <!-- A key marked for removal stays in the list, greyed, until Save. A
                 row that vanished would read as already done. -->
            <li
              v-for="k in heldKeys"
              :key="k.id"
              class="flex items-center justify-between gap-4 rounded-lg border border-border px-3 py-2"
              :class="removedIds.has(k.id) ? 'border-dashed' : ''"
            >
              <div class="min-w-0" :class="removedIds.has(k.id) ? 'opacity-50' : ''">
                <div class="truncate text-sm font-medium">{{ k.label || "Unnamed key" }}</div>
                <div class="truncate font-mono text-xs text-muted-foreground">
                  {{ k.fingerprint }}
                </div>
                <div class="text-xs text-muted-foreground">
                  <template v-if="removedIds.has(k.id)">Will be removed when you save</template>
                  <template v-else>Added {{ addedOn(k.added_at) }}</template>
                </div>
              </div>
              <Button
                v-if="removedIds.has(k.id)"
                variant="ghost"
                size="sm"
                :disabled="saving"
                :aria-label="`Keep ${k.label || 'key'}`"
                @click="undoRemove(k.id)"
              >
                <Undo2 class="size-4" />
                Undo
              </Button>
              <Button
                v-else
                variant="ghost"
                size="sm"
                :disabled="saving"
                :aria-label="`Remove ${k.label || 'key'}`"
                @click="markRemoved(k.id)"
              >
                <Trash2 class="size-4" />
                Remove
              </Button>
            </li>

            <li
              v-for="a in draft.added"
              :key="a.tempId"
              class="space-y-1 rounded-lg border border-dashed border-accent px-3 py-2"
            >
              <div class="flex items-center justify-between gap-4">
                <div class="min-w-0">
                  <div class="truncate text-sm font-medium">{{ a.label || "New key" }}</div>
                  <div class="truncate font-mono text-xs text-muted-foreground">
                    {{ pendingSummary(a.publicKey) }}
                  </div>
                  <div class="text-xs text-muted-foreground">Will be added when you save</div>
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  :disabled="saving"
                  :aria-label="`Remove ${a.label || 'new key'}`"
                  @click="dropPending(a.tempId)"
                >
                  <Trash2 class="size-4" />
                  Remove
                </Button>
              </div>
              <p v-if="pendingKeyErrors[a.tempId]" class="text-sm text-destructive">
                {{ pendingKeyErrors[a.tempId] }}
              </p>
            </li>
          </ul>
          <p v-else-if="!showAddKey" class="text-sm text-muted-foreground">No keys yet.</p>

          <p v-if="invalidReason" class="text-sm text-destructive">{{ invalidReason }}</p>

          <div v-if="!showAddKey">
            <Button variant="secondary" size="sm" :disabled="saving" @click="showAddKey = true">
              Add a key
            </Button>
          </div>

          <form v-else class="space-y-3 border-t border-border pt-3" @submit.prevent="stageKey">
            <label class="block space-y-1">
              <span class="text-xs text-muted-foreground">Public key</span>
              <textarea
                v-model="keyText"
                rows="3"
                required
                spellcheck="false"
                placeholder="ssh-ed25519 AAAA… you@laptop"
                class="w-full rounded-lg border border-border bg-background px-3 py-1.5 font-mono text-xs outline-none focus:border-accent"
              ></textarea>
            </label>
            <div class="flex flex-wrap items-end gap-3">
              <label class="block min-w-0 flex-1 space-y-1">
                <span class="text-xs text-muted-foreground">Name it (optional)</span>
                <input
                  v-model="keyLabel"
                  type="text"
                  placeholder="Laptop"
                  class="w-full rounded-lg border border-border bg-background px-3 py-1.5 text-sm outline-none focus:border-accent"
                />
              </label>
              <Button type="button" variant="secondary" size="sm" @click="fileInput?.click()">
                <Upload class="size-4" />
                Choose a .pub file
              </Button>
              <input
                ref="fileInput"
                type="file"
                accept=".pub,text/plain"
                class="hidden"
                @change="pickFile"
              />
            </div>
            <p v-if="addKeyError" class="text-sm text-destructive">{{ addKeyError }}</p>
            <div class="flex justify-end gap-2">
              <Button type="button" variant="ghost" size="sm" @click="cancelAddKey">Cancel</Button>
              <Button type="submit" size="sm" :disabled="!keyText.trim()">Add key</Button>
            </div>
          </form>
        </div>
      </div>
    </section>

    <!-- One Save for the whole screen: one request, one password prompt. Shown
         only while there is something to save or cancel, and Save is left out
         while the draft is invalid (the reason is on the key card). Both stay up
         while a save is in flight, so the "Saving…" state has somewhere to show. -->
    <div
      v-if="!ssh.isLoading.value && !ssh.isError.value && (dirty || saving || saveError)"
      class="space-y-2 border-t border-border pt-4"
    >
      <p v-if="saveError" class="text-sm text-destructive">{{ saveError }}</p>
      <div v-if="dirty || saving" class="flex justify-end gap-2">
        <Button variant="ghost" :disabled="saving" @click="discard">Cancel</Button>
        <Button v-if="!invalidReason" :disabled="saving" @click="submit">
          {{ saving ? "Saving…" : "Save" }}
        </Button>
      </div>
    </div>
  </div>
</template>
