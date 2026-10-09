<script setup lang="ts">
// StoreAppChip: a small icon tile for one app, its name as the tooltip. Used
// on pack cards and as the fallback art of a slide or pack with no
// illustration. Size and corner radius come from the parent's class.
import { ref } from "vue";
import type { CatalogEntry } from "../api";
import AppGlyph from "./AppGlyph.vue";

defineProps<{ app: CatalogEntry }>();

// brokenIcon falls back to the glyph when the icon fails to load.
const brokenIcon = ref(false);
</script>

<template>
  <div
    class="grid shrink-0 place-items-center overflow-hidden border border-border bg-background text-muted-foreground"
    :title="app.name"
  >
    <img
      v-if="app.icon_url && !brokenIcon"
      :src="app.icon_url"
      :alt="`${app.name} icon`"
      class="size-3/5 object-contain"
      loading="lazy"
      @error="brokenIcon = true"
    />
    <AppGlyph v-else :name="app.icon_glyph" class="size-2/5" />
  </div>
</template>
