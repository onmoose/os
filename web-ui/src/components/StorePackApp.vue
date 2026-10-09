<script setup lang="ts">
// StorePackApp: one app on a pack page: its icon, name and tagline (a link to
// the app page), and its own Install button. The button runs that app's
// normal install flow (useStartInstall), the same as Install on the app page:
// the user installs a pack one app at a time. An app the caller already has
// shows Open instead, and an app this box cannot install shows why, as on the
// app page.
import { computed, toRef } from "vue";
import { RouterLink } from "vue-router";
import type { CatalogEntry } from "../api";
import { useAppInstances, useStartInstall } from "../useInstall";
import HealthGated from "./HealthGated.vue";
import SplitButton from "./SplitButton.vue";
import StoreAppChip from "./StoreAppChip.vue";

const props = defineProps<{ app: CatalogEntry }>();
const manifestId = toRef(() => props.app.id);

const { householdInstance, ownPersonalInstance, installing, canInstallHousehold } = useAppInstances(manifestId);
const { planQuery, unavailable, goInstall, pending } = useStartInstall(manifestId);

const dropdownItems = computed(() =>
  canInstallHousehold.value ? [{ label: "Install for the whole household", action: () => goInstall(true) }] : [],
);
</script>

<template>
  <li class="flex flex-col gap-3 p-4 sm:flex-row sm:items-center sm:gap-4 sm:p-5">
    <RouterLink :to="`/store/${encodeURIComponent(app.id)}`" class="group flex min-w-0 flex-1 items-center gap-4">
      <StoreAppChip :app="app" class="size-12 rounded-2xl" />
      <div class="min-w-0 flex-1">
        <div class="truncate text-base/6 font-medium text-foreground">{{ app.name }}</div>
        <div v-if="app.short_description" class="mt-0.5 text-sm/6 text-muted-foreground">{{ app.short_description }}</div>
      </div>
    </RouterLink>

    <div class="flex shrink-0 items-center gap-2 sm:justify-end">
      <a
        v-if="householdInstance && householdInstance.state !== 'installing'"
        :href="householdInstance.url"
        target="_blank"
        rel="noopener"
        class="rounded-lg border border-border px-3 py-1.5 text-sm hover:bg-muted"
      >
        Open shared app
      </a>
      <a
        v-else-if="ownPersonalInstance && ownPersonalInstance.state !== 'installing'"
        :href="ownPersonalInstance.url"
        target="_blank"
        rel="noopener"
        class="rounded-lg bg-accent px-4 py-1.5 text-sm font-medium text-accent-foreground hover:opacity-90"
      >
        Open
      </a>
      <HealthGated
        v-if="!unavailable && (!ownPersonalInstance || ownPersonalInstance.state === 'installing')"
        blocks="apps"
      >
        <SplitButton
          :label="installing ? 'Installing…' : pending ? 'Starting…' : 'Install'"
          :loading="installing || pending || planQuery.isPending.value"
          :disabled="installing || pending || planQuery.isPending.value"
          :items="dropdownItems"
          @click="goInstall()"
        />
      </HealthGated>
      <p v-if="unavailable" class="max-w-xs text-sm text-muted-foreground">{{ unavailable }}</p>
    </div>
  </li>
</template>
