<script setup lang="ts">
// The folder step of the install flow: the consent screen for folder access
// (INSTALL_STEPS.md # Build rules, rule 9, # 5b). It shows every folder the
// app uses, each with one line saying what the app can do there (write access
// in red), in the same words as the permission lines (folderAccess). A folder
// with a real choice of source has radios (yours or the household's). Each
// folder reads as a path, "Notes / Obsidian", with Change, which opens the
// folder chooser (#658). The suggestion is picked in advance. An optional
// folder has "Don't use a folder" as a choice in its row. An app with
// extra_folders has Add a folder, and each added folder has Remove. A 422 on
// a row shows under that row.
import { computed, ref } from "vue";
import type { InstallPlanFolder, Scope } from "../../api";
import { useAuth } from "../../auth";
import {
  folderAccess,
  folderName,
  showPath,
  sourceLabel,
  suggestedPath,
  type ExtraPick,
  type FolderPick,
} from "../../installSteps";
import Button from "../ui/Button.vue";
import FolderChooser from "./FolderChooser.vue";

const props = defineProps<{
  folders: InstallPlanFolder[];
  extraFolders?: string;
  scope: Scope;
  appName: string;
  // error is a 422 the brain put on one row (config.folders.<folder> or
  // config.extra_folders[<i>]).
  error?: { location: string; message: string } | null;
}>();
const picks = defineModel<Record<string, FolderPick>>("picks", { required: true });
const extras = defineModel<ExtraPick[]>("extras", { required: true });

const { singleUserMode } = useAuth();

function options(f: InstallPlanFolder): string[] {
  return f.sources[props.scope].options ?? [];
}

// open is the row whose chooser is open: a folder name, or "extra" for Add a
// folder. Only one at a time.
const open = ref("");

function setSource(f: InstallPlanFolder, src: string) {
  // The path belongs to its tree, so a new source starts on the suggestion.
  picks.value = { ...picks.value, [f.folder]: { source: src, path: suggestedPath(f), isNew: false, skip: false } };
  if (open.value === f.folder) open.value = "";
}
function setSkip(f: InstallPlanFolder, skip: boolean) {
  picks.value = { ...picks.value, [f.folder]: { ...picks.value[f.folder]!, skip } };
}
function chose(f: InstallPlanFolder, c: { source: string; path: string; isNew: boolean }) {
  picks.value = { ...picks.value, [f.folder]: { ...c, skip: false } };
  open.value = "";
}

const extraSources = computed(() => (props.scope === "household" ? ["shared"] : ["personal", "shared"]));
function addExtra(c: { source: string; path: string; isNew: boolean }) {
  if (!extras.value.some((x) => x.source === c.source && x.path === c.path)) extras.value = [...extras.value, c];
  open.value = "";
}
function removeExtra(i: number) {
  extras.value = extras.value.filter((_, j) => j !== i);
}

const errorOf = (location: string) => (props.error?.location === location ? props.error.message : "");
const whose = (src: string) => (src === "shared" ? (singleUserMode.value ? "Shared" : "The household's") : "Yours");
</script>

<template>
  <div class="space-y-8">
    <div v-for="f in folders" :key="f.folder" class="space-y-3">
      <p v-if="folders.length > 1 || extraFolders" class="text-sm/6 font-medium text-foreground">
        {{ folderName(f.folder) }}
      </p>
      <p class="text-sm" :class="f.mode === 'write' ? 'font-medium text-destructive' : 'text-muted-foreground'">
        {{ appName }} can {{ folderAccess(f.mode) }} in this folder.
      </p>
      <p v-if="options(f).length === 1" class="text-sm text-foreground">
        {{ sourceLabel(f.folder, options(f)[0] ?? "", singleUserMode) }}
      </p>
      <fieldset v-else :aria-label="`Which ${folderName(f.folder)} folder`" class="space-y-2">
        <label
          v-for="opt in options(f)"
          :key="opt"
          class="relative flex cursor-pointer items-center gap-3 rounded-lg border border-border bg-card px-4 py-2.5 text-sm hover:border-olive-400 has-checked:border-accent has-checked:outline-1 has-checked:-outline-offset-2 has-checked:outline-accent"
        >
          <input
            type="radio"
            :name="`folder-source-${f.folder}`"
            :value="opt"
            :checked="picks[f.folder]?.source === opt"
            class="accent-accent"
            @change="setSource(f, opt)"
          />
          {{ sourceLabel(f.folder, opt, singleUserMode) }}
        </label>
      </fieldset>

      <fieldset :aria-label="`Where the ${folderName(f.folder)} folder points`" class="space-y-2">
        <label
          class="relative flex items-center gap-3 rounded-lg border border-border bg-card px-4 py-2.5 text-sm"
          :class="f.optional ? 'cursor-pointer hover:border-olive-400 has-checked:border-accent has-checked:outline-1 has-checked:-outline-offset-2 has-checked:outline-accent' : ''"
        >
          <input
            v-if="f.optional"
            type="radio"
            :name="`folder-use-${f.folder}`"
            :checked="!picks[f.folder]?.skip"
            class="accent-accent"
            @change="setSkip(f, false)"
          />
          <span class="min-w-0 flex-1 truncate text-foreground">
            {{ showPath(picks[f.folder]?.path ?? "") }}
            <span v-if="picks[f.folder]?.isNew" class="text-muted-foreground">(new)</span>
          </span>
          <Button
            v-if="open !== f.folder"
            type="button"
            size="sm"
            variant="secondary"
            @click.prevent="open = f.folder"
          >
            Change
          </Button>
        </label>
        <label
          v-if="f.optional"
          class="relative flex cursor-pointer items-center gap-3 rounded-lg border border-border bg-card px-4 py-2.5 text-sm hover:border-olive-400 has-checked:border-accent has-checked:outline-1 has-checked:-outline-offset-2 has-checked:outline-accent"
        >
          <input
            type="radio"
            :name="`folder-use-${f.folder}`"
            :checked="!!picks[f.folder]?.skip"
            class="accent-accent"
            @change="setSkip(f, true)"
          />
          Don't use a folder
        </label>
      </fieldset>
      <FolderChooser
        v-if="open === f.folder"
        :sources="[picks[f.folder]?.source ?? 'personal']"
        :source="picks[f.folder]?.source ?? 'personal'"
        :start="picks[f.folder]?.path ?? ''"
        :start-new="!!picks[f.folder]?.isNew"
        @choose="(c) => chose(f, c)"
        @cancel="open = ''"
      />
      <p v-if="errorOf(`config.folders.${f.folder}`)" class="text-sm text-destructive" role="alert">
        {{ errorOf(`config.folders.${f.folder}`) }}
      </p>
    </div>

    <div v-if="extraFolders" class="space-y-3">
      <p class="text-sm/6 font-medium text-foreground">More folders</p>
      <p class="text-sm" :class="extraFolders === 'write' ? 'font-medium text-destructive' : 'text-muted-foreground'">
        {{ appName }} can {{ folderAccess(extraFolders) }} in each folder you add.
      </p>
      <div v-for="(x, i) in extras" :key="`${x.source}:${x.path}`" class="space-y-2">
        <div class="flex items-center gap-3 rounded-lg border border-border bg-card px-4 py-2.5 text-sm">
          <span class="min-w-0 flex-1 truncate text-foreground">
            {{ showPath(x.path) }}
            <span class="text-muted-foreground">({{ whose(x.source) }}{{ x.isNew ? ", new" : "" }})</span>
          </span>
          <Button type="button" size="sm" variant="ghost" @click="removeExtra(i)">Remove</Button>
        </div>
        <p v-if="errorOf(`config.extra_folders[${i}]`)" class="text-sm text-destructive" role="alert">
          {{ errorOf(`config.extra_folders[${i}]`) }}
        </p>
      </div>
      <FolderChooser
        v-if="open === 'extra'"
        :sources="extraSources"
        :source="extraSources[0]!"
        start=""
        :start-new="false"
        @choose="addExtra"
        @cancel="open = ''"
      />
      <Button v-else type="button" size="sm" variant="secondary" @click="open = 'extra'">Add a folder</Button>
    </div>
  </div>
</template>
