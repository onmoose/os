<script setup lang="ts">
// What the App page and the install pages say when an app does not fit this
// box's memory or CPU (#599). Nothing is drawn when it fits.
//
// Tight is a warning: the box has enough memory in total, but not next to the
// apps that are running. It names them, largest first, and says to stop or
// uninstall one. Too big is an error: this box can't run the app as it is.
// Both carry an "Upgrade your box" link on a hosted box, for its owner only
// (upgradeBoxURL); on an appliance the fix is hardware, so there is no link.
import { computed } from "vue";
import { TriangleAlert } from "lucide-vue-next";
import type { AppFit } from "../api";
import { upgradeBoxURL } from "../auth";
import { othersLine, runningLine, tightText, tooBigText } from "../fit";

const props = defineProps<{ fit?: AppFit | null }>();

const tooBig = computed(() => props.fit?.state === "too-big");
const tight = computed(() => props.fit?.state === "tight");
const running = computed(() => props.fit?.running ?? []);
// Other people's own apps are counted, never named (canSee in the brain).
const others = computed(() => props.fit?.other_running ?? 0);
const upgradeURL = computed(() => upgradeBoxURL());
</script>

<template>
  <div
    v-if="fit && (tooBig || tight)"
    class="space-y-2 rounded-lg border px-4 py-3 text-sm"
    :class="tooBig ? 'border-destructive/40 bg-destructive/10' : 'border-warning/40 bg-warning/10'"
    :role="tooBig ? 'alert' : 'status'"
  >
    <p class="flex gap-2">
      <TriangleAlert
        class="mt-0.5 size-4 shrink-0"
        :class="tooBig ? 'text-destructive' : 'text-warning'"
        aria-hidden="true"
      />
      <span>{{ tooBig ? tooBigText(fit) : tightText(fit) }}</span>
    </p>
    <template v-if="tight && running.length + others > 0">
      <p>Running now:</p>
      <ul class="list-disc space-y-0.5 pl-5">
        <li v-for="r in running" :key="r.instance_id">{{ runningLine(r) }}</li>
        <li v-if="others > 0">{{ othersLine(others) }}</li>
      </ul>
      <p v-if="running.length > 0">To make room, stop or uninstall one of them.</p>
    </template>
    <a
      v-if="upgradeURL"
      :href="upgradeURL"
      target="_blank"
      rel="noopener"
      class="inline-block font-medium text-accent hover:underline"
    >
      Upgrade your box
    </a>
  </div>
</template>
