<script setup lang="ts">
// Store — browse the catalog through the control plane's *segmented* model
// (issue #63, cloud specs/CATALOG.md # Serve) rather than loading the whole
// catalog and filtering client-side. The box never pulls the entire catalog up
// front: the landing asks the brain for /catalog/home (the categories present on
// this box, the curated spotlight + category groups authored in a curated
// home.yml, and a flat featured row), a category pill asks for
// /catalog/category?name=…, and typing asks /catalog/search?q=…. Every request
// stays same-origin on the brain, which serves these from its own synced snapshot
// (so browse still works offline — step 3's last-good cache), never the public
// control plane directly (AUTH_AND_ACCESS.md — the box UI stays box-identity-gated).
//
// The three views are mutually exclusive entry points: a non-empty search box wins
// over a selected category, which wins over the landing. Selecting a pill clears
// the search, and vice versa, so the grid always reflects exactly one of them.
// Category and search are both *filtered* views — the curated Featured row is a
// landing-only concept and never appears under a pill or a search (mirrors how
// the control plane's own store surface renders a category; `catalog.Category`
// still carries `Featured` on the wire, for parity, but the box UI does not
// render it there).
//
// The landing itself falls back in three steps so it is never empty
// (docs/specs/APP_STORE.md # Landing page): the authored home (spotlight banner +
// packed category-group rows, mirroring the control plane's own row-packing) →
// the flat featured row → a plain "pick a category or search" line.
//
// Door 2 (custom-container install) is admin-only and sits as a "Custom app" link
// beside the search, never in the browse grid (DASHBOARD.md # Door-2). Members
// never see it.
//
// The layout follows the Oatmeal-skinned Tailwind Plus application-UI patterns: a
// page heading with an inline search, pill tabs for categories, section headings
// for the featured/browse rows, a grid list of cards, and empty-state blocks. All
// colour flows from the olive semantic tokens (style.css).
import { computed, onUnmounted, ref, watch } from "vue";
import { useQuery } from "@tanstack/vue-query";
import { Search, SearchX, PackageOpen, Sparkles } from "lucide-vue-next";
import { useAuth } from "../auth";
import { api, type CatalogEntry, type CatalogHome, type CatalogCategory } from "../api";
import StoreAppCard from "../components/StoreAppCard.vue";
import StoreSpotlight from "../components/StoreSpotlight.vue";
import Heading from "@/components/ui/Heading.vue";
import { packRows, groupSpan, groupCols } from "../lib/storeLayout";
import Button from "@/components/ui/Button.vue";

const { currentUser } = useAuth();
const isAdmin = computed(() => currentUser.value?.role === "admin");

// Free-text query and the active category pill ("recommended" = the curated
// landing, mirroring the control plane's own store surface). They are exclusive:
// selecting a pill clears the search, so mode() resolves to one view.
// "recommended" is never a user-visible pill — the landing is the default view,
// not a pill — and it can't collide with a real category id: the catalog's
// category ids today are productivity, developer-tools, media, documents, ai,
// personal, security, automation.
const query = ref("");
const activeCategory = ref("recommended");

// Debounced search term feeding the search request: each keystroke would otherwise
// round-trip to the brain, so we wait for a short pause in typing. mode() keys off
// the live query (the view switches immediately); the request keys off searchTerm.
const searchTerm = ref("");
let debounce: ReturnType<typeof setTimeout> | undefined;
watch(query, (q) => {
  // Typing is the "vice versa" of selectCategory: it drops the active pill
  // immediately (not debounced — mode() already switches to "search" on the same
  // tick), so a category never lingers underneath a cleared search box and then
  // pop back in once the query empties out again.
  if (q.trim() !== "") activeCategory.value = "recommended";
  clearTimeout(debounce);
  debounce = setTimeout(() => {
    searchTerm.value = q.trim();
  }, 200);
});
// Cancel a pending debounce if the view unmounts mid-keystroke, so the timer
// doesn't fire against a torn-down component.
onUnmounted(() => clearTimeout(debounce));

const mode = computed<"home" | "category" | "search">(() => {
  if (query.value.trim() !== "") return "search";
  if (activeCategory.value !== "recommended") return "category";
  return "home";
});

// Landing: categories + featured. Always enabled — it backs the pill row in every
// mode, so it is the one request the store cannot render without.
const home = useQuery({
  queryKey: ["catalog", "home"],
  queryFn: () => api.get<CatalogHome>("/catalog/home"),
});

// One category's apps (+ featured). Fetched only while a pill is active; the
// reactive key re-fetches when the pill changes.
const category = useQuery({
  queryKey: ["catalog", "category", activeCategory],
  queryFn: () =>
    api.get<CatalogCategory>(`/catalog/category?name=${encodeURIComponent(activeCategory.value)}`),
  enabled: computed(() => mode.value === "category"),
});

// Search results. Fetched only once the debounced term is non-empty.
const search = useQuery({
  queryKey: ["catalog", "search", searchTerm],
  queryFn: () =>
    api.get<{ apps: CatalogEntry[] }>(`/catalog/search?q=${encodeURIComponent(searchTerm.value)}`),
  enabled: computed(() => mode.value === "search" && searchTerm.value !== ""),
});

// Pills: the categories the landing advertised for this box, and nothing else —
// the curated landing is the default view, not a pill (mirrors the control
// plane's own store surface). Sorted by the brain, so a new catalog category
// appears without a UI change.
const categories = computed(() => home.data.value?.categories ?? []);

// The authored landing (a curated home.yml, carried through /catalog/home): a
// spotlight app and its category groups. Both are already environment-filtered
// by the brain, so an app not advertised on this box simply doesn't appear.
// Only meaningful in "home" mode — a category or search view never shows them.
const spotlight = computed<CatalogEntry | undefined>(() =>
  mode.value === "home" ? (home.data.value?.spotlight ?? undefined) : undefined,
);
const homeGroups = computed(() => (mode.value === "home" ? (home.data.value?.groups ?? []) : []));
const hasCuratedHome = computed(() => !!spotlight.value || homeGroups.value.length > 0);
// Packed two-or-more to a row, mirroring the control plane's own store surface
// (lib/storeLayout.ts).
const packedGroupRows = computed(() => packRows(homeGroups.value));

// Featured row: landing-only, and only as a fallback when nothing is authored in
// home.yml (the curated spotlight/groups take its place there —
// docs/specs/APP_STORE.md # Landing page). Never on a category or search view —
// both are filtered views, and the curated row is a landing-only concept, the
// same posture the control plane's own store surface takes for a category (its
// category view renders only the heading + that category's grid, never a
// featured row), even though catalog.Category still carries `Featured` on the
// wire for parity.
const featured = computed<CatalogEntry[]>(() => {
  if (mode.value === "home" && !hasCuratedHome.value) return home.data.value?.featured ?? [];
  return [];
});

// The main browse grid below the featured row: the category's apps, or the search
// results. The landing has no full grid — featured + pills are the whole entry point.
const browseApps = computed<CatalogEntry[]>(() => {
  if (mode.value === "category") return category.data.value?.apps ?? [];
  if (mode.value === "search") return search.data.value?.apps ?? [];
  return [];
});

// searchPending covers the debounce gap (query typed, term not yet caught up) and
// the in-flight fetch, so the browse region shows "Loading…" instead of flashing
// the no-matches state for a keystroke.
const searchPending = computed(
  () =>
    mode.value === "search" &&
    (searchTerm.value !== query.value.trim() || search.isFetching.value),
);

const isLoading = computed(() => {
  if (mode.value === "search") return searchPending.value;
  if (mode.value === "category") return category.isLoading.value;
  return home.isLoading.value;
});

// A failed landing fetch takes down the whole store (no pills); a failed
// category/search fetch only takes down the browse region.
const isError = computed(
  () =>
    home.isError.value ||
    (mode.value === "category" && category.isError.value) ||
    (mode.value === "search" && search.isError.value),
);
// Mirrors isError's mode-scoping: a stale error left over on a query that isn't
// the current mode's (e.g. a failed category fetch from before the user switched
// to search) must never outrank the error actually driving isError.
const errorMessage = computed(() => {
  const e =
    (home.error.value as Error) ??
    (mode.value === "category" ? (category.error.value as Error) : undefined) ??
    (mode.value === "search" ? (search.error.value as Error) : undefined);
  return e?.message ?? "";
});

// The catalog is genuinely empty (never synced, or nothing published for this box)
// when the landing carries neither categories nor featured apps.
const catalogEmpty = computed(
  () =>
    !home.isLoading.value &&
    (home.data.value?.categories?.length ?? 0) === 0 &&
    (home.data.value?.featured?.length ?? 0) === 0,
);

// activeCategoryLabel is the heading for the category view. The category payload
// carries its own authored label, so this prefers that and only falls back to the
// pill list while the request is in flight.
const activeCategoryLabel = computed(
  () =>
    category.data.value?.label ??
    categories.value.find((c) => c.id === activeCategory.value)?.label ??
    activeCategory.value,
);

// pillActive reports whether c is the pill currently shown as selected — used
// both for styling and to decide the click's toggle direction, so the two never
// disagree. A search in progress de-selects every pill (mirrors the control
// plane's own store surface).
function pillActive(id: string): boolean {
  return activeCategory.value === id && mode.value !== "search";
}

// Clicking a pill selects it; clicking the already-active pill toggles back to
// the landing (mirrors the control plane's own store surface: "the recommended
// landing is the default view, not a pill — clicking the active category
// toggles back to it").
function selectCategory(id: string) {
  activeCategory.value = pillActive(id) ? "recommended" : id;
  // Pills and search are exclusive entry points — picking a pill drops the search.
  query.value = "";
  searchTerm.value = "";
}

// Reset both filters back to the landing — surfaced from the no-results empty state.
function clearFilters() {
  query.value = "";
  searchTerm.value = "";
  activeCategory.value = "recommended";
}
</script>

<template>
  <div class="space-y-10 pt-2">
    <section class="space-y-6">
      <!-- Page heading: title + description on the left, the search input-group
           inline on the right (stacks above the pills on narrow screens). -->
      <div class="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <Heading :level="2">Store</Heading>
          <p class="mt-1 text-sm text-muted-foreground">Browse apps to run on your moose.</p>
        </div>

        <!-- Right cluster: the admin-only "Custom app" link (Door 2) sits to the
             left of the page-wide search. -->
        <div class="flex items-center gap-3">
          <!-- Door 2: custom-container install, as a plain text link. -->
          <RouterLink
            v-if="isAdmin"
            to="/store/custom"
            class="shrink-0 text-sm text-muted-foreground transition-colors hover:text-foreground"
          >
            Custom app
          </RouterLink>

          <!-- Page-wide search: queries the catalog over name, tagline, and
               categories. Leading-icon input-group, pill-shaped to match the idiom. -->
          <div class="relative w-full sm:w-64">
            <Search
              class="pointer-events-none absolute left-3.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
              aria-hidden="true"
            />
            <input
              v-model="query"
              type="search"
              placeholder="Search apps…"
              aria-label="Search apps"
              class="w-full rounded-full border border-border bg-card py-2 pl-10 pr-4 text-sm outline-none placeholder:text-muted-foreground focus:border-accent"
            />
          </div>
        </div>
      </div>

      <!-- Category pills: the catalog's own categories only — the curated landing
           is the default view, not a pill. Highlighted only when browsing that
           category (a search de-selects the pills); clicking the active pill
           toggles back to the landing. -->
      <div v-if="categories.length > 0" class="flex flex-wrap gap-2">
        <button
          v-for="c in categories"
          :key="c.id"
          type="button"
          class="cursor-pointer rounded-full border px-3.5 py-1 text-sm font-medium transition-colors"
          :class="
            pillActive(c.id)
              ? 'border-accent bg-accent text-accent-foreground'
              : 'border-border bg-card text-muted-foreground hover:bg-muted hover:text-foreground'
          "
          @click="selectCategory(c.id)"
        >
          {{ c.label }}
        </button>
      </div>

      <!-- Transient states stay as a quiet line; content-absence states get a
           proper empty-state block below. -->
      <p v-if="isLoading" class="text-sm text-muted-foreground">Loading…</p>
      <p v-else-if="isError" class="text-sm text-destructive">
        Couldn't load the catalog. {{ errorMessage }}
      </p>

      <!-- Empty catalog: nothing to browse yet (never synced / nothing published). -->
      <div
        v-else-if="catalogEmpty"
        class="rounded-2xl border border-dashed border-border py-16 text-center"
      >
        <PackageOpen class="mx-auto size-8 text-muted-foreground" aria-hidden="true" />
        <h3 class="mt-3 text-sm font-semibold text-foreground">No apps in the catalog yet</h3>
        <p class="mt-1 text-sm text-muted-foreground">Check back soon — the catalog is still filling out.</p>
      </div>

      <template v-else>
        <!-- Featured row: landing-only fallback (see the `featured` computed) —
             never shown on a category or search view. -->
        <section v-if="featured.length" class="space-y-4">
          <h3 class="flex items-center gap-2 text-base font-semibold text-foreground">
            <Sparkles class="size-4 text-accent" aria-hidden="true" />
            Featured
          </h3>
          <div class="grid grid-cols-2 gap-x-6 gap-y-8 sm:grid-cols-3 lg:grid-cols-4">
            <StoreAppCard v-for="c in featured" :key="c.id" :app="c" />
          </div>
        </section>

        <!-- Category view: that category's apps under a section heading. -->
        <section v-if="mode === 'category'" class="space-y-4">
          <h3 class="text-base font-semibold text-foreground">{{ activeCategoryLabel }}</h3>
          <div
            v-if="browseApps.length"
            class="grid grid-cols-2 gap-x-6 gap-y-8 sm:grid-cols-3 lg:grid-cols-4"
          >
            <StoreAppCard v-for="c in browseApps" :key="c.id" :app="c" />
          </div>
          <div v-else class="rounded-2xl border border-dashed border-border py-16 text-center">
            <SearchX class="mx-auto size-8 text-muted-foreground" aria-hidden="true" />
            <h3 class="mt-3 text-sm font-semibold text-foreground">No apps in this category</h3>
            <Button variant="secondary" size="sm" class="mt-4" @click="clearFilters">Back to recommended</Button>
          </div>
        </section>

        <!-- Search view: results for the current query, or a one-click reset. -->
        <section v-else-if="mode === 'search'" class="space-y-4">
          <div
            v-if="browseApps.length"
            class="grid grid-cols-2 gap-x-6 gap-y-8 sm:grid-cols-3 lg:grid-cols-4"
          >
            <StoreAppCard v-for="c in browseApps" :key="c.id" :app="c" />
          </div>
          <div v-else class="rounded-2xl border border-dashed border-border py-16 text-center">
            <SearchX class="mx-auto size-8 text-muted-foreground" aria-hidden="true" />
            <h3 class="mt-3 text-sm font-semibold text-foreground">No apps match your search</h3>
            <p class="mt-1 text-sm text-muted-foreground">Try a different search term or category.</p>
            <Button variant="secondary" size="sm" class="mt-4" @click="clearFilters">Clear search</Button>
          </div>
        </section>

        <!-- Landing, authored home: the spotlight banner, then the category
             groups from a curated home.yml, packed two-or-more to a row
             (lib/storeLayout.ts packRows). Rows sit in their own container so the
             gap between two packed rows is wider than the gap between a group's
             heading and its cards — without it the rows read as one
             undifferentiated field of icons (mirrors the control plane's own
             store surface). -->
        <section v-else-if="hasCuratedHome" class="space-y-10">
          <StoreSpotlight v-if="spotlight" :app="spotlight" />
          <div v-if="packedGroupRows.length" class="flex flex-col gap-12">
            <div v-for="(row, i) in packedGroupRows" :key="i" class="grid gap-x-6 gap-y-10 sm:grid-cols-4">
              <div v-for="g in row" :key="g.category" class="flex flex-col gap-4" :class="groupSpan(g.apps)">
                <h3 class="text-base font-semibold text-foreground">{{ g.label }}</h3>
                <div class="grid grid-cols-2 gap-x-6 gap-y-8" :class="groupCols(g.apps)">
                  <StoreAppCard v-for="c in g.apps" :key="c.id" :app="c" />
                </div>
              </div>
            </div>
          </div>
        </section>

        <!-- Landing with no authored home and no featured row: point the user at
             the pills / search. -->
        <p v-else-if="!featured.length" class="text-sm text-muted-foreground">
          Pick a category or search to browse apps.
        </p>
      </template>
    </section>
  </div>
</template>
