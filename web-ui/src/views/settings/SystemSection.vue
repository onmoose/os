<script setup lang="ts">
// Settings → System (#488, #509): what this box runs, when it updates itself,
// what the last updates did, and Update now for admins.
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
// Update now covers both streams (UPDATES.md # 8.4): the control plane
// (stream B), which replaces the brain and this dashboard, and an OS release
// that is installed and waiting (stream A, #509), which restarts the box. So
// the brain, or the whole box, goes away while it runs. The page keeps what it
// waits for in sessionStorage, and requests that fail meanwhile only mean "ask
// again". Stream B is followed through its job on host-agent; when it replaced
// the dashboard, the page reloads once so it runs the new one. Stream A is
// followed through the update-target read: done when the box runs the new
// release, or when a new OS outcome (os.last) names it. The 426 path in
// BRAIN_UI_PROTOCOL.md is not built: the reload stands in for it on the tab
// that pressed the button.
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { useQuery, useQueryClient } from "@tanstack/vue-query";
import { api, ApiError, type ControlPlaneOutcome, type SystemUpdateJob, type SystemVersion, type UpdateTarget } from "@/api";
import { useAuth } from "@/auth";
import Button from "@/components/ui/Button.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";

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
// Pending is what Update now waits for, kept in sessionStorage so a reload
// (ours after a dashboard change, or the admin's) comes back to "Updating…".
type Pending = {
  // job is stream B's update job, null when Update now had no stream B part.
  job: string | null;
  // os is the OS release being switched to, null when there was none, and
  // osLast the id of the last OS outcome before the click.
  os: string | null;
  osLast: string | null;
  // cpJob is stream B's job id, kept after `job` is cleared, so the page can
  // still tell from `last` whether that update replaced the dashboard when
  // the OS restart hid the job (host-agent forgets jobs when it restarts).
  // uiReloaded says the page already reloaded for it.
  cpJob?: string | null;
  uiReloaded?: boolean;
  // osJob is the os-switch job when Update now started it at once (an OS-only
  // click). A switch that fails before the restart ends that job "failed",
  // with no new os.last, so this is how the wait learns of it.
  osJob?: string | null;
  // at is when Update now was pressed, ms since the epoch, and osAt when the
  // OS part began (stream B done, or the click when there was none).
  at: number;
  osAt?: number;
};
const pendingKey = "moose.system-update.pending";

function readPending(): Pending | null {
  try {
    const raw = sessionStorage.getItem(pendingKey);
    return raw ? (JSON.parse(raw) as Pending) : null;
  } catch {
    return null;
  }
}
function writePending(p: Pending | null) {
  try {
    if (p) sessionStorage.setItem(pendingKey, JSON.stringify(p));
    else sessionStorage.removeItem(pendingKey);
  } catch {
    // Private mode or blocked storage: the view still works, it just will not
    // survive a reload.
  }
}

const pending = ref<Pending | null>(readPending());
const updating = computed(() => pending.value !== null);

const target = useQuery({
  queryKey: ["system-update-target"],
  queryFn: () => api.get<UpdateTarget>("/system/update-target"),
  enabled: isAdmin,
  // The loop checks every 15 minutes; a minute keeps "last checked" honest.
  refetchInterval: 60_000,
});
const t = computed<UpdateTarget | undefined>(() => target.data.value);
// The section is for a box that updates itself. The appliance waits for #400.
// A hosted box whose update settings are unusable reports auto_apply false
// (nothing on it will apply anything), and that is exactly the state its admin
// must see, so it is shown too.
const showUpdates = computed(
  () =>
    isAdmin.value &&
    (t.value?.auto_apply === true || (t.value?.state === "disabled" && t.value?.profile === "hosted")),
);

const windowStart = computed(() => t.value?.window?.split("-")[0] ?? "03:00");
const windowEnd = computed(() => t.value?.window?.split("-")[1] ?? "04:00");
const zone = computed(() => t.value?.zone || "");

// What Update now would do: stream B when a control-plane target is waiting,
// stream A when an OS release is installed and waits for its switch. An OS
// release that is still downloading is not ready yet.
const cpReady = computed(() => t.value?.auto_apply === true && t.value?.state === "available");
const osReady = computed(() => t.value?.auto_apply === true && t.value?.os?.state === "installed" && !!t.value?.os?.target);
const canUpdateNow = computed(() => cpReady.value || osReady.value);
const cpVersion = computed(() => t.value?.target?.version || "");
const osVersion = computed(() => t.value?.os?.target?.version || "");

const stateSentence = computed(() => {
  const s = t.value;
  if (!s) return "";
  switch (s.state) {
    case "available":
      return cpVersion.value
        ? `Control plane ${cpVersion.value} will be installed tonight.`
        : "A new control plane will be installed tonight.";
    case "current":
      return "Your control plane is up to date.";
    case "none":
      return "There is no control-plane update for your box right now.";
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

// osSentence is stream A's line. Nothing is said for a box that cannot
// update its OS (unsupported) or whose answer names no OS release (none).
const osSentence = computed(() => {
  const o = t.value?.os;
  if (!o) return "";
  const v = o.target?.version ? `moose ${o.target.version}` : "A system update";
  switch (o.state) {
    case "current":
      return "Your system is up to date.";
    case "installing":
      return `${v} is downloading. Your box installs it in tonight's window and restarts.`;
    case "installed":
      return `${v} is ready. Your box restarts tonight to install it.`;
    case "waiting":
      return `${v} waits for another update to finish.`;
    case "rebooting":
      return `Your box is restarting to install ${v}.`;
    case "held":
      return `The update to ${v} did not work. Your box tries again in the next window.`;
    case "failed":
      return `${v} could not be downloaded or installed. Your box tries again in the next window.`;
    case "refused":
      return "Your box was offered a system update it could not trust, so it did not install it. It keeps running as it is.";
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
    return o.version ? `Control plane updated to ${o.version} on ${when(o.at)}.` : `Control plane updated on ${when(o.at)}.`;
  }
  const to = o.version ? `The update to control plane ${o.version}` : "The control-plane update";
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

// The last OS switch (os.last, kept across the restart by host-agent).
const osLast = computed(() => t.value?.os?.last);
const osLastSentence = computed(() => {
  const o = osLast.value;
  if (!o) return "";
  if (o.outcome === "good") return `System updated to moose ${o.version} on ${when(o.at)}.`;
  return o.from
    ? `The system update to moose ${o.version} did not start properly on ${when(o.at)}, so your box went back to moose ${o.from}.`
    : `The system update to moose ${o.version} did not start properly on ${when(o.at)}, so your box went back to the system before it.`;
});

// ── Update now ─────────────────────────────────────────────────────────────
const confirmDialog = ref<InstanceType<typeof ConfirmDialog> | null>(null);
const applyError = ref("");
// waitError is set when a wait ends without the update happening.
const waitError = ref("");
const starting = ref(false);
const slow = ref(false);
let slowTimer: ReturnType<typeof setTimeout> | undefined;
let pollTimer: ReturnType<typeof setTimeout> | undefined;
// alive is false once the page is left. A request in flight then must not
// schedule another poll or finish the wait: finish can reload, and that would
// reload whatever page the user moved to. The marker stays in sessionStorage,
// so coming back to this page picks the wait up again.
let alive = true;
onBeforeUnmount(() => {
  alive = false;
  clearTimeout(slowTimer);
  clearTimeout(pollTimer);
});

// The title of the confirmation names what will be installed.
const confirmTitle = computed(() => {
  const parts: string[] = [];
  if (cpReady.value) parts.push(cpVersion.value ? `control plane ${cpVersion.value}` : "the control-plane update");
  if (osReady.value) parts.push(osVersion.value ? `moose ${osVersion.value}` : "the system update");
  return `Install ${parts.join(" and ")} now?`;
});

function openConfirm() {
  applyError.value = "";
  confirmDialog.value?.open();
}

async function applyNow() {
  applyError.value = "";
  starting.value = true;
  // What this click covers, read before it: the screen's view of the box is
  // what the admin agreed to.
  const withCP = cpReady.value;
  const withOS = osReady.value ? osVersion.value || null : null;
  const osLastBefore = t.value?.os?.last?.id ?? null;
  try {
    const job = await api.post<SystemUpdateJob>("/system/update-target/apply");
    const p: Pending = {
      job: withCP ? job.job_id : null,
      cpJob: withCP ? job.job_id : null,
      os: withOS,
      osLast: osLastBefore,
      osJob: !withCP && withOS ? job.job_id : null,
      at: Date.now(),
    };
    waitError.value = "";
    writePending(p);
    pending.value = p;
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

// osWaitLimit is how long an OS switch may take: a restart, and up to 15
// minutes for a new slot that does not come up to be given up.
const osWaitLimit = 25 * 60_000;

// finish ends the wait and reads the new state.
async function finish() {
  clearTimeout(slowTimer);
  writePending(null);
  pending.value = null;
  slow.value = false;
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ["system-update-target"] }),
    queryClient.invalidateQueries({ queryKey: ["system-version"] }),
  ]);
}

// osDone reports whether the OS switch Update now asked for has ended: the box
// runs the new release, or a new OS outcome names it (a revert included).
// Only the outcome counts: a box that runs the new release may still be on
// trial, and a trial can still give the slot up and go back.
function osDone(p: Pending, s: UpdateTarget): boolean {
  const o = s.os;
  if (!o || !p.os) return true;
  return !!o.last && o.last.id !== p.osLast && o.last.version === p.os;
}

// needsReload reports whether stream B replaced the dashboard and this tab has
// not reloaded for it yet, read from `last` when its job is gone.
function needsReload(p: Pending, s: UpdateTarget): boolean {
  return !!p.cpJob && !p.uiReloaded && s.last?.id === p.cpJob && !!s.last.ui_changed;
}

// poll follows the update until it ends. Stream B first, through its job,
// which host-agent keeps, so the read works again as soon as the new brain
// answers. Then stream A, through the update-target read, which works again
// once the box is back after its restart. A request that fails meanwhile is
// expected and only means "ask again".
async function poll() {
  const p = pending.value;
  if (!alive || !p) return;
  try {
    if (p.job) {
      const job = await api.get<SystemUpdateJob>(`/system/update/${encodeURIComponent(p.job)}`);
      if (!alive) return;
      if (job.status !== "running") {
        const uiChanged = job.status === "completed" && !!job.result?.ui_changed;
        const next: Pending = { ...p, job: null, uiReloaded: p.uiReloaded || uiChanged, osAt: Date.now() };
        if (!next.os) {
          if (uiChanged) {
            writePending(null);
            window.location.reload();
            return;
          }
          await finish();
          return;
        }
        // The OS switch comes next. A new dashboard is loaded now, and the
        // wait goes on from the marker after the reload.
        writePending(next);
        pending.value = next;
        if (uiChanged) {
          window.location.reload();
          return;
        }
      }
    } else {
      if (p.osJob) {
        // The switch job ends "completed" just before the restart, or
        // "failed" when it stopped first and the box never restarted. After
        // the restart host-agent no longer knows it (404, caught below).
        const job = await api.get<SystemUpdateJob>(`/system/update/${encodeURIComponent(p.osJob)}`);
        if (!alive) return;
        if (job.status === "failed") {
          waitError.value = "The system update could not start, so your box did not restart. It tries again in the next window.";
          await finish();
          return;
        }
      }
      const s = await api.get<UpdateTarget>("/system/update-target");
      if (!alive) return;
      if (osDone(p, s)) {
        if (needsReload(p, s)) {
          writePending(null);
          window.location.reload();
          return;
        }
        await finish();
        return;
      }
      // A switch that never restarted the box leaves no outcome behind, and
      // neither does a good switch whose record could not be written. Past
      // the time limit, counted from when the OS part began, stop waiting
      // unless the box is restarting or its boot is still on trial
      // (os.trial, read from the slot's trial marker): a trial ends in an
      // outcome.
      const osStart = p.osAt ?? p.at;
      if (Date.now() - osStart > osWaitLimit && s.os?.state !== "rebooting" && !s.os?.trial) {
        await finish();
        return;
      }
    }
  } catch (e) {
    if (!alive) return;
    // 404 on the switch job: the box restarted, so follow the read instead.
    if (e instanceof ApiError && e.status === 404 && !p.job && p.osJob) {
      const next: Pending = { ...p, osJob: null };
      writePending(next);
      pending.value = next;
    }
    // 404 on the job: host-agent no longer knows it (it restarted). The
    // outcome on disk is still the answer, so go on reading it: the next
    // poll reads `last`, reloads when that update replaced the dashboard,
    // and follows the OS part if there is one.
    if (e instanceof ApiError && e.status === 404 && p.job) {
      const next: Pending = { ...p, job: null, osAt: Date.now() };
      writePending(next);
      pending.value = next;
    }
  }
  pollTimer = setTimeout(() => void poll(), 3000);
}

watch(
  () => pending.value !== null,
  (waiting) => {
    clearTimeout(pollTimer);
    clearTimeout(slowTimer);
    slow.value = false;
    if (!waiting) return;
    // A control-plane update takes about a minute. An OS switch restarts the
    // box, and a new slot that does not come up is given up after up to 15
    // minutes. Past that, say so instead of spinning on.
    const limit = pending.value?.os ? osWaitLimit : 10 * 60_000;
    const left = Math.max(0, (pending.value?.at ?? Date.now()) + limit - Date.now());
    slowTimer = setTimeout(() => (slow.value = true), left);
    void poll();
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
        <p v-if="pending?.os" class="text-sm text-muted-foreground">
          Your box restarts to install moose {{ pending.os }}. Your apps and the dashboard are offline for a few
          minutes. This page will show the result when your box is back.
        </p>
        <p v-else class="text-sm text-muted-foreground">
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
          <details v-if="t?.detail" class="text-sm">
            <summary class="cursor-pointer text-muted-foreground hover:text-foreground">Technical reason</summary>
            <p class="mt-1 break-words font-mono text-xs text-muted-foreground">{{ t.detail }}</p>
          </details>
          <p v-if="osSentence" class="text-sm">{{ osSentence }}</p>
          <details v-if="osSentence && t?.os?.detail" class="text-sm">
            <summary class="cursor-pointer text-muted-foreground hover:text-foreground">Technical reason</summary>
            <p class="mt-1 break-words font-mono text-xs text-muted-foreground">{{ t.os.detail }}</p>
          </details>
          <p v-if="t?.checked_at" class="text-sm text-muted-foreground">Last checked {{ ago(t.checked_at) }}.</p>
        </div>

        <div v-if="canUpdateNow" class="space-y-2">
          <p v-if="osReady" class="text-sm text-muted-foreground">
            Updating now restarts your box. Your apps and the dashboard are offline for a few minutes.
          </p>
          <p v-else class="text-sm text-muted-foreground">
            An update replaces the dashboard and the service behind it. Your apps keep working, the dashboard is away
            for up to a minute, and your box does not restart.
          </p>
          <Button :disabled="starting" @click="openConfirm">Update now</Button>
        </div>
        <p v-if="applyError" class="text-sm text-destructive">{{ applyError }}</p>
        <p v-if="waitError" class="text-sm text-destructive">{{ waitError }}</p>

        <div v-if="last || osLast" class="space-y-1 border-t border-border pt-4">
          <p class="text-base font-medium">Last updates</p>
          <template v-if="last">
            <p class="text-sm" :class="last.outcome === 'good' ? '' : 'text-destructive'">{{ lastSentence }}</p>
            <details v-if="last.revert_error" class="text-sm">
              <summary class="cursor-pointer text-muted-foreground hover:text-foreground">Technical reason</summary>
              <p class="mt-1 break-words font-mono text-xs text-muted-foreground">{{ last.revert_error }}</p>
            </details>
          </template>
          <p v-if="osLast" class="text-sm" :class="osLast.outcome === 'good' ? '' : 'text-destructive'">
            {{ osLastSentence }}
          </p>
        </div>
      </div>
    </div>

    <ConfirmDialog ref="confirmDialog" :title="confirmTitle" confirm-label="Update now" @confirm="applyNow">
      <p v-if="osReady">
        Your box restarts. Your apps and the dashboard are offline for a few minutes. If the new system does not start
        properly, your box goes back to the one it runs now on its own, which can take up to 15 minutes.
      </p>
      <p v-else>
        Your apps keep working. The dashboard is away for up to a minute while it is replaced, and your box does not
        restart. If the new version does not start properly, your box goes back to the version it runs now.
      </p>
    </ConfirmDialog>
  </section>
</template>
