<script setup lang="ts">
// Store: browse the catalog through the brain's segmented store routes
// rather than loading the whole catalog and filtering client-side. The landing
// asks the brain for /catalog/home (the categories present on this box, the
// authored landing sections, and the older spotlight, groups and featured
// row), a category pill asks for /catalog/category?name=…, and typing asks
// /catalog/search?q=…. Every request stays same-origin on the brain, which
// serves these from the snapshot it holds, never the public catalog service
// directly (AUTH_AND_ACCESS.md: the box UI stays box-identity-gated).
//
// The three views are mutually exclusive entry points: a non-empty search box wins
// over a selected category, which wins over the landing. Selecting a pill clears
// the search, and vice versa, so the grid always reflects exactly one of them.
// Category and search are both *filtered* views (components/StoreResults.vue);
// the landing's sections and the curated Featured row never appear under a
// pill or a search.
//
// The landing (docs/specs/APP_STORE.md # Landing page) is drawn from the
// authored sections, in the order the catalog sends them:
//   - search: the search box and suggestion chips that fill it;
//   - discover: a carousel of hero and side slides (StoreDiscover);
//   - intents and packs: pack cards that open the pack page (StorePackCard);
//   - categories: the category groups, packed into rows (lib/storeLayout.ts).
// A search or category view shows right under the search section, and the
// other sections hide while it shows, so the search box never moves while the
// user types. With no search section, the filtered view goes first and the
// search box sits in the page heading.
//
// A catalog with no sections gets the older landing, in three steps so it is
// never blank: the spotlight banner and packed category groups, then the flat
// Featured row, then a plain "pick a category or search" line.
//
// Door 2 (custom-container install) is admin-only and sits as a "Custom app" link
// beside the heading, never in the browse grid (DASHBOARD.md # Door-2). Members
// never see it.
//
// All colour flows from the olive semantic tokens (style.css).
import { computed, onUnmounted, ref, watch } from "vue";
import { useQuery } from "@tanstack/vue-query";
import { Search, PackageOpen, Sparkles } from "lucide-vue-next";
import { useAuth } from "../auth";
import {
  api,
  type CatalogEntry,
  type CatalogHome,
  type CatalogCategory,
  type CatalogSearchResult,
  type HomeSection,
} from "../api";
import StoreAppCard from "../components/StoreAppCard.vue";
import StoreSpotlight from "../components/StoreSpotlight.vue";
import StoreDiscover from "../components/StoreDiscover.vue";
import StorePackCard from "../components/StorePackCard.vue";
import StoreResults from "../components/StoreResults.vue";
import Heading from "@/components/ui/Heading.vue";
import Button from "@/components/ui/Button.vue";
import { packRows, groupSpan, groupCols } from "../lib/storeLayout";

const { currentUser } = useAuth();
const isAdmin = computed(() => currentUser.value?.role === "admin");

// Free-text query and the active category pill ("recommended" = the landing).
// They are exclusive: selecting a pill clears the search, so mode() resolves
// to one view. "recommended" is never a user-visible pill, and it can't
// collide with a real category id.
const query = ref("");
const activeCategory = ref("recommended");

// Debounced search term feeding the search request: each keystroke would otherwise
// round-trip to the brain, so we wait for a short pause in typing. mode() keys off
// the live query (the view switches immediately); the request keys off searchTerm.
const searchTerm = ref("");
let debounce: ReturnType<typeof setTimeout> | undefined;
watch(query, (q) => {
  // Typing drops the active pill immediately (not debounced: mode() already
  // switches to "search" on the same tick), so a category never lingers under
  // a cleared search box and pops back in once the query empties out again.
  if (q.trim() !== "") activeCategory.value = "recommended";
  clearTimeout(debounce);
  debounce = setTimeout(() => {
    searchTerm.value = q.trim();
  }, 200);
});
onUnmounted(() => clearTimeout(debounce));

const mode = computed<"home" | "category" | "search">(() => {
  if (query.value.trim() !== "") return "search";
  if (activeCategory.value !== "recommended") return "category";
  return "home";
});

// Landing. Always enabled: it backs the pill row in every mode, so it is the
// one request the store cannot render without.
const home = useQuery({
  queryKey: ["catalog", "home"],
  queryFn: () => api.get<CatalogHome>("/catalog/home"),
});

// One category's apps. Fetched only while a pill is active.
const category = useQuery({
  queryKey: ["catalog", "category", activeCategory],
  queryFn: () =>
    api.get<CatalogCategory>(`/catalog/category?name=${encodeURIComponent(activeCategory.value)}`),
  enabled: computed(() => mode.value === "category"),
});

// Search results: apps, then packs. Fetched once the debounced term is non-empty.
const search = useQuery({
  queryKey: ["catalog", "search", searchTerm],
  queryFn: () =>
    api.get<CatalogSearchResult>(`/catalog/search?q=${encodeURIComponent(searchTerm.value)}`),
  enabled: computed(() => mode.value === "search" && searchTerm.value !== ""),
});

// Pills: the categories the landing advertised for this box, in authored order.
const categories = computed(() => home.data.value?.categories ?? []);

// --- the sectioned landing ---------------------------------------------------

// The authored sections, minus any after the first search section: one search
// box per page.
const sections = computed<HomeSection[]>(() => {
  const out: HomeSection[] = [];
  let searchSeen = false;
  for (const s of home.data.value?.sections ?? []) {
    if (s.type === "search") {
      if (searchSeen) continue;
      searchSeen = true;
    }
    out.push(s);
  }
  return out;
});
const hasSections = computed(() => sections.value.length > 0);
const hasSearchSection = computed(() => sections.value.some((s) => s.type === "search"));

function packHeading(s: HomeSection) {
  return s.title || (s.type === "intents" ? "I want to…" : "Starter packs");
}
function packGrid(s: HomeSection) {
  return s.type === "intents"
    ? "grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4"
    : "grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3";
}

// A suggestion chip fills the search box and searches at once, with no debounce.
function suggest(s: string) {
  query.value = s;
  clearTimeout(debounce);
  searchTerm.value = s.trim();
}

// --- the older landing -------------------------------------------------------

// The spotlight app and its category groups, for a catalog with no sections.
// Only meaningful in "home" mode.
const spotlight = computed<CatalogEntry | undefined>(() =>
  mode.value === "home" ? (home.data.value?.spotlight ?? undefined) : undefined,
);
const homeGroups = computed(() => (mode.value === "home" ? (home.data.value?.groups ?? []) : []));
const hasCuratedHome = computed(() => !!spotlight.value || homeGroups.value.length > 0);
const packedGroupRows = computed(() => packRows(homeGroups.value));

// Featured row: landing-only, and only as a fallback when the older landing
// has no spotlight or groups either.
const featured = computed<CatalogEntry[]>(() => {
  if (mode.value === "home" && !hasCuratedHome.value) return home.data.value?.featured ?? [];
  return [];
});

// --- the filtered views ------------------------------------------------------

const resultsMode = computed(() => (mode.value === "search" ? "search" : "category"));
const resultApps = computed<CatalogEntry[]>(() => {
  if (mode.value === "category") return category.data.value?.apps ?? [];
  if (mode.value === "search") return search.data.value?.apps ?? [];
  return [];
});
const resultPacks = computed(() => (mode.value === "search" ? (search.data.value?.packs ?? []) : []));

// resultsLoading covers the debounce gap (query typed, term not yet caught up)
// and the in-flight fetch, so the results show "Loading…" instead of flashing
// the no-matches state for a keystroke.
const resultsLoading = computed(() => {
  if (mode.value === "search") return searchTerm.value !== query.value.trim() || search.isFetching.value;
  if (mode.value === "category") return category.isLoading.value;
  return false;
});
const resultsError = computed<string | null>(() => {
  const q = mode.value === "search" ? search : mode.value === "category" ? category : null;
  if (!q?.isError.value) return null;
  return (q.error.value as Error)?.message ?? "";
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

// pillActive reports whether a pill shows as selected. A search in progress
// de-selects every pill.
function pillActive(id: string): boolean {
  return activeCategory.value === id && mode.value !== "search";
}

// Clicking a pill selects it; clicking the already-active pill toggles back to
// the landing. Picking a pill drops the search.
function selectCategory(id: string) {
  activeCategory.value = pillActive(id) ? "recommended" : id;
  query.value = "";
  searchTerm.value = "";
}

// Reset both filters back to the landing.
function clearFilters() {
  query.value = "";
  searchTerm.value = "";
  activeCategory.value = "recommended";
}
</script>

<template>
  <div class="space-y-10 pt-2">
    <section class="space-y-6">
      <!-- Page heading, with the admin-only "Custom app" link (Door 2) and,
           when the landing has no search section, the search box. -->
      <div class="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <Heading :level="2">Store</Heading>

        <div class="flex items-center gap-3">
          <RouterLink
            v-if="isAdmin"
            to="/store/custom"
            class="shrink-0 text-sm text-muted-foreground transition-colors hover:text-foreground"
          >
            Custom app
          </RouterLink>

          <div v-if="!hasSearchSection" class="relative w-full sm:w-64">
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

      <!-- Category pills: the catalog's own categories only. Highlighted only
           when browsing that category; clicking the active pill toggles back to
           the landing. -->
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

      <p v-if="home.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
      <p v-else-if="home.isError.value" class="text-sm text-destructive">
        Couldn't load the catalog. {{ (home.error.value as Error)?.message }}
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

      <!-- The sectioned landing. -->
      <div v-else-if="hasSections" class="flex flex-col gap-16 pt-4">
        <StoreResults
          v-if="!hasSearchSection && mode !== 'home'"
          :mode="resultsMode"
          :apps="resultApps"
          :packs="resultPacks"
          :label="activeCategoryLabel"
          :loading="resultsLoading"
          :error="resultsError"
          @clear="clearFilters"
        />

        <template v-for="(sec, i) in sections" :key="i">
          <!-- Search: the search box and the authored suggestions. The
               filtered view shows right under it. -->
          <template v-if="sec.type === 'search'">
            <div class="flex flex-col gap-3">
              <form
                role="search"
                class="relative flex items-center rounded-full border border-border bg-card p-1.5 pl-11 focus-within:border-accent"
                @submit.prevent="suggest(query)"
              >
                <Search
                  class="pointer-events-none absolute left-4 top-1/2 size-5 -translate-y-1/2 text-muted-foreground"
                  aria-hidden="true"
                />
                <input
                  v-model="query"
                  type="search"
                  placeholder="Search apps, or describe what you need"
                  aria-label="Search apps"
                  class="min-w-0 flex-1 bg-transparent py-2 pr-2 text-base text-foreground outline-none placeholder:text-muted-foreground"
                />
                <Button type="submit">Search</Button>
              </form>
              <div
                v-if="sec.suggestions?.length"
                class="flex flex-wrap items-center gap-2 px-4 text-sm text-muted-foreground"
              >
                <span>Try:</span>
                <button
                  v-for="s in sec.suggestions"
                  :key="s"
                  type="button"
                  class="cursor-pointer rounded-full border border-border bg-card px-3 py-0.5 text-sm text-foreground transition-colors hover:bg-muted"
                  @click="suggest(s)"
                >
                  {{ s }}
                </button>
              </div>
            </div>
            <StoreResults
              v-if="mode !== 'home'"
              :mode="resultsMode"
              :apps="resultApps"
              :packs="resultPacks"
              :label="activeCategoryLabel"
              :loading="resultsLoading"
              :error="resultsError"
              @clear="clearFilters"
            />
          </template>

          <StoreDiscover
            v-else-if="sec.type === 'discover'"
            v-show="mode === 'home'"
            :section="sec"
            :visible="mode === 'home'"
          />

          <!-- Intents and packs: pack cards that open the pack page. -->
          <section
            v-else-if="sec.type === 'intents' || sec.type === 'packs'"
            v-show="mode === 'home'"
            class="flex flex-col gap-6"
          >
            <Heading :level="2">{{ packHeading(sec) }}</Heading>
            <div :class="packGrid(sec)">
              <StorePackCard v-for="p in sec.packs ?? []" :key="p.id" :pack="p" />
            </div>
          </section>

          <!-- Categories: the groups, packed into rows. -->
          <section
            v-else-if="sec.type === 'categories'"
            v-show="mode === 'home'"
            class="flex flex-col gap-6"
          >
            <Heading :level="2">{{ sec.title || "Categories" }}</Heading>
            <div class="flex flex-col gap-12">
              <div v-for="(row, r) in packRows(sec.groups ?? [])" :key="r" class="grid gap-x-6 gap-y-10 sm:grid-cols-4">
                <div v-for="g in row" :key="g.category" class="flex flex-col gap-4" :class="groupSpan(g.apps)">
                  <h3 class="text-base font-semibold text-foreground">{{ g.label }}</h3>
                  <div class="grid grid-cols-2 gap-x-6 gap-y-8" :class="groupCols(g.apps)">
                    <StoreAppCard v-for="c in g.apps" :key="c.id" :app="c" />
                  </div>
                </div>
              </div>
            </div>
          </section>
        </template>
      </div>

      <!-- The older landing, for a catalog with no sections. -->
      <template v-else>
        <StoreResults
          v-if="mode !== 'home'"
          :mode="resultsMode"
          :apps="resultApps"
          :packs="resultPacks"
          :label="activeCategoryLabel"
          :loading="resultsLoading"
          :error="resultsError"
          @clear="clearFilters"
        />

        <!-- The spotlight banner, then the category groups, packed two or more
             to a row (lib/storeLayout.ts packRows). -->
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

        <!-- Featured row: the fallback when there is no spotlight and no group. -->
        <section v-else-if="featured.length" class="space-y-4">
          <h3 class="flex items-center gap-2 text-base font-semibold text-foreground">
            <Sparkles class="size-4 text-accent" aria-hidden="true" />
            Featured
          </h3>
          <div class="grid grid-cols-2 gap-x-6 gap-y-8 sm:grid-cols-3 lg:grid-cols-4">
            <StoreAppCard v-for="c in featured" :key="c.id" :app="c" />
          </div>
        </section>

        <p v-else class="text-sm text-muted-foreground">Pick a category or search to browse apps.</p>
      </template>
    </section>
  </div>
</template>
