<script setup lang="ts">
// Recovery-code redemption (AUTH.md # Using the recovery code). A public screen,
// reached while logged out from the login screen's "Forgot password?" link —
// App.vue renders it directly when the path is /recover and there's no session.
//
// One form (username + recovery code + new password) maps to the brain's single
// POST /api/v1/recover call. The real code is 24 hex chars (what Setup.vue shows
// at first-run), so the field is plain text — no XXXX-XXXX mask. On success the
// brain resets the password, consumes the old code, and returns a fresh one,
// which we show exactly once before sending the user back to sign in.
import { ref } from "vue";
import { RouterLink, useRouter } from "vue-router";
import { redeemRecoveryCode } from "./auth";
import type { ApiError } from "./api";
import Button from "@/components/ui/Button.vue";
import Heading from "@/components/ui/Heading.vue";

const router = useRouter();

const username = ref("");
const code = ref("");
const newPassword = ref("");
const submitting = ref(false);
const error = ref("");

// Set once recovery succeeds — flips the view to the show-once code screen.
const newCode = ref<string | null>(null);
const saved = ref(false);
const copied = ref(false);

async function submit() {
  error.value = "";
  submitting.value = true;
  try {
    newCode.value = await redeemRecoveryCode(username.value.trim(), code.value.trim(), newPassword.value);
  } catch (e) {
    // 401 covers both an unknown username and a wrong code — stay generic so we
    // never confirm whether the username exists. Surface anything else (e.g. a
    // 502 if host-agent is unreachable) so the user knows to retry.
    const ae = e as ApiError;
    error.value = ae.status === 401 ? "Recovery code incorrect." : ae.message || "Recovery failed.";
  } finally {
    submitting.value = false;
  }
}

async function copyCode() {
  if (!newCode.value) return;
  try {
    await navigator.clipboard.writeText(newCode.value);
    copied.value = true;
  } catch {
    // Clipboard can be unavailable on an insecure context (.local is HTTP-only);
    // the code is on screen to copy by hand.
  }
}

function done() {
  // Recovery terminates at the login screen — the user signs in with the new
  // password. While logged out, "/" renders Login (App.vue's auth gate).
  router.push("/");
}
</script>

<template>
  <main class="auth">
    <Heading :level="1" class="mb-8 text-center">moose</Heading>

    <!-- Phase 1: redeem the recovery code -->
    <form v-if="!newCode" class="card" @submit.prevent="submit">
      <h2>Recover your account</h2>
      <p class="hint">Enter your recovery code to set a new password.</p>
      <label>
        Username
        <input v-model="username" autocomplete="username" required autofocus />
      </label>
      <label>
        Recovery code
        <input v-model="code" autocomplete="off" spellcheck="false" required />
      </label>
      <label>
        New password
        <input v-model="newPassword" type="password" autocomplete="new-password" required />
      </label>
      <Button type="submit" :disabled="submitting" class="mt-2">
        {{ submitting ? "Resetting…" : "Reset password" }}
      </Button>
      <p v-if="error" class="error">{{ error }}</p>
      <RouterLink to="/" class="back-link">← Back to sign in</RouterLink>
    </form>

    <!-- Phase 2: show the fresh recovery code exactly once -->
    <form v-else class="card" @submit.prevent="done">
      <h2>Save your new recovery code</h2>
      <p class="hint">
        Your password is reset. Write this new code down — your old one no longer works, and we
        don't store the code itself, just a hash.
      </p>
      <div class="recovery">{{ newCode }}</div>
      <Button type="button" variant="secondary" size="sm" class="self-start" @click="copyCode">
        {{ copied ? "Copied" : "Copy" }}
      </Button>
      <label class="ack">
        <input type="checkbox" v-model="saved" />
        I've saved this recovery code
      </label>
      <Button type="submit" :disabled="!saved" class="mt-2">Continue to sign in</Button>
    </form>
  </main>
</template>

<style>
/* Recovery-specific styles (auth base styles live in style.css). Colors on
   .back-link / .ack come from the olive semantic tokens; the Copy button is now
   the shared <Button variant="secondary">, not a .auth CSS rule. */
.auth .back-link { align-self: center; color: var(--color-muted-foreground); font-size: 0.85rem; text-decoration: none; margin-top: 0.25rem; }
.auth .back-link:hover { color: var(--color-foreground); }
.auth .ack { flex-direction: row; align-items: center; gap: 0.5rem; font-size: 0.85rem; color: var(--color-foreground); }
</style>
