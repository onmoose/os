<script setup lang="ts">
// Settings → Installed apps: the per-app management index. Each row links to
// that instance's detail page (InstalledAppDetailSection), where stop/start,
// uninstall, and logs live. The list itself is just navigation now; it used to
// carry inline Logs/Uninstall buttons before the detail page existed.
//
// The apps list is visibility-scoped server-side, so a member only sees
// household apps and their own personal apps; the detail page re-checks every
// action against the brain.
//
// One card, stacked-list rows (DASHBOARD.md # Settings), built on the Tailwind
// Plus "In card with links" component. Each row: logo, name + short
// description, a readable status, and a second line with owner/version/access.
import { computed } from "vue";
import { useQuery } from "@tanstack/vue-query";
import { RouterLink } from "vue-router";
import { api, type Instance } from "@/api";
import { useAuth, isHosted } from "@/auth";
import { useHealth } from "@/useHealth";
import { ChevronRight } from "lucide-vue-next";
import AppGlyph from "@/components/AppGlyph.vue";

const { singleUserMode } = useAuth();
const { activeIssues } = useHealth();

const apps = useQuery({
  queryKey: ["apps"],
  queryFn: () => api.get<{ apps: Instance[] }>("/apps"),
});

// A running app with an open restart-loop or unresponsive issue reads as
// "Needs attention" instead of "Running" — the health surface already tracks
// these per-instance (HEALTH.md `container-restart-loop`, `app-unresponsive`).
const attentionIDs = computed(() => {
  const ids = new Set<string>();
  for (const issue of activeIssues.value) {
    if (
      (issue.category === "container-restart-loop" || issue.category === "app-unresponsive") &&
      issue.instance_key
    ) {
      ids.add(issue.instance_key);
    }
  }
  return ids;
});

type StatusDot = "green" | "gray" | "amber";

function status(app: Instance): { dot: StatusDot; label: string } {
  if (app.state === "running") {
    return attentionIDs.value.has(app.id)
      ? { dot: "amber", label: "Needs attention" }
      : { dot: "green", label: "Running" };
  }
  if (app.state === "stopped") return { dot: "gray", label: "Stopped" };
  if (app.state === "failed") return { dot: "amber", label: "Failed" };
  return { dot: "amber", label: readableState(app.state) };
}

// Any other raw state string, turned into a plain word — never shown lowercase
// verbatim (e.g. "installing" -> "Installing").
function readableState(state: string): string {
  if (!state) return "Unknown";
  return state.charAt(0).toUpperCase() + state.slice(1).replace(/_/g, " ");
}

const dotClass: Record<StatusDot, string> = {
  green: "bg-emerald-500",
  gray: "bg-muted-foreground",
  amber: "bg-amber-500",
};

// Same three-way access label as AppTile's globe badge and the detail page's
// Access control, so the three places can't disagree (hosted-only).
function access(app: Instance): string | null {
  if (!isHosted()) return null;
  if (app.exposure === "public") return "Public";
  return (app.public_paths ?? []).length > 0 ? "Partly public" : "Only me";
}

function detailParts(app: Instance): string[] {
  const parts: string[] = [];
  if (!singleUserMode.value) {
    parts.push(app.scope === "household" ? "Shared" : app.owner_username);
  }
  if (app.version) parts.push(`v${app.version}`);
  const a = access(app);
  if (a) parts.push(a);
  return parts;
}
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
    <ul v-else role="list" class="divide-y divide-border overflow-hidden bg-card sm:rounded-xl sm:border sm:border-border">
      <li v-for="a in apps.data.value!.apps" :key="a.id" class="relative flex items-center justify-between gap-3 px-4 py-4 hover:bg-muted sm:px-6">
        <div class="flex min-w-0 items-center gap-3">
          <div class="flex size-10 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-border bg-muted">
            <img v-if="a.icon_url" :src="a.icon_url" :alt="`${a.name} icon`" class="size-full object-contain" />
            <AppGlyph v-else :name="a.icon_glyph" class="size-5 text-muted-foreground" />
          </div>
          <div class="min-w-0">
            <p class="text-sm font-medium text-foreground">
              <RouterLink :to="`/settings/apps/${a.id}`">
                <span class="absolute inset-0" />
                {{ a.name }}
              </RouterLink>
            </p>
            <p v-if="a.short_description" class="mt-0.5 truncate text-xs text-muted-foreground">{{ a.short_description }}</p>
          </div>
        </div>
        <div class="flex shrink-0 items-center gap-3">
          <div class="flex flex-col items-end">
            <div class="flex items-center gap-1.5">
              <span class="size-1.5 shrink-0 rounded-full" :class="dotClass[status(a).dot]" />
              <span class="text-sm text-foreground">{{ status(a).label }}</span>
            </div>
            <p v-if="detailParts(a).length" class="mt-1 hidden text-xs text-muted-foreground sm:block">
              {{ detailParts(a).join(" · ") }}
            </p>
          </div>
          <ChevronRight class="size-4 shrink-0 text-muted-foreground" />
        </div>
      </li>
    </ul>
  </section>
</template>
