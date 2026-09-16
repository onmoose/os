<script setup lang="ts">
// App is the auth-aware root. It picks one of these states from auth:
//   - bootstrapping (briefly, while /auth/state + /me settle)
//   - Setup (the first-run wizard — runs until first_run_complete, FIRST_RUN.md
//     # Phase 2; gated on the marker, not has_users, so a half-finished wizard
//     resumes rather than dropping the user onto the dashboard)
//   - the signed-in shell (AppShell + routed views)
//   - Recover (logged out, at /recover — the public recovery-code flow)
//   - Login (appliance, has users, no active session)
// On the hosted profile there is no login or setup page: an unauthenticated
// visitor is bounced to the onmoose.network portal, which signs them back in via
// the portal-to-box SSO handshake (cloud specs/AUTH_AND_ACCESS.md). Any 401 from
// a later API call drops currentUser via the handler wired in auth.ts, which
// flips us back to Login (appliance) or the portal (hosted) without a route change.
import { onMounted, watchEffect } from "vue";
import { useRoute } from "vue-router";
import { bootstrap, useAuth, isHosted, redirectToPortal } from "./auth";
import Login from "./Login.vue";
import Setup from "./Setup.vue";
import RecoverView from "./RecoverView.vue";
import AppShell from "./components/AppShell.vue";

const { currentUser, hasUsers, firstRunComplete, booted } = useAuth();
// The recovery screen is the one logged-out destination that isn't Login. The
// router-view never mounts while logged out (it lives in AppShell), so App.vue
// renders RecoverView directly off the reactive path.
const route = useRoute();

onMounted(() => {
  bootstrap();
});

// Hosted bootstrap: the moment we know there is no session, leave for the portal
// rather than render a box login/setup page. Covers both the first visit and a
// later 401 that clears currentUser. watchEffect re-runs whenever its reactive
// inputs change, so it fires as soon as bootstrap() settles.
watchEffect(() => {
  if (booted.value && isHosted() && !currentUser.value) {
    redirectToPortal();
  }
});
</script>

<template>
  <div v-if="!booted" class="grid h-full place-items-center text-muted-foreground">Loading…</div>
  <!-- Hosted + no session: redirecting to the portal (watchEffect above). Render
       a neutral placeholder, never the appliance login/setup pages. -->
  <div
    v-else-if="isHosted() && !currentUser"
    class="grid h-full place-items-center text-muted-foreground"
  >
    Redirecting…
  </div>
  <!-- Wizard interrupted after the admin was created but before completion, and
       the session is gone (e.g. a reload): the remaining steps are admin-gated,
       so re-auth first, then the wizard resumes at its next step. Appliance only —
       hosted never reaches here (no session ⇒ redirected above). -->
  <Login v-else-if="!firstRunComplete && hasUsers && !currentUser" />
  <Setup v-else-if="!firstRunComplete" />
  <AppShell v-else-if="currentUser" />
  <RecoverView v-else-if="route.path === '/recover'" />
  <Login v-else />
</template>
