<script setup lang="ts">
// Pack page (/store/packs/:id): one pack's title, description and art, and the
// apps it holds. A pack never installs in one step. Each app has its own
// Install button (StorePackApp), which runs that app's normal install flow, so
// the user installs the pack one app at a time and sees each app's own steps.
//
// The pack comes from GET /api/v1/catalog/pack?id=, which returns 404 for a
// pack this box cannot show: the catalog does not carry it, or one of its apps
// cannot run here (APP_STORE.md # Landing page).
import { computed } from "vue";
import { useRoute, RouterLink } from "vue-router";
import { useQuery } from "@tanstack/vue-query";
import { ArrowLeft } from "lucide-vue-next";
import { api, ApiError, type CatalogPack } from "../api";
import Heading from "@/components/ui/Heading.vue";
import StoreArt from "../components/StoreArt.vue";
import StorePackApp from "../components/StorePackApp.vue";

const route = useRoute();
const packId = computed(() => String(route.params.id));

const packQuery = useQuery({
  queryKey: computed(() => ["catalog", "pack", packId.value]),
  queryFn: () => api.get<CatalogPack>(`/catalog/pack?id=${encodeURIComponent(packId.value)}`),
  retry: (n, err) => !(err instanceof ApiError && err.status === 404) && n < 2,
});
const pack = computed(() => packQuery.data.value ?? null);
const apps = computed(() => pack.value?.apps ?? []);
const notFound = computed(() => packQuery.error.value instanceof ApiError && packQuery.error.value.status === 404);
</script>

<template>
  <div class="flex flex-col gap-8 pt-2">
    <RouterLink
      to="/store"
      class="inline-flex w-fit items-center gap-1.5 text-sm text-muted-foreground transition-colors hover:text-foreground"
    >
      <ArrowLeft class="size-4" aria-hidden="true" />
      Store
    </RouterLink>

    <p v-if="packQuery.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
    <p v-else-if="notFound" class="text-sm text-muted-foreground">This pack is not available on this box.</p>
    <p v-else-if="packQuery.isError.value" class="text-sm text-destructive">
      Couldn't load this pack. {{ (packQuery.error.value as Error)?.message }}
    </p>

    <template v-else-if="pack">
      <header class="grid gap-8 lg:grid-cols-[1fr_20rem] lg:items-center">
        <div class="flex min-w-0 flex-col gap-4">
          <Heading :level="1" class="text-balance">{{ pack.title }}</Heading>
          <p v-if="pack.description" class="text-lg/8 text-muted-foreground">{{ pack.description }}</p>
        </div>
        <div class="grid aspect-[4/3] place-items-center overflow-hidden rounded-3xl border border-border bg-card">
          <StoreArt :url="pack.illustration_url" :apps="apps" size="lg" />
        </div>
      </header>

      <section class="flex flex-col gap-4">
        <h2 class="text-base font-semibold text-foreground">In this pack</h2>
        <p class="text-sm text-muted-foreground">Install the apps one at a time. Each one asks only what it needs.</p>
        <ul class="flex flex-col divide-y divide-border rounded-3xl border border-border bg-card">
          <StorePackApp v-for="a in apps" :key="a.id" :app="a" />
        </ul>
      </section>
    </template>
  </div>
</template>
