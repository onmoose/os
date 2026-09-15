<script setup lang="ts">
// Settings → Outgoing email → add an account. Two real routes, so the two steps
// of the flow are two pages and the browser Back button does what it looks like
// it should:
//
//   /settings/mail/add           pick a provider
//   /settings/mail/add/:preset   fill in what that preset cannot know
//
// The picked preset is read from the route, not held in local state. That is the
// whole point: Back from the form returns to the picker, Back from the picker
// returns to the account list, and a half-filled form can be linked or reloaded.
// The form fields themselves are not in the URL. A credential must never land
// in history, so a reload re-seeds them from the preset.
//
// Admin-only, mirroring the list view: the redirect here is defence in depth
// (the nav already hides the section) and the brain refuses a non-admin anyway.
import { ref, computed, watch } from "vue";
import { useRoute, useRouter, RouterLink } from "vue-router";
import { useMutation, useQueryClient } from "@tanstack/vue-query";
import { ArrowLeft } from "lucide-vue-next";
import { api, type MailProvider, type MailPreset } from "@/api";
import { withElevation } from "@/elevate";
import { useAuth } from "@/auth";
import Button from "@/components/ui/Button.vue";
import MailProviderLogo from "@/components/MailProviderLogo.vue";
import {
  useMailPresets, emptyForm, formFromPreset, hostFor, syncSameAsPassword,
  formValid, portWarning, bodyOf, errorMessage, fieldClass,
  type ProviderForm,
} from "@/mailProviderForm";

const route = useRoute();
const router = useRouter();
const qc = useQueryClient();
const { currentUser } = useAuth();

watch(
  currentUser,
  (u) => {
    if (u && u.role !== "admin") router.replace("/settings");
  },
  { immediate: true },
);

const presets = useMailPresets();
const presetList = computed(() => presets.data.value?.presets ?? []);

// The route decides the step. No param means the picker.
const presetID = computed(() => (route.params.preset as string | undefined) ?? "");
const preset = computed<MailPreset | undefined>(() =>
  presetList.value.find((p) => p.id === presetID.value),
);

// A preset id that names nothing (a stale link, or a preset we withdrew)
// falls back to the picker rather than rendering an empty form. Waits for the
// query, since presets are empty on the first tick.
watch([presetID, presetList], ([id, list]) => {
  if (id && list.length > 0 && !list.some((p) => p.id === id)) {
    router.replace("/settings/mail/add");
  }
});

const form = ref<ProviderForm>(emptyForm());
const advancedOpen = ref(false);
const createError = ref<string | null>(null);

// On by default: a provider that cannot connect is the common failure, and
// finding out at save time beats finding out when an app first tries to send.
const testOnAdd = ref(true);
const checking = ref(false);

// Seed the form whenever the route lands on a preset, including on a reload
// straight into the form URL and on Back into it from elsewhere.
watch(
  preset,
  (p) => {
    if (!p) return;
    form.value = formFromPreset(p);
    // Custom is the old hand-typed form: nothing is prefilled, so there is
    // nothing to hide behind the disclosure.
    advancedOpen.value = p.id === "custom";
    createError.value = null;
  },
  { immediate: true },
);

function onRegionChange() {
  if (preset.value) form.value.host = hostFor(preset.value, form.value.region);
}

const create = useMutation({
  mutationFn: async () => {
    syncSameAsPassword(form.value, preset.value);
    const body = bodyOf(form.value);
    // Check before saving, not after: a config that cannot connect never
    // becomes an account the admin then has to find and delete. The check
    // connects and authenticates but sends nothing, so nobody gets a surprise
    // email from a form submission.
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
  onSuccess: () => {
    qc.invalidateQueries({ queryKey: ["mail-providers"] });
    // Back to the list, and replace so Back from there does not re-open the
    // form for an account that now exists.
    router.replace("/settings/mail");
  },
  onError: (e) => {
    createError.value = errorMessage(e);
  },
});

// Labels are unique across the create form only, so plain ids are enough here.
function fid(name: string): string {
  return `mail-new-${name}`;
}
</script>

<template>
  <div class="space-y-6">
    <!-- Step 1: pick a provider -->
    <section v-if="!presetID" class="space-y-3">
      <RouterLink
        to="/settings/mail"
        class="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft class="size-4" /> Email accounts
      </RouterLink>

      <div class="space-y-3 rounded-2xl border border-border bg-card p-5 sm:p-6">
        <h3 class="text-sm font-semibold text-foreground">Who sends your email?</h3>
        <p class="text-xs text-muted-foreground">
          Pick your provider and malmo fills in the server settings for you.
        </p>
        <p v-if="presets.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
        <!-- The list is server-side, and "Custom SMTP server" is one of its
             entries, so a failed load leaves nothing to click. Say so and offer
             a retry rather than rendering an empty grid. -->
        <div v-else-if="presets.isError.value" class="space-y-2">
          <p class="text-sm text-destructive">Could not load the provider list.</p>
          <Button variant="secondary" size="sm" @click="presets.refetch()">Try again</Button>
        </div>
        <div v-else class="grid gap-3 sm:grid-cols-3">
          <RouterLink
            v-for="p in presetList"
            :key="p.id"
            :to="`/settings/mail/add/${p.id}`"
            class="flex cursor-pointer flex-col items-center justify-center gap-2 rounded-xl border border-border bg-background px-3 py-5 text-center transition-colors hover:border-accent hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-card active:scale-[0.98]"
          >
            <span class="flex h-9 items-center justify-center">
              <MailProviderLogo :id="p.id" :label="p.label" />
            </span>
            <span class="text-sm font-medium text-foreground">{{ p.label }}</span>
          </RouterLink>
        </div>
      </div>
    </section>

    <!-- Step 2: fill in what the preset cannot know. One field per line, each
         with its own label and, where the name alone is not enough, a hint. -->
    <section v-else-if="preset" class="space-y-3">
      <RouterLink
        to="/settings/mail/add"
        class="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft class="size-4" /> Choose a different provider
      </RouterLink>

      <div class="space-y-4 rounded-2xl border border-border bg-card p-5 sm:p-6">
        <div class="flex items-center gap-2.5">
          <MailProviderLogo :id="preset.id" :label="preset.label" size="inline" />
          <h3 class="text-sm font-semibold text-foreground">{{ preset.label }}</h3>
        </div>

        <p class="text-xs text-muted-foreground">
          {{ preset.help }}
          <a
            v-if="preset.docs_url"
            :href="preset.docs_url"
            target="_blank"
            rel="noopener noreferrer"
            class="underline"
          >Provider docs</a>
        </p>

        <!-- Account name: malmo's own label for the account, not anything the
             provider knows about. -->
        <div class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('label')">Account name</label>
          <input :id="fid('label')" v-model="form.label" :class="fieldClass" autocomplete="off" />
          <p class="text-xs text-muted-foreground">
            What you'll see when an app asks which account to send from. Only you see this.
          </p>
        </div>

        <div class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('from')">From address</label>
          <input
            :id="fid('from')"
            v-model="form.from_address"
            type="email"
            placeholder="hello@example.com"
            :class="fieldClass"
            autocomplete="off"
          />
          <p class="text-xs text-muted-foreground">
            The address your apps' email will come from. Your provider has to allow it.
          </p>
        </div>

        <!-- Region: only the presets whose region changes the host carry one. -->
        <div v-if="preset.region" class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('region')">{{ preset.region.label }}</label>
          <select :id="fid('region')" v-model="form.region" :class="fieldClass" @change="onRegionChange">
            <option v-for="o in (preset.region.options ?? [])" :key="o.value" :value="o.value">
              {{ o.label }}
            </option>
          </select>
          <p class="text-xs text-muted-foreground">
            Pick the region your account is in. It decides which server malmo connects to.
          </p>
        </div>

        <!-- Username: hidden when the provider fixes it or reuses the credential. -->
        <div v-if="preset.username_mode === 'user'" class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('username')">Username</label>
          <input :id="fid('username')" v-model="form.username" :class="fieldClass" autocomplete="off" />
        </div>

        <div class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('password')">{{ preset.credential_label }}</label>
          <input
            :id="fid('password')"
            v-model="form.password"
            type="password"
            :class="fieldClass"
            autocomplete="new-password"
          />
        </div>

        <!-- Server settings: prefilled and editable, so a non-standard endpoint
             is never trapped. :open is read at mount only, which holds because
             this block remounts when the route changes preset. -->
        <details class="rounded-xl border border-border px-3 py-2" :open="advancedOpen">
          <summary class="cursor-pointer text-sm font-medium text-muted-foreground">Server settings</summary>
          <div class="mt-3 space-y-4">
            <p class="text-xs text-muted-foreground">
              Filled in for you. Change these only if your provider gave you different values.
            </p>
            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('host')">SMTP server</label>
              <input
                :id="fid('host')"
                v-model="form.host"
                placeholder="smtp.example.com"
                :class="fieldClass"
                autocomplete="off"
              />
            </div>
            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('port')">Port</label>
              <input :id="fid('port')" v-model.number="form.port" type="number" min="1" max="65535" :class="fieldClass" />
            </div>
            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('encryption')">Encryption</label>
              <select :id="fid('encryption')" v-model="form.encryption" :class="fieldClass">
                <option value="starttls">STARTTLS (usually port 587)</option>
                <option value="tls">TLS (usually port 465)</option>
                <option value="none">No encryption</option>
              </select>
            </div>
            <!-- A same_as_password preset (Postmark) keeps username and password
                 equal, so this field is shown but not editable: an independent
                 edit would be silently overwritten here, and on the edit form it
                 would diverge from the stored credential and break AUTH. -->
            <div v-if="preset.username_mode !== 'user'" class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('adv-username')">Username</label>
              <input
                :id="fid('adv-username')"
                v-model="form.username"
                :class="fieldClass"
                :disabled="preset.username_mode === 'same_as_password'"
                autocomplete="off"
              />
              <p class="text-xs text-muted-foreground">
                {{ preset.username_mode === "fixed"
                  ? `${preset.label} requires this exact value.`
                  : `${preset.label} uses the same value as the ${preset.credential_label.toLowerCase()}.` }}
              </p>
            </div>
          </div>
        </details>

        <p v-if="portWarning(form)" class="text-xs text-destructive">{{ portWarning(form) }}</p>

        <label class="flex cursor-pointer items-start gap-2.5">
          <input
            v-model="testOnAdd"
            type="checkbox"
            class="mt-0.5 size-4 shrink-0 cursor-pointer rounded border-border accent-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2"
          />
          <span class="text-sm">
            Test configuration when adding
            <span class="block text-xs text-muted-foreground">
              Connects to the server and signs in to check the settings work. No email is sent.
            </span>
          </span>
        </label>

        <div class="flex items-center gap-3">
          <Button :disabled="create.isPending.value || !formValid(form)" @click="create.mutate()">
            {{ checking ? "Testing…" : create.isPending.value ? "Adding…" : "Add account" }}
          </Button>
          <Button variant="ghost" :as="RouterLink" to="/settings/mail">Cancel</Button>
          <p v-if="createError" class="text-xs text-destructive">{{ createError }}</p>
        </div>
      </div>
    </section>

    <!-- Deep link straight into a form URL, with no preset to render yet. It
         is still loading, or the load failed and there is nothing to fall back
         on, so say which. -->
    <div v-else-if="presets.isError.value" class="space-y-2">
      <p class="text-sm text-destructive">Could not load the provider list.</p>
      <Button variant="secondary" size="sm" @click="presets.refetch()">Try again</Button>
    </div>
    <p v-else class="text-sm text-muted-foreground">Loading…</p>
  </div>
</template>
