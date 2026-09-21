<script setup lang="ts">
// Settings → Activity: the audit-log browser (LOGGING.md # Activity (audit log),
// issue #11). Lives as a section of the Settings left-nav shell
// (SettingsLayout.vue). Consumes the already-built GET /api/v1/audit: admins see
// the full box-wide feed, members see only events where they are the actor or the
// target. The brain enforces that split server-side (LOGGING.md # Visibility
// rules). This view renders whatever the API returns and does no client-side row
// filtering. Open to all authenticated users (not admin-gated), unlike the
// sibling Users section.
import { computed } from "vue";
import { useQuery, useInfiniteQuery } from "@tanstack/vue-query";
import { api, type AuditEvent, type User } from "@/api";
import { useAuth } from "@/auth";
import Button from "@/components/ui/Button.vue";

const { currentUser } = useAuth();
const isAdmin = computed(() => currentUser.value?.role === "admin");

const PAGE = 50;

// Append-style pagination. The API returns rows newest-first and has no has_more
// field; passing after_id = <oldest id loaded> walks backward into older rows.
// "Load more" shows while the last page came back full (length === PAGE).
const audit = useInfiniteQuery({
  queryKey: ["audit"],
  initialPageParam: 0,
  queryFn: ({ pageParam }) =>
    api.get<{ events: AuditEvent[] }>(`/audit?limit=${PAGE}${pageParam ? `&after_id=${pageParam}` : ""}`),
  getNextPageParam: (last) =>
    last.events.length < PAGE ? undefined : last.events[last.events.length - 1]?.id,
});

const events = computed<AuditEvent[]>(() => audit.data.value?.pages.flatMap((p) => p.events) ?? []);

// Admin name resolution: actor_user_id → username via the admin-only user list.
// Members can't call /users, so for them this stays empty. A member can still
// receive events where an admin acted on them (LOGGING.md # Visibility rules:
// actor-or-target). currentUser names the self rows, and any id we can't
// resolve degrades to a role label rather than leaking a raw UUID.
const usersQuery = useQuery({
  queryKey: ["users"],
  queryFn: () => api.get<{ users: User[] }>("/users"),
  enabled: isAdmin,
});
const nameById = computed(() => {
  const m = new Map<string, string>();
  for (const u of usersQuery.data.value?.users ?? []) m.set(u.id, u.display_name);
  return m;
});

// Resolve a user id to the person's name, or null when we can't. Never the raw
// UUID. currentUser always names the signed-in user; the /users map names the
// rest for an admin; a member viewing another user's id falls through to null.
function userName(id: string): string | null {
  if (id === currentUser.value?.id) return currentUser.value!.display_name;
  return nameById.value.get(id) ?? null;
}

function actorName(e: AuditEvent): string {
  if (e.actor_role === "system" || !e.actor_user_id) return "System";
  return userName(e.actor_user_id) ?? (e.actor_role === "admin" ? "An administrator" : "Another user");
}

// Target: app slug / person / health-issue key, by target_kind. A user target
// resolves to that person's name when we can name it, else a generic label.
// Names are resolved at render time from the current user list, so a person who
// has been renamed reads under their new name throughout the feed.
function targetText(e: AuditEvent): string {
  if (!e.target_kind || !e.target_id) return "None";
  if (e.target_kind === "user") return userName(e.target_id) ?? "A user";
  return e.target_id;
}

// Plain-English labels for the v1 action vocabulary (LOGGING.md # v1 action
// vocabulary). An unmapped action degrades to its raw string rather than hiding,
// so a newly added action never silently disappears from the feed.
const ACTION_LABELS: Record<string, string> = {
  "setup.complete": "First admin created",
  "login.success": "Signed in",
  "login.failure": "Failed sign-in attempt",
  "login.lockout": "Account locked (too many failures)",
  logout: "Signed out",
  "app.install": "App installed",
  "app.uninstall": "App uninstalled",
  "app.custom.create": "Custom container installed",
  "user.create": "User created",
  "user.role.change": "Role changed",
  "user.rename": "Name changed",
  "user.delete": "User deleted",
  "user.password.reset": "Password reset by admin",
  "user.password.change": "Password changed",
  "recover.success": "Recovery code used",
  "recover.failure": "Failed recovery attempt",
  "auth.elevate.success": "Elevation granted",
  "auth.elevate.failure": "Failed elevation attempt",
  "health.issue.raised": "Health issue detected",
  "health.issue.cleared": "Health issue resolved",
};
function actionLabel(a: string): string {
  return ACTION_LABELS[a] ?? a;
}

function fmtAbsolute(ms: number): string {
  return new Date(ms).toLocaleString();
}
// Mirrors NotificationBell's relative-time wording for a consistent feel.
function relativeTime(ms: number): string {
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

// Export the currently-loaded rows (LOGGING.md # Activity: export-to-file is part
// of the v1 spec). CSV for spreadsheets, JSON for "keep my own copy". Scope is the
// rows on screen, including any pages the user expanded via Load more.
function triggerDownload(filename: string, body: string, mime: string) {
  const blob = new Blob([body], { type: mime });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
const CSV_COLS = [
  "id", "ts", "actor_role", "actor_user_id", "action",
  "target_kind", "target_id", "source_ip", "success", "metadata",
] as const;
function csvCell(v: unknown): string {
  const s = v == null ? "" : String(v);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}
function exportCsv() {
  const header = CSV_COLS.join(",");
  const rows = events.value.map((e) =>
    CSV_COLS.map((c) => csvCell((e as Record<string, unknown>)[c])).join(","),
  );
  triggerDownload("activity.csv", [header, ...rows].join("\n"), "text/csv");
}
function exportJson() {
  triggerDownload("activity.json", JSON.stringify(events.value, null, 2), "application/json");
}
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-center justify-between gap-2">
      <p class="text-sm text-muted-foreground">
        {{ isAdmin ? "Everything that happened on this box." : "Your account activity." }}
      </p>
      <div class="flex gap-2">
        <Button variant="secondary" size="sm" :disabled="events.length === 0" @click="exportCsv">
          Export CSV
        </Button>
        <Button variant="secondary" size="sm" :disabled="events.length === 0" @click="exportJson">
          Export JSON
        </Button>
      </div>
    </div>

    <p v-if="audit.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
    <p v-else-if="audit.isError.value" class="text-sm text-destructive">Couldn't load activity.</p>
    <p v-else-if="events.length === 0" class="text-sm text-muted-foreground">No activity yet.</p>

    <div v-else class="overflow-x-auto rounded-xl border border-border bg-card">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th class="px-4 py-2 font-medium">When</th>
            <th class="px-4 py-2 font-medium">Who</th>
            <th class="px-4 py-2 font-medium">Action</th>
            <th class="px-4 py-2 font-medium">Target</th>
            <th class="px-4 py-2 font-medium">From</th>
            <th class="px-4 py-2 font-medium">Result</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="e in events" :key="e.id" class="border-b border-border/60 last:border-0">
            <td class="whitespace-nowrap px-4 py-2 text-muted-foreground" :title="fmtAbsolute(e.ts)">
              {{ relativeTime(e.ts) }}
            </td>
            <td class="whitespace-nowrap px-4 py-2">{{ actorName(e) }}</td>
            <td class="px-4 py-2">{{ actionLabel(e.action) }}</td>
            <td class="px-4 py-2 text-muted-foreground">{{ targetText(e) }}</td>
            <td class="whitespace-nowrap px-4 py-2 text-muted-foreground">{{ e.source_ip || "Unknown" }}</td>
            <td class="whitespace-nowrap px-4 py-2">
              <span
                class="rounded-full px-2 py-0.5 text-xs"
                :class="e.success ? 'bg-emerald-500/10 text-emerald-600' : 'bg-destructive/10 text-destructive'"
              >
                {{ e.success ? "OK" : "Failed" }}
              </span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="audit.hasNextPage.value" class="flex justify-center">
      <Button
        variant="secondary"
        size="sm"
        :disabled="audit.isFetchingNextPage.value"
        @click="audit.fetchNextPage()"
      >
        {{ audit.isFetchingNextPage.value ? "Loading…" : "Load more" }}
      </Button>
    </div>
  </div>
</template>
