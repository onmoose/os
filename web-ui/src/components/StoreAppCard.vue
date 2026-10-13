<script setup lang="ts">
// A single store browse-grid card (DASHBOARD.md # global navigation, APP_STORE.md
// # Catalog schema). Visually a sibling of the dashboard's AppTile — square,
// bordered, rounded — but it's a navigation target, not an app launcher: the
// whole card is a RouterLink to /store/:id, the detail page where Install lives.
// Renders the catalog icon when the manifest declares one, falling back to a
// generic glyph (icon_url is omitted by the brain when absent, and we also guard
// against a load error). The tagline (short_description) sits truncated under
// the name, matching how the control plane's own store surface renders a card.
import { ref } from "vue";
import { RouterLink } from "vue-router";
import type { CatalogEntry } from "../api";
import AppGlyph from "./AppGlyph.vue";

defineProps<{ app: CatalogEntry }>();

// brokenIcon flips if the <img> fails to load (e.g. asset 404) so we fall back to
// the glyph rather than show a broken-image chrome.
const brokenIcon = ref(false);
</script>

<template>
  <RouterLink
    :to="`/store/${app.id}`"
    class="group flex flex-col items-center gap-3 text-center"
    :title="`View ${app.name}`"
  >
    <!-- The big square button: parchment fill, bordered, the app icon (or glyph).
         Mirrors AppTile's tile so the store reads as the same family. -->
    <div
      class="grid aspect-square w-full place-items-center overflow-hidden rounded-3xl border border-border bg-card text-muted-foreground transition group-hover:shadow-md"
    >
      <img
        v-if="app.icon_url && !brokenIcon"
        :src="app.icon_url"
        :alt="`${app.name} icon`"
        class="size-3/5 object-contain"
        @error="brokenIcon = true"
      />
      <AppGlyph v-else :name="app.icon_glyph" class="size-3/5" />
    </div>
    <div class="w-full min-w-0">
      <div class="truncate text-base font-medium">{{ app.name }}</div>
      <div class="mt-0.5 truncate text-sm text-muted-foreground">{{ app.short_description }}</div>
    </div>
  </RouterLink>
</template>
