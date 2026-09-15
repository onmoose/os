<script setup lang="ts">
// Settings → Account: the signed-in user's own identity and password. Extracted
// from the old single-page SettingsView when Settings became a left-nav shell
// (SettingsLayout.vue).
//
// Self-service password change (AUTH.md # Password lifecycle). No elevation
// window: supplying the current password IS the verification step, enforced
// server-side via PAM. On success the brain revokes our session, so
// changeMyPassword forces a local logout and App.vue drops to the login screen.
// This component unmounts, so we only ever reset submitting on the error path.
import { ref } from "vue";
import { api, type ApiError } from "@/api";
import Button from "@/components/ui/Button.vue";
import { changeMyPassword, useAuth } from "@/auth";

const { currentUser } = useAuth();

const showPwForm = ref(false);
const currentPw = ref("");
const newPw = ref("");
const pwError = ref("");
const pwSubmitting = ref(false);

async function submitPasswordChange() {
  pwError.value = "";
  pwSubmitting.value = true;
  try {
    await changeMyPassword(currentPw.value, newPw.value);
  } catch (e) {
    const ae = e as ApiError;
    pwError.value = ae.status === 401 ? "Incorrect password." : ae.message || "Could not change password.";
    pwSubmitting.value = false;
  }
}

function cancelPwChange() {
  showPwForm.value = false;
  currentPw.value = "";
  newPw.value = "";
  pwError.value = "";
}
</script>

<template>
  <section class="space-y-3">
    <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Account</h2>
    <div class="space-y-3 rounded-xl border border-border bg-card px-4 py-3">
      <div class="flex items-center justify-between gap-4">
        <div class="min-w-0">
          <div class="text-sm font-medium">{{ currentUser?.username }}</div>
          <div class="text-xs capitalize text-muted-foreground">{{ currentUser?.role }}</div>
        </div>
        <Button v-if="!showPwForm" variant="secondary" size="sm" @click="showPwForm = true">
          Change password
        </Button>
      </div>
      <form v-if="showPwForm" class="space-y-3 border-t border-border pt-3" @submit.prevent="submitPasswordChange">
        <label class="block space-y-1">
          <span class="text-xs text-muted-foreground">Current password</span>
          <input
            v-model="currentPw"
            type="password"
            autocomplete="current-password"
            required
            class="w-full rounded-lg border border-border bg-background px-3 py-1.5 text-sm outline-none focus:border-accent"
          />
        </label>
        <label class="block space-y-1">
          <span class="text-xs text-muted-foreground">New password</span>
          <input
            v-model="newPw"
            type="password"
            autocomplete="new-password"
            required
            class="w-full rounded-lg border border-border bg-background px-3 py-1.5 text-sm outline-none focus:border-accent"
          />
        </label>
        <p v-if="pwError" class="text-xs text-destructive">{{ pwError }}</p>
        <div class="flex justify-end gap-2">
          <Button type="button" variant="ghost" size="sm" @click="cancelPwChange">Cancel</Button>
          <Button type="submit" size="sm" :disabled="pwSubmitting || !currentPw || !newPw">
            {{ pwSubmitting ? "Changing…" : "Change password" }}
          </Button>
        </div>
      </form>
    </div>
  </section>
</template>
