<script setup lang="ts">
// The service grid of the install flow (INSTALL_STEPS.md # 3): every AI
// service the app can use, in the provider data's order (popularity order),
// with My own server last. Three columns on a computer, two or one on a
// phone. The name is text on the tile, not only a logo. No search, no More,
// no Recommended badge.
//
// It is a radio group: one tile is in the tab order (the chosen one, else the
// first), and the arrow keys move the choice between tiles.
import { computed, nextTick, ref } from "vue";
import { Check } from "lucide-vue-next";
import type { AIProvider } from "../../api";
import AIProviderLogo from "../AIProviderLogo.vue";

const props = defineProps<{ services: AIProvider[]; label: string }>();
const selected = defineModel<string>({ required: true });

const tiles = ref<HTMLButtonElement[]>([]);
const focusIndex = computed(() => Math.max(0, props.services.findIndex((s) => s.id === selected.value)));

async function move(from: number, by: number) {
  const n = props.services.length;
  if (n === 0) return;
  const to = (from + by + n) % n;
  selected.value = props.services[to]!.id;
  await nextTick();
  tiles.value[to]?.focus();
}

function onKey(e: KeyboardEvent, i: number) {
  if (e.key === "ArrowRight" || e.key === "ArrowDown") {
    e.preventDefault();
    move(i, 1);
  } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
    e.preventDefault();
    move(i, -1);
  } else if (e.key === " " || e.key === "Enter") {
    e.preventDefault();
    selected.value = props.services[i]!.id;
  }
}
</script>

<template>
  <div role="radiogroup" :aria-label="label" class="grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 md:grid-cols-3">
    <button
      v-for="(s, i) in services"
      :key="s.id"
      ref="tiles"
      type="button"
      role="radio"
      :aria-checked="selected === s.id"
      :tabindex="i === focusIndex ? 0 : -1"
      :class="[
        'relative flex w-full cursor-pointer items-center gap-3 rounded-lg border bg-card px-4 py-3.5 text-left transition-colors hover:border-olive-400 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent',
        selected === s.id ? 'border-accent outline-1 -outline-offset-2 outline-accent' : 'border-border',
      ]"
      @click="selected = s.id"
      @keydown="onKey($event, i)"
    >
      <AIProviderLogo :provider="s" />
      <span class="min-w-0 flex-1 text-sm font-medium wrap-anywhere text-foreground">{{ s.name }}</span>
      <Check v-if="selected === s.id" class="size-4 shrink-0 text-accent" aria-hidden="true" />
    </button>
  </div>
</template>
