<script setup lang="ts">
// The Email row of the install setup page (SERVICE_PROVISIONING.md # BYO
// outgoing mail). The user picks which account the app sends from, or None:
// the app then installs with email features off.
//
// An admin can add an account right here, without leaving the install. The
// form is the Settings add flow in short, and shares its rules through
// mailProviderForm.ts. Adding an account is admin-only and elevation-class, so
// it goes through withElevation, and a dismissed prompt is a quiet no-op.
//
// A new account reaches the picker through the install-plan query: the page
// refetches the plan, and this picks the new account once it is there.
import { computed, ref } from "vue";
import { useMutation, useQueryClient } from "@tanstack/vue-query";
import { MailX, Plus } from "lucide-vue-next";
import { api, type MailPreset, type MailProvider, type MailProviderOption } from "../../api";
import { withElevation } from "../../elevate";
import { useAuth } from "../../auth";
import Button from "../ui/Button.vue";
import MailProviderLogo from "../MailProviderLogo.vue";
import OptionCards from "./OptionCards.vue";
import {
  useMailPresets,
  formFromPreset,
  hostFor,
  syncSameAsPassword,
  formValid,
  portWarning,
  bodyOf,
  errorMessage,
  type ProviderForm,
} from "../../mailProviderForm";

const props = defineProps<{
  manifestId: string;
  appName: string;
  providers: MailProviderOption[];
}>();
// "" is None.
const selected = defineModel<string>({ required: true });

const qc = useQueryClient();
const { currentUser } = useAuth();
const isAdmin = computed(() => currentUser.value?.role === "admin");

const NONE = "__none";
const options = computed(() => [
  { id: NONE, label: "None", description: "Email features stay off" },
  ...props.providers.map((p) => ({ id: p.id, label: p.label })),
]);
const selectedIds = computed(() => [selected.value === "" ? NONE : selected.value]);

function pick(id: string) {
  selected.value = id === NONE ? "" : id;
}

function typeOf(id: string): string {
  return props.providers.find((p) => p.id === id)?.provider_type ?? "custom";
}

// ── Inline add ──────────────────────────────────────────────────────────────
// Three states: closed, picking a provider, filling in the form.
const adding = ref(false);
const preset = ref<MailPreset | null>(null);
const form = ref<ProviderForm | null>(null);
const addError = ref("");
const testOnAdd = ref(true);
const checking = ref(false);

// Only fetched once the admin opens the add flow: the endpoint is admin-only.
const presets = useMailPresets(adding);
const presetOptions = computed(() =>
  (presets.data.value?.presets ?? []).map((p) => ({ id: p.id, label: p.label })),
);

function startAdd() {
  adding.value = true;
  preset.value = null;
  form.value = null;
  addError.value = "";
}

function cancelAdd() {
  adding.value = false;
  preset.value = null;
  form.value = null;
}

function pickPreset(id: string) {
  const p = presets.data.value?.presets?.find((x) => x.id === id);
  if (!p) return;
  preset.value = p;
  form.value = formFromPreset(p);
  addError.value = "";
}

function onRegionChange() {
  if (preset.value && form.value) form.value.host = hostFor(preset.value, form.value.region);
}

const create = useMutation({
  mutationFn: async () => {
    if (!form.value) throw new Error("no form");
    syncSameAsPassword(form.value, preset.value ?? undefined);
    const body = bodyOf(form.value);
    // Check first, as the Settings flow does: an account that cannot connect
    // should never be saved. The check signs in but sends nothing.
    if (testOnAdd.value) {
      checking.value = true;
      try {
        await api.post<void>("/mail-providers/verify", body);
      } finally {
        checking.value = false;
      }
    }
    return withElevation(() => api.post<MailProvider>("/mail-providers", body));
  },
  onSuccess: async (created) => {
    qc.invalidateQueries({ queryKey: ["mail-providers"] });
    await qc.invalidateQueries({ queryKey: ["install-plan", props.manifestId] });
    selected.value = created.id;
    cancelAdd();
  },
  onError: (e) => {
    addError.value = errorMessage(e);
  },
});

const inputClass =
  "block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 " +
  "outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 " +
  "focus:outline-accent disabled:cursor-not-allowed disabled:opacity-60 sm:text-sm/6";
</script>

<template>
  <div class="space-y-4">
    <p class="text-sm text-muted-foreground">
      {{ appName }} can send email, like password resets and reminders. Pick the account it should send from.
    </p>

    <OptionCards label="Email account" :options="options" :selected="selectedIds" @pick="pick">
      <template #icon="{ option }">
        <span
          v-if="option.id === NONE"
          class="flex size-10 items-center justify-center rounded-lg bg-muted text-muted-foreground"
        >
          <MailX class="size-5 stroke-[1.5]" aria-hidden="true" />
        </span>
        <MailProviderLogo v-else :id="typeOf(option.id)" :label="option.label" size="inline" />
      </template>
      <template v-if="isAdmin && !adding" #extra>
        <button
          type="button"
          class="flex w-full cursor-pointer items-center gap-3 rounded-lg border border-dashed border-border px-4 py-3.5 text-left hover:border-olive-400 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
          @click="startAdd"
        >
          <span class="flex size-10 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
            <Plus class="size-5" aria-hidden="true" />
          </span>
          <span class="text-sm font-medium text-foreground">Add an account</span>
        </button>
      </template>
    </OptionCards>

    <p v-if="!isAdmin && providers.length === 0" class="text-sm text-muted-foreground">
      No email accounts yet. An admin can add one in Settings, and you can pick it later on the app's settings screen.
    </p>

    <!-- Inline add, step 1: who sends the email. -->
    <div v-if="adding" class="space-y-4 rounded-lg border border-border bg-card p-4">
      <template v-if="!preset">
        <h4 class="text-sm font-semibold text-foreground">Who sends your email?</h4>
        <p v-if="presets.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
        <div v-else-if="presets.isError.value" class="space-y-2">
          <p class="text-sm text-destructive">Could not load the provider list.</p>
          <Button variant="secondary" size="sm" @click="presets.refetch()">Try again</Button>
        </div>
        <OptionCards v-else label="Email provider" :options="presetOptions" :selected="[]" @pick="pickPreset">
          <template #icon="{ option }">
            <MailProviderLogo :id="option.id" :label="option.label" size="inline" />
          </template>
        </OptionCards>
        <Button size="sm" variant="ghost" @click="cancelAdd">Cancel</Button>
      </template>

      <!-- Step 2: what the preset cannot know. -->
      <template v-else-if="form">
        <div class="flex items-center gap-2.5">
          <MailProviderLogo :id="preset.id" :label="preset.label" size="inline" />
          <h4 class="text-sm font-semibold text-foreground">{{ preset.label }}</h4>
        </div>
        <p class="text-sm text-muted-foreground">
          {{ preset.help }}
          <a v-if="preset.docs_url" :href="preset.docs_url" target="_blank" rel="noopener noreferrer" class="underline">
            Provider docs
          </a>
        </p>

        <div>
          <label for="mail-add-label" class="block text-sm/6 font-medium text-foreground">Account name</label>
          <input id="mail-add-label" v-model="form.label" autocomplete="off" :class="[inputClass, 'mt-2']" />
          <p class="mt-2 text-sm text-muted-foreground">What you'll see when an app asks which account to send from.</p>
        </div>

        <div>
          <label for="mail-add-from" class="block text-sm/6 font-medium text-foreground">From address</label>
          <input
            id="mail-add-from"
            v-model="form.from_address"
            type="email"
            placeholder="hello@example.com"
            autocomplete="off"
            :class="[inputClass, 'mt-2']"
          />
          <p class="mt-2 text-sm text-muted-foreground">The address your apps' email will come from.</p>
        </div>

        <div v-if="preset.region">
          <label for="mail-add-region" class="block text-sm/6 font-medium text-foreground">{{ preset.region.label }}</label>
          <select id="mail-add-region" v-model="form.region" :class="[inputClass, 'mt-2']" @change="onRegionChange">
            <option v-for="o in preset.region.options ?? []" :key="o.value" :value="o.value">{{ o.label }}</option>
          </select>
        </div>

        <div v-if="preset.username_mode === 'user'">
          <label for="mail-add-username" class="block text-sm/6 font-medium text-foreground">Username</label>
          <input id="mail-add-username" v-model="form.username" autocomplete="off" :class="[inputClass, 'mt-2']" />
        </div>

        <div>
          <label for="mail-add-password" class="block text-sm/6 font-medium text-foreground">
            {{ preset.credential_label }}
          </label>
          <input
            id="mail-add-password"
            v-model="form.password"
            type="password"
            autocomplete="new-password"
            :class="[inputClass, 'mt-2']"
          />
        </div>

        <!-- Server settings: filled in from the preset, open only for a
             custom server where nothing is filled in. -->
        <details class="rounded-md border border-border px-3 py-2" :open="preset.id === 'custom'">
          <summary class="cursor-pointer text-sm font-medium text-muted-foreground">Server settings</summary>
          <div class="mt-3 space-y-4">
            <div>
              <label for="mail-add-host" class="block text-sm/6 font-medium text-foreground">SMTP server</label>
              <input
                id="mail-add-host"
                v-model="form.host"
                placeholder="smtp.example.com"
                autocomplete="off"
                :class="[inputClass, 'mt-2']"
              />
            </div>
            <div>
              <label for="mail-add-port" class="block text-sm/6 font-medium text-foreground">Port</label>
              <input
                id="mail-add-port"
                v-model.number="form.port"
                type="number"
                min="1"
                max="65535"
                :class="[inputClass, 'mt-2']"
              />
            </div>
            <div>
              <label for="mail-add-enc" class="block text-sm/6 font-medium text-foreground">Encryption</label>
              <select id="mail-add-enc" v-model="form.encryption" :class="[inputClass, 'mt-2']">
                <option value="starttls">STARTTLS (usually port 587)</option>
                <option value="tls">TLS (usually port 465)</option>
                <option value="none">No encryption</option>
              </select>
            </div>
            <div v-if="preset.username_mode !== 'user'">
              <label for="mail-add-adv-user" class="block text-sm/6 font-medium text-foreground">Username</label>
              <input
                id="mail-add-adv-user"
                v-model="form.username"
                :disabled="preset.username_mode === 'same_as_password'"
                autocomplete="off"
                :class="[inputClass, 'mt-2']"
              />
            </div>
          </div>
        </details>

        <p v-if="portWarning(form)" class="text-sm text-destructive">{{ portWarning(form) }}</p>

        <label class="flex cursor-pointer items-start gap-2.5">
          <input v-model="testOnAdd" type="checkbox" class="mt-0.5 size-4 shrink-0 cursor-pointer accent-accent" />
          <span class="text-sm">
            Test the settings first
            <span class="block text-muted-foreground">Signs in to the server to check. No email is sent.</span>
          </span>
        </label>

        <div class="flex flex-wrap items-center gap-2">
          <Button size="sm" :disabled="create.isPending.value || !formValid(form)" @click="create.mutate()">
            {{ checking ? "Testing…" : create.isPending.value ? "Adding…" : "Add account" }}
          </Button>
          <Button size="sm" variant="secondary" @click="preset = null">Other provider</Button>
          <Button size="sm" variant="ghost" @click="cancelAdd">Cancel</Button>
        </div>
        <p v-if="addError" class="text-sm text-destructive">{{ addError }}</p>
      </template>
    </div>
  </div>
</template>
