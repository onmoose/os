<script setup lang="ts">
// First-run wizard, step 1 — create the founding admin (FIRST_RUN.md # Step 2 +
// Step 2a). Appliance only: the hosted box auto-creates its admin via the
// portal-to-box SSO handshake and never reaches this step (cloud
// specs/AUTH_AND_ACCESS.md; the wizard skips straight to time zone when an admin
// already exists — Setup.vue).
//
// Recovery is on by default (FIRST_RUN.md # Step 2a). When kept on, the setup
// response carries the code once and we hold on a save-it screen until the user
// acknowledges — the brain stores only a hash, so this is the single reveal.
// When toggled off, the form shows the tradeoff copy and no code is generated.
//
// The field is the person's first name (FIRST_RUN.md # Step 2: "two fields,
// first name and password"). The box derives the Linux account name from it, so
// there is no username to type and none is shown here.
import { ref } from "vue";
import { setup } from "../auth";
import type { ApiError } from "../api";
import Button from "@/components/ui/Button.vue";

const emit = defineEmits<{ done: [] }>();

const displayName = ref("");
const password = ref("");

const recovery = ref(true);
const submitting = ref(false);
const error = ref("");
// Held after a successful setup when recovery was on: the code to show once.
const recoveryCode = ref<string | null>(null);
// Save-it screen gating (FIRST_RUN.md # Step 2a): Continue stays disabled until
// the user ticks the acknowledgement, so the single-reveal code isn't skipped past.
const acknowledged = ref(false);
const copied = ref(false);

// copyCode copies the recovery code to the clipboard. The Clipboard API needs a
// secure context — true on hosted (HTTPS) but not on the appliance dashboard
// (http://*.local) — so fall back to a hidden-textarea execCommand there.
async function copyCode() {
  if (!recoveryCode.value) return;
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(recoveryCode.value);
    } else {
      const ta = document.createElement("textarea");
      ta.value = recoveryCode.value;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
    }
    copied.value = true;
    setTimeout(() => (copied.value = false), 2000);
  } catch {
    // Clipboard blocked — the code is still on screen to copy by hand.
  }
}

async function submit() {
  error.value = "";
  submitting.value = true;
  try {
    const res = await setup(displayName.value.trim(), password.value, {
      recovery: recovery.value,
    });
    if (recovery.value && res.recovery_code) {
      recoveryCode.value = res.recovery_code; // show-once screen below
    } else {
      emit("done"); // recovery off → nothing to save, advance immediately
    }
  } catch (e) {
    error.value = (e as ApiError).message || "Setup failed";
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <form v-if="!recoveryCode" @submit.prevent="submit">
    <h2>Create your admin account</h2>
    <p class="hint">This is the first account on the box — the administrator.</p>

    <label>
      Your first name
      <input v-model="displayName" autocomplete="given-name" required autofocus />
    </label>
    <label>
      Password
      <input v-model="password" type="password" autocomplete="new-password" required />
    </label>

    <label class="check">
      <input v-model="recovery" type="checkbox" />
      Save a recovery code (recommended)
    </label>
    <p v-if="!recovery" class="hint warn">
      You won't be able to recover your account if you forget your password.
      Continue without a recovery code?
    </p>

    <Button type="submit" :disabled="submitting" class="mt-2">
      {{ submitting ? "Creating…" : "Continue" }}
    </Button>
    <p v-if="error" class="error">{{ error }}</p>
  </form>

  <form v-else @submit.prevent="emit('done')">
    <h2>Save your recovery code</h2>
    <p class="hint">
      Write this down or take a photo. If you forget your password, this code is
      the only way back in — we store only a hash, never the code itself.
    </p>
    <div class="recovery-row">
      <div class="recovery">{{ recoveryCode }}</div>
      <Button type="button" variant="secondary" size="sm" @click="copyCode">
        {{ copied ? "Copied" : "Copy" }}
      </Button>
    </div>

    <label class="check">
      <input v-model="acknowledged" type="checkbox" />
      I have saved this recovery code
    </label>

    <Button type="submit" :disabled="!acknowledged" class="mt-2">Continue</Button>
  </form>
</template>
