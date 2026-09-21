<script setup lang="ts">
// The top bar (DASHBOARD.md # the top bar): quiet controls clustered top-right —
// the notification bell + live-resources chevron + account menu. Everything here
// is deliberately understated; it goes loud only under pressure, which none of
// these surfaces do yet:
//   - Bell — no unread dot until NOTIFICATIONS.md is wired (queue item #2).
//   - Live-resources chevron — opens a compact CPU/RAM/net/disk panel backed by
//     the GET /api/v1/system/live SSE stream; available to every signed-in user.
//   - Account menu — the person's name + role; sign-out is intentionally omitted in the
//     single-user dev phase (re-enabled with the AUTH.md login screen).
// The storage pill is deferred until a capacity endpoint exists; it will return
// here (per the design, top-right) rather than the old left-side Settings link.
import { ref } from "vue";
import { RouterLink } from "vue-router";
import { useAuth } from "../auth";
import NotificationBell from "../NotificationBell.vue";
import LiveResources from "../LiveResources.vue";
import Button from "./ui/Button.vue";

const { currentUser } = useAuth();
const menuOpen = ref(false);

function initial(name: string | undefined): string {
  return (name ?? "?").charAt(0).toUpperCase();
}
</script>

<template>
  <header class="flex items-center justify-end px-4 py-3">
    <div class="flex items-center gap-2">
      <NotificationBell />

      <LiveResources />

      <div class="relative">
        <Button
          type="button"
          variant="primary"
          size="icon"
          :title="currentUser?.display_name"
          @click="menuOpen = !menuOpen"
        >
          {{ initial(currentUser?.display_name) }}
        </Button>
        <div
          v-if="menuOpen"
          class="absolute right-0 z-10 mt-2 w-44 rounded-lg border border-border bg-card py-1 shadow-lg"
          @click="menuOpen = false"
        >
          <div class="px-3 py-2">
            <div class="text-sm font-medium">{{ currentUser?.display_name }}</div>
            <div class="text-xs capitalize text-muted-foreground">{{ currentUser?.role }}</div>
          </div>
          <RouterLink
            to="/settings"
            class="block px-3 py-2 text-sm hover:bg-muted"
          >
            Settings
          </RouterLink>
        </div>
      </div>
    </div>
  </header>
</template>
