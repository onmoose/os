<script setup lang="ts">
// First-run wizard shell (FIRST_RUN.md # Phase 2). Renders one step at a time
// and advances on each step's `done` event. The step list is data-driven on
// purpose: this C4 slice ships the trimmed hosted set, and the same steps
// serve the appliance too. The appliance's network/storage and the enrollment
// step (FIRST_RUN.md # Step 1 / Step 5) are spliced into this list by a later
// change (B4) per profile, without touching the shell.
//
// Telemetry consent (FIRST_RUN.md # Step 4) is disabled for now — we collect
// nothing yet, so the step is omitted rather than shown with no backing
// pipeline. TelemetryStep.vue is kept for when telemetry is actually wired up.
import { ref, computed, markRaw, type Component } from "vue";
import { useAuth } from "./auth";
import AdminStep from "./setup/AdminStep.vue";
import TimezoneStep from "./setup/TimezoneStep.vue";
import DoneStep from "./setup/DoneStep.vue";
import Heading from "@/components/ui/Heading.vue";

const { hasUsers } = useAuth();

const steps: Component[] = [
  markRaw(AdminStep),
  markRaw(TimezoneStep),
  markRaw(DoneStep),
];

// Resume: if an admin already exists, the wizard was interrupted after POST
// /setup (the account is made, the box just isn't first-run-complete yet —
// FIRST_RUN.md # Phase 3). Skip the admin step and pick up at time zone.
const current = ref(hasUsers.value ? 1 : 0);
const step = computed(() => steps[current.value]!);

function next() {
  if (current.value < steps.length - 1) current.value++;
}
</script>

<template>
  <main class="auth">
    <Heading :level="1" class="mb-8 text-center">moose</Heading>
    <component :is="step" @done="next" />
  </main>
</template>
