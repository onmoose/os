<script setup lang="ts">
// Settings → Installed apps: the per-app management index. Each row links to
// that instance's detail page (InstalledAppDetailSection), where stop/start,
// uninstall, and logs live. The list itself is just navigation now; it used to
// carry inline Logs/Uninstall buttons before the detail page existed.
//
// The apps list is visibility-scoped server-side, so a member only sees
// household apps and their own personal apps; the detail page re-checks every
// action against the brain.
import { useQuery } from "@tanstack/vue-query";
import { RouterLink } from "vue-router";
import { api, type Instance } from "@/api";
import { useAuth } from "@/auth";
import { ChevronRight } from "lucide-vue-next";

const { singleUserMode } = useAuth();

const apps = useQuery({
  queryKey: ["apps"],
  queryFn: () => api.get<{ apps: Instance[] }>("/apps"),
});
</script>

<template>
  <section class="space-y-3">
    <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Installed apps</h2>
    <p v-if="apps.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
    <p
      v-else-if="(apps.data.value?.apps.length ?? 0) === 0"
      class="text-sm text-muted-foreground"
    >
      Nothing installed yet.
    </p>
    <ul v-else class="space-y-2">
      <li v-for="a in apps.data.value!.apps" :key="a.id">
        <RouterLink
          :to="`/settings/apps/${a.id}`"
          class="flex items-center justify-between gap-3 rounded-xl border border-border bg-card px-4 py-3 hover:bg-muted"
        >
          <div class="flex items-baseline gap-2">
            <strong class="text-sm">{{ a.name }}</strong>
            <span v-if="!singleUserMode" class="text-xs text-muted-foreground">{{ a.scope === "household" ? "Shared" : a.owner_username }}</span>
            <span class="text-xs text-muted-foreground">· {{ a.state }}</span>
          </div>
          <ChevronRight class="size-4 shrink-0 text-muted-foreground" />
        </RouterLink>
      </li>
    </ul>
  </section>
</template>
