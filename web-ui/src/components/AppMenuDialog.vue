<script setup lang="ts">
// Per-tile quick menu (DASHBOARD.md # Tile). A small popup opened from the menu
// button beside an app name on the home launcher — a lighter-weight surface than
// the full Settings → Installed apps detail page. It shows the app's logo, name,
// and short description, then two full-width actions:
//   - the service control (Stop running / Start stopped / Retry failed), the same
//     transaction the tile and the detail page run; and
//   - a link to the app's settings page (/settings/apps/<id>).
// Only ever opened for a viewer who may control the app (AppTile gates the button
// to controllers), but the brain re-checks on every mutation regardless.
//
// short_description isn't on the instance — it lives in the catalog, so we fetch
// it lazily here (best-effort, like InstalledAppDetailSection): a Door-2 custom
// app legitimately 404s, leaving just the logo + name with no description.
import { computed, onMounted, onUnmounted, ref } from "vue";
import { RouterLink } from "vue-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/vue-query";
import { Settings, X } from "lucide-vue-next";
import { api, waitForJob, type Instance, type CatalogDetail, type Job } from "../api";
import AppGlyph from "./AppGlyph.vue";

const props = defineProps<{ instance: Instance }>();
const emit = defineEmits<{ close: [] }>();

const qc = useQueryClient();

const running = computed(() => props.instance.state === "running");
const stopped = computed(() => props.instance.state === "stopped");
// `failed` shares the Start control as a click-to-retry (#154): the brain runs
// the identical Start transaction from `stopped` or `failed`.
// An update whose rollback did not bring the old version back (update_failed,
// #631) is retried the same way.
const failed = computed(() => props.instance.state === "failed" || props.instance.state === "update_failed");

// Catalog lookup for the short description. Best-effort: no retry (a Door-2 app
// legitimately 404s) and any failure just leaves the logo + name with no blurb.
const catalogQuery = useQuery({
  queryKey: computed(() => ["catalog", props.instance.manifest_id]),
  queryFn: () => api.get<CatalogDetail>(`/catalog/${props.instance.manifest_id}`),
  enabled: computed(() => !!props.instance.manifest_id),
  retry: false,
});
const shortDescription = computed(() => catalogQuery.data.value?.short_description ?? "");

function invalidate() {
  qc.invalidateQueries({ queryKey: ["apps"] });
  qc.invalidateQueries({ queryKey: ["apps", props.instance.id] });
}

// awaitJob polls to terminal and throws on a failed job, so a job-level failure
// (e.g. compose up never goes healthy) surfaces via useMutation's isError.
async function awaitJob(job: Job): Promise<Job> {
  const done = await waitForJob(job.job_id);
  if (done.status === "failed") throw new Error(done.error?.message || "the operation failed");
  return done;
}

const stop = useMutation({
  mutationFn: async () => awaitJob(await api.post<Job>(`/apps/${props.instance.id}/stop`)),
  onSettled: invalidate,
});
const start = useMutation({
  mutationFn: async () => awaitJob(await api.post<Job>(`/apps/${props.instance.id}/start`)),
  onSettled: invalidate,
});
const busy = computed(() => stop.isPending.value || start.isPending.value);

// Falls the logo back to the glyph if the icon asset fails to load. (The popup
// stays open after a stop/start so the user sees the control flip Stop ↔ Start
// as the instance prop updates from the invalidated query.)
const brokenIcon = ref(false);

// Focus trap: keeps keyboard navigation inside the card so Tab/Shift-Tab cannot
// reach tiles behind the overlay. cardRef targets the card div; focusableEls
// collects interactive descendants in DOM order.
const cardRef = ref<HTMLElement | null>(null);

function focusableEls() {
  return Array.from(
    cardRef.value?.querySelectorAll<HTMLElement>(
      'button:not([disabled]),a[href],[tabindex]:not([tabindex="-1"])'
    ) ?? []
  );
}

function trapFocus(e: KeyboardEvent) {
  if (e.key !== "Tab") return;
  const els = focusableEls();
  if (!els.length) return;
  const first = els[0]!;
  const last = els[els.length - 1]!;
  if (e.shiftKey) {
    if (document.activeElement === first) { e.preventDefault(); last.focus(); }
  } else {
    if (document.activeElement === last) { e.preventDefault(); first.focus(); }
  }
}

// Escape closes the popup — expected of a transient overlay (the backdrop click
// and the X button are the other two dismissals).
function onKey(e: KeyboardEvent) {
  if (e.key === "Escape") emit("close");
}
onMounted(() => {
  window.addEventListener("keydown", onKey);
  focusableEls()[0]?.focus();
});
onUnmounted(() => window.removeEventListener("keydown", onKey));
</script>

<template>
  <Teleport to="body">
    <div
      class="fixed inset-0 z-50 grid place-items-center bg-black/40 px-4"
      @click.self="emit('close')"
    >
      <div
        ref="cardRef"
        class="w-full max-w-sm space-y-5 rounded-xl border border-border bg-card p-6 shadow-lg"
        @keydown="trapFocus"
      >
        <!-- Header: logo · name/description · close -->
        <div class="flex items-start gap-4">
          <div
            class="grid size-14 shrink-0 place-items-center overflow-hidden rounded-2xl border border-border bg-card text-muted-foreground"
          >
            <img
              v-if="instance.icon_url && !brokenIcon"
              :src="instance.icon_url"
              :alt="`${instance.name} icon`"
              class="size-1/2 object-contain"
              @error="brokenIcon = true"
            />
            <AppGlyph v-else :name="instance.icon_glyph" class="size-7" />
          </div>
          <div class="min-w-0 flex-1">
            <h2 class="truncate text-base font-medium">{{ instance.name }}</h2>
            <p v-if="shortDescription" class="mt-0.5 text-sm text-muted-foreground">
              {{ shortDescription }}
            </p>
          </div>
          <button
            type="button"
            class="-mr-1 -mt-1 shrink-0 rounded-lg p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
            aria-label="Close"
            @click="emit('close')"
          >
            <X class="size-4" />
          </button>
        </div>

        <!-- Full-width actions: service control + settings link -->
        <div class="space-y-2">
          <button
            v-if="running"
            type="button"
            class="w-full rounded-lg border border-border px-3 py-2 text-sm hover:bg-muted disabled:opacity-50"
            :disabled="busy"
            @click="stop.mutate()"
          >
            {{ stop.isPending.value ? "Stopping…" : "Stop service" }}
          </button>
          <button
            v-else
            type="button"
            class="w-full rounded-lg border border-border px-3 py-2 text-sm hover:bg-muted disabled:opacity-50"
            :disabled="busy"
            @click="start.mutate()"
          >
            <template v-if="failed">{{ start.isPending.value ? "Retrying…" : "Retry" }}</template>
            <template v-else>{{ start.isPending.value ? "Starting…" : "Start service" }}</template>
          </button>

          <RouterLink
            :to="`/settings/apps/${instance.id}`"
            class="flex w-full items-center justify-center gap-1.5 rounded-lg border border-border px-3 py-2 text-sm hover:bg-muted"
            @click="emit('close')"
          >
            <Settings class="size-4" />
            App settings
          </RouterLink>
        </div>

        <!-- Action error surface (job failure / 409 / host 5xx). Gated to the
             state where the relevant button is visible so a stale stop error
             doesn't linger after the user successfully starts the app. -->
        <p v-if="running && stop.isError.value" class="text-sm text-destructive">
          Couldn't stop: {{ (stop.error.value as Error)?.message }}
        </p>
        <p v-if="!running && start.isError.value" class="text-sm text-destructive">
          Couldn't start: {{ (start.error.value as Error)?.message }}
        </p>
      </div>
    </div>
  </Teleport>
</template>
