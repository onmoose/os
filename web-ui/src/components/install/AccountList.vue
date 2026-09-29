<script lang="ts">
export type AccountRow = { id: string; label: string; detail?: string };
</script>

<script setup lang="ts">
// The B layout of the install flow (INSTALL_STEPS.md # 3 and # 4): the
// user's saved accounts an app can use, each with its service's logo (the
// #logo slot). Picking an account also picks the service. A link under the
// list opens another service ("Use a different AI service"), and an optional
// need also offers not using one ("Don't send email").
//
// It is a radio group, with the arrow keys moving the choice.
import { computed, nextTick, ref } from "vue";
import { ArrowRight, Check, CircleSlash } from "lucide-vue-next";

const props = defineProps<{
  rows: AccountRow[];
  label: string;
  // noneLabel, when set, adds the "not used" choice, whose value is "".
  noneLabel?: string;
  otherLabel: string;
}>();
const selected = defineModel<string>({ required: true });
const emit = defineEmits<{ other: [] }>();

const NONE = "";
const ids = computed(() => [...props.rows.map((r) => r.id), ...(props.noneLabel ? [NONE] : [])]);
const items = ref<HTMLButtonElement[]>([]);
const noneItem = ref<HTMLButtonElement | null>(null);
const focusIndex = computed(() => Math.max(0, ids.value.indexOf(selected.value)));

async function onKey(e: KeyboardEvent, i: number) {
  const by = e.key === "ArrowDown" || e.key === "ArrowRight" ? 1 : e.key === "ArrowUp" || e.key === "ArrowLeft" ? -1 : 0;
  if (by === 0) return;
  e.preventDefault();
  const n = ids.value.length;
  const to = (i + by + n) % n;
  selected.value = ids.value[to]!;
  await nextTick();
  (to < props.rows.length ? items.value[to] : noneItem.value)?.focus();
}

const rowClass = (on: boolean) => [
  "relative flex w-full cursor-pointer items-center gap-3 rounded-lg border bg-card px-4 py-3 text-left transition-colors hover:border-olive-400 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent",
  on ? "border-accent outline-1 -outline-offset-2 outline-accent" : "border-border",
];
</script>

<template>
  <div class="space-y-4">
    <div role="radiogroup" :aria-label="label" class="space-y-2">
      <button
        v-for="(r, i) in rows"
        :key="r.id"
        ref="items"
        type="button"
        role="radio"
        :aria-checked="selected === r.id"
        :tabindex="i === focusIndex ? 0 : -1"
        :class="rowClass(selected === r.id)"
        @click="selected = r.id"
        @keydown="onKey($event, i)"
      >
        <slot name="logo" :row="r" />
        <span class="min-w-0 flex-1">
          <span class="block text-sm font-medium wrap-anywhere text-foreground">{{ r.label }}</span>
          <span v-if="r.detail" class="block text-sm text-muted-foreground">{{ r.detail }}</span>
        </span>
        <Check v-if="selected === r.id" class="size-4 shrink-0 text-accent" aria-hidden="true" />
      </button>
      <button
        v-if="noneLabel"
        ref="noneItem"
        type="button"
        role="radio"
        :aria-checked="selected === NONE"
        :tabindex="rows.length === focusIndex ? 0 : -1"
        :class="rowClass(selected === NONE)"
        @click="selected = NONE"
        @keydown="onKey($event, rows.length)"
      >
        <span class="flex size-10 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <CircleSlash class="size-5 stroke-[1.5]" aria-hidden="true" />
        </span>
        <span class="min-w-0 flex-1 text-sm font-medium text-foreground">{{ noneLabel }}</span>
        <Check v-if="selected === NONE" class="size-4 shrink-0 text-accent" aria-hidden="true" />
      </button>
    </div>
    <div class="border-t border-border pt-4">
      <button
        type="button"
        class="inline-flex cursor-pointer items-center gap-1.5 text-sm font-medium text-accent hover:underline"
        @click="emit('other')"
      >
        {{ otherLabel }} <ArrowRight class="size-4" aria-hidden="true" />
      </button>
    </div>
  </div>
</template>
