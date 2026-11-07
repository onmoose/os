<script setup lang="ts">
// The signed-in chrome: a quiet top bar, the routed view, and the floating
// four-item dock (DASHBOARD.md # the top bar / # global navigation). The SSE
// subscription lives here — one subscription for the whole signed-in app
// (WEB_UI.md: useEvents() at app mount), so cache invalidation works on every
// view, not just Home.
import TopBar from "./TopBar.vue";
import Dock from "./Dock.vue";
import ToastHost from "./ToastHost.vue";
import ElevateDialog from "./ElevateDialog.vue";
import HealthBanner from "./HealthBanner.vue";
import UpdatePrompt from "./UpdatePrompt.vue";
import UpdatesToast from "./UpdatesToast.vue";
import { useEvents } from "../useEvents";

useEvents();
</script>

<template>
  <div class="flex h-full flex-col">
    <TopBar />
    <!-- Degraded-mode bar: shows above the routed view on every page when an
         error/critical health issue is active (HEALTH.md # Display). -->
    <HealthBanner />
    <!-- The content wrapper is min-h-full (fills the viewport when short, grows
         with content when long) rather than a fixed h-full box — a fixed box lets
         tall pages overflow *past* the scroll clearance, so the fixed dock would
         cover the last rows. flex-col so a full-height section (Settings' logs
         pane) can still flex-1 to fill.
         `main` itself must be flex/flex-col (not just a flex *item* of the shell)
         so the wrapper below is a genuine flex item of it — only then does the
         wrapper's resolved height count as "definite" for percentage/flex-basis
         resolution in its descendants. Without this, min-h-full's floor only
         behaves like a real height while content is shorter than the viewport;
         once content (e.g. an expanded Settings logs pane) is taller, the whole
         h-full/flex-1 chain below stops being clamped and grows unbounded instead
         of scrolling internally. w-full on the wrapper restores full-width sizing
         under max-w-6xl/mx-auto, since a flex item with auto margins on the cross
         axis opts out of the default stretch sizing. -->
    <main class="flex min-h-0 flex-1 flex-col overflow-y-auto px-4 pt-2">
      <div class="mx-auto flex w-full min-h-full max-w-6xl flex-1 flex-col">
        <RouterView />
        <!-- Dock clearance. This MUST be a real flex child, not padding on the
             wrapper: a flex + overflow-y-auto scroll container drops trailing
             padding-bottom from its scrollable height (verified in Chrome), so a
             pb-* here leaves the last rows stranded behind the fixed dock. A
             shrink-0 spacer is counted in scrollHeight, so any page can always
             scroll its last row clear of the dock. -->
        <div class="h-28 shrink-0" aria-hidden="true"></div>
      </div>
    </main>
    <Dock />
    <ToastHost />
    <ElevateDialog />
    <!-- Asks the owner about app updates that want more access, once per
         session (UPDATES.md # 6, #633). -->
    <UpdatePrompt />
    <!-- "3 apps updated overnight", once (UPDATES.md # 6, #634). -->
    <UpdatesToast />
  </div>
</template>
