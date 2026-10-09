<script setup lang="ts">
// StoreCategories: the landing's categories section, drawn as the other store
// surface draws it. One tab per authored group. The active tab shows at least
// four apps: the group's hand-picked ones first, then the category's other apps
// in listing order. "Show more" appears only when the category has five apps or
// more, so it always reveals something, and it shows every app in the category.
import { computed, ref, watch } from "vue";
import type { CatalogEntry, HomeSection } from "../api";
import Heading from "@/components/ui/Heading.vue";
import Button from "@/components/ui/Button.vue";
import StoreAppCard from "./StoreAppCard.vue";

// CATEGORY_MIN is how many apps a category shows before "Show more".
const CATEGORY_MIN = 4;

const props = defineProps<{
  section: HomeSection;
  // apps is every app the store lists (GET /catalog), for the category's apps
  // beyond the hand-picked ones. Empty while it loads: the picked apps show.
  apps: CatalogEntry[];
}>();

const groups = computed(() => (props.section.groups ?? []).filter((g) => (g.apps ?? []).length > 0));
const activeId = ref<string | null>(null);
const all = ref(false);
const active = computed(() => groups.value.find((g) => g.category === activeId.value) ?? groups.value[0]);
watch(groups, () => {
  if (!groups.value.some((g) => g.category === activeId.value)) activeId.value = null;
});

function pick(id: string) {
  activeId.value = id;
  all.value = false;
}

const ordered = computed<CatalogEntry[]>(() => {
  const g = active.value;
  if (!g) return [];
  const picked = g.apps ?? [];
  const ids = new Set(picked.map((a) => a.id));
  const rest = props.apps.filter((a) => !ids.has(a.id) && (a.categories ?? []).includes(g.category));
  return [...picked, ...rest];
});
const shown = computed(() =>
  all.value ? ordered.value : ordered.value.slice(0, Math.max(CATEGORY_MIN, active.value?.apps?.length ?? 0)),
);
const showToggle = computed(
  () => ordered.value.length > CATEGORY_MIN && (all.value || ordered.value.length > shown.value.length),
);
</script>

<template>
  <section v-if="groups.length" class="flex flex-col gap-6">
    <Heading :level="2">{{ section.title || "Categories" }}</Heading>
    <div class="flex gap-2 overflow-x-auto">
      <button
        v-for="g in groups"
        :key="g.category"
        type="button"
        :aria-pressed="g === active"
        class="shrink-0 cursor-pointer whitespace-nowrap rounded-full border px-3.5 py-1 text-sm font-medium transition-colors"
        :class="
          g === active
            ? 'border-accent bg-accent text-accent-foreground'
            : 'border-border bg-card text-muted-foreground hover:bg-muted hover:text-foreground'
        "
        @click="pick(g.category)"
      >
        {{ g.label }}
      </button>
    </div>
    <div class="flex flex-col items-center gap-6">
      <div class="grid w-full grid-cols-2 gap-x-6 gap-y-10 sm:grid-cols-3 lg:grid-cols-4">
        <StoreAppCard v-for="c in shown" :key="c.id" :app="c" />
      </div>
      <Button v-if="showToggle" variant="secondary" size="sm" @click="all = !all">
        {{ all ? "Show less" : "Show more" }}
      </Button>
    </div>
  </section>
</template>
