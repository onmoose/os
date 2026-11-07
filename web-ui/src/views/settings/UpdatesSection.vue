<script setup lang="ts">
// Settings → Updates (UPDATES.md # 6 Where updates appear, Failure signaling,
// #634): one view of what the app updates did. "X apps updated last night, Y
// waiting on you, Z failed", then the three lists:
//
//   - Updated recently: each good update that can still be rolled back (7
//     days), with Go back to <version> (RollbackButton).
//   - Waiting for you: an update that asks for more access (Allow & update for
//     whoever may allow it, #633), and an update the box does not apply on its
//     own because the app's automatic update is off.
//   - Did not update: the last update that did not go through, with the step
//     that broke, the message, the paused text (#632) and View logs, which
//     opens the app's logs.
//
// The lists are drawn from the apps list the dashboard already loads (GET
// /apps carries every field), so there is no read of its own. A person sees
// the apps they control: an admin all of them, a member their own personal
// apps.
import { computed } from "vue";
import { RouterLink } from "vue-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/vue-query";
import { TriangleAlert } from "lucide-vue-next";
import { api, waitForJob, type Instance, type Job } from "@/api";
import { useAuth } from "@/auth";
import { wantLines } from "@/installSteps";
import { PAUSED_TEXT, failedStep, lastNight, plural } from "@/updates";
import Button from "@/components/ui/Button.vue";
import PermissionList from "@/components/PermissionList.vue";
import RollbackButton from "@/components/RollbackButton.vue";

const qc = useQueryClient();
const { currentUser } = useAuth();
const isAdmin = computed(() => currentUser.value?.role === "admin");

const appsQuery = useQuery({
  queryKey: ["apps"],
  queryFn: () => api.get<{ apps: Instance[] }>("/apps"),
});
// The brain's control gate: an admin controls every app, a member only their
// own personal ones.
const apps = computed(() =>
  (appsQuery.data.value?.apps ?? []).filter((a) => isAdmin.value || a.scope === "personal"),
);

const updated = computed(() => apps.value.filter((a) => !!a.update.updated_from));
const waiting = computed(() =>
  apps.value.filter(
    (a) =>
      a.update.status === "needs_approval" ||
      (a.update.status === "available" && !a.auto_update && a.update.can_apply),
  ),
);
const failed = computed(() => apps.value.filter((a) => !!a.update.failed_version));

const summary = computed(() => {
  const n = lastNight(apps.value).length;
  return [
    `${plural(n, "app", "apps")} updated last night`,
    `${waiting.value.length} waiting on you`,
    `${failed.value.length} failed`,
  ].join(", ") + ".";
});

function date(at: string | undefined | null): string {
  return at ? new Date(at).toLocaleString() : "";
}

function waitsFor(a: Instance): string {
  if (a.scope === "household") return "It waits for an admin to allow it.";
  return `It waits for ${a.owner_username || "its owner"} to allow it. Only they can allow it.`;
}

// Allow & update, or Update for an app whose automatic update is off: the
// same jobs as on the app's settings page.
const run = useMutation({
  mutationFn: async (a: Instance) => {
    const job =
      a.update.status === "needs_approval"
        ? await api.post<Job>(`/apps/${a.id}/update/approve`, { version: a.update.available_version })
        : await api.post<Job>(`/apps/${a.id}/update`);
    const done = await waitForJob(job.job_id);
    if (done.status === "failed") throw new Error(done.error?.message || "the update did not go through");
    return done;
  },
  onSettled: (_d, _e, a) => {
    qc.invalidateQueries({ queryKey: ["apps"] });
    qc.invalidateQueries({ queryKey: ["apps", a.id] });
  },
});
</script>

<template>
  <div class="space-y-6">
    <div class="space-y-1">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Updates</h2>
      <p class="text-sm text-muted-foreground">What moose did with your apps' updates, and what waits for you.</p>
    </div>

    <p v-if="appsQuery.isPending.value" class="text-sm text-muted-foreground">Loading…</p>
    <p v-else-if="appsQuery.isError.value" class="text-sm text-destructive">
      Couldn't load your apps: {{ (appsQuery.error.value as Error)?.message }}
    </p>
    <template v-else>
      <p class="text-sm">{{ summary }}</p>

      <section class="space-y-2">
        <h3 class="text-sm font-medium">Updated recently</h3>
        <p v-if="updated.length === 0" class="text-sm text-muted-foreground">No app was updated in the last 7 days.</p>
        <div
          v-for="a in updated"
          :key="a.id"
          class="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card px-4 py-3"
        >
          <div class="min-w-0 flex-1">
            <RouterLink :to="`/settings/apps/${a.id}`" class="text-sm font-medium hover:underline">{{ a.name }}</RouterLink>
            <div class="text-xs text-muted-foreground">
              Updated from {{ a.update.updated_from }} to {{ a.version }}
              <template v-if="a.update.updated_overnight"> overnight</template>, {{ date(a.update.updated_at) }}.
              <template v-if="a.update.can_rollback">
                You can go back to {{ a.update.updated_from }} until {{ date(a.update.rollback_until) }}.
              </template>
            </div>
          </div>
          <RollbackButton v-if="a.update.can_rollback" :app="a" />
        </div>
      </section>

      <section class="space-y-2">
        <h3 class="text-sm font-medium">Waiting for you</h3>
        <p v-if="waiting.length === 0" class="text-sm text-muted-foreground">Nothing waits for you.</p>
        <div
          v-for="a in waiting"
          :key="a.id"
          class="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card px-4 py-3"
        >
          <div class="min-w-0 flex-1">
            <RouterLink :to="`/settings/apps/${a.id}`" class="text-sm font-medium hover:underline">{{ a.name }}</RouterLink>
            <div v-if="a.update.status === 'needs_approval'" class="space-y-1 text-xs text-muted-foreground">
              <p>{{ a.name }} {{ a.update.available_version }} wants new access:</p>
              <PermissionList :lines="wantLines(a.update.wants)" />
              <p v-if="!a.update.can_approve">{{ waitsFor(a) }}</p>
            </div>
            <div v-else class="text-xs text-muted-foreground">
              Version {{ a.update.available_version }} is available. Automatic updates are off for this app.
            </div>
          </div>
          <Button
            v-if="a.update.can_approve || a.update.can_apply"
            size="sm"
            :disabled="run.isPending.value || a.state === 'updating'"
            @click="run.mutate(a)"
          >
            <template v-if="run.isPending.value && run.variables.value?.id === a.id">Updating…</template>
            <template v-else-if="a.update.status === 'needs_approval'">Allow & update</template>
            <template v-else>Update to {{ a.update.available_version }}</template>
          </Button>
        </div>
        <p v-if="run.isError.value" class="text-sm text-destructive">
          Couldn't update: {{ (run.error.value as Error)?.message }}
        </p>
      </section>

      <section class="space-y-2">
        <h3 class="text-sm font-medium">Did not update</h3>
        <p v-if="failed.length === 0" class="text-sm text-muted-foreground">No update failed.</p>
        <div
          v-for="a in failed"
          :key="a.id"
          class="flex flex-wrap items-start gap-3 rounded-xl border border-border bg-card px-4 py-3"
        >
          <TriangleAlert class="mt-0.5 size-4 shrink-0 text-amber-600" />
          <div class="min-w-0 flex-1 space-y-0.5 text-xs text-muted-foreground">
            <RouterLink :to="`/settings/apps/${a.id}`" class="text-sm font-medium text-foreground hover:underline">
              {{ a.name }}
            </RouterLink>
            <p>
              Could not update to {{ a.update.failed_version }}<template v-if="a.update.failed_overnight"> overnight</template>,
              {{ date(a.update.failed_at) }}. {{ failedStep(a.update.failed_kind) }}
            </p>
            <p v-if="a.state === 'update_failed'">The old version did not start again. Open the app's settings to start it.</p>
            <p v-else>It still runs version {{ a.version }}.</p>
            <p v-if="a.update.failed_message">{{ a.update.failed_message }}</p>
            <p v-if="a.update.paused" class="text-amber-700">
              {{ PAUSED_TEXT }} moose no longer tries it at night, but you can still update it by hand.
            </p>
          </div>
          <RouterLink
            :to="`/settings/apps/${a.id}#logs`"
            class="text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground"
          >
            View logs
          </RouterLink>
        </div>
      </section>
    </template>
  </div>
</template>
