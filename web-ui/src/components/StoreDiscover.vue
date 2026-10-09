<script setup lang="ts">
// StoreDiscover: the landing's discover section: a carousel of pages, each a
// hero slide with up to two side slides beside it (lib/storeLayout.ts
// slidePages). It scrolls natively with scroll-snap, so touch and trackpad
// swipes work with no gesture code. Dots and the arrow keys move a page at a
// time. Mirrors the other store surface's discover section.
//
// Each slide has one button, Install, which opens the app page (/store/:id),
// where the install starts. There are no badges. On a hero slide the headline
// links to the app page too, and so does the art; the art repeats the
// headline's link, so it is left out of the tab order and the accessibility
// tree. A side slide is one link as a whole, so its button is drawn, not
// nested.
import { computed, nextTick, onUnmounted, ref, watch } from "vue";
import { RouterLink } from "vue-router";
import type { CatalogEntry, CatalogSlide, HomeSection } from "../api";
import Heading from "@/components/ui/Heading.vue";
import Button from "@/components/ui/Button.vue";
import StoreArt from "./StoreArt.vue";
import { slidePages } from "../lib/storeLayout";

const props = defineProps<{
  section: HomeSection;
  // visible is false while the landing is hidden under a search or a category.
  // A hidden scroller loses its position without a scroll event, so the
  // carousel re-reads where it is when it shows again.
  visible: boolean;
}>();

const pages = computed(() => slidePages(props.section.slides ?? []));
const title = computed(() => props.section.title || "Discover");

const track = ref<HTMLElement | null>(null);
const current = ref(0);

function appHref(app: CatalogEntry) {
  return `/store/${encodeURIComponent(app.id)}`;
}
function headline(s: CatalogSlide) {
  return s.headline || s.app.name;
}
function blurb(s: CatalogSlide) {
  return s.blurb || s.app.short_description;
}

function go(i: number) {
  const el = track.value;
  if (!el) return;
  current.value = Math.max(0, Math.min(pages.value.length - 1, i));
  el.scrollTo({ left: current.value * el.clientWidth, behavior: "smooth" });
}

function resync() {
  const el = track.value;
  if (!el) return;
  current.value = Math.round(el.scrollLeft / Math.max(1, el.clientWidth));
}

let frame = 0;
function onScroll() {
  cancelAnimationFrame(frame);
  frame = requestAnimationFrame(resync);
}
onUnmounted(() => cancelAnimationFrame(frame));

function onKey(e: KeyboardEvent) {
  if (e.target !== track.value) return;
  if (e.key === "ArrowRight") go(current.value + 1);
  else if (e.key === "ArrowLeft") go(current.value - 1);
  else return;
  e.preventDefault();
}

watch(
  () => props.visible,
  (v) => {
    if (v) nextTick(resync);
  },
);
</script>

<template>
  <section v-if="pages.length" class="flex flex-col gap-6">
    <Heading :level="2">{{ title }}</Heading>
    <div
      ref="track"
      role="region"
      aria-roledescription="carousel"
      :aria-label="title"
      :tabindex="pages.length > 1 ? 0 : undefined"
      class="flex snap-x snap-mandatory overflow-x-auto overscroll-x-contain rounded-3xl outline-offset-4 [scrollbar-width:none] focus-visible:outline-2 focus-visible:outline-accent [&::-webkit-scrollbar]:hidden"
      @scroll="onScroll"
      @keydown="onKey"
    >
      <div
        v-for="(p, i) in pages"
        :key="i"
        role="group"
        aria-roledescription="slide"
        :aria-label="`${i + 1} of ${pages.length}`"
        class="grid w-full shrink-0 snap-start gap-4 lg:grid-cols-3"
      >
        <!-- Hero slide -->
        <article
          class="flex flex-col overflow-hidden rounded-3xl border border-border bg-card sm:flex-row"
          :class="p.sides.length ? 'lg:col-span-2' : 'lg:col-span-3'"
        >
          <div class="flex min-w-0 flex-1 flex-col justify-center gap-3 p-6 sm:p-8">
            <h3 class="break-words font-display text-4xl/10 tracking-tight text-foreground">
              <RouterLink :to="appHref(p.hero.app)">{{ headline(p.hero) }}</RouterLink>
            </h3>
            <p v-if="blurb(p.hero)" class="max-w-md text-base/7 text-muted-foreground">{{ blurb(p.hero) }}</p>
            <Button :as="RouterLink" :to="appHref(p.hero.app)" class="mt-2 self-start">Install</Button>
          </div>
          <RouterLink
            :to="appHref(p.hero.app)"
            tabindex="-1"
            aria-hidden="true"
            class="grid min-h-48 place-items-center overflow-hidden bg-muted sm:min-h-72 sm:w-2/5"
          >
            <StoreArt :url="p.hero.illustration_url" :apps="[p.hero.app]" size="lg" />
          </RouterLink>
        </article>

        <!-- Side slides: each card is one link. -->
        <div v-if="p.sides.length" class="flex flex-col gap-4">
          <RouterLink
            v-for="s in p.sides"
            :key="s.app.id"
            :to="appHref(s.app)"
            class="group flex flex-1 items-center gap-4 overflow-hidden rounded-3xl border border-border bg-card p-5 transition hover:shadow-md"
          >
            <div class="flex min-w-0 flex-1 flex-col gap-1.5">
              <h3 class="font-display text-2xl/8 tracking-tight text-foreground">{{ headline(s) }}</h3>
              <p v-if="blurb(s)" class="line-clamp-2 text-sm/6 text-muted-foreground">{{ blurb(s) }}</p>
              <span
                class="mt-1.5 inline-flex items-center justify-center self-start rounded-full bg-accent px-3 py-1 text-sm/7 font-medium text-accent-foreground group-hover:bg-olive-800"
              >Install</span>
            </div>
            <div class="grid size-20 shrink-0 place-items-center overflow-hidden rounded-2xl bg-muted sm:size-24">
              <StoreArt :url="s.illustration_url" :apps="[s.app]" size="sm" />
            </div>
          </RouterLink>
        </div>
      </div>
    </div>

    <div v-if="pages.length > 1" class="flex justify-center gap-2">
      <button
        v-for="(_, i) in pages"
        :key="i"
        type="button"
        :aria-label="`Show slide ${i + 1}`"
        :aria-current="i === current ? 'true' : 'false'"
        class="cursor-pointer rounded-full transition-all"
        :class="i === current ? 'h-2 w-6 bg-accent' : 'size-2 bg-olive-300 hover:bg-olive-500'"
        @click="go(i)"
      />
    </div>
  </section>
</template>
