<script setup lang="ts">
// First-run wizard, step 3 — telemetry consent (FIRST_RUN.md # Step 4,
// TELEMETRY.md). Off by default; the founding admin makes the box-wide choice
// once. Copy is taken verbatim from the spec, including the third-party
// (PostHog) disclosure the spec requires inside the "what's collected" panel.
import { ref } from "vue";
import { setTelemetryConsent } from "../auth";
import type { ApiError } from "../api";
import Button from "@/components/ui/Button.vue";

const emit = defineEmits<{ done: [] }>();

const enabled = ref(false);
const showDetails = ref(false);
const submitting = ref(false);
const error = ref("");

async function submit() {
  error.value = "";
  submitting.value = true;
  try {
    await setTelemetryConsent(enabled.value);
    emit("done");
  } catch (e) {
    error.value = (e as ApiError).message || "Couldn't save your choice";
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <form @submit.prevent="submit">
    <h2>Help improve moose</h2>
    <label class="check">
      <input v-model="enabled" type="checkbox" />
      Send anonymous usage statistics and crash reports to help improve moose.
    </label>
    <button type="button" class="link" @click="showDetails = !showDetails">
      {{ showDetails ? "Hide details" : "What does this collect?" }}
    </button>
    <div v-if="showDetails" class="hint details">
      <p>
        Anonymous usage data and crash reports are sent to moose, processed via
        PostHog (a third-party analytics provider). No identifying information
        is included:
      </p>
      <ul>
        <li>moose version and basic hardware class (CPU, memory and disk size ranges)</li>
        <li>App installs and updates for store apps, and health issues</li>
        <li>Crash reports (with file paths and personal data scrubbed)</li>
      </ul>
      <p>
        Never collected: your files or app content, user accounts, IP
        addresses, or the names of apps you add yourself. You can change this
        any time in Settings → Privacy.
      </p>
    </div>
    <Button type="submit" :disabled="submitting" class="mt-2">
      {{ submitting ? "Saving…" : "Continue" }}
    </Button>
    <p v-if="error" class="error">{{ error }}</p>
  </form>
</template>
