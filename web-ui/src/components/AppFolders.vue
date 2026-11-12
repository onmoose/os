<script setup lang="ts">
// The Folders section of an installed app's page (DASHBOARD.md # Changing
// folders after install, #659). It lists each folder the app uses and where it
// points, with the install's own folder step (FolderChoices): Change opens the
// folder chooser, an optional folder has "Don't use a folder", and an app with
// extra_folders has Add a folder and Remove. Save sends the full set with
// PUT /api/v1/apps/{id}/folders, a job that restarts the app, so the section
// says that first. A caller who may see the folders but not change them (an
// admin on another person's personal app) gets the list only.
import { useMutation, useQuery, useQueryClient } from "@tanstack/vue-query";
import { computed, ref, watch } from "vue";
import {
  ApiError,
  api,
  waitForJob,
  type AppFolders,
  type AppFoldersUpdate,
  type ExtraFolderElection,
  type FolderElection,
  type Job,
} from "../api";
import { useAuth } from "../auth";
import { folderAccess, folderName, showPath, suggestedPath, type ExtraPick, type FolderPick } from "../installSteps";
import FolderChoices from "./install/FolderChoices.vue";
import Button from "./ui/Button.vue";

const props = defineProps<{ id: string; appName: string }>();
const qc = useQueryClient();
const { singleUserMode } = useAuth();

const query = useQuery({
  queryKey: computed(() => ["app-folders", props.id]),
  queryFn: () => api.get<AppFolders>(`/apps/${props.id}/folders`),
});
const data = computed(() => query.data.value);
const shown = computed(() => !!data.value && ((data.value.folders ?? []).length > 0 || !!data.value.extra_folders_mode));

// The picks start from what the app binds now, and again after each load
// (a save, or another tab's save), unless the user has changed something.
const picks = ref<Record<string, FolderPick>>({});
const extras = ref<ExtraPick[]>([]);
const seeded = ref("");
function fromData(d: AppFolders) {
  const p: Record<string, FolderPick> = {};
  for (const f of d.folders ?? []) {
    p[f.folder] = f.skipped
      ? { source: f.sources[d.scope as "personal" | "household"].default, path: suggestedPath(f), isNew: false, skip: true }
      : { source: f.source ?? "personal", path: f.path ?? "", isNew: false, skip: false };
  }
  return { picks: p, extras: (d.extra_folders ?? []).map((x) => ({ source: x.source, path: x.path, isNew: false })) };
}
const snapshot = (p: Record<string, FolderPick>, x: ExtraPick[]) => JSON.stringify({ p, x });
// seeded is "" until the first load has filled the picks, and nothing is
// changed before that.
const dirty = computed(() => seeded.value !== "" && snapshot(picks.value, extras.value) !== seeded.value);
watch(
  data,
  (d) => {
    if (!d || dirty.value) return;
    const s = fromData(d);
    picks.value = s.picks;
    extras.value = s.extras;
    seeded.value = snapshot(s.picks, s.extras);
  },
  { immediate: true },
);
function reset() {
  if (!data.value) return;
  const s = fromData(data.value);
  picks.value = s.picks;
  extras.value = s.extras;
  seeded.value = snapshot(s.picks, s.extras);
  error.value = null;
}

function body(d: AppFolders): AppFoldersUpdate {
  const folders: FolderElection[] = (d.folders ?? []).map((f) => {
    const pick = picks.value[f.folder];
    if (!pick) return { folder: f.folder };
    if (pick.skip) return { folder: f.folder, skip: true };
    return { folder: f.folder, source: pick.source, path: pick.path, ...(pick.isNew ? { new: true } : {}) };
  });
  const extra_folders: ExtraFolderElection[] = extras.value.map((x) => ({
    source: x.source,
    path: x.path,
    ...(x.isNew ? { new: true } : {}),
  }));
  return { folders, extra_folders };
}

// error is the brain's answer: a 422 with a location shows on its row, any
// other failure under the Save button.
const error = ref<{ location: string; message: string } | null>(null);
// The controls are locked while a save runs (the fieldset below), and the
// choice that was sent is what becomes saved.
const save = useMutation({
  mutationFn: async () => {
    error.value = null;
    const sent = snapshot(picks.value, extras.value);
    const job = await api.put<Job | undefined>(`/apps/${props.id}/folders`, body(data.value!));
    if (job) {
      // 202: wait for the job. 204: nothing changed.
      const done = await waitForJob(job.job_id);
      if (done.status === "failed") throw new Error(done.error?.message || "the change did not go through");
    }
    return sent;
  },
  onSuccess: (sent) => {
    // The saved choice is the new start, so the reload may seed again.
    seeded.value = sent;
  },
  onError: (e) => {
    if (e instanceof ApiError && e.location) error.value = { location: e.location, message: e.message };
    else error.value = { location: "", message: (e as Error).message };
  },
  onSettled: () => {
    qc.invalidateQueries({ queryKey: ["app-folders", props.id] });
    qc.invalidateQueries({ queryKey: ["apps"] });
  },
});

const whose = (src: string) => (src === "shared" ? (singleUserMode.value ? "Shared" : "The household's") : "Yours");
</script>

<template>
  <p v-if="query.isError.value" class="text-sm text-destructive">
    Couldn't load folders: {{ (query.error.value as Error)?.message }}
  </p>
  <section v-if="shown && data" class="space-y-2">
    <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Folders</h2>

    <!-- Read-only: the caller may see the folders but not point them
         somewhere new (DASHBOARD.md # Changing folders after install, Who). -->
    <template v-if="!data.can_change">
      <p class="text-xs text-muted-foreground">Only the person who installed {{ appName }} can change its folders.</p>
      <ul class="divide-y divide-border rounded-xl border border-border bg-card">
        <li v-for="f in data.folders ?? []" :key="f.folder" class="px-4 py-3 text-sm">
          <div class="font-medium">{{ folderName(f.folder) }}</div>
          <div class="text-muted-foreground">
            <template v-if="f.skipped">Not used</template>
            <template v-else>{{ showPath(f.path ?? "") }} ({{ whose(f.source ?? "") }})</template>
          </div>
          <div class="text-xs" :class="f.mode === 'write' ? 'font-medium text-destructive' : 'text-muted-foreground'">
            {{ appName }} can {{ folderAccess(f.mode) }} in this folder.
          </div>
        </li>
        <li v-for="x in data.extra_folders" :key="x.name" class="px-4 py-3 text-sm">
          <div class="font-medium">{{ x.name }}</div>
          <div class="text-muted-foreground">{{ showPath(x.path) }} ({{ whose(x.source) }})</div>
        </li>
      </ul>
    </template>

    <template v-else>
      <p class="text-xs text-muted-foreground">Saving restarts {{ appName }}. Anything running in it stops.</p>
      <fieldset :disabled="save.isPending.value" class="rounded-xl border border-border bg-card px-4 py-4 disabled:opacity-60">
        <FolderChoices
          v-model:picks="picks"
          v-model:extras="extras"
          :folders="data.folders ?? []"
          :extra-folders="data.extra_folders_mode"
          :scope="data.scope as 'personal' | 'household'"
          :app-name="appName"
          :error="error?.location ? error : null"
        />
      </fieldset>
      <div class="flex items-center gap-3">
        <Button type="button" size="sm" :disabled="!dirty || save.isPending.value" @click="save.mutate()">
          {{ save.isPending.value ? "Saving…" : "Save folders" }}
        </Button>
        <Button v-if="dirty && !save.isPending.value" type="button" size="sm" variant="ghost" @click="reset">
          Cancel
        </Button>
        <span v-if="save.isPending.value" class="text-xs text-muted-foreground">{{ appName }} is restarting.</span>
      </div>
      <p v-if="error && !error.location" class="text-sm text-destructive" role="alert">
        Couldn't change the folders: {{ error.message }}
      </p>
    </template>
  </section>
</template>
