<script setup lang="ts">
// StoreArt: the art of a discover slide or a pack: its illustration, or, when
// it has none or the image fails to load, the apps' icons on the frame's plain
// ground. The frame (size, ground, corners) is the parent's. The illustration
// URL is the brain's own route (GET /api/v1/catalog/illustration), so the art
// stays same-origin like an app icon.
import { ref, watch } from "vue";
import type { CatalogEntry } from "../api";
import StoreAppChip from "./StoreAppChip.vue";

const props = defineProps<{
  url?: string | null;
  apps: CatalogEntry[];
  // lg is the hero slide and the pack page header; sm everything else.
  size: "lg" | "sm";
}>();

const broken = ref(false);
watch(
  () => props.url,
  () => {
    broken.value = false;
  },
);
</script>

<template>
  <img
    v-if="url && !broken"
    :src="url"
    alt=""
    class="size-full object-cover"
    loading="lazy"
    @error="broken = true"
  />
  <div v-else class="flex flex-wrap items-center justify-center gap-2 p-4">
    <StoreAppChip
      v-for="a in apps.slice(0, 4)"
      :key="a.id"
      :app="a"
      :class="size === 'lg' ? 'size-20 rounded-3xl' : 'size-11 rounded-2xl'"
    />
  </div>
</template>
