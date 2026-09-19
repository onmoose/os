<script setup lang="ts">
// Settings → SSH: the signed-in user's own shell access (AUTH.md # Device
// access, issue #482). Every user manages their own, and no admin manages it for
// them, which is why the routes sit under /me and this screen has no role gate.
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
// The profile rule is NOT decided here. The brain answers it in key_required:
// true on hosted, where a public key is the mandatory method, and false on the
// appliance, where the moose password is. The server enforces both rows whatever
// this screen does, so reading the flag keeps the two from drifting.
//
// Every write is elevation-class server-side, so all three go through
// withElevation: the first one re-prompts for the password (or, for a hosted
// owner, takes a portal round-trip) and retries.
import { computed, ref } from "vue";
import { useQuery, useMutation, useQueryClient } from "@tanstack/vue-query";
import { SwitchRoot, SwitchThumb } from "reka-ui";
import { KeyRound, Lock, Trash2, Upload } from "lucide-vue-next";
import { api, ApiError, type SSHAccess } from "@/api";
import { withElevation } from "@/elevate";
import { isHosted, isBoxOwner, useAuth } from "@/auth";
import Button from "@/components/ui/Button.vue";

const qc = useQueryClient();
const { currentUser } = useAuth();

const ssh = useQuery({
  queryKey: ["ssh"],
  queryFn: () => api.get<SSHAccess>("/me/ssh"),
});

const access = computed(() => ssh.data.value);
// keys is nullable on the wire: an account with none serialises as null.
const keys = computed(() => access.value?.keys ?? []);
const keyRequired = computed(() => access.value?.key_required ?? false);
const enabled = computed(() => access.value?.enabled ?? false);
const requirePassword = computed(() => access.value?.require_password ?? false);

// Hosted refuses an enable with no key, so the top switch stays off until there
// is one. The appliance always has the password behind it, so it never blocks.
const needsKeyFirst = computed(() => keyRequired.value && keys.value.length === 0);

// Hidden while SSH is off, because a lock on a door nobody can open is noise.
// The one exception is the hosted box with no key yet: the key card is the only
// way to reach the first key, and without a key the switch above cannot move.
const showMethods = computed(() => enabled.value || needsKeyFirst.value);

// The hosted box owner cannot use the password method. Their box password was
// generated during the portal handshake and thrown away (internal/api/sso.go),
// so nobody knows it and sshd would demand a string they cannot type. The brain
// does not guard this, and should not: it cannot tell an owner asking for a
// second lock from anybody else asking for one. This screen is the only place
// the case is handled, which is why it is written down here.
const ownerHasNoPassword = computed(() => isHosted() && isBoxOwner());

// A key is "in use" when the account holds one. There is no separate flag for it
// on the wire, and there should not be: a key set of zero IS the method being
// off. On hosted the box requires the method whatever the key list says, so the
// switch reads on and cannot be moved, and the card asks for the first key.
//
// The open add form counts. Without it the first click on the switch opens the
// form while the switch itself stays off, because adding the key is what turns
// the method on and that has not happened yet.
const showAddKey = ref(false);
const keysInUse = computed(
  () => keyRequired.value || keys.value.length > 0 || showAddKey.value,
);

const actionError = ref("");
// Two error refs, not one. The Remove button on a key stays live while the add
// form is open, so a shared ref would print a refused removal inside the form as
// if the paste had failed, and cancelling the form would then throw the message
// away unread.
const addKeyError = ref("");
const keyError = ref("");

function errorMessage(e: unknown): string {
  // A dismissed confirm prompt is a deliberate no-op, not a failure.
  if (e instanceof ApiError && e.code === "elevation_cancelled") return "";
  return e instanceof ApiError ? e.message : "Something went wrong.";
}

// ── the on/off switch and the password method ──────────────────────────────────
// Both write the same endpoint, which takes the whole desired state, so each one
// sends the other field's current value rather than a default.
const setAccess = useMutation({
  mutationFn: (body: { enabled: boolean; require_password: boolean }) =>
    withElevation(() => api.put<SSHAccess>("/me/ssh", body)),
  onSuccess: (next) => {
    actionError.value = "";
    // Turning SSH off hides the methods, so close anything open behind them
    // rather than letting a half-typed key reappear when SSH comes back on.
    if (!next.enabled) {
      showAddKey.value = false;
      confirmDropKeys.value = false;
      keyText.value = "";
      keyLabel.value = "";
    }
    qc.setQueryData(["ssh"], next);
  },
  onError: (e) => {
    actionError.value = errorMessage(e);
    // The switch is showing what the user clicked, which the box refused. Refetch
    // so it snaps back to the real state.
    qc.invalidateQueries({ queryKey: ["ssh"] });
  },
});

function toggleEnabled(on: boolean) {
  setAccess.mutate({ enabled: on, require_password: requirePassword.value });
}

function togglePassword(on: boolean) {
  setAccess.mutate({ enabled: enabled.value, require_password: on });
}

// ── the key method ─────────────────────────────────────────────────────────────
const confirmDropKeys = ref(false);
const keyText = ref("");
const keyLabel = ref("");
const fileInput = ref<HTMLInputElement | null>(null);

// Turning the key method on opens the add form, because a method with no key is
// not on yet. Turning it off means having no keys, so it asks first: there is no
// "keys kept but ignored" state to fall back to, and this throws away something
// the user pasted from another machine.
function toggleKeys(on: boolean) {
  keyError.value = "";
  addKeyError.value = "";
  if (on) {
    confirmDropKeys.value = false;
    showAddKey.value = true;
    return;
  }
  showAddKey.value = false;
  confirmDropKeys.value = keys.value.length > 0;
}

// A .pub file is read here and dropped into the same box the user could have
// pasted into, so both paths send one request shape and the server validates
// once. The server re-serialises and fingerprints the key, so nothing about a key
// is parsed in the browser.
async function pickFile(event: Event) {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  input.value = "";
  if (!file) return;
  addKeyError.value = "";
  keyText.value = (await file.text()).trim();
  if (!keyLabel.value) keyLabel.value = file.name.replace(/\.pub$/, "");
}

const addKey = useMutation({
  mutationFn: () =>
    withElevation(() =>
      api.post<SSHAccess>("/me/ssh/keys", {
        public_key: keyText.value,
        label: keyLabel.value.trim(),
      }),
    ),
  onSuccess: (next) => {
    addKeyError.value = "";
    keyText.value = "";
    keyLabel.value = "";
    showAddKey.value = false;
    qc.setQueryData(["ssh"], next);
  },
  onError: (e) => {
    addKeyError.value = errorMessage(e);
  },
});

const removeKey = useMutation({
  mutationFn: (id: string) => withElevation(() => api.del<void>(`/me/ssh/keys/${id}`)),
  onSuccess: () => {
    keyError.value = "";
    qc.invalidateQueries({ queryKey: ["ssh"] });
  },
  onError: (e) => {
    keyError.value = errorMessage(e);
  },
});

// Dropping the method means deleting every key, one call each, because the API
// deletes by id. Stop at the first refusal and show it rather than carrying on:
// a partial removal the user cannot see would be worse than none.
const dropAllKeys = useMutation({
  mutationFn: async () => {
    for (const k of keys.value) {
      await withElevation(() => api.del<void>(`/me/ssh/keys/${k.id}`));
    }
  },
  onSuccess: () => {
    keyError.value = "";
    confirmDropKeys.value = false;
    qc.invalidateQueries({ queryKey: ["ssh"] });
  },
  onError: (e) => {
    keyError.value = errorMessage(e);
    qc.invalidateQueries({ queryKey: ["ssh"] });
  },
});

function cancelAddKey() {
  showAddKey.value = false;
  keyText.value = "";
  keyLabel.value = "";
  addKeyError.value = "";
}

function addedOn(seconds: number): string {
  return new Date(seconds * 1000).toLocaleDateString();
}

const busy = computed(
  () =>
    setAccess.isPending.value ||
    addKey.isPending.value ||
    removeKey.isPending.value ||
    dropAllKeys.isPending.value,
);
</script>

<template>
  <div class="space-y-6">
    <section class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">SSH</h2>
      <p class="text-sm text-muted-foreground">
        SSH gives you a command line on this box from another computer. It is off until you turn it
        on, and it only ever covers your own account.
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
          <div class="text-sm font-medium">Allow SSH to my account</div>
          <div class="text-xs text-muted-foreground">
            <template v-if="needsKeyFirst">
              Add a public key below first. This box needs a key, and a password on its own is not
              enough.
            </template>
            <template v-else-if="enabled">
              You can sign in from another computer as
              <span class="font-mono">{{ currentUser?.username }}</span
              >.
            </template>
            <template v-else>Nobody can reach this box over SSH while this is off.</template>
          </div>
        </div>
        <SwitchRoot
          :model-value="enabled"
          :disabled="busy || (needsKeyFirst && !enabled)"
          aria-label="Allow SSH to my account"
          class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors disabled:cursor-default disabled:opacity-60 data-[state=checked]:border-accent data-[state=checked]:bg-accent"
          @update:model-value="toggleEnabled"
        >
          <SwitchThumb
            class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
          />
        </SwitchRoot>
      </div>

      <p v-if="actionError" class="text-sm text-destructive">{{ actionError }}</p>
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

      <!-- Password. -->
      <div class="space-y-3 rounded-xl border border-border bg-card px-4 py-3">
        <div class="flex items-center justify-between gap-4">
          <div class="flex min-w-0 items-center gap-3">
            <Lock class="size-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0">
              <div class="text-sm font-medium">moose password</div>
              <div class="text-xs text-muted-foreground">
                <template v-if="!keyRequired">
                  Required by this box. It is the same password you sign in to the dashboard with.
                </template>
                <template v-else-if="ownerHasNoPassword">
                  You sign in through the portal, so this box has no password for you to type.
                </template>
                <template v-else>
                  The same password you sign in to the dashboard with.
                </template>
              </div>
            </div>
          </div>
          <SwitchRoot
            :model-value="requirePassword"
            :disabled="busy || !keyRequired || ownerHasNoPassword"
            aria-label="Ask for my moose password"
            class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors disabled:cursor-default disabled:opacity-60 data-[state=checked]:border-accent data-[state=checked]:bg-accent"
            @update:model-value="togglePassword"
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
                  Required by this box. Add your first key to finish turning SSH on.
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
            :disabled="busy || keyRequired"
            aria-label="Ask for a public key"
            class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors disabled:cursor-default disabled:opacity-60 data-[state=checked]:border-accent data-[state=checked]:bg-accent"
            @update:model-value="toggleKeys"
          >
            <SwitchThumb
              class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
            />
          </SwitchRoot>
        </div>

        <div v-if="keysInUse || confirmDropKeys" class="space-y-3 border-t border-border pt-3">
          <p class="text-xs text-muted-foreground">
            Add the public half of your key, the file ending in
            <span class="font-mono">.pub</span>. Never share the other one. Several keys is normal,
            one for each computer you use.
          </p>

          <ul v-if="keys.length" class="space-y-2">
            <li
              v-for="k in keys"
              :key="k.id"
              class="flex items-center justify-between gap-4 rounded-lg border border-border px-3 py-2"
            >
              <div class="min-w-0">
                <div class="truncate text-sm font-medium">{{ k.label || "Unnamed key" }}</div>
                <div class="truncate font-mono text-xs text-muted-foreground">
                  {{ k.fingerprint }}
                </div>
                <div class="text-xs text-muted-foreground">Added {{ addedOn(k.added_at) }}</div>
              </div>
              <Button
                variant="ghost"
                size="sm"
                :disabled="busy"
                :aria-label="`Remove ${k.label || 'key'}`"
                @click="removeKey.mutate(k.id)"
              >
                <Trash2 class="size-4" />
                Remove
              </Button>
            </li>
          </ul>
          <p v-else-if="!showAddKey" class="text-sm text-muted-foreground">No keys yet.</p>

          <!-- Turning the method off throws keys away, so it asks first. -->
          <div v-if="confirmDropKeys" class="space-y-2 rounded-lg border border-border px-3 py-2">
            <p class="text-sm">
              Turning this off removes
              {{ keys.length === 1 ? "your key" : `all ${keys.length} of your keys` }}. You would
              have to paste them again from each computer.
            </p>
            <div class="flex justify-end gap-2">
              <Button type="button" variant="ghost" size="sm" @click="confirmDropKeys = false">
                Cancel
              </Button>
              <Button type="button" size="sm" :disabled="busy" @click="dropAllKeys.mutate()">
                {{ dropAllKeys.isPending.value ? "Removing…" : "Remove and turn off" }}
              </Button>
            </div>
          </div>

          <div v-else-if="!showAddKey">
            <Button variant="secondary" size="sm" @click="showAddKey = true">Add a key</Button>
          </div>

          <form v-else class="space-y-3 border-t border-border pt-3" @submit.prevent="addKey.mutate()">
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
              <Button type="submit" size="sm" :disabled="busy || !keyText.trim()">
                {{ addKey.isPending.value ? "Adding…" : "Add key" }}
              </Button>
            </div>
          </form>

          <p v-if="keyError" class="text-sm text-destructive">{{ keyError }}</p>
        </div>
      </div>
    </section>
  </div>
</template>
