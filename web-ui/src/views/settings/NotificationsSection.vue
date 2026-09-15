<script setup lang="ts">
// Settings → Notifications: per-category notification mutes (NOTIFICATIONS.md #
// Configuration). Extracted from the old single-page SettingsView when Settings
// became a left-nav shell (SettingsLayout.vue). The toggle shows "receiving" (on)
// = not muted, so an empty mute set reads as everything-on.
import { computed } from "vue";
import { SwitchRoot, SwitchThumb } from "reka-ui";
import { useNotificationMutes } from "@/useNotificationMutes";

const { mutes, setMuted } = useNotificationMutes();
const mutedSet = computed(() => new Set(mutes.data.value?.muted ?? []));

// Display metadata for the notification taxonomy. The ids and their order mirror
// the wire contract (`notify.Categories`: storage | system | updates | security
// | account | app); the brain owns the taxonomy, the UI owns the wording. A new
// brain category needs a row here or it won't get a toggle.
const notificationCategories = [
  { id: "storage", label: "Storage", description: "Drives, free space, and data safety." },
  { id: "system", label: "System", description: "Startup, services, and background health." },
  { id: "updates", label: "Updates", description: "Available and installed updates." },
  { id: "security", label: "Security", description: "Sign-ins and account safety." },
  { id: "account", label: "Account", description: "Changes to people and access." },
  { id: "app", label: "Apps", description: "Activity and problems from your apps." },
];
</script>

<template>
  <section class="space-y-3">
    <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Notifications</h2>
    <p class="text-sm text-muted-foreground">Choose which kinds of notifications reach you. Turn a kind off to stop it from showing in your bell.</p>
    <p v-if="mutes.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
    <ul v-else class="space-y-2">
      <li
        v-for="c in notificationCategories"
        :key="c.id"
        class="flex items-center justify-between gap-4 rounded-xl border border-border bg-card px-4 py-3"
      >
        <div class="min-w-0">
          <div class="text-sm font-medium">{{ c.label }}</div>
          <div class="text-xs text-muted-foreground">{{ c.description }}</div>
        </div>
        <SwitchRoot
          :model-value="!mutedSet.has(c.id)"
          :aria-label="`${c.label} notifications`"
          class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors data-[state=checked]:border-accent data-[state=checked]:bg-accent"
          @update:model-value="(on: boolean) => setMuted.mutate({ category: c.id, muted: !on })"
        >
          <SwitchThumb
            class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
          />
        </SwitchRoot>
      </li>
    </ul>
  </section>
</template>
