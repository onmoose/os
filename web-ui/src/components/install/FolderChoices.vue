<script setup lang="ts">
// The folder step of the install flow: the consent screen for folder access
// (INSTALL_STEPS.md # Build rules, rule 9). It shows every folder the app
// uses, each with one line saying what the app can do there (write access in
// red), in the same words as the permission lines (folderAccess). A folder
// with a real choice has radios (yours or the household's), and one the app
// manages part of has the subfolder box. A folder with one possible source
// shows only its name and that line. Every folder has a default, picked.
import type { InstallPlanFolder, Scope } from "../../api";
import { useAuth } from "../../auth";
import { folderAccess, folderName, sourceLabel } from "../../installSteps";

const props = defineProps<{ folders: InstallPlanFolder[]; scope: Scope; appName: string }>();
const sources = defineModel<Record<string, string>>("sources", { required: true });
const subfolders = defineModel<Record<string, string>>("subfolders", { required: true });

const { singleUserMode } = useAuth();

function options(f: InstallPlanFolder): string[] {
  return f.sources[props.scope].options ?? [];
}
</script>

<template>
  <div class="space-y-8">
    <div v-for="f in folders" :key="f.folder" class="space-y-3">
      <p v-if="folders.length > 1" class="text-sm/6 font-medium text-foreground">{{ folderName(f.folder) }}</p>
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
            v-model="sources[f.folder]"
            type="radio"
            :name="`folder-source-${f.folder}`"
            :value="opt"
            class="accent-accent"
          />
          {{ sourceLabel(f.folder, opt, singleUserMode) }}
        </label>
      </fieldset>
      <div v-if="f.scope === 'pick-subfolder'">
        <label :for="`sub-${f.folder}`" class="block text-sm/6 text-muted-foreground">
          Which subfolder should this app manage?
        </label>
        <input
          :id="`sub-${f.folder}`"
          v-model="subfolders[f.folder]"
          type="text"
          :placeholder="f.subfolder_default ?? ''"
          class="mt-2 block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 focus:outline-accent sm:text-sm/6"
        />
      </div>
    </div>
  </div>
</template>
