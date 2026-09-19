<script setup lang="ts">
// Shared display heading — Instrument Serif (font-display), the Oatmeal heading
// idiom, written fresh from Tailwind utilities. `level` sets both the semantic
// tag (h1/h2/h3) and the size; `tracking-tight` + the display serif give the
// calm, branded look shared with cloud. Colour from the olive tokens.
import { computed, type HTMLAttributes } from "vue";
import { cn } from "@/lib/utils";

const props = withDefaults(
  defineProps<{ level?: 1 | 2 | 3; class?: HTMLAttributes["class"] }>(),
  { level: 2 },
);

const sizes: Record<NonNullable<typeof props.level>, string> = {
  // Level 1 is the hero/wordmark size (the auth-screen "moose" wordmark).
  1: "text-[2.75rem] leading-none",
  2: "text-2xl/8",
  3: "text-xl/7",
};

const classes = computed(() =>
  cn("font-display tracking-tight text-foreground", sizes[props.level], props.class),
);
</script>

<template>
  <component :is="`h${props.level}`" :class="classes">
    <slot />
  </component>
</template>
