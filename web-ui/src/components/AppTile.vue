<script setup lang="ts">
// A single app tile (DASHBOARD.md # Tile). Calm by default — icon, name, and a
// quiet label — with state surfacing *only* when not nominal. The only click
// affordance is the logo square, and only to *open* a running app; a stopped or
// failed tile is inert. Starting / stopping a service happens from the quick menu
// (the controller-only button beside the name → AppMenuDialog), not the tile.
//   - running:  a link that opens the app in a new tab at its own subdomain.
//   - stopped:  grayed, with a "Service stopped" hover caption. A deliberate stop
//               is not an error, so it gets no corner alert mark.
//   - failed:   amber-tinted (warning, distinct from the gray stopped tile) with
//               the corner alert mark and a "View details" link to the app page
//               where the failure reason / logs live (retry is in the quick menu).
//   - other:    grayed with the corner alert mark — needs attention.
//   - needs setup (INSTALL_SETUP.md piece 4): a required setting is empty or
//               the app has no LLM provider, often after its account was
//               deleted. The corner mark shows in amber, and a controller gets
//               a "Needs setup" link to the app's settings screen.
//
// Icon rendering mirrors StoreAppCard: icon_url when present, AppGlyph fallback.
import { computed, ref } from "vue";
import { RouterLink } from "vue-router";
import { AlertTriangle, EllipsisVertical } from "lucide-vue-next";
import type { Instance } from "../api";
import { useAuth, isHosted } from "../auth";
import AppGlyph from "./AppGlyph.vue";
import AppMenuDialog from "./AppMenuDialog.vue";

const props = defineProps<{ instance: Instance }>();
const { singleUserMode, currentUser } = useAuth();

const running = computed(() => props.instance.state === "running");
const stopped = computed(() => props.instance.state === "stopped");
const failed = computed(() => props.instance.state === "failed");
const label = computed(() => (props.instance.scope === "household" ? "Shared" : "Personal"));

// Mirrors the brain's control gate: admins anything, members only their own
// personal instance. Drives whether the quick-menu button is shown.
const canControl = computed(
  () => currentUser.value?.role === "admin" || props.instance.scope === "personal",
);

// How open the app is, drawn as a globe in the bottom corner of the logo. Three
// states, because the manifest can keep single paths open on an otherwise closed
// app (#415):
//   - "open"    exposure=public — anyone with the link opens the whole app (#306).
//   - "partial" exposure=restricted, but the manifest declares public paths, so
//               part of the app answers anonymously. Drawn as a faded globe.
//   - "closed"  owner-only, no badge — the quiet default needs no mark.
// Hosted-only: the appliance has no public app subdomains, every app is LAN-only
// there, and exposure defaults to "public" on that profile, so an ungated badge
// would mark every tile.
const access = computed<"open" | "partial" | "closed">(() => {
  if (!isHosted()) return "closed";
  if (props.instance.exposure === "public") return "open";
  return (props.instance.public_paths ?? []).length > 0 ? "partial" : "closed";
});
const accessTitle = computed(() =>
  access.value === "open"
    ? "Public. Anyone with the link can access this app."
    : "Partly public. Some app paths are open to the public.",
);

const brokenIcon = ref(false);

// The corner alert is for trouble (failed/crashed), not a deliberate stop.
const showAlert = computed(() => !running.value && !stopped.value);
// Needs setup is a softer warning than a failure, so it gets the amber mark
// only when no failure mark is already there.
const needsSetup = computed(() => !!props.instance.needs_setup);

// Element + behavior: a link when running (opens the app), otherwise an inert div
// — the logo is the open affordance only, never a start/retry button.
const tag = computed(() => (running.value ? "a" : "div"));

// The quick menu (logo, description, service control, settings link) lives beside
// the name. Gated to controllers — a member viewing a household app can't act on
// it, so they get no menu button (the brain re-checks regardless).
const menuOpen = ref(false);
</script>

<template>
  <div class="group/tile flex flex-col items-center gap-2 text-center">
    <!-- The logo square is the *only* tile affordance, and only to open a running
         app. A stopped/failed tile is inert (start/stop lives in the quick menu).
         Parchment fill, bordered, app icon (or glyph) — mirrors StoreAppCard.
         Grays out when not running; failed gets an amber warning tint instead. -->
    <component
      :is="tag"
      :href="running ? instance.url : undefined"
      :target="running ? '_blank' : undefined"
      rel="noopener"
      class="relative grid aspect-square w-full place-items-center overflow-hidden rounded-3xl border text-muted-foreground transition"
      :class="[
        running ? 'cursor-pointer' : 'cursor-default',
        running
          ? 'border-border bg-card group-hover/tile:shadow-md'
          : failed
            ? 'border-amber-400 bg-amber-50 opacity-90'
            : 'border-border bg-card opacity-50',
      ]"
      :title="running ? `Open ${instance.name}` : `${instance.name} is ${instance.state}`"
    >
      <AlertTriangle
        v-if="showAlert"
        class="absolute right-3 top-3 size-4 text-destructive"
      />
      <AlertTriangle
        v-else-if="needsSetup"
        class="absolute right-3 top-3 size-4 text-amber-600"
        aria-label="Needs setup"
      />
      <span
        v-if="access !== 'closed'"
        class="absolute bottom-3 right-3 block size-4 text-muted-foreground"
        :class="access === 'partial' ? 'opacity-30' : ''"
        :title="accessTitle"
      >
        <!-- The globe is drawn as a mask filled with currentColor, not as a plain
             <img>: the source PNG is black line art, which would vanish on a dark
             tile. One drawing serves both states. "Partly public" is the same
             globe at 30% opacity, so a partly open app reads as a fainter version
             of an open one rather than as a second symbol to learn.
             Decorative here: the state is named in the sr-only text below. -->
        <span class="globe-mask block size-full bg-current" />
        <span class="sr-only">{{ access === "open" ? "Public" : "Partly public" }}</span>
      </span>
      <img
        v-if="instance.icon_url && !brokenIcon"
        :src="instance.icon_url"
        :alt="`${instance.name} icon`"
        class="size-1/2 object-contain"
        @error="brokenIcon = true"
      />
      <AppGlyph v-else :name="instance.icon_glyph" class="size-1/2" />
    </component>

    <div class="min-w-0 w-full">
      <!-- Name, centered; the quick-menu button (controllers only) is pinned to
           the right edge of the tile, overlaid so the name stays centered. -->
      <div class="relative">
        <div class="truncate px-5 text-center text-base font-medium">{{ instance.name }}</div>
        <button
          v-if="canControl"
          type="button"
          class="absolute right-0 top-1/2 -translate-y-1/2 cursor-pointer rounded-md p-0.5 text-muted-foreground hover:bg-muted hover:text-foreground"
          :aria-label="`${instance.name} options`"
          @click="menuOpen = true"
        >
          <EllipsisVertical class="size-4" />
        </button>
      </div>
      <!-- Status caption under the logo, revealed on hover of the tile. -->
      <div
        v-if="failed"
        class="text-xs text-amber-700 opacity-0 transition group-hover/tile:opacity-100"
      >
        Failed
      </div>
      <div
        v-else-if="stopped"
        class="text-xs text-muted-foreground opacity-0 transition group-hover/tile:opacity-100"
      >
        Service stopped
      </div>
      <div v-else-if="!singleUserMode" class="text-xs uppercase tracking-wide text-muted-foreground">{{ label }}</div>
    </div>

    <!-- Needs setup: the app's settings screen says what is missing. -->
    <RouterLink
      v-if="needsSetup && !failed && canControl"
      :to="`/settings/apps/${instance.id}`"
      class="text-xs text-amber-700 underline underline-offset-2 hover:text-foreground"
    >
      Needs setup
    </RouterLink>

    <!-- Don't retry blind: a controller can open the app page for the failure
         reason / logs rather than looping on the tile (#154). -->
    <RouterLink
      v-if="failed && canControl"
      :to="`/settings/apps/${instance.id}`"
      class="text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground"
    >
      View details
    </RouterLink>

    <AppMenuDialog v-if="menuOpen" :instance="instance" @close="menuOpen = false" />
  </div>
</template>

<style scoped>
/* The globe badge. The PNG is black line art on transparency, so it is used as a
   MASK filled with currentColor rather than drawn as an image — that way it takes
   the tile's text color and stays visible in both themes. Vite resolves the
   relative url() at build time and hashes the asset. */
.globe-mask {
  -webkit-mask-image: url("../assets/globe.png");
  mask-image: url("../assets/globe.png");
  -webkit-mask-size: contain;
  mask-size: contain;
  -webkit-mask-repeat: no-repeat;
  mask-repeat: no-repeat;
  -webkit-mask-position: center;
  mask-position: center;
}

</style>
