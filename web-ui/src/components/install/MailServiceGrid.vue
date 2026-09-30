<script setup lang="ts">
// The email service grid of the install flow (INSTALL_STEPS.md # 4). First
// the two accounts people already have, Gmail and iCloud, which say "For
// personal use". Then, under "Sending services", the services made for
// sending app email. "Custom server" is last. The order is the preset table's
// (internal/mailpreset), which lists them this way.
//
// It is one radio group across both headings: one tile is in the tab order
// (the chosen one, else the first), and the arrow keys move the choice.
import { computed, nextTick, ref } from "vue";
import { Check } from "lucide-vue-next";
import type { MailPreset } from "../../api";
import MailProviderLogo from "../MailProviderLogo.vue";

const props = defineProps<{ presets: MailPreset[]; label: string }>();
const selected = defineModel<string>({ required: true });

const personal = computed(() => props.presets.filter((p) => p.personal));
const services = computed(() => props.presets.filter((p) => !p.personal && p.id !== "custom"));
const custom = computed(() => props.presets.filter((p) => p.id === "custom"));
// order is every tile in the order the keyboard walks them.
const order = computed(() => [...personal.value, ...services.value, ...custom.value]);

const tiles = ref<Record<string, HTMLButtonElement>>({});
const focusId = computed(() =>
  order.value.some((p) => p.id === selected.value) ? selected.value : (order.value[0]?.id ?? ""),
);

function setTile(id: string, el: unknown) {
  if (el instanceof HTMLButtonElement) tiles.value[id] = el;
}

async function onKey(e: KeyboardEvent, id: string) {
  const by = e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : 0;
  if (e.key === " " || e.key === "Enter") {
    e.preventDefault();
    selected.value = id;
    return;
  }
  if (by === 0) return;
  e.preventDefault();
  const n = order.value.length;
  const i = order.value.findIndex((p) => p.id === id);
  const next = order.value[(i + by + n) % n]!;
  selected.value = next.id;
  await nextTick();
  tiles.value[next.id]?.focus();
}

const tileClass = (on: boolean) => [
  "relative flex w-full cursor-pointer items-center gap-3 rounded-lg border bg-card px-4 py-3.5 text-left transition-colors hover:border-olive-400 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent",
  on ? "border-accent outline-1 -outline-offset-2 outline-accent" : "border-border",
];
const gridClass = "grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 md:grid-cols-3";
</script>

<template>
  <div role="radiogroup" :aria-label="label" class="space-y-6">
    <template v-for="group in [personal, services, custom]" :key="group[0]?.id ?? 'empty'">
      <div v-if="group.length > 0" class="space-y-3">
        <h3 v-if="group === services" class="text-sm font-medium text-muted-foreground">Sending services</h3>
        <div :class="gridClass">
          <button
            v-for="p in group"
            :key="p.id"
            :ref="(el) => setTile(p.id, el)"
            type="button"
            role="radio"
            :aria-checked="selected === p.id"
            :tabindex="p.id === focusId ? 0 : -1"
            :class="tileClass(selected === p.id)"
            @click="selected = p.id"
            @keydown="onKey($event, p.id)"
          >
            <MailProviderLogo :id="p.id" :label="p.label" size="icon" />
            <span class="min-w-0 flex-1">
              <span class="block text-sm font-medium wrap-anywhere text-foreground">{{ p.label }}</span>
              <span v-if="p.personal" class="block text-sm text-muted-foreground">For personal use</span>
            </span>
            <Check v-if="selected === p.id" class="size-4 shrink-0 text-accent" aria-hidden="true" />
          </button>
        </div>
      </div>
    </template>
  </div>
</template>
