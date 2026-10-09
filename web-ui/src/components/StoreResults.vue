<script setup lang="ts">
// StoreResults: the store's filtered views: one category's apps, or what a
// search found. A search shows the apps first, then the packs as cards that
// open the pack page. The "Apps" and "Packs" headings show only when both have
// results; with one kind, the heading would say nothing the cards do not.
//
// It shows its own loading and error lines, so the landing around it (and the
// search box above it) stays put while a request runs.
import { computed } from "vue";
import { SearchX } from "lucide-vue-next";
import type { CatalogEntry, CatalogPack } from "../api";
import Button from "@/components/ui/Button.vue";
import StoreAppCard from "./StoreAppCard.vue";
import StorePackCard from "./StorePackCard.vue";

const props = defineProps<{
  mode: "category" | "search";
  apps: CatalogEntry[];
  packs: CatalogPack[];
  // label is the category view's heading.
  label: string;
  loading: boolean;
  error: string | null;
}>();
defineEmits<{ clear: [] }>();

const both = computed(() => props.apps.length > 0 && props.packs.length > 0);
const nothing = computed(() => props.apps.length === 0 && props.packs.length === 0);
</script>

<template>
  <section class="space-y-4">
    <h3 v-if="mode === 'category'" class="text-base font-semibold text-foreground">{{ label }}</h3>

    <p v-if="loading" class="text-sm text-muted-foreground">Loading…</p>
    <p v-else-if="error !== null" class="text-sm text-destructive">Couldn't load the catalog. {{ error }}</p>

    <div v-else-if="nothing" class="rounded-2xl border border-dashed border-border py-16 text-center">
      <SearchX class="mx-auto size-8 text-muted-foreground" aria-hidden="true" />
      <template v-if="mode === 'category'">
        <h3 class="mt-3 text-sm font-semibold text-foreground">No apps in this category</h3>
        <Button variant="secondary" size="sm" class="mt-4" @click="$emit('clear')">Back to recommended</Button>
      </template>
      <template v-else>
        <h3 class="mt-3 text-sm font-semibold text-foreground">Nothing matches your search</h3>
        <p class="mt-1 text-sm text-muted-foreground">Try a different search term or category.</p>
        <Button variant="secondary" size="sm" class="mt-4" @click="$emit('clear')">Clear search</Button>
      </template>
    </div>

    <template v-else>
      <template v-if="apps.length">
        <h4 v-if="both" class="text-sm font-semibold text-muted-foreground">Apps</h4>
        <div class="grid grid-cols-2 gap-x-6 gap-y-8 sm:grid-cols-3 lg:grid-cols-4">
          <StoreAppCard v-for="c in apps" :key="c.id" :app="c" />
        </div>
      </template>
      <template v-if="packs.length">
        <h4 v-if="both" class="pt-4 text-sm font-semibold text-muted-foreground">Packs</h4>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <StorePackCard v-for="p in packs" :key="p.id" :pack="p" />
        </div>
      </template>
    </template>
  </section>
</template>
