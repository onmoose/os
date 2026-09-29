<script setup lang="ts">
// Settings → Integrations → Email → add an account. Two real routes, so the two steps
// of the flow are two pages and the browser Back button does what it looks like
// it should:
//
//   /settings/email/add           pick a service (the install flow's email grid)
//   /settings/email/add/:preset   fill in what that preset cannot know
//
// Both steps reuse the install flow's views (INSTALL_STEPS.md # 4): the
// MailServiceGrid, then the MailAddForm, which in Settings also asks the
// Gmail username and keeps the server settings behind a closed section. The
// picked preset is read from the route, not held in local state: Back from
// the form returns to the grid, Back from the grid returns to the account
// list, and a half-filled form can be linked or reloaded. The form fields
// themselves are not in the URL. A credential must never land in history,
// so a reload re-seeds them from the preset.
//
// Open to every user, like the list view. The new account belongs to whoever
// adds it. Adding needs no password re-prompt (the account is the user's own).
import { ref, computed, watch } from "vue";
import { useRoute, useRouter, RouterLink } from "vue-router";
import { useQuery } from "@tanstack/vue-query";
import { ArrowLeft } from "lucide-vue-next";
import { api, type MailPreset, type MailProvider } from "@/api";
import Button from "@/components/ui/Button.vue";
import MailProviderLogo from "@/components/MailProviderLogo.vue";
import MailServiceGrid from "@/components/install/MailServiceGrid.vue";
import MailAddForm from "@/components/install/MailAddForm.vue";
import { useMailPresets } from "@/mailProviderForm";

const route = useRoute();
const router = useRouter();

const presets = useMailPresets();
const presetList = computed(() => presets.data.value?.presets ?? []);

// The user's accounts, so a new one gets a free default name.
const accountsQuery = useQuery({
  queryKey: ["mail-providers"],
  queryFn: () => api.get<{ providers: MailProvider[] | null }>("/mail-providers"),
});
const labels = computed(() => (accountsQuery.data.value?.providers ?? []).map((p) => p.label));

// The route decides the step. No param means the grid.
const presetID = computed(() => (route.params.preset as string | undefined) ?? "");
const preset = computed<MailPreset | undefined>(() => presetList.value.find((p) => p.id === presetID.value));

// A preset id that names nothing (a stale link, or a preset we withdrew)
// falls back to the grid rather than rendering an empty form. Waits for the
// query, since presets are empty on the first tick.
watch([presetID, presetList], ([id, list]) => {
  if (id && list.length > 0 && !list.some((p) => p.id === id)) router.replace("/settings/email/add");
});

const picked = ref("");
const mailForm = ref<InstanceType<typeof MailAddForm> | null>(null);

// goBack goes one page back, as the browser's Back does, when the page before
// is this app's; a page opened by a link goes to fallback instead. Back and
// Continue are the only buttons, as in the install flow.
function goBack(fallback: string) {
  if (window.history.state?.back) router.back();
  else router.push(fallback);
}

async function submit() {
  const created = await mailForm.value?.save();
  // Back to the list, and replace so Back from there does not re-open the
  // form for an account that now exists.
  if (created) router.replace("/settings/email");
}
</script>

<template>
  <div class="space-y-6">
    <!-- Step 1: pick a service -->
    <section v-if="!presetID" class="space-y-3">
      <div class="space-y-4 rounded-2xl border border-border bg-card p-5 sm:p-6">
        <h3 class="text-sm font-semibold text-foreground">Which email service?</h3>
        <p v-if="presets.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
        <!-- The list is server-side, and "Custom server" is one of its
             entries, so a failed load leaves nothing to click. Say so and
             offer a retry rather than rendering an empty grid. -->
        <div v-else-if="presets.isError.value" class="space-y-2">
          <p class="text-sm text-destructive">Could not load the list of email services.</p>
          <Button variant="secondary" size="sm" @click="presets.refetch()">Try again</Button>
        </div>
        <MailServiceGrid v-else v-model="picked" :presets="presetList" label="Email service" />
        <div class="flex gap-2">
          <Button variant="ghost" @click="goBack('/settings/email')"><ArrowLeft class="size-4" /> Back</Button>
          <Button :disabled="!picked" @click="router.push(`/settings/email/add/${picked}`)">Continue</Button>
        </div>
      </div>
    </section>

    <!-- Step 2: fill in what the preset cannot know. -->
    <section v-else-if="preset" class="space-y-3">
      <div class="space-y-5 rounded-2xl border border-border bg-card p-5 sm:p-6">
        <div class="flex items-center gap-2.5">
          <MailProviderLogo :id="preset.id" :label="preset.label" size="inline" />
          <h3 class="text-sm font-semibold text-foreground">
            {{ preset.id === "custom" ? "Your email server" : `Your ${preset.account_name || preset.label} account` }}
          </h3>
        </div>
        <MailAddForm ref="mailForm" :preset="preset" :labels="labels" settings />
        <div class="flex gap-2">
          <Button variant="ghost" @click="goBack('/settings/email/add')"><ArrowLeft class="size-4" /> Back</Button>
          <Button :disabled="!mailForm?.valid || mailForm?.pending" @click="submit">
            {{ mailForm?.checking ? "Testing…" : mailForm?.pending ? "Adding…" : "Continue" }}
          </Button>
        </div>
      </div>
    </section>

    <!-- A direct link to a form whose preset list could not load: say so, with
         a retry and a way back. An id that names no preset goes to the grid
         once the list loads (the watch above). -->
    <div v-else-if="presets.isError.value" class="space-y-3">
      <p class="text-sm text-destructive" role="alert">Could not load the list of email services.</p>
      <div class="flex flex-wrap gap-2">
        <Button variant="secondary" size="sm" @click="presets.refetch()">Try again</Button>
        <Button variant="ghost" size="sm" :as="RouterLink" to="/settings/email">Email accounts</Button>
      </div>
    </div>
    <p v-else class="text-sm text-muted-foreground">Loading…</p>
  </div>
</template>
