<script setup lang="ts">
// Settings → System (#488): what this box runs, when it updates itself, what
// the last update did, and Update now for admins.
//
// Two parts, gated differently:
//
//   - The version card is for every signed-in user. It shows the two release
//     lines (BUILD.md # Versioning): the moose release, which is the OS, and the
//     control plane (the brain and the dashboard share one version). The brain's
//     own `version` is the control-plane number, so it is never labelled
//     "moose".
//   - The update section is for admins on a box that updates itself (hosted,
//     auto_apply). It reads GET /system/update-target, which is admin-only. On
//     the appliance it is hidden: its updates are parked (#400).
//
// The dashboard writes every sentence from `state`; `detail` is a diagnostic and
// only shows underneath, as the technical reason (UPDATES.md # 8.4).
//
// Update now replaces the brain and this dashboard, so the brain goes away
// while it runs. The page keeps the job id in sessionStorage, polls the job on
// host-agent through the brain until it ends (requests fail while the brain is
// down, and that is expected), then shows `last`. When the update replaced the
// dashboard, the page reloads once so it runs the new one. The 426 path in
// BRAIN_UI_PROTOCOL.md is not built: this reload is what stands in for it on
// the tab that pressed the button.
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { useQuery, useQueryClient } from "@tanstack/vue-query";
import { api, ApiError, type ControlPlaneOutcome, type SystemUpdateJob, type SystemVersion, type UpdateTarget } from "@/api";
import { useAuth } from "@/auth";
import Button from "@/components/ui/Button.vue";

const { currentUser } = useAuth();
const isAdmin = computed(() => currentUser.value?.role === "admin");
const queryClient = useQueryClient();

// ── Version ────────────────────────────────────────────────────────────────
const version = useQuery({
  queryKey: ["system-version"],
  queryFn: () => api.get<SystemVersion>("/system/version"),
});
const v = computed<SystemVersion | undefined>(() => version.data.value);
// The moose release is the OS. host_agent_version is the same number (the
// host-agent ships in the OS image), so it stands in when os_version is absent.
const mooseVersion = computed(() => v.value?.os_version || v.value?.host_agent_version || "");
const controlPlaneVersion = computed(() => v.value?.version || "");
const commitShort = computed(() => {
  const c = v.value?.commit ?? "";
  return c.length > 12 ? c.slice(0, 12) : c;
});
const uiImage = computed(() => v.value?.ui_image ?? "");

// ── Update status (admins) ─────────────────────────────────────────────────
// pendingKey holds the job Update now started, so a reload during the update
// comes back to the "Updating…" view.
const pendingKey = "moose.system-update.job";

function readPending(): string | null {
  try {
    return sessionStorage.getItem(pendingKey);
  } catch {
    return null;
  }
}
function writePending(id: string | null) {
  try {
    if (id) sessionStorage.setItem(pendingKey, id);
    else sessionStorage.removeItem(pendingKey);
  } catch {
    // Private mode or blocked storage: the view still works, it just will not
    // survive a reload.
  }
}

const pendingJob = ref<string | null>(readPending());
const updating = computed(() => pendingJob.value !== null);

const target = useQuery({
  queryKey: ["system-update-target"],
  queryFn: () => api.get<UpdateTarget>("/system/update-target"),
  enabled: isAdmin,
  // The loop checks every 15 minutes; a minute keeps "last checked" honest.
  refetchInterval: 60_000,
});
const t = computed<UpdateTarget | undefined>(() => target.data.value);
// The section is for a box that updates itself. The appliance waits for #400.
const showUpdates = computed(() => isAdmin.value && t.value?.auto_apply === true);

const windowStart = computed(() => t.value?.window?.split("-")[0] ?? "03:00");
const windowEnd = computed(() => t.value?.window?.split("-")[1] ?? "04:00");
const zone = computed(() => t.value?.zone || "");

const stateSentence = computed(() => {
  const s = t.value;
  if (!s) return "";
  switch (s.state) {
    case "available":
      return s.target?.version
        ? `Version ${s.target.version} will be installed tonight.`
        : "A new version will be installed tonight.";
    case "current":
      return "Your box is up to date.";
    case "none":
      return "There is no update for your box right now.";
    case "unreachable":
      return "Your box could not check for updates. It keeps running as it is and checks again in 15 minutes.";
    case "refused":
      return "Your box was offered an update it could not trust, so it did not install it. It keeps running as it is.";
    case "disabled":
      return "Your box cannot update itself, because its update settings are not valid. Please contact support.";
    case "unknown":
      return "Your box has not checked for updates yet.";
  }
  return "";
});

// A clock that ticks, so "10 minutes ago" moves while the page is open.
const nowMs = ref(Date.now());
const clock = setInterval(() => (nowMs.value = Date.now()), 30_000);
onBeforeUnmount(() => clearInterval(clock));

function ago(iso: string): string {
  const s = Math.max(0, Math.round((nowMs.value - Date.parse(iso)) / 1000));
  if (s < 60) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return m === 1 ? "1 minute ago" : `${m} minutes ago`;
  const h = Math.round(m / 60);
  if (h < 24) return h === 1 ? "1 hour ago" : `${h} hours ago`;
  const d = Math.round(h / 24);
  return d === 1 ? "1 day ago" : `${d} days ago`;
}

// when formats a time in the box's zone when it is known, so "03:04" is the
// same clock the window is read in.
function when(iso: string): string {
  const d = new Date(iso);
  const tz = zone.value || undefined;
  try {
    const day = d.toLocaleDateString(undefined, { day: "numeric", month: "short", timeZone: tz });
    const time = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", timeZone: tz });
    return `${day} at ${time}`;
  } catch {
    return d.toLocaleString();
  }
}

// failedStep says in plain words where an update stopped (the failure_mode
// values of host-agent's record).
function failedStep(mode?: string): string {
  switch (mode) {
    case "read":
      return " before it started";
    case "pull":
      return " while downloading";
    case "snapshot":
      return " while backing up the dashboard's data";
    case "declare":
      return " while preparing";
    case "recreate":
      return " while starting the new version";
    case "health":
      return ", because the new version did not start properly";
  }
  return "";
}

const last = computed<ControlPlaneOutcome | undefined>(() => t.value?.last);
const lastSentence = computed(() => {
  const o = last.value;
  if (!o) return "";
  if (o.outcome === "good") {
    return o.version ? `Updated to ${o.version} on ${when(o.at)}.` : `Updated on ${when(o.at)}.`;
  }
  const to = o.version ? `The update to ${o.version}` : "The update";
  if (o.revert_error) {
    return `${to} failed${failedStep(o.failure_mode)}, and your box could not go back to the version before it. Please contact support.`;
  }
  if (o.reverted) {
    return o.from
      ? `${to} failed${failedStep(o.failure_mode)}. Your box went back to ${o.from}.`
      : `${to} failed${failedStep(o.failure_mode)}. Your box went back to the version before it.`;
  }
  return `${to} failed${failedStep(o.failure_mode)}. Nothing on your box changed.`;
});

// ── Update now ─────────────────────────────────────────────────────────────
const confirmDialog = ref<HTMLDialogElement | null>(null);
const applyError = ref("");
const starting = ref(false);
const slow = ref(false);
let slowTimer: ReturnType<typeof setTimeout> | undefined;
let pollTimer: ReturnType<typeof setTimeout> | undefined;
onBeforeUnmount(() => {
  clearTimeout(slowTimer);
  clearTimeout(pollTimer);
});

function openConfirm() {
  applyError.value = "";
  confirmDialog.value?.showModal();
}

async function applyNow() {
  confirmDialog.value?.close();
  applyError.value = "";
  starting.value = true;
  try {
    const job = await api.post<SystemUpdateJob>("/system/update-target/apply");
    writePending(job.job_id);
    pendingJob.value = job.job_id;
  } catch (e) {
    // Both 409s mean the screen is out of date: another update is running, or
    // there is nothing left to install. Read the state again either way.
    if (e instanceof ApiError && e.status === 409) {
      applyError.value = e.message.includes("already updating")
        ? "Your box is busy with another update. Please try again in a few minutes."
        : "There is nothing to install right now.";
    } else {
      applyError.value = "The update could not start. Please try again.";
    }
    void queryClient.invalidateQueries({ queryKey: ["system-update-target"] });
  } finally {
    starting.value = false;
  }
}

// finish ends the wait. When the update replaced this dashboard, the page
// reloads once, so it runs the new one; otherwise it reads the new state.
async function finish(job: SystemUpdateJob | null) {
  clearTimeout(slowTimer);
  writePending(null);
  pendingJob.value = null;
  slow.value = false;
  if (job?.status === "completed" && job.result?.ui_changed) {
    window.location.reload();
    return;
  }
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ["system-update-target"] }),
    queryClient.invalidateQueries({ queryKey: ["system-version"] }),
  ]);
}

// poll reads the job until it ends. The job lives on host-agent, which stays
// up, so the read works again as soon as the new brain answers. A request that
// fails while the brain restarts is expected and only means "ask again".
async function poll(id: string) {
  if (pendingJob.value !== id) return;
  try {
    const job = await api.get<SystemUpdateJob>(`/system/update/${encodeURIComponent(id)}`);
    if (job.status !== "running") {
      await finish(job);
      return;
    }
  } catch (e) {
    // 404: host-agent no longer knows the job (it restarted). The outcome on
    // disk is still the answer, so stop waiting and show it.
    if (e instanceof ApiError && e.status === 404) {
      await finish(null);
      return;
    }
  }
  pollTimer = setTimeout(() => void poll(id), 3000);
}

watch(
  pendingJob,
  (id) => {
    clearTimeout(pollTimer);
    clearTimeout(slowTimer);
    if (!id) return;
    // An update takes about a minute; past ten, say so instead of spinning on.
    slowTimer = setTimeout(() => (slow.value = true), 10 * 60_000);
    void poll(id);
  },
  { immediate: true },
);
</script>

<template>
  <section class="space-y-6">
    <div class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Version</h2>
      <div class="space-y-2 rounded-xl border border-border bg-card px-4 py-4">
        <p v-if="version.isLoading.value" class="text-sm text-muted-foreground">Checking…</p>
        <p v-else-if="version.isError.value" class="text-sm text-destructive">
          Couldn't read what this box is running.
        </p>
        <template v-else>
          <dl class="space-y-1">
            <div class="flex items-baseline gap-2">
              <dt class="text-base font-medium">moose</dt>
              <dd class="text-sm text-muted-foreground">{{ mooseVersion || "Version unknown" }}</dd>
            </div>
            <div class="flex items-baseline gap-2">
              <dt class="text-base font-medium">Control plane</dt>
              <dd class="text-sm text-muted-foreground">{{ controlPlaneVersion || "Version unknown" }}</dd>
            </div>
          </dl>
          <details class="group">
            <summary class="cursor-pointer text-sm text-muted-foreground hover:text-foreground">
              Technical details
            </summary>
            <dl class="mt-2 space-y-1 text-sm">
              <div class="flex gap-2">
                <dt class="w-28 shrink-0 text-muted-foreground">Commit</dt>
                <dd class="font-mono" :title="v?.commit || ''">{{ commitShort || "Unknown" }}</dd>
              </div>
              <div class="flex gap-2">
                <dt class="w-28 shrink-0 text-muted-foreground">Host agent</dt>
                <dd>{{ v?.host_agent_version || "Unknown" }}</dd>
              </div>
              <div v-if="v?.os_slot" class="flex gap-2">
                <dt class="w-28 shrink-0 text-muted-foreground">OS slot</dt>
                <dd>{{ v.os_slot }}</dd>
              </div>
              <div class="flex gap-2">
                <dt class="w-28 shrink-0 text-muted-foreground">Dashboard</dt>
                <dd class="break-all font-mono" :title="uiImage">{{ uiImage || "Unknown" }}</dd>
              </div>
            </dl>
          </details>
        </template>
      </div>
    </div>

    <div v-if="isAdmin && target.isError.value" class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Updates</h2>
      <p class="rounded-xl border border-border bg-card px-4 py-4 text-sm text-destructive">
        Couldn't read the update status of this box.
      </p>
    </div>

    <div v-else-if="showUpdates || updating" class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Updates</h2>

      <div v-if="updating" class="space-y-2 rounded-xl border border-border bg-card px-4 py-4" aria-live="polite">
        <p class="text-base font-medium">Updating…</p>
        <p class="text-sm text-muted-foreground">
          The dashboard may stop answering for up to a minute. Your apps keep working. This page will show the result when
          the update is done.
        </p>
        <p v-if="slow" class="text-sm text-muted-foreground">
          This is taking longer than expected. Your box keeps working on it, so you can come back to this page later.
        </p>
      </div>

      <div v-else class="space-y-4 rounded-xl border border-border bg-card px-4 py-4">
        <div class="space-y-1">
          <p class="text-base font-medium">Automatic updates</p>
          <p class="text-sm text-muted-foreground">
            Your box updates itself between {{ windowStart }} and {{ windowEnd }}<template v-if="zone"> ({{ zone }})</template>.
          </p>
        </div>

        <div class="space-y-1">
          <p class="text-sm">{{ stateSentence }}</p>
          <p v-if="t?.checked_at" class="text-sm text-muted-foreground">Last checked {{ ago(t.checked_at) }}.</p>
          <details v-if="t?.detail" class="text-sm">
            <summary class="cursor-pointer text-muted-foreground hover:text-foreground">Technical reason</summary>
            <p class="mt-1 break-words font-mono text-xs text-muted-foreground">{{ t.detail }}</p>
          </details>
        </div>

        <div v-if="t?.state === 'available'" class="space-y-2">
          <p class="text-sm text-muted-foreground">
            An update replaces the dashboard and the service behind it. Your apps keep working, the dashboard is away
            for up to a minute, and your box does not restart.
          </p>
          <Button :disabled="starting" @click="openConfirm">Update now</Button>
        </div>
        <p v-if="applyError" class="text-sm text-destructive">{{ applyError }}</p>

        <div v-if="last" class="space-y-1 border-t border-border pt-4">
          <p class="text-base font-medium">Last update</p>
          <p class="text-sm" :class="last.outcome === 'good' ? '' : 'text-destructive'">{{ lastSentence }}</p>
          <details v-if="last.revert_error" class="text-sm">
            <summary class="cursor-pointer text-muted-foreground hover:text-foreground">Technical reason</summary>
            <p class="mt-1 break-words font-mono text-xs text-muted-foreground">{{ last.revert_error }}</p>
          </details>
        </div>
      </div>
    </div>

    <dialog
      ref="confirmDialog"
      aria-labelledby="update-now-title"
      class="m-auto w-[calc(100%-2rem)] max-w-md rounded-xl border border-border bg-card p-0 text-foreground backdrop:bg-black/50"
    >
      <div class="space-y-4 px-5 py-5">
        <h3 id="update-now-title" class="text-base font-medium">
          Install {{ t?.target?.version ? `version ${t.target.version}` : "the update" }} now?
        </h3>
        <p class="text-sm text-muted-foreground">
          Your apps keep working. The dashboard is away for up to a minute while it is replaced, and your box does not
          restart. If the new version does not start properly, your box goes back to the version it runs now.
        </p>
        <div class="flex justify-end gap-2">
          <Button variant="secondary" @click="confirmDialog?.close()">Cancel</Button>
          <Button @click="applyNow">Update now</Button>
        </div>
      </div>
    </dialog>
  </section>
</template>
