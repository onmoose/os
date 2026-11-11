<script setup lang="ts">
// The folder chooser (DASHBOARD.md # Folder chooser, #658). It is not a file
// browser: it shows folders only, one level at a time, from
// GET /api/v1/me/folders, and never files. A personal app starts in the
// user's own home, whose top shows only the use-case folders and the folders
// made through moose; a household app starts in the shared folders. New
// folder goes into a folder that is made when the app is installed, and
// Choose this folder picks the folder that is open. The rules are the
// brain's: it checks the choice again on install.
import { useQuery } from "@tanstack/vue-query";
import { ChevronLeft, ChevronRight, Folder, FolderPlus } from "lucide-vue-next";
import { computed, nextTick, ref } from "vue";
import { ApiError, api, type FolderEntry, type FolderList } from "../../api";
import { useAuth } from "../../auth";
import { newFolderNameError, showPath, USE_CASE_FOLDERS } from "../../installSteps";
import Button from "../ui/Button.vue";

const props = defineProps<{
  // sources are the trees the user may choose in, the first one open.
  sources: string[];
  source: string;
  // start is the folder the chooser opens in, relative to the top ("" is the
  // top). startNew says its first level is a new folder not made yet.
  start: string;
  startNew: boolean;
}>();
const emit = defineEmits<{
  choose: [pick: { source: string; path: string; isNew: boolean }];
  cancel: [];
}>();

const source = ref(props.source);
const segs = ref<string[]>(props.start ? props.start.split("/").filter(Boolean) : []);
// made are the folders made in this chooser (and the start folder, when it is
// new). They are not on disk yet, so they are not listed from the brain.
const made = ref<Set<string>>(new Set(props.startNew && segs.value[0] ? [`${props.source}:${segs.value[0]}`] : []));

const path = computed(() => segs.value.join("/"));
const atTop = computed(() => segs.value.length === 0);
const key = (src: string, p: string) => `${src}:${p}`;

// notOnDisk: a folder inside one made here, or a use-case folder the brain
// listed as missing, has nothing in it to list.
const missingTops = ref<Set<string>>(new Set());
const notOnDisk = computed(() => {
  for (let i = 1; i <= segs.value.length; i++) {
    if (made.value.has(key(source.value, segs.value.slice(0, i).join("/")))) return true;
  }
  return !!segs.value[0] && missingTops.value.has(key(source.value, segs.value[0]));
});

// The key names the signed-in user, so a cached listing is never shown to
// the next person who signs in on the same tab.
const { currentUser } = useAuth();
const query = useQuery({
  queryKey: computed(() => ["me-folders", currentUser.value?.id ?? "", source.value, path.value]),
  queryFn: async () => {
    const out = await api.get<FolderList>(
      `/me/folders?source=${encodeURIComponent(source.value)}&path=${encodeURIComponent(path.value)}`,
    );
    if (!path.value) {
      for (const f of out.folders ?? []) if (f.missing) missingTops.value.add(key(source.value, f.name));
    }
    return out;
  },
  enabled: computed(() => !notOnDisk.value),
  retry: false,
  refetchOnWindowFocus: false,
});

// A folder that is not there yet (404) is shown empty: it is made on install.
const notThere = computed(() => query.error.value instanceof ApiError && query.error.value.status === 404);
const listError = computed(() => (query.error.value && !notThere.value ? query.error.value.message : ""));

const entries = computed<FolderEntry[]>(() => {
  const listed = notOnDisk.value || notThere.value ? [] : (query.data.value?.folders ?? []);
  const here = path.value ? `${path.value}/` : "";
  const extra: FolderEntry[] = [];
  for (const m of made.value) {
    const [src, p] = [m.slice(0, m.indexOf(":")), m.slice(m.indexOf(":") + 1)];
    if (src !== source.value || !p.startsWith(here)) continue;
    const rest = p.slice(here.length);
    if (rest && !rest.includes("/") && !listed.some((f) => f.name === rest)) extra.push({ name: rest });
  }
  return [...listed, ...extra].sort((a, b) => a.name.localeCompare(b.name));
});

const loading = computed(() => !notOnDisk.value && query.isPending.value && !query.error.value);

function open(name: string) {
  segs.value = [...segs.value, name];
  naming.value = false;
}
function back() {
  segs.value = segs.value.slice(0, -1);
  naming.value = false;
}
function switchSource(src: string) {
  source.value = src;
  segs.value = [];
  naming.value = false;
}

// ── New folder ──────────────────────────────────────────────────────────────
const naming = ref(false);
const newName = ref("");
const nameError = ref("");
const nameInput = ref<HTMLInputElement | null>(null);
async function startNaming() {
  naming.value = true;
  newName.value = "";
  nameError.value = "";
  await nextTick();
  nameInput.value?.focus();
}
function makeFolder() {
  const n = newName.value.trim();
  nameError.value = newFolderNameError(n, atTop.value, entries.value.map((e) => e.name));
  if (nameError.value) return;
  const p = path.value ? `${path.value}/${n}` : n;
  made.value = new Set([...made.value, key(source.value, p)]);
  open(n);
}

// isNew: the first level is a folder made here that is not a use-case folder.
// Only that level needs new: true; a deeper level is made with the folder.
function choose() {
  const top = segs.value[0] ?? "";
  const isNew = made.value.has(key(source.value, top)) && !USE_CASE_FOLDERS.includes(top);
  emit("choose", { source: source.value, path: path.value, isNew });
}

const sourceName = (src: string) => (src === "shared" ? "Shared folders" : "Your folders");
const heading = computed(() => (atTop.value ? sourceName(source.value) : (segs.value[segs.value.length - 1] ?? "")));
</script>

<template>
  <div class="space-y-3 rounded-lg border border-border bg-card p-3">
    <div v-if="sources.length > 1 && atTop" class="flex gap-2" role="group" aria-label="Which folders">
      <Button
        v-for="src in sources"
        :key="src"
        type="button"
        size="sm"
        :variant="src === source ? 'primary' : 'secondary'"
        :aria-pressed="src === source"
        @click="switchSource(src)"
      >
        {{ sourceName(src) }}
      </Button>
    </div>

    <div class="flex items-center gap-2">
      <Button v-if="!atTop" type="button" variant="ghost" size="sm" @click="back">
        <ChevronLeft class="size-4" aria-hidden="true" /> Back
      </Button>
      <p class="min-w-0 truncate text-sm font-medium text-foreground" aria-live="polite">{{ heading }}</p>
    </div>
    <p v-if="!atTop" class="text-xs text-muted-foreground">{{ showPath(path) }}</p>

    <p v-if="loading" class="text-sm text-muted-foreground">Loading folders…</p>
    <p v-else-if="listError" class="text-sm text-destructive" role="alert">{{ listError }}</p>
    <ul v-else-if="entries.length > 0" class="max-h-64 divide-y divide-border overflow-y-auto rounded-md border border-border">
      <li v-for="f in entries" :key="f.name">
        <button
          type="button"
          class="flex w-full cursor-pointer items-center gap-3 px-3 py-2 text-left text-sm hover:bg-muted"
          @click="open(f.name)"
        >
          <Folder class="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
          <span class="min-w-0 flex-1 truncate">{{ f.name }}</span>
          <span v-if="f.missing" class="text-xs text-muted-foreground">New</span>
          <ChevronRight class="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
        </button>
      </li>
    </ul>
    <p v-else class="text-sm text-muted-foreground">No folders in here.</p>
    <p v-if="query.data.value?.truncated && !notOnDisk" class="text-xs text-muted-foreground">
      Only the first 1000 folders are shown.
    </p>

    <form v-if="naming" class="space-y-2" @submit.prevent="makeFolder">
      <label for="new-folder-name" class="block text-sm/6 text-muted-foreground">Name of the new folder</label>
      <input
        id="new-folder-name"
        ref="nameInput"
        v-model="newName"
        type="text"
        autocomplete="off"
        class="block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 outline-border focus:outline-2 focus:-outline-offset-2 focus:outline-accent sm:text-sm/6"
      />
      <p v-if="nameError" class="text-sm text-destructive" role="alert">{{ nameError }}</p>
      <div class="flex gap-2">
        <Button type="submit" size="sm">Make folder</Button>
        <Button type="button" size="sm" variant="ghost" @click="naming = false">Cancel</Button>
      </div>
    </form>

    <div class="flex flex-wrap items-center gap-2 pt-1">
      <!-- The top of the shared tree takes only the shared use-case folders, so
           a new folder there would always be refused (APP_ISOLATION.md). -->
      <Button v-if="!naming && !(atTop && source === 'shared')" type="button" size="sm" variant="secondary" @click="startNaming">
        <FolderPlus class="size-4" aria-hidden="true" /> New folder
      </Button>
      <span class="flex-1" />
      <Button type="button" size="sm" variant="ghost" @click="emit('cancel')">Cancel</Button>
      <Button type="button" size="sm" :disabled="atTop" @click="choose">Choose this folder</Button>
    </div>
  </div>
</template>
