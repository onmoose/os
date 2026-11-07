<script setup lang="ts">
// The update prompt (UPDATES.md # 6 The permission-expansion prompt, #633).
// An app update that asks for more access waits for a yes. This modal asks,
// once per session, on the first dashboard load: one entry per app, "<App>
// wants new access:" and the lines in the install screen's words, with two
// buttons, Allow & update and Keep current version.
//
// Which apps: the ones the caller may allow (update.can_approve: the owner of
// a personal app, an admin for a household app), that wait for a yes, and
// that the caller did not say "Keep" to for that version. The brain says all
// of it on each app of GET /apps, so the list is drawn from the apps cache
// with no read of its own.
//
// Allow runs the update at once, as a job, the way the Update button on the
// app's settings page does. Keep is stored per version, so the prompt does
// not ask about that version again (the settings page still offers it). When
// every app is decided, the modal closes. Closing it without deciding (the X,
// Escape, a click outside) is stored on the session, so it comes back at the
// next sign-in, in no other tab and on no other page load.
import { computed, ref, watch } from "vue";
import { useQuery, useQueryClient } from "@tanstack/vue-query";
import { X } from "lucide-vue-next";
import { api, waitForJob, type Instance, type Job } from "@/api";
import { markUpdatePromptDismissed, useAuth } from "@/auth";
import { wantLines } from "@/installSteps";
import Button from "@/components/ui/Button.vue";
import PermissionList from "@/components/PermissionList.vue";

const qc = useQueryClient();
const { currentUser } = useAuth();

const apps = useQuery({
  queryKey: ["apps"],
  queryFn: () => api.get<{ apps: Instance[] }>("/apps"),
});

const waiting = computed(() =>
  (apps.data.value?.apps ?? []).filter(
    (a) => a.update.status === "needs_approval" && a.update.can_approve && !a.update.declined,
  ),
);

// The entries are fixed when the modal opens, so an app stays in it while
// its update runs and after the person decided.
type Entry = { id: string; name: string; version: string; app: Instance };
type Choice = "updating" | "updated" | "kept" | "failed";
const entries = ref<Entry[]>([]);
const choice = ref<Record<string, Choice>>({});
const errors = ref<Record<string, string>>({});
const dialog = ref<HTMLDialogElement | null>(null);
// The prompt opens at most once per page load. A session that closed it does
// not open it again; the brain keeps that on the session (/me).
const shown = ref(false);

watch(
  [waiting, () => currentUser.value?.update_prompt_dismissed],
  ([list, dismissed]) => {
    if (shown.value || dismissed || list.length === 0 || !apps.isSuccess.value) return;
    shown.value = true;
    entries.value = list.map((a) => ({ id: a.id, name: a.name, version: a.update.available_version ?? "", app: a }));
    dialog.value?.showModal();
  },
  { immediate: true, flush: "post" },
);

function invalidate(id: string) {
  qc.invalidateQueries({ queryKey: ["apps"] });
  qc.invalidateQueries({ queryKey: ["apps", id] });
}

// Every entry decided: the modal closes, and nothing is stored on the
// session (there is nothing left to ask).
// A failed Allow is not decided: the entry stays open with its error and
// its two buttons, so the person sees what went wrong and can choose again.
function closeIfDecided() {
  const left = entries.value.some(
    (e) => !choice.value[e.id] || choice.value[e.id] === "updating" || choice.value[e.id] === "failed",
  );
  if (!left) dialog.value?.close();
}

function setChoice(id: string, c: Choice) {
  choice.value = { ...choice.value, [id]: c };
}

async function allow(e: Entry) {
  setChoice(e.id, "updating");
  errors.value = { ...errors.value, [e.id]: "" };
  try {
    const job = await api.post<Job>(`/apps/${e.id}/update/approve`, { version: e.version });
    const done = await waitForJob(job.job_id);
    if (done.status === "failed") throw new Error(done.error?.message || "the update did not go through");
    setChoice(e.id, "updated");
  } catch (err) {
    setChoice(e.id, "failed");
    errors.value = { ...errors.value, [e.id]: (err as Error).message };
  } finally {
    invalidate(e.id);
  }
  closeIfDecided();
}

async function keep(e: Entry) {
  errors.value = { ...errors.value, [e.id]: "" };
  try {
    await api.post(`/apps/${e.id}/update/decline`, { version: e.version });
    setChoice(e.id, "kept");
  } catch (err) {
    errors.value = { ...errors.value, [e.id]: (err as Error).message };
  } finally {
    invalidate(e.id);
  }
  closeIfDecided();
}

// Closing without deciding is a dismissal for this session. The <dialog>
// fires `cancel` on Escape; a click on the backdrop lands on the dialog
// element itself.
const closing = ref(false);
async function dismiss() {
  if (closing.value) return;
  closing.value = true;
  dialog.value?.close();
  const undecided = entries.value.some((e) => !choice.value[e.id] || choice.value[e.id] === "failed");
  if (!undecided) return;
  try {
    await api.post("/me/update-prompt/dismiss");
    markUpdatePromptDismissed();
  } catch {
    // The session keeps no mark, so the prompt shows again on the next page
    // load. That is the safe side.
  }
}

function onCancel(ev: Event) {
  ev.preventDefault();
  dismiss();
}

function onBackdrop(ev: MouseEvent) {
  if (ev.target === dialog.value) dismiss();
}
</script>

<template>
  <dialog
    ref="dialog"
    aria-labelledby="update-prompt-title"
    class="m-auto w-[calc(100%-2rem)] max-w-lg rounded-xl border border-border bg-card p-0 text-foreground backdrop:bg-black/50"
    @cancel="onCancel"
    @click="onBackdrop"
  >
    <div class="space-y-4 px-5 py-5">
      <div class="flex items-start justify-between gap-3">
        <h3 id="update-prompt-title" class="text-base font-medium">
          {{ entries.length === 1 ? "An update asks for more access" : "Updates ask for more access" }}
        </h3>
        <button
          type="button"
          class="rounded-md p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
          aria-label="Close"
          @click="dismiss"
        >
          <X class="size-4" />
        </button>
      </div>
      <p class="text-sm text-muted-foreground">
        Each app stays on the version it has until you choose.
      </p>
      <ul class="space-y-3">
        <li v-for="e in entries" :key="e.id" class="space-y-2 rounded-lg border border-border px-4 py-3 text-sm">
          <p class="font-medium">{{ e.name }} {{ e.version }} wants new access:</p>
          <PermissionList :lines="wantLines(e.app.update.wants)" class="text-muted-foreground" />
          <p v-if="choice[e.id] === 'updated'" class="text-xs text-muted-foreground">Updated to {{ e.version }}.</p>
          <p v-else-if="choice[e.id] === 'kept'" class="text-xs text-muted-foreground">
            Kept the current version. You can still allow it from the app's settings.
          </p>
          <p v-else-if="choice[e.id] === 'updating'" class="text-xs text-muted-foreground">
            Updating. The app is down for a moment.
          </p>
          <p v-if="errors[e.id]" class="text-xs text-destructive">Couldn't do it: {{ errors[e.id] }}</p>
          <div v-if="!choice[e.id] || choice[e.id] === 'failed'" class="flex flex-wrap justify-end gap-2">
            <Button type="button" size="sm" variant="secondary" @click="keep(e)">Keep current version</Button>
            <Button type="button" size="sm" @click="allow(e)">Allow &amp; update</Button>
          </div>
          <div v-else-if="choice[e.id] === 'updating'" class="flex justify-end">
            <Button type="button" size="sm" disabled>Updating…</Button>
          </div>
        </li>
      </ul>
    </div>
  </dialog>
</template>
