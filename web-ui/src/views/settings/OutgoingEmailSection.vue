<script setup lang="ts">
// Settings → Outgoing email: the signed-in user's own SMTP accounts
// (SERVICE_PROVISIONING.md # BYO outgoing mail, issues #122 and #426). Every
// user has this screen, and it lists only the accounts they added. Only they
// can bind an app to one, at install time or later from the app's settings
// screen; the brain injects the credentials as MOOSE_MAIL_* env vars.
//
// This view is the account list. Adding one lives on its own two routes
// (OutgoingEmailAddSection, /settings/mail/add) so the picker and the form are
// real pages with working Back. Editing stays inline on the row it belongs to:
// no navigation happens, so there is no Back to get wrong.
//
// Edit and test-send need no password re-prompt: the account is the user's
// own. Delete keeps it, since it cannot be undone and unbinds every app that
// uses the account, so it goes through withElevation. Rejections surface as
// inline errors on the row.
import { ref, computed } from "vue";
import { RouterLink } from "vue-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/vue-query";
import { Plus } from "lucide-vue-next";
import { api, type MailProvider } from "@/api";
import { withElevation } from "@/elevate";
import Button from "@/components/ui/Button.vue";
import MailProviderLogo from "@/components/MailProviderLogo.vue";
import paperAirplane from "@/assets/paper-airplane.png";
import {
  useMailPresets, emptyForm, syncSameAsPassword, formValid, portWarning,
  bodyOf, errorMessage, fieldClass,
  type ProviderForm,
} from "@/mailProviderForm";

const qc = useQueryClient();

// ── provider list ───────────────────────────────────────────────────────────────
const providers = useQuery({
  queryKey: ["mail-providers"],
  queryFn: () => api.get<{ providers: MailProvider[] }>("/mail-providers"),
});

// The empty state replaces the list entirely, so both the header and the list
// branch on it.
const providerList = computed(() => providers.data.value?.providers ?? []);
const isEmpty = computed(() => !providers.isLoading.value && providerList.value.length === 0);

// Presets back the inline edit form: they decide the credential label and which
// username field is shown for a typed provider.
const presets = useMailPresets();
const presetList = computed(() => presets.data.value?.presets ?? []);

// ── per-row state ────────────────────────────────────────────────────────────────
// Only one expanded panel (edit / test / delete-confirm) per row at a time.
const editFor = ref<string | null>(null);
const editForm = ref<ProviderForm>(emptyForm());
const editAdvancedOpen = ref(false);
const testFor = ref<string | null>(null);
const testTo = ref("");
const testSent = ref<Record<string, string>>({}); // id → "sent to <addr>" confirmation
const confirmDeleteFor = ref<string | null>(null);
const rowError = ref<Record<string, string>>({});

const editPreset = computed(() => presetList.value.find((p) => p.id === editForm.value.provider_type));

// The preset list can be missing: it is a second request, so it may still be in
// flight when a row is opened, and it can fail on its own. A hand-typed row is
// safe to edit without it, because nothing pairs its fields. A preset row is not: with
// no preset we cannot tell whether its username is tied to the credential
// (Postmark), and a blank password on save means "keep the stored one". So the
// username stays read-only until the list arrives, rather than falling back to
// the hand-typed form and letting the pair drift apart.
const editPresetUnknown = computed(() => !editPreset.value && editForm.value.provider_type !== "custom");

function clearRowError(id: string) {
  const next = { ...rowError.value };
  delete next[id];
  rowError.value = next;
}

function setRowError(id: string, e: unknown) {
  rowError.value = { ...rowError.value, [id]: errorMessage(e) };
}

function startEdit(p: MailProvider) {
  testFor.value = null;
  confirmDeleteFor.value = null;
  editFor.value = editFor.value === p.id ? null : p.id;
  // Password stays blank. An empty password on save keeps the stored one.
  editForm.value = {
    label: p.label, host: p.host, port: p.port, username: p.username,
    password: "", from_address: p.from_address,
    encryption: p.encryption as ProviderForm["encryption"],
    provider_type: p.provider_type,
    // Only the host the region resolved to is stored, not the region, so edit shows
    // the host in the advanced fields rather than guessing the region back.
    region: "",
  };
  // A hand-typed provider (including every row that predates presets) opens on
  // the full form; a preset row opens on the short one.
  editAdvancedOpen.value = p.provider_type === "custom";
}

function startTest(id: string) {
  editFor.value = null;
  confirmDeleteFor.value = null;
  testFor.value = testFor.value === id ? null : id;
  testTo.value = "";
}

// ── update ───────────────────────────────────────────────────────────────────────
const update = useMutation({
  mutationFn: (id: string) => {
    syncSameAsPassword(editForm.value, editPreset.value);
    return api.put<MailProvider>(`/mail-providers/${id}`, bodyOf(editForm.value));
  },
  onSuccess: (_, id) => {
    clearRowError(id);
    editFor.value = null;
    qc.invalidateQueries({ queryKey: ["mail-providers"] });
  },
  onError: (e, id) => setRowError(id, e),
});

// ── delete ───────────────────────────────────────────────────────────────────────
const deleteProvider = useMutation({
  mutationFn: (id: string) => withElevation(() => api.del<void>(`/mail-providers/${id}`)),
  onSuccess: (_, id) => {
    clearRowError(id);
    confirmDeleteFor.value = null;
    qc.invalidateQueries({ queryKey: ["mail-providers"] });
  },
  onError: (e, id) => setRowError(id, e),
});

// ── test send ────────────────────────────────────────────────────────────────────
// Synchronous on the brain side (it dials the SMTP host), so this can take a
// few seconds; the button shows "Sending…" meanwhile.
const sendTest = useMutation({
  mutationFn: ({ id, to }: { id: string; to: string }) =>
    api.post<void>(`/mail-providers/${id}/test`, { to }),
  onSuccess: (_, { id, to }) => {
    clearRowError(id);
    testSent.value = { ...testSent.value, [id]: to };
    testFor.value = null;
    testTo.value = "";
  },
  onError: (e, { id }) => setRowError(id, e),
});

// Every input is labelled, so each needs an id unique across whichever rows are
// open at once.
function fid(form: "new" | "edit", name: string, rowID = ""): string {
  return `mail-${form}-${name}${rowID ? `-${rowID}` : ""}`;
}
</script>

<template>
  <div class="space-y-6">
    <!-- Header: the account list is the section; adding lives on its own route. -->
    <section class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Outgoing email</h2>
      <p class="text-sm text-muted-foreground">
        Your email accounts. The apps you install can send from them: password resets, reminders, invites. Only you can see and use the accounts you add here.
      </p>
      <!-- With no accounts the call to action is the empty state below, so the
           header does not repeat it. -->
      <Button v-if="!isEmpty" :as="RouterLink" to="/settings/mail/add">
        <Plus class="size-4" /> Add account
      </Button>
    </section>

    <!-- Empty state: the whole main area, with the airplane sitting in the
         bottom-right corner behind it. A positioned <img> rather than a CSS
         background so its opacity can drop without fading the text on top. -->
    <section v-if="isEmpty" class="relative flex min-h-[24rem] items-center justify-center overflow-hidden rounded-2xl border border-border bg-card px-6 py-12">
      <img
        :src="paperAirplane"
        alt=""
        aria-hidden="true"
        class="pointer-events-none absolute bottom-0 right-0 w-[30%] opacity-30"
      />
      <div class="relative text-center">
        <h3 class="text-sm font-semibold text-foreground">No email accounts</h3>
        <p class="mx-auto mt-1 max-w-sm text-sm text-muted-foreground">
          Add the account your apps will send from, and they can start sending password resets, reminders and invites.
        </p>
        <div class="mt-6">
          <Button :as="RouterLink" to="/settings/mail/add">
            <Plus class="size-4" /> Add account
          </Button>
        </div>
      </div>
    </section>

    <!-- Provider list -->
    <section v-else class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Your accounts</h2>
      <p v-if="providers.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
      <ul v-else class="space-y-2">
        <li
          v-for="p in providerList"
          :key="p.id"
          class="space-y-3 rounded-2xl border border-border bg-card p-5 sm:p-6"
        >
          <!-- Main row -->
          <div class="flex flex-wrap items-center gap-3">
            <MailProviderLogo :id="p.provider_type" :label="p.provider_label" size="inline" />
            <div class="min-w-0 flex-1">
              <div class="text-sm font-medium">{{ p.label }}</div>
              <!-- The provider name reads better than the raw host; a hand-typed
                   account has no name to show, so it falls back to the host. -->
              <div class="text-xs text-muted-foreground">
                {{ p.from_address }} via
                {{ p.provider_type === "custom" ? `${p.host}:${p.port}` : p.provider_label }}
              </div>
            </div>
            <Button variant="secondary" size="sm" :disabled="sendTest.isPending.value" @click="startTest(p.id)">
              Send test
            </Button>
            <Button variant="secondary" size="sm" @click="startEdit(p)">Edit</Button>
            <Button
              variant="ghost"
              size="sm"
              class="text-destructive hover:bg-destructive/10"
              :disabled="deleteProvider.isPending.value"
              @click="editFor = null; testFor = null; confirmDeleteFor = confirmDeleteFor === p.id ? null : p.id"
            >
              Delete
            </Button>
          </div>

          <!-- Delete confirmation -->
          <div
            v-if="confirmDeleteFor === p.id"
            class="flex flex-wrap items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/5 px-3 py-2"
          >
            <span class="text-sm">Delete <strong>{{ p.label }}</strong>? Apps using it will stop sending email until you pick another account for them.</span>
            <Button
              variant="secondary"
              size="sm"
              class="border-destructive text-destructive hover:bg-destructive/10"
              :disabled="deleteProvider.isPending.value"
              @click="deleteProvider.mutate(p.id)"
            >
              Delete
            </Button>
            <Button variant="ghost" size="sm" @click="confirmDeleteFor = null">Cancel</Button>
          </div>

          <!-- Inline test-send form -->
          <div v-if="testFor === p.id" class="flex flex-wrap gap-2">
            <div class="min-w-48 flex-1 space-y-1.5">
              <label class="text-sm font-medium" :for="fid('edit', 'testto', p.id)">Send a test email to</label>
              <input
                :id="fid('edit', 'testto', p.id)"
                v-model="testTo"
                type="email"
                placeholder="you@example.com"
                :class="fieldClass"
                autocomplete="off"
                @keydown.enter="!sendTest.isPending.value && testTo.includes('@') && sendTest.mutate({ id: p.id, to: testTo })"
              />
            </div>
            <Button
              class="mt-6"
              :disabled="sendTest.isPending.value || !testTo.includes('@')"
              @click="sendTest.mutate({ id: p.id, to: testTo })"
            >
              {{ sendTest.isPending.value ? "Sending…" : "Send" }}
            </Button>
            <Button variant="ghost" class="mt-6" @click="testFor = null">Cancel</Button>
          </div>

          <!-- Inline edit form: the same short form the account was created on,
               with the advanced fields open for a hand-typed account. -->
          <div v-if="editFor === p.id" class="space-y-3">
            <p v-if="editPreset && editPreset.id !== 'custom'" class="text-xs text-muted-foreground">
              {{ editPreset.help }}
              <a
                v-if="editPreset.docs_url"
                :href="editPreset.docs_url"
                target="_blank"
                rel="noopener noreferrer"
                class="underline"
              >Provider docs</a>
            </p>
            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('edit', 'label', p.id)">Account name</label>
              <input
                :id="fid('edit', 'label', p.id)"
                v-model="editForm.label"
                :class="fieldClass"
                autocomplete="off"
              />
              <p class="text-xs text-muted-foreground">
                What you'll see when an app asks which account to send from. Only you see this.
              </p>
            </div>

            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('edit', 'from', p.id)">From address</label>
              <input
                :id="fid('edit', 'from', p.id)"
                v-model="editForm.from_address"
                type="email"
                :class="fieldClass"
                autocomplete="off"
              />
            </div>

            <div
              v-if="!editPresetUnknown && (!editPreset || editPreset.username_mode === 'user')"
              class="space-y-1.5"
            >
              <label class="text-sm font-medium" :for="fid('edit', 'username', p.id)">Username</label>
              <input
                :id="fid('edit', 'username', p.id)"
                v-model="editForm.username"
                :class="fieldClass"
                autocomplete="off"
              />
            </div>

            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('edit', 'password', p.id)">
                {{ editPreset?.credential_label ?? "Password" }}
              </label>
              <input
                :id="fid('edit', 'password', p.id)"
                v-model="editForm.password"
                type="password"
                :class="fieldClass"
                autocomplete="new-password"
              />
              <p class="text-xs text-muted-foreground">Leave blank to keep the one you saved.</p>
            </div>

            <details class="rounded-xl border border-border px-3 py-2" :open="editAdvancedOpen">
              <summary class="cursor-pointer text-sm font-medium text-muted-foreground">Server settings</summary>
              <div class="mt-3 space-y-4">
                <div class="space-y-1.5">
                  <label class="text-sm font-medium" :for="fid('edit', 'host', p.id)">SMTP server</label>
                  <input
                    :id="fid('edit', 'host', p.id)"
                    v-model="editForm.host"
                    :class="fieldClass"
                    autocomplete="off"
                  />
                </div>
                <div class="space-y-1.5">
                  <label class="text-sm font-medium" :for="fid('edit', 'port', p.id)">Port</label>
                  <input
                    :id="fid('edit', 'port', p.id)"
                    v-model.number="editForm.port"
                    type="number"
                    min="1"
                    max="65535"
                    :class="fieldClass"
                  />
                </div>
                <div class="space-y-1.5">
                  <label class="text-sm font-medium" :for="fid('edit', 'encryption', p.id)">Encryption</label>
                  <select :id="fid('edit', 'encryption', p.id)" v-model="editForm.encryption" :class="fieldClass">
                    <option value="starttls">STARTTLS (usually port 587)</option>
                    <option value="tls">TLS (usually port 465)</option>
                    <option value="none">No encryption</option>
                  </select>
                </div>
                <!-- Not editable for a same_as_password preset (Postmark): the
                     two fields must stay equal, and a blank password on edit
                     means "keep the stored one", so an edit here would leave the
                     account with a username the credential no longer matches. -->
                <div
                  v-if="editPresetUnknown || (editPreset && editPreset.username_mode !== 'user')"
                  class="space-y-1.5"
                >
                  <label class="text-sm font-medium" :for="fid('edit', 'adv-username', p.id)">Username</label>
                  <input
                    :id="fid('edit', 'adv-username', p.id)"
                    v-model="editForm.username"
                    :class="fieldClass"
                    :disabled="editPresetUnknown || editPreset?.username_mode === 'same_as_password'"
                    autocomplete="off"
                  />
                  <p v-if="editPresetUnknown" class="text-xs text-muted-foreground">
                    {{ presets.isError.value
                      ? "The provider list did not load, so the username stays locked."
                      : "Loading the provider settings…" }}
                    <button
                      v-if="presets.isError.value"
                      type="button"
                      class="underline"
                      @click="presets.refetch()"
                    >Try again</button>
                  </p>
                </div>
              </div>
            </details>

            <p v-if="portWarning(editForm)" class="text-xs text-destructive">{{ portWarning(editForm) }}</p>
            <p class="text-xs text-muted-foreground">
              Apps already using this account pick up changes the next time they restart or rebind.
            </p>
            <div class="flex gap-2">
              <Button :disabled="update.isPending.value || !formValid(editForm)" @click="update.mutate(p.id)">
                {{ update.isPending.value ? "Saving…" : "Save" }}
              </Button>
              <Button variant="ghost" @click="editFor = null">Cancel</Button>
            </div>
          </div>

          <!-- Per-row test confirmation / error -->
          <p v-if="testSent[p.id]" class="text-xs text-muted-foreground">
            Test email sent to {{ testSent[p.id] }}. Check that inbox.
          </p>
          <p v-if="rowError[p.id]" class="text-xs text-destructive">{{ rowError[p.id] }}</p>
        </li>
      </ul>
    </section>
  </div>
</template>
