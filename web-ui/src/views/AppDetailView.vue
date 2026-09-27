<script setup lang="ts">
// App detail page (/store/:id) — the catalog app's full view, structured like an
// app-store product page (header → screenshots → description → pricing + info).
// The screenshots open full screen, and the pricing panel shows what moose
// charges plus the third-party costs the manifest declares. Both are imported
// from the marketing store's app page (cloud internal/web/templates/pages/app.html)
// so the two store surfaces show one catalog the same way. This is
// where Install starts (the browse grid only navigates here): the button goes
// to the setup page (/store/:id/install), and the household item of the split
// button adds ?scope=household. Which copies the caller already has, and so
// whether the button reads Install, Open, or "Installing…", comes from
// useAppInstances.
//
// The long description is author markdown rendered to HTML and sanitized before
// it touches the DOM (catalog text is author-controlled; sanitize anyway).
import { computed, onUnmounted, ref, watch } from "vue";
import { useRoute, useRouter, RouterLink } from "vue-router";
import { useQuery } from "@tanstack/vue-query";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { ChevronLeft, ChevronRight, Info, X } from "lucide-vue-next";
import { api, type CatalogDetail, type CatalogHome } from "../api";
import { useAppInstances } from "../useInstall";
import { formatSize, safeExternalUrl } from "../utils";
import AppGlyph from "../components/AppGlyph.vue";
import SplitButton from "../components/SplitButton.vue";
import HealthGated from "../components/HealthGated.vue";

const route = useRoute();
const router = useRouter();
// The route param is the manifest id; keep it reactive so navigating between two
// detail pages without unmounting re-drives the queries.
const manifestId = computed(() => String(route.params.id));

const detailQuery = useQuery({
  queryKey: computed(() => ["catalog", manifestId.value]),
  queryFn: () => api.get<CatalogDetail>(`/catalog/${manifestId.value}`),
});
const app = computed(() => detailQuery.data.value ?? null);

// The detail payload carries category ids, not display text. The landing payload
// carries the authored label for each, so read it from there rather than deriving
// one from the id — deriving is what let this page and the store page disagree.
// Same query key as StoreView, so this is a cache read, not a second request.
const homeQuery = useQuery({
  queryKey: ["catalog", "home"],
  queryFn: () => api.get<CatalogHome>("/catalog/home"),
});

// categoryLabels is the app's categories as authored display text, joined for the
// info panel. An id the vocabulary doesn't carry falls back to the id itself.
const categoryLabels = computed(() => {
  const vocab = homeQuery.data.value?.categories ?? [];
  return (app.value?.categories ?? [])
    .map((id) => vocab.find((c) => c.id === id)?.label ?? id)
    .join(", ");
});

const { householdInstance, ownPersonalInstance, installing, canInstallHousehold } = useAppInstances(manifestId);

function goInstall(household = false) {
  router.push({
    path: `/store/${encodeURIComponent(manifestId.value)}/install`,
    query: household ? { scope: "household" } : undefined,
  });
}

// The split button's menu. Only admins outside single-user mode get the
// household item; everyone else gets a plain Install button.
const dropdownItems = computed(() =>
  canInstallHousehold.value ? [{ label: "Install for the whole household", action: () => goInstall(true) }] : [],
);

// brokenIcon falls the header icon back to the glyph if the asset fails to load;
// reset when navigating to a different app so a fresh icon gets a fresh chance.
const brokenIcon = ref(false);
watch(manifestId, () => { brokenIcon.value = false; });

// Rendered, sanitized markdown body. Empty when the manifest carries no long
// description (the section is hidden in that case).
const descriptionHtml = computed(() => {
  const md = app.value?.long_description;
  if (!md) return "";
  return DOMPurify.sanitize(marked.parse(md, { async: false }) as string);
});

// sizeLabel is the coarse catalog footprint (image disk bytes); shown only when
// the manifest carries sized images. The install setup page shows the sharper,
// box-specific figure (DASHBOARD.md # the consent screen shows the on-disk footprint).
const sizeLabel = computed(() => {
  const b = app.value?.footprint.image_disk_bytes ?? 0;
  return b > 0 ? formatSize(b) : null;
});

// External links are app-provided; pass them through safeExternalUrl so a
// non-http(s) value (e.g. a javascript: URL) is dropped rather than bound to an
// :href. The template renders a link only when the vetted URL is present.
const authorUrl = computed(() => safeExternalUrl(app.value?.author?.url));
const homepageUrl = computed(() => safeExternalUrl(app.value?.links?.homepage));
const sourceUrl = computed(() => safeExternalUrl(app.value?.links?.source));
const supportUrl = computed(() => safeExternalUrl(app.value?.links?.support));
const changelogUrl = computed(() => safeExternalUrl(app.value?.changelog_url));
const hasLinks = computed(
  () => !!(homepageUrl.value || sourceUrl.value || supportUrl.value || changelogUrl.value),
);

// PRICE is what moose charges for this app. Every catalog app is free today and
// the catalog wire carries no per-app price — it is authored in the curation
// source (APP_STORE.md # Catalog schema) — so the page states it in one place
// rather than the template hardcoding it, the way the marketing store's app page
// does (cloud cmd/control-plane/main.go # appPrice). When a price does reach the
// wire, this is the line that reads it.
const PRICE = "Free";

// extraCosts is what a THIRD PARTY charges to make the app useful (a model
// provider, a mail provider). Not moose's charge; the note behind the panel's
// info tooltip says so.
const extraCosts = computed(() => app.value?.external_costs ?? []);

// ── Screenshot viewer ────────────────────────────────────────────────────────
// A thumbnail is too small to decide from, so a click opens the shot fit to the
// viewport — the whole image, not a zoom of the thumbnail in place — with the
// arrow keys and prev/next moving between shots.
//
// This is a native <dialog>, not the fixed-overlay idiom AppMenuDialog uses,
// because the browser then supplies the top layer, the backdrop, Esc, the focus
// trap, and focus restore to the thumbnail that opened it. What is left is an
// index and a src swap. Imported from the marketing store's app page (cloud
// internal/web/static/app-gallery.js), minus its progressive-enhancement half:
// that page is server-rendered HTML, this one is a Vue view that needs script
// anyway, so the thumbnails are buttons here rather than links.
const viewer = ref<HTMLDialogElement | null>(null);
const shots = computed(() => app.value?.screenshot_urls ?? []);
const shotIndex = ref(0);
// Navigating straight from one detail page to another reuses this component, so
// the index has to go back to the first shot of the new app.
watch(manifestId, () => { shotIndex.value = 0; });
// showModal blocks interaction behind the dialog, but not every browser's wheel
// scroll, so the document is pinned for as long as the viewer is open.
//
// The pin is tracked with its own flag rather than lifted on the dialog's close
// event alone, because a dialog can stop being on screen WITHOUT closing: taking
// an open <dialog> out of the DOM fires no close event. This view is reached by
// a router route, so that is an ordinary thing to do here — the Back button
// unmounts the whole component mid-viewer — where the server-rendered page this
// was ported from could only ever leave by a full document load. Miss it and the
// pin outlives the dialog: every other page in the dashboard is then unscrollable
// until a reload.
let restoreOverflow = "";
let scrollPinned = false;

function pinScroll() {
  if (scrollPinned) return;
  restoreOverflow = document.documentElement.style.overflow;
  document.documentElement.style.overflow = "hidden";
  scrollPinned = true;
}

function releaseScroll() {
  if (!scrollPinned) return;
  document.documentElement.style.overflow = restoreOverflow;
  scrollPinned = false;
}

function openShot(i: number) {
  shotIndex.value = i;
  pinScroll();
  viewer.value?.showModal();
}

// stepShot wraps in both directions, so the arrows never dead-end and there is
// no disabled state to keep in sync.
function stepShot(by: number) {
  const n = shots.value.length;
  if (n > 0) shotIndex.value = (shotIndex.value + by + n) % n;
}

// Focus is inside the dialog while it is modal, so its own keydown sees every
// key. Esc is the dialog's native cancel and is deliberately not handled here.
function onViewerKey(e: KeyboardEvent) {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const by = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
  if (by === 0) return;
  e.preventDefault();
  stepShot(by);
}

// Every path that CLOSES the dialog lands here — the X, a click on the surround,
// Esc. The two paths that end the viewer without closing it are below.
function onViewerClose() {
  releaseScroll();
}

// Leaving the page while the viewer is open (the Back button, a link, a redirect)
// takes the dialog down with the component and fires no close event.
onUnmounted(releaseScroll);

// A background refetch can return the same app with fewer screenshots, or none:
// the v-if then removes the open dialog from the DOM, again with no close event.
// An index past the end of a shorter list would otherwise render a broken image
// under a "6 of 3" counter.
watch(shots, (list) => {
  if (list.length === 0) {
    releaseScroll();
  } else if (shotIndex.value >= list.length) {
    shotIndex.value = 0;
  }
});
</script>

<template>
  <div class="space-y-8 pt-2">
    <RouterLink to="/store" class="inline-block text-sm text-muted-foreground hover:text-foreground">
      ← Store
    </RouterLink>

    <p v-if="detailQuery.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
    <p v-else-if="detailQuery.isError.value" class="text-sm text-destructive">
      Couldn't load this app. {{ (detailQuery.error.value as Error)?.message }}
    </p>

    <template v-else-if="app">
      <!-- Header: icon · name/tagline/developer · Install -->
      <header class="flex flex-col gap-5 sm:flex-row sm:items-center">
        <div
          class="grid size-20 shrink-0 place-items-center overflow-hidden rounded-3xl border border-border bg-card text-muted-foreground"
        >
          <img
            v-if="app.icon_url && !brokenIcon"
            :src="app.icon_url"
            :alt="`${app.name} icon`"
            class="size-full object-cover"
            @error="brokenIcon = true"
          />
          <AppGlyph v-else :name="app.icon_glyph" class="size-9" />
        </div>

        <div class="min-w-0 flex-1">
          <h1 class="text-xl font-semibold">{{ app.name }}</h1>
          <p v-if="app.short_description" class="mt-0.5 text-sm text-muted-foreground">
            {{ app.short_description }}
          </p>
          <p v-if="app.author?.name" class="mt-1 text-xs text-muted-foreground">
            by
            <a
              v-if="authorUrl"
              :href="authorUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="hover:text-foreground hover:underline"
            >{{ app.author.name }}</a>
            <span v-else>{{ app.author.name }}</span>
          </p>
        </div>

        <!-- Install / Open affordance — same state machine as the old Store row.
             "Open" only appears once the instance is past "installing": the brain
             emits app.state_changed (state="installing") at the very start of the
             job, so the row shows up in ["apps"] ~1s in — long before the app is
             actually up. Gating Open on state keeps the button in its "Installing…"
             phase for the whole job instead of flipping to a dead Open link. -->
        <div class="flex shrink-0 items-center gap-2">
          <a
            v-if="householdInstance && householdInstance.state !== 'installing'"
            :href="householdInstance.url"
            target="_blank"
            rel="noopener"
            class="rounded-lg border border-border px-3 py-1.5 text-sm hover:bg-muted"
          >
            Open shared app
          </a>
          <a
            v-else-if="ownPersonalInstance && ownPersonalInstance.state !== 'installing'"
            :href="ownPersonalInstance.url"
            target="_blank"
            rel="noopener"
            class="rounded-lg bg-accent px-4 py-1.5 text-sm font-medium text-accent-foreground hover:opacity-90"
          >
            Open
          </a>

          <!-- Install button: shown when the caller has no own instance yet, or
               while their own copy is still installing, or alongside "Open shared
               app" so the caller can still install their own copy. -->
          <HealthGated
            v-if="!ownPersonalInstance || ownPersonalInstance.state === 'installing'"
            blocks="apps"
          >
            <SplitButton
              :label="installing ? 'Installing…' : 'Install'"
              :loading="installing"
              :disabled="installing"
              :items="dropdownItems"
              @click="goInstall()"
            />
          </HealthGated>
        </div>
      </header>

      <!-- Screenshots gallery. Each shot is a button that opens the full-screen
           viewer below; the border stays because it is what gives a white
           screenshot an edge, and hover lifts with a shadow rather than darkening
           that hairline (a pale line vanishes into a light screenshot and frames
           a dark one — edge brightness across the catalog runs the whole range). -->
      <section v-if="shots.length" class="space-y-3">
        <div class="-mx-4 flex snap-x gap-4 overflow-x-auto px-4 pb-2">
          <button
            v-for="(src, i) in shots"
            :key="src"
            type="button"
            class="shrink-0 cursor-pointer snap-start rounded-xl focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-background"
            :aria-label="`View ${app.name} screenshot ${i + 1} full screen`"
            @click="openShot(i)"
          >
            <img
              :src="src"
              :alt="`${app.name} screenshot ${i + 1}`"
              class="h-56 rounded-xl border border-border object-cover transition hover:shadow-md"
            />
          </button>
        </div>

        <!-- The viewer. Inert until openShot calls showModal(): a dialog that is
             never shown renders nothing. Prev/next are dropped for a single
             screenshot, where there is nowhere to navigate to. -->
        <dialog
          ref="viewer"
          :aria-label="`${app.name} screenshots`"
          class="m-0 h-dvh max-h-none w-dvw max-w-none bg-transparent p-0 backdrop:bg-black/90"
          @close="onViewerClose"
          @keydown="onViewerKey"
        >
          <!-- Clicking the space around the image dismisses, the way a lightbox is
               expected to. @click.self keeps clicks on the image or a control as
               navigation, not a dismissal. -->
          <div
            class="relative flex h-full w-full flex-col items-center justify-center gap-5 p-4 sm:p-10"
            @click.self="viewer?.close()"
          >
            <img
              :src="shots[shotIndex]"
              :alt="`${app.name} screenshot ${shotIndex + 1}`"
              class="max-h-[80vh] w-auto max-w-full rounded-xl object-contain"
            />

            <div v-if="shots.length > 1" class="flex items-center gap-5">
              <button
                type="button"
                class="grid size-10 cursor-pointer place-items-center rounded-full bg-white/10 text-white hover:bg-white/20"
                @click="stepShot(-1)"
              >
                <span class="sr-only">Previous screenshot</span>
                <ChevronLeft class="size-5" />
              </button>
              <p aria-live="polite" class="text-sm tabular-nums text-white/80">
                {{ shotIndex + 1 }} of {{ shots.length }}
              </p>
              <button
                type="button"
                class="grid size-10 cursor-pointer place-items-center rounded-full bg-white/10 text-white hover:bg-white/20"
                @click="stepShot(1)"
              >
                <span class="sr-only">Next screenshot</span>
                <ChevronRight class="size-5" />
              </button>
            </div>

            <button
              type="button"
              autofocus
              class="absolute right-4 top-4 grid size-10 cursor-pointer place-items-center rounded-full bg-white/10 text-white hover:bg-white/20 sm:right-6 sm:top-6"
              @click="viewer?.close()"
            >
              <span class="sr-only">Close</span>
              <X class="size-5" />
            </button>
          </div>
        </dialog>
      </section>

      <div class="grid gap-8 lg:grid-cols-[1fr_16rem]">
        <!-- Long description -->
        <section v-if="descriptionHtml" class="markdown-body min-w-0" v-html="descriptionHtml" />
        <p v-else class="text-sm text-muted-foreground">No description provided.</p>

        <aside class="space-y-8 text-sm">
          <!-- Pricing panel: what moose charges, then what a third party charges
               to make the app useful. A required cost opens expanded — it is the
               one someone has to read before installing; an optional one stays
               collapsed. The "you pay the provider, not moose" note sits behind
               an info tooltip so the panel stays quiet when nobody asks. The
               tooltip is CSS only (group-hover / group-focus-within), so it opens
               on hover for a pointer and on focus for a keyboard or a tap. -->
          <section class="space-y-4">
            <div class="flex items-baseline justify-between gap-4 rounded-lg bg-card px-3 py-2">
              <span class="text-muted-foreground">Price</span>
              <span class="font-medium">{{ PRICE }}</span>
            </div>

            <div v-if="extraCosts.length" class="space-y-2 px-3">
              <div class="flex items-center gap-1">
                <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Extra costs</h2>
                <span class="group relative inline-flex">
                  <button
                    type="button"
                    aria-describedby="extra-costs-note"
                    class="cursor-pointer text-muted-foreground hover:text-foreground"
                  >
                    <span class="sr-only">About extra costs</span>
                    <Info class="size-3.5" />
                  </button>
                  <span
                    id="extra-costs-note"
                    role="tooltip"
                    class="pointer-events-none absolute left-0 top-full z-10 mt-1.5 w-56 rounded-lg border border-border bg-card p-3 text-xs text-muted-foreground opacity-0 shadow-lg transition-opacity duration-100 group-hover:opacity-100 group-focus-within:opacity-100"
                  >
                    You pay the provider directly. moose never charges you for these.
                  </span>
                </span>
              </div>

              <details v-for="cost in extraCosts" :key="cost.id" class="group" :open="cost.required">
                <summary
                  class="flex cursor-pointer list-none items-center gap-1.5 [&::-webkit-details-marker]:hidden"
                >
                  <ChevronRight
                    class="size-3 shrink-0 text-muted-foreground transition-transform group-open:rotate-90"
                  />
                  <span class="font-medium">{{ cost.title }}</span>
                  <span
                    v-if="cost.required"
                    class="ml-auto shrink-0 rounded-full bg-muted px-2 py-0.5 text-xs font-medium"
                  >
                    Required
                  </span>
                  <span v-else class="ml-auto shrink-0 text-xs text-muted-foreground">Optional</span>
                </summary>
                <div class="mt-1 space-y-1 pl-4.5 text-xs text-muted-foreground">
                  <p v-if="cost.estimate" class="font-medium text-foreground">{{ cost.estimate }}</p>
                  <p><span class="text-muted-foreground">About:</span> {{ cost.description }}</p>
                </div>
              </details>
            </div>
          </section>

          <!-- Info panel -->
          <section class="space-y-3">
            <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Information</h2>
            <dl class="space-y-2">
              <div class="flex justify-between gap-4">
                <dt class="text-muted-foreground">Version</dt>
                <dd class="text-right">{{ app.version }}</dd>
              </div>
              <div v-if="app.categories?.length" class="flex justify-between gap-4">
                <dt class="text-muted-foreground">Category</dt>
                <dd class="text-right">{{ categoryLabels }}</dd>
              </div>
              <div v-if="sizeLabel" class="flex justify-between gap-4">
                <dt class="text-muted-foreground">Size</dt>
                <dd class="text-right">{{ sizeLabel }}</dd>
              </div>
              <div v-if="app.license" class="flex justify-between gap-4">
                <dt class="text-muted-foreground">License</dt>
                <dd class="text-right">{{ app.license }}</dd>
              </div>
            </dl>

            <div v-if="hasLinks" class="space-y-1.5 border-t border-border pt-3">
              <a v-if="homepageUrl" :href="homepageUrl" target="_blank" rel="noopener noreferrer" class="block text-accent hover:underline">Website</a>
              <a v-if="sourceUrl" :href="sourceUrl" target="_blank" rel="noopener noreferrer" class="block text-accent hover:underline">Source code</a>
              <a v-if="supportUrl" :href="supportUrl" target="_blank" rel="noopener noreferrer" class="block text-accent hover:underline">Support</a>
              <a v-if="changelogUrl" :href="changelogUrl" target="_blank" rel="noopener noreferrer" class="block text-accent hover:underline">Changelog</a>
            </div>
          </section>
        </aside>
      </div>

    </template>
  </div>
</template>

<style scoped>
/* Minimal markdown styling — Tailwind preflight strips heading/list defaults, and
   the project has no typography plugin. Scoped, so :deep() reaches the v-html
   subtree. Keeps the body readable without pulling in @tailwindcss/typography. */
.markdown-body :deep(h1),
.markdown-body :deep(h2),
.markdown-body :deep(h3) {
  font-weight: 600;
  line-height: 1.3;
  margin: 1.25rem 0 0.5rem;
}
.markdown-body :deep(h1) { font-size: 1.25rem; }
.markdown-body :deep(h2) { font-size: 1.1rem; }
.markdown-body :deep(h3) { font-size: 1rem; }
.markdown-body :deep(p) { margin: 0.75rem 0; line-height: 1.6; }
.markdown-body :deep(ul),
.markdown-body :deep(ol) { margin: 0.75rem 0; padding-left: 1.5rem; }
.markdown-body :deep(ul) { list-style: disc; }
.markdown-body :deep(ol) { list-style: decimal; }
.markdown-body :deep(li) { margin: 0.25rem 0; }
.markdown-body :deep(a) { color: var(--color-accent); text-decoration: underline; }
.markdown-body :deep(code) {
  font-family: ui-monospace, monospace;
  font-size: 0.85em;
  background: var(--color-muted);
  padding: 0.1em 0.3em;
  border-radius: 0.25rem;
}
.markdown-body :deep(pre) {
  background: var(--color-muted);
  padding: 0.75rem;
  border-radius: 0.5rem;
  overflow-x: auto;
}
.markdown-body :deep(pre code) { background: none; padding: 0; }
.markdown-body :deep(strong) { font-weight: 600; }
</style>
