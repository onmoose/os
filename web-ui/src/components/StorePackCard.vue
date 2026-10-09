<script setup lang="ts">
// StorePackCard: one pack on the store landing and in search results: its
// art, title and description, then the apps' icons and one "View pack"
// button. The art, the title and the button all open the pack page
// (/store/packs/:id). A pack never installs in one step: the pack page lists
// its apps, each with its own Install button. The art link repeats the title's
// link, so it is left out of the tab order and the accessibility tree.
import { computed } from "vue";
import { RouterLink } from "vue-router";
import type { CatalogPack } from "../api";
import Button from "@/components/ui/Button.vue";
import StoreAppChip from "./StoreAppChip.vue";
import StoreArt from "./StoreArt.vue";

const props = defineProps<{ pack: CatalogPack }>();

const to = computed(() => `/store/packs/${encodeURIComponent(props.pack.id)}`);
const apps = computed(() => props.pack.apps ?? []);
</script>

<template>
  <div class="flex flex-col overflow-hidden rounded-3xl border border-border bg-card transition hover:shadow-md">
    <RouterLink
      :to="to"
      tabindex="-1"
      aria-hidden="true"
      class="grid aspect-[16/9] place-items-center overflow-hidden border-b border-border bg-muted"
    >
      <StoreArt :url="pack.illustration_url" :apps="apps" size="sm" />
    </RouterLink>
    <div class="flex flex-1 flex-col gap-2 p-5">
      <RouterLink :to="to" class="font-display text-2xl/8 tracking-tight text-foreground">{{ pack.title }}</RouterLink>
      <p v-if="pack.description" class="text-sm/6 text-muted-foreground">{{ pack.description }}</p>
      <div class="mt-auto flex items-center justify-between gap-3 pt-3">
        <div class="flex min-w-0 flex-wrap gap-1.5">
          <StoreAppChip v-for="a in apps" :key="a.id" :app="a" class="size-9 rounded-xl" />
        </div>
        <!-- Every card's button reads the same, so name the pack for a screen
             reader's list of links. -->
        <Button :as="RouterLink" :to="to" size="sm" :aria-label="`View pack: ${pack.title}`">View pack</Button>
      </div>
    </div>
  </div>
</template>
