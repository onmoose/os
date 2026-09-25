<script lang="ts">
export type Option = { id: string; label: string; description?: string };
</script>

<script setup lang="ts">
// OptionCards: a grid of selectable cards for the install setup page (email
// accounts, AI providers, models). The card is the Tailwind Plus horizontal
// link card, made a button. Under it sits the divider-with-button, a "More"
// that shows every option plus a search box. The first `featured` options show
// before that (the plan's "about five, plus search").
//
// The parent owns selection. `selected` only says which cards to draw as
// chosen; a click emits `pick` with the option id. With `allowTyped`, a search
// that matches no option offers the typed text as a pick of its own, so a
// model missing from our list never blocks the user.
import { computed, ref } from "vue";
import { Check, ChevronDown, ChevronUp, Search } from "lucide-vue-next";

const props = withDefaults(
  defineProps<{
    options: Option[];
    selected: string[];
    // label names the list for screen readers ("Email account", "Model").
    label: string;
    featured?: number;
    // multiple: cards toggle on their own (AI providers). Otherwise the grid is
    // a single choice (radio group).
    multiple?: boolean;
    allowTyped?: boolean;
  }>(),
  { featured: 5, multiple: false, allowTyped: false },
);

const emit = defineEmits<{ pick: [id: string] }>();

const expanded = ref(false);
const query = ref("");

// Collapsed: the featured options, plus any chosen option past them, so the
// current choice is never hidden behind "More".
const visible = computed<Option[]>(() => {
  if (expanded.value) {
    const q = query.value.trim().toLowerCase();
    if (!q) return props.options;
    return props.options.filter(
      (o) =>
        o.label.toLowerCase().includes(q) ||
        o.id.toLowerCase().includes(q) ||
        (o.description ?? "").toLowerCase().includes(q),
    );
  }
  const head = props.options.slice(0, props.featured);
  const extra = props.options.filter((o, i) => i >= props.featured && props.selected.includes(o.id));
  return [...head, ...extra];
});

// With allowTyped the button shows even for a short list, because the search
// box behind it is also where a typed value goes.
const hasMore = computed(() => props.options.length > props.featured || props.allowTyped);

// A typed value is offered when the search has text that is not already an
// option id.
const typed = computed(() => {
  const q = query.value.trim();
  if (!props.allowTyped || !expanded.value || !q) return "";
  return props.options.some((o) => o.id === q) ? "" : q;
});

function toggleMore() {
  expanded.value = !expanded.value;
  if (!expanded.value) query.value = "";
}

function isSelected(id: string) {
  return props.selected.includes(id);
}

const cardClass =
  "relative flex w-full cursor-pointer items-center gap-3 rounded-lg border bg-card px-4 py-3.5 text-left " +
  "transition-colors hover:border-olive-400 focus-visible:outline-2 focus-visible:outline-offset-2 " +
  "focus-visible:outline-accent";
</script>

<template>
  <div class="space-y-3">
    <!-- Search, ported from the Tailwind Plus combobox input. Shown only once
         "More" is open: five cards need no search. -->
    <div v-if="expanded" class="grid grid-cols-1">
      <input
        v-model="query"
        type="search"
        :aria-label="`Search ${label.toLowerCase()}`"
        :placeholder="allowTyped ? 'Search, or type a name' : 'Search'"
        class="col-start-1 row-start-1 block w-full rounded-md bg-card py-1.5 pr-3 pl-9 text-base text-foreground outline-1 -outline-offset-1 outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 focus:outline-accent sm:text-sm/6"
      />
      <Search
        class="pointer-events-none col-start-1 row-start-1 ml-3 size-4 self-center text-muted-foreground"
        aria-hidden="true"
      />
    </div>

    <div
      :role="multiple ? 'group' : 'radiogroup'"
      :aria-label="label"
      class="grid grid-cols-1 gap-3 sm:grid-cols-2"
    >
      <button
        v-for="o in visible"
        :key="o.id"
        type="button"
        :role="multiple ? undefined : 'radio'"
        :aria-checked="multiple ? undefined : isSelected(o.id)"
        :aria-pressed="multiple ? isSelected(o.id) : undefined"
        :class="[cardClass, isSelected(o.id) ? 'border-accent outline-1 -outline-offset-2 outline-accent' : 'border-border']"
        @click="emit('pick', o.id)"
      >
        <span class="flex size-10 shrink-0 items-center justify-center">
          <slot name="icon" :option="o" />
        </span>
        <span class="min-w-0 flex-1">
          <span class="block truncate text-sm font-medium text-foreground">{{ o.label }}</span>
          <span v-if="o.description" class="block truncate text-sm text-muted-foreground">{{ o.description }}</span>
        </span>
        <Check v-if="isSelected(o.id)" class="size-4 shrink-0 text-accent" aria-hidden="true" />
      </button>

      <button
        v-if="typed"
        type="button"
        :class="[cardClass, 'border-dashed border-border']"
        @click="emit('pick', typed)"
      >
        <span class="min-w-0 flex-1">
          <span class="block truncate text-sm font-medium text-foreground">Use "{{ typed }}"</span>
          <span class="block truncate text-sm text-muted-foreground">Not in the list. You can still use it.</span>
        </span>
      </button>

      <!-- An action card at the end of the grid, like "Add an account". -->
      <slot name="extra" />
    </div>

    <p v-if="expanded && visible.length === 0 && !typed" class="text-sm text-muted-foreground">
      Nothing matches "{{ query }}".
    </p>

    <!-- Divider with a button (Tailwind Plus layout/dividers). -->
    <div v-if="hasMore" class="flex items-center">
      <div aria-hidden="true" class="w-full border-t border-border" />
      <div class="relative flex justify-center">
        <button
          type="button"
          class="inline-flex cursor-pointer items-center gap-x-1.5 rounded-full bg-card px-3 py-1.5 text-sm font-semibold whitespace-nowrap text-foreground shadow-xs inset-ring inset-ring-border hover:bg-muted"
          :aria-expanded="expanded"
          @click="toggleMore"
        >
          <component :is="expanded ? ChevronUp : ChevronDown" class="-ml-1 size-4 text-muted-foreground" aria-hidden="true" />
          {{ expanded ? "Fewer" : "More" }}
        </button>
      </div>
      <div aria-hidden="true" class="w-full border-t border-border" />
    </div>
  </div>
</template>
