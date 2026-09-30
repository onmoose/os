<script setup lang="ts">
// The first view of an optional email or AI step when the user has no saved
// account (INSTALL_STEPS.md # 4): two choices, "Not now" picked in advance
// and "Set up email" (or "Set up an AI service"), which opens the service
// grid on Continue. Under them, one quiet line. When the manifest gains a
// `recommends.without` sentence (step 4), it goes here.
import { ArrowRight } from "lucide-vue-next";

defineProps<{ label: string; setupLabel: string }>();
const choice = defineModel<"later" | "setup">({ required: true });
</script>

<template>
  <div class="space-y-3">
    <fieldset :aria-label="label" class="space-y-2">
      <label
        v-for="opt in (['later', 'setup'] as const)"
        :key="opt"
        class="relative flex cursor-pointer items-center gap-3 rounded-lg border border-border bg-card px-4 py-3 text-sm hover:border-olive-400 has-checked:border-accent has-checked:outline-1 has-checked:-outline-offset-2 has-checked:outline-accent"
      >
        <input v-model="choice" type="radio" name="optional-offer" :value="opt" class="accent-accent" />
        <span v-if="opt === 'later'">Not now</span>
        <span v-else class="inline-flex items-center gap-1.5">{{ setupLabel }} <ArrowRight class="size-4" aria-hidden="true" /></span>
      </label>
    </fieldset>
    <p class="text-sm text-muted-foreground">You can set it up later in the app's settings.</p>
  </div>
</template>
