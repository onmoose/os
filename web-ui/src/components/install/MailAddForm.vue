<script setup lang="ts">
// "Your Gmail account" (INSTALL_STEPS.md # 4): the form for a new email
// account in the install flow. It has no Save of its own. The page's
// Continue calls save(), which checks the settings when the user leaves
// "Test the settings first" on, saves the account, and returns it; the page
// then picks it. The password typed here stays in this component's memory
// only.
//
// It asks only what the preset needs. Gmail and iCloud: the address and the
// app password, with the numbered steps for making one folded under "Where
// do I find my app password?". SES and Mailgun: also
// the region, and the username. A preset with a fixed username (SendGrid,
// Resend) or a shared one (Postmark) does not ask for it. The server
// settings show only for Custom server. The optional name is always shown
// as a quiet underlined box. The field rules are shared with
// Settings through mailProviderForm.ts.
import { computed, ref, watch } from "vue";
import { useMutation, useQueryClient } from "@tanstack/vue-query";
import { ExternalLink } from "lucide-vue-next";
import { api, type MailPreset, type MailProvider } from "../../api";
import {
  bodyOf,
  errorMessage,
  formFromPreset,
  hostFor,
  portWarning,
  syncPersonalUsername,
  syncSameAsPassword,
  usernameBoxInSettings,
  type ProviderForm,
} from "../../mailProviderForm";
import { defaultKeyLabel } from "../../installSteps";
import Button from "../ui/Button.vue";

const props = defineProps<{
  preset: MailPreset;
  // labels are the names of the user's email accounts, so the default is free.
  labels: string[];
  // manifestId is the app being installed, whose install plan lists the
  // user's accounts. Empty in Settings.
  manifestId?: string;
  // settings: the form adds an account in Settings → Integrations → Email.
  // There it also asks the username for Gmail or Google Workspace (prefilled
  // from the address, for an alias), and shows a preset's server settings
  // behind a closed "Server settings" section, so an unusual server can be
  // typed in.
  settings?: boolean;
}>();

const qc = useQueryClient();

// The name starts empty, so its box shows the default ("Gmail", "Gmail 2").
function blankForm(): ProviderForm {
  return { ...formFromPreset(props.preset), label: "" };
}
const form = ref<ProviderForm>(blankForm());
const testOnAdd = ref(true);
const checking = ref(false);
const error = ref("");

watch(
  () => props.preset.id,
  () => {
    form.value = blankForm();
    error.value = "";
  },
);

const custom = computed(() => props.preset.id === "custom");
const personal = computed(() => !!props.preset.personal);
const defaultName = computed(() =>
  defaultKeyLabel(custom.value ? "My email" : props.preset.account_name || props.preset.label, props.labels),
);
const askUsername = computed(() =>
  props.settings ? usernameBoxInSettings(props.preset) : props.preset.username_mode === "user" && !personal.value,
);

// In Settings the Gmail username box starts as the from address and follows
// it until the user types a different one.
watch(
  () => form.value.from_address,
  (now, before) => {
    if (!props.settings || props.preset.id !== "google_workspace") return;
    if (form.value.username === "" || form.value.username === (before ?? "")) form.value.username = now;
  },
);
const setupLink = computed(() =>
  props.preset.setup_url && /^https?:\/\//i.test(props.preset.setup_url) ? props.preset.setup_url : "",
);

function onRegionChange() {
  form.value.host = hostFor(props.preset, form.value.region);
}

// valid keeps Continue off until the form can pass. The brain checks every
// rule again.
const valid = computed(() => {
  const f = form.value;
  if (!f.from_address.includes("@") || f.password.trim() === "") return false;
  if (askUsername.value && f.username.trim() === "") return false;
  if ((custom.value || props.settings) && (f.host.trim() === "" || f.port < 1 || f.port > 65535)) return false;
  return true;
});

const create = useMutation({
  mutationFn: async (body: ReturnType<typeof bodyOf>) => {
    // Check first, as Settings does: an account that cannot sign in should
    // never be saved. The check signs in but sends nothing.
    if (testOnAdd.value) {
      checking.value = true;
      try {
        await api.post<void>("/mail-providers/verify", body);
      } finally {
        checking.value = false;
      }
    }
    return api.post<MailProvider>("/mail-providers", body);
  },
});

// save stores the account and returns it, or null when it failed (the form
// then shows why).
async function save(): Promise<MailProvider | null> {
  if (!valid.value || create.isPending.value) return null;
  error.value = "";
  const f = { ...form.value, label: form.value.label.trim() };
  if (f.label === "") f.label = defaultName.value;
  syncPersonalUsername(f, props.preset);
  syncSameAsPassword(f, props.preset);
  try {
    const created = await create.mutateAsync(bodyOf(f));
    qc.invalidateQueries({ queryKey: ["mail-providers"] });
    // The install plan lists the user's accounts, so the last page can name
    // the new one.
    if (props.manifestId) await qc.invalidateQueries({ queryKey: ["install-plan", props.manifestId] });
    form.value.password = "";
    return created;
  } catch (e) {
    error.value = errorMessage(e) || "The account could not be saved. Try again.";
    return null;
  }
}

defineExpose({ valid, save, pending: create.isPending, checking });

const inputClass =
  "mt-2 block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 " +
  "outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 " +
  "focus:outline-accent sm:text-sm/6";
</script>

<template>
  <div class="space-y-6">
    <p v-if="(preset.steps ?? []).length === 0" class="text-sm text-muted-foreground">
      {{ preset.help }}
      <a v-if="preset.docs_url" :href="preset.docs_url" target="_blank" rel="noopener noreferrer" class="underline">
        {{ preset.label }} help
      </a>
    </p>

    <div>
      <label for="mail-add-from" class="block text-sm/6 font-medium text-foreground">
        {{ personal ? `Your ${preset.account_name || preset.label} address` : "Send from this address" }}
      </label>
      <input
        id="mail-add-from"
        v-model="form.from_address"
        type="email"
        placeholder="you@example.com"
        autocomplete="email"
        :class="inputClass"
      />
      <p v-if="!personal" class="mt-2 text-sm text-muted-foreground">The address your apps' email will come from.</p>
    </div>

    <div v-if="preset.region">
      <label for="mail-add-region" class="block text-sm/6 font-medium text-foreground">{{ preset.region.label }}</label>
      <select id="mail-add-region" v-model="form.region" :class="inputClass" @change="onRegionChange">
        <option v-for="o in preset.region.options ?? []" :key="o.value" :value="o.value">{{ o.label }}</option>
      </select>
    </div>

    <div v-if="askUsername">
      <label for="mail-add-username" class="block text-sm/6 font-medium text-foreground">Username</label>
      <input id="mail-add-username" v-model="form.username" autocomplete="off" :class="inputClass" />
    </div>

    <div>
      <label for="mail-add-password" class="block text-sm/6 font-medium text-foreground">
        {{ preset.credential_label }}
      </label>
      <input id="mail-add-password" v-model="form.password" type="password" autocomplete="new-password" :class="inputClass" />
    </div>

    <!-- Gmail and iCloud: the app-password steps, folded under the box. -->
    <details v-if="(preset.steps ?? []).length > 0" class="rounded-md border border-border px-4 py-3">
      <summary class="cursor-pointer text-sm font-medium text-foreground">Where do I find my app password?</summary>
      <ol class="mt-3 list-decimal space-y-3 pl-5 text-sm text-foreground marker:text-muted-foreground">
        <li v-for="(s, i) in preset.steps ?? []" :key="i">
          {{ s }}
          <div v-if="i === 1 && setupLink" class="mt-2">
            <Button size="sm" variant="secondary" as="a" :href="setupLink" target="_blank" rel="noopener noreferrer">
              Open your {{ preset.id === "icloud" ? "Apple" : "Google" }} account
              <ExternalLink class="size-3.5" aria-hidden="true" />
            </Button>
          </div>
        </li>
      </ol>
    </details>

    <!-- Server settings: open for a custom server, which has nothing filled
         in. In Settings a preset's settings are here too, closed. -->
    <details v-if="custom || settings" :open="custom" class="rounded-md border border-border px-4 py-3">
      <summary class="cursor-pointer text-sm font-medium text-muted-foreground">Server settings</summary>
      <div class="mt-3 space-y-4">
      <div>
        <label for="mail-add-host" class="block text-sm/6 font-medium text-foreground">SMTP server</label>
        <input id="mail-add-host" v-model="form.host" placeholder="smtp.example.com" autocomplete="off" :class="inputClass" />
      </div>
      <div>
        <label for="mail-add-port" class="block text-sm/6 font-medium text-foreground">Port</label>
        <input id="mail-add-port" v-model.number="form.port" type="number" min="1" max="65535" :class="inputClass" />
      </div>
      <div>
        <label for="mail-add-enc" class="block text-sm/6 font-medium text-foreground">Encryption</label>
        <select id="mail-add-enc" v-model="form.encryption" :class="inputClass">
          <option value="starttls">STARTTLS (usually port 587)</option>
          <option value="tls">TLS (usually port 465)</option>
          <option value="none">No encryption</option>
        </select>
      </div>
      <p v-if="portWarning(form)" class="text-sm text-destructive">{{ portWarning(form) }}</p>
      </div>
    </details>

    <div>
      <input
        id="mail-add-label"
        v-model="form.label"
        placeholder="Name this account (optional)"
        aria-label="Name this account (optional)"
        aria-describedby="mail-add-label-hint"
        autocomplete="off"
        class="block w-full border-0 border-b border-border bg-transparent px-0 py-1.5 text-base text-foreground placeholder:text-muted-foreground focus:border-accent focus:outline-none sm:text-sm/6"
      />
      <p id="mail-add-label-hint" class="mt-1 text-xs text-muted-foreground">Saved as "{{ defaultName }}" if empty.</p>
    </div>

    <label class="flex cursor-pointer items-start gap-2.5">
      <input v-model="testOnAdd" type="checkbox" class="mt-0.5 size-4 shrink-0 cursor-pointer accent-accent" />
      <span class="text-sm">
        Test the settings first
        <span class="block text-muted-foreground">Signs in to the server to check. No email is sent.</span>
      </span>
    </label>

    <p class="text-sm text-muted-foreground">Saved on your box. Your other apps can use it too.</p>
    <p v-if="checking" class="text-sm text-muted-foreground" role="status">Testing the settings…</p>
    <p v-if="error" class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive" role="alert">{{ error }}</p>
  </div>
</template>
