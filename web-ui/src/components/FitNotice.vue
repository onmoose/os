<script setup lang="ts">
// What the App page and the install pages say when an app does not fit this
// box's memory or CPU (#599). Nothing is drawn when it fits. A yellow alert
// with a title and a description (Tailwind UI # Alerts # With description).
// On the App page it is its own block above the price panel, in the right
// column.
//
// Tight: the box has enough memory in total, but not next to the apps that
// are running. It names them, largest first, and says to stop or uninstall
// one. Too big: this app can't run on this box as it is. Both carry an
// "Upgrade your box" link on a hosted box, for its owner only
// (upgradeBoxURL); on an appliance the fix is hardware, so there is no link.
import { computed } from "vue";
import type { AppFit } from "../api";
import { upgradeBoxURL } from "../auth";
import WarningAlert from "./WarningAlert.vue";
import { TIGHT_TITLE, TOO_BIG_TITLE, runningText, tightText, tooBigDetails } from "../fit";

const props = defineProps<{ fit?: AppFit | null }>();

const tooBig = computed(() => props.fit?.state === "too-big");
const tight = computed(() => props.fit?.state === "tight");
// Other people's own apps are counted, never named (canSee in the brain).
const running = computed(() => runningText(props.fit?.running ?? [], props.fit?.other_running ?? 0));
const upgradeURL = computed(() => upgradeBoxURL());
</script>

<template>
  <WarningAlert
    v-if="fit && (tooBig || tight)"
    :title="tooBig ? TOO_BIG_TITLE : TIGHT_TITLE"
    :role="tooBig ? 'alert' : 'status'"
  >
    <p>{{ tooBig ? tooBigDetails(fit) : tightText(fit) }}</p>
    <p v-if="tight && running">{{ running }}</p>
    <a
      v-if="upgradeURL"
      :href="upgradeURL"
      target="_blank"
      rel="noopener"
      class="inline-block font-medium text-yellow-800 underline hover:text-yellow-900"
    >
      Upgrade your box
    </a>
  </WarningAlert>
</template>
