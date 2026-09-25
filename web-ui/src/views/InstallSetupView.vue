<script setup lang="ts">
// Install setup page (/store/:id/install), step 1 of INSTALL_SETUP.md. It
// replaces the old consent modal and keeps everything it did (DASHBOARD.md #
// Install authorization): permissions, folder sources and subfolders, the
// storage estimate and its not-enough-space warning, the required-field gate,
// inline 422 errors, and the 409 duplicate warning with confirm-and-retry. It
// adds an Email row with an inline "add an account", and an AI providers row.
//
// Driven by GET /api/v1/catalog/:id/install-plan (advisory; the brain checks
// everything again on POST /api/v1/apps). Scope comes from the URL:
// ?scope=household is the split-button's "Install for the whole household".
// Anything else is personal.
//
// The layout is the Tailwind Plus left-aligned description list: one row per
// section, label on the left, content on the right, stacked on a phone.
//
// UI owns all wording. The brain returns structured values; we write sentences.
import { computed, ref, watch } from "vue";
import { useRoute, RouterLink } from "vue-router";
import { useQuery } from "@tanstack/vue-query";
import { ArrowLeft, TriangleAlert } from "lucide-vue-next";
import {
  api,
  type CatalogDetail,
  type FolderElection,
  type InstallPlan,
  type InstallPlanFolder,
  type InstallRequest,
  type Scope,
} from "../api";
import { useAuth } from "../auth";
import { useInstallSubmit } from "../useInstall";
import { formatSize } from "../utils";
import { aiSlots, claimedEnvs, fieldValues, type AIChoice } from "../aiProviders";
import AppGlyph from "../components/AppGlyph.vue";
import HealthGated from "../components/HealthGated.vue";
import Button from "../components/ui/Button.vue";
import Heading from "../components/ui/Heading.vue";
import ConfigFieldInput from "../components/install/ConfigFieldInput.vue";
import MailAccountSection from "../components/install/MailAccountSection.vue";
import AIProviderSection from "../components/install/AIProviderSection.vue";

const route = useRoute();
const { singleUserMode } = useAuth();

const manifestId = computed(() => String(route.params.id));
const scope = computed<Scope>(() => (route.query.scope === "household" ? "household" : "personal"));

const planQuery = useQuery({
  queryKey: computed(() => ["install-plan", manifestId.value]),
  queryFn: () => api.get<InstallPlan>(`/catalog/${encodeURIComponent(manifestId.value)}/install-plan`),
  staleTime: 0,
  // A refetch on focus would swap the plan under a half-filled form. The page
  // refetches on purpose only after an email account is added inline.
  refetchOnWindowFocus: false,
});
const plan = computed(() => planQuery.data.value ?? null);

// Same key as the detail page, so the icon is a cache read.
const detailQuery = useQuery({
  queryKey: computed(() => ["catalog", manifestId.value]),
  queryFn: () => api.get<CatalogDetail>(`/catalog/${encodeURIComponent(manifestId.value)}`),
});
const brokenIcon = ref(false);

const { submit, confirmDuplicate, dismissDuplicate, submitError, duplicateInfo, pending } =
  useInstallSubmit(manifestId);

// folders / devices normalize the brain's nullable arrays (a nil Go slice is
// null on the wire) so the template can iterate without guards.
const folders = computed(() => plan.value?.permissions.folders ?? []);
const devices = computed(() => plan.value?.permissions.devices ?? []);
const configFields = computed(() => plan.value?.config ?? []);

// ── Form state ──────────────────────────────────────────────────────────────
const folderSources = ref<Record<string, string>>({});
const folderSubfolders = ref<Record<string, string>>({});
const mailProviderId = ref(""); // "" is None
const configValues = ref<Record<string, string>>({});
const aiChoices = ref<Record<string, AIChoice>>({});

// Seed the form once per app, from the first plan that arrives. A later
// refetch (after an inline email account add) must not wipe what the user
// already typed.
let seededFor = "";
watch(
  [plan, scope],
  ([p]) => {
    if (!p || seededFor === `${p.manifest_id}|${scope.value}`) return;
    seededFor = `${p.manifest_id}|${scope.value}`;
    const sources: Record<string, string> = {};
    const subs: Record<string, string> = {};
    for (const f of p.permissions.folders ?? []) {
      sources[f.folder] = f.sources[scope.value].default;
      if (f.scope === "pick-subfolder") subs[f.folder] = f.subfolder_default ?? "";
    }
    folderSources.value = sources;
    folderSubfolders.value = subs;
    // A sole registered account is the obvious intent, so it starts chosen.
    // With several, the user picks.
    const providers = p.mail?.providers ?? [];
    mailProviderId.value = providers.length === 1 ? (providers[0]?.id ?? "") : "";
    const values: Record<string, string> = {};
    for (const f of p.config ?? []) values[f.app_env] = f.default ?? "";
    configValues.value = values;
    aiChoices.value = {};
  },
  { immediate: true },
);

// ── Storage footprint (DASHBOARD.md # the consent screen shows the on-disk
// footprint) ────────────────────────────────────────────────────────────────
// The page shows only the disk space the app takes. The download size and the
// "grows as you use it" estimate are left out on purpose. The row is skipped
// for an unsized manifest rather than showing a bare 0.
const fp = computed(() => plan.value?.footprint);
const hasSize = computed(() => (fp.value?.image_disk_bytes ?? 0) > 0);
// Warn, never block, when the projected need nears the free space. 90% is a
// UI judgement of "nears"; free_bytes 0 means the brain could not measure.
const notEnoughSpace = computed(() => {
  if (!fp.value) return false;
  const need = fp.value.image_disk_bytes + (fp.value.estimated_state_bytes ?? 0);
  return fp.value.free_bytes > 0 && need >= fp.value.free_bytes * 0.9;
});

// ── Folders ─────────────────────────────────────────────────────────────────
function sourceOptions(f: InstallPlanFolder): string[] {
  return f.sources[scope.value].options ?? [];
}

function folderName(folder: string): string {
  return folder.charAt(0).toUpperCase() + folder.slice(1);
}

function sourceLabel(folder: string, source: string): string {
  const name = folderName(folder);
  if (source === "shared") {
    return singleUserMode.value ? `Shared ${name} (accessible from your other devices)` : `The household's shared ${name}`;
  }
  return `Your ${name}`;
}

const noPermissions = computed(
  () =>
    !!plan.value &&
    !plan.value.permissions.internet &&
    !plan.value.permissions.lan &&
    !plan.value.permissions.gpu &&
    devices.value.length === 0 &&
    folders.value.length === 0,
);

// ── AI and plain settings ───────────────────────────────────────────────────
// The AI row takes the fields the temporary lookup in aiProviders.ts
// recognises. Every other field stays a plain input in the Settings row.
const slots = computed(() => aiSlots(configFields.value));
const claimed = computed(() => claimedEnvs(slots.value));
const plainFields = computed(() => configFields.value.filter((f) => !claimed.value.has(f.app_env)));

// fieldAnswers is what the app gets: the plain inputs, overlaid with the
// values each saved AI choice fills.
const fieldAnswers = computed<Record<string, string>>(() => {
  const out: Record<string, string> = {};
  for (const f of plainFields.value) out[f.app_env] = configValues.value[f.app_env] ?? "";
  for (const s of slots.value) {
    const c = aiChoices.value[s.id];
    if (c) Object.assign(out, fieldValues(s, c));
  }
  return out;
});

// Install stays disabled until every required field has a value: an app
// missing a required token would install straight into a crash loop.
const missing = computed(() =>
  configFields.value.filter((f) => f.required && (fieldAnswers.value[f.app_env] ?? "").trim() === ""),
);

// ── Submit ──────────────────────────────────────────────────────────────────
function buildRequest(p: InstallPlan): InstallRequest {
  const elections: FolderElection[] = folders.value.map((f) => {
    const e: FolderElection = { folder: f.folder };
    if (sourceOptions(f).length > 1) e.source = folderSources.value[f.folder];
    if (f.scope === "pick-subfolder") {
      const sub = folderSubfolders.value[f.folder];
      if (sub) e.subfolder = sub;
    }
    return e;
  });
  const req: InstallRequest = { manifest_id: p.manifest_id, scope: scope.value, config: { folders: elections } };
  if (p.mail && mailProviderId.value) req.config!.mail_provider_id = mailProviderId.value;
  // An optional field left blank is omitted, so the app keeps its own default.
  // A bool always carries "true" or "false".
  const fields: Record<string, string> = {};
  for (const [k, v] of Object.entries(fieldAnswers.value)) if (v !== "") fields[k] = v;
  if (Object.keys(fields).length > 0) req.config!.fields = fields;
  return req;
}

function onInstall() {
  if (!plan.value || missing.value.length > 0 || pending.value) return;
  submit(buildRequest(plan.value));
}

const scopeLine = computed(() => {
  if (scope.value === "household") return "For the whole household.";
  return singleUserMode.value ? "" : "Just for you.";
});

const rowClass = "px-4 py-6 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-0";
const dtClass = "text-sm/6 font-medium text-foreground";
const ddClass = "mt-2 text-sm/6 text-foreground sm:col-span-2 sm:mt-0";
</script>

<template>
  <div class="mx-auto w-full max-w-3xl space-y-6 pt-2 pb-10">
    <RouterLink
      :to="`/store/${manifestId}`"
      class="inline-flex items-center gap-1 text-sm text-muted-foreground transition-colors hover:text-foreground"
    >
      <ArrowLeft class="size-4" aria-hidden="true" /> {{ plan?.name ?? "Back" }}
    </RouterLink>

    <p v-if="planQuery.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
    <div v-else-if="planQuery.isError.value" class="space-y-2">
      <p class="text-sm text-destructive">
        Couldn't load what this app needs. {{ (planQuery.error.value as Error)?.message }}
      </p>
      <Button variant="secondary" size="sm" @click="planQuery.refetch()">Try again</Button>
    </div>

    <template v-else-if="plan">
      <header class="flex items-center gap-4 px-4 sm:px-0">
        <div
          class="grid size-14 shrink-0 place-items-center overflow-hidden rounded-2xl border border-border bg-card text-muted-foreground"
        >
          <img
            v-if="detailQuery.data.value?.icon_url && !brokenIcon"
            :src="detailQuery.data.value.icon_url"
            :alt="`${plan.name} icon`"
            class="size-full object-cover"
            @error="brokenIcon = true"
          />
          <AppGlyph v-else :name="detailQuery.data.value?.icon_glyph" class="size-7" />
        </div>
        <div class="min-w-0">
          <Heading :level="2">Install {{ plan.name }}</Heading>
          <p class="mt-1 text-sm text-muted-foreground">
            Version {{ plan.version }}. {{ scopeLine }}
          </p>
        </div>
      </header>

      <div class="border-t border-border">
        <dl class="divide-y divide-border">
          <!-- Permissions. Folder write access is drawn in red, so it stands
               out from read access (APP_ISOLATION.md # User content). -->
          <div :class="rowClass">
            <dt :class="dtClass">Permissions</dt>
            <dd :class="ddClass">
              <p v-if="noPermissions" class="text-muted-foreground">No special permissions needed.</p>
              <ul v-else class="list-disc space-y-1 pl-5 marker:text-muted-foreground">
                <li v-if="plan.permissions.internet">Connect to the internet</li>
                <li v-if="plan.permissions.lan">Reach other devices on your network</li>
                <li v-if="plan.permissions.gpu">Use the graphics card</li>
                <li v-for="d in devices" :key="d">Use the device {{ d }}</li>
                <li
                  v-for="f in folders"
                  :key="f.folder"
                  :class="f.mode === 'write' ? 'font-medium text-destructive marker:text-destructive' : ''"
                >
                  <template v-if="f.mode === 'write'">
                    Add, change, and delete files in your {{ folderName(f.folder) }} folder
                  </template>
                  <template v-else>Read files in your {{ folderName(f.folder) }} folder</template>
                </li>
              </ul>
            </dd>
          </div>

          <!-- Folders: which copy of each folder the app gets. -->
          <div v-if="folders.length > 0" :class="rowClass">
            <dt :class="dtClass">Folders</dt>
            <dd :class="[ddClass, 'space-y-5']">
              <div v-for="f in folders" :key="f.folder" class="space-y-2">
                <p class="font-medium">{{ folderName(f.folder) }}</p>
                <p v-if="sourceOptions(f).length === 1" class="text-muted-foreground">
                  {{ sourceLabel(f.folder, sourceOptions(f)[0] ?? "") }}
                </p>
                <fieldset v-else :aria-label="`Which ${folderName(f.folder)} folder`" class="space-y-2">
                  <label
                    v-for="opt in sourceOptions(f)"
                    :key="opt"
                    class="relative flex cursor-pointer items-center gap-3 rounded-lg border border-border bg-card px-4 py-2.5 hover:border-olive-400 has-checked:border-accent has-checked:outline-1 has-checked:-outline-offset-2 has-checked:outline-accent"
                  >
                    <input
                      v-model="folderSources[f.folder]"
                      type="radio"
                      :name="`folder-source-${f.folder}`"
                      :value="opt"
                      class="accent-accent"
                    />
                    {{ sourceLabel(f.folder, opt) }}
                  </label>
                </fieldset>
                <div v-if="f.scope === 'pick-subfolder'">
                  <label :for="`sub-${f.folder}`" class="block text-sm/6 text-muted-foreground">
                    Which subfolder should this app manage?
                  </label>
                  <input
                    :id="`sub-${f.folder}`"
                    v-model="folderSubfolders[f.folder]"
                    type="text"
                    :placeholder="f.subfolder_default ?? ''"
                    class="mt-2 block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 focus:outline-accent sm:text-sm/6"
                  />
                </div>
              </div>
            </dd>
          </div>

          <div v-if="plan.mail" :class="rowClass">
            <dt :class="dtClass">Email</dt>
            <dd :class="ddClass">
              <MailAccountSection
                v-model="mailProviderId"
                :manifest-id="manifestId"
                :app-name="plan.name"
                :providers="plan.mail.providers ?? []"
              />
            </dd>
          </div>

          <div v-if="slots.length > 0" :class="rowClass">
            <dt :class="dtClass">AI providers</dt>
            <dd :class="ddClass">
              <AIProviderSection v-model="aiChoices" :slots="slots" :app-name="plan.name" />
            </dd>
          </div>

          <div v-if="plainFields.length > 0" :class="rowClass">
            <dt :class="dtClass">Settings</dt>
            <dd :class="[ddClass, 'space-y-6']">
              <ConfigFieldInput
                v-for="f in plainFields"
                :key="f.app_env"
                v-model="configValues[f.app_env]!"
                :field="f"
              />
            </dd>
          </div>

          <div v-if="(hasSize || notEnoughSpace) && fp" :class="rowClass">
            <dt :class="dtClass">Size</dt>
            <dd :class="[ddClass, 'space-y-3']">
              <p v-if="hasSize">About {{ formatSize(fp.image_disk_bytes) }}</p>
              <p v-if="notEnoughSpace" class="flex gap-2 rounded-md bg-destructive/10 px-3 py-2 text-destructive">
                <TriangleAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
                This might not fit. Only about {{ formatSize(fp.free_bytes) }} is free on your box. You can still
                install.
              </p>
            </dd>
          </div>
        </dl>
      </div>

      <!-- 409: another copy exists. Warn, don't block. -->
      <div v-if="duplicateInfo" class="mx-4 space-y-3 rounded-lg border border-border bg-card px-4 py-3 sm:mx-0">
        <p class="text-sm">{{ duplicateInfo }}</p>
        <div class="flex flex-wrap gap-2">
          <Button size="sm" :disabled="pending" @click="confirmDuplicate">Install my own copy</Button>
          <Button size="sm" variant="ghost" @click="dismissDuplicate">Cancel</Button>
        </div>
      </div>

      <p
        v-if="submitError"
        class="mx-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive sm:mx-0"
        role="alert"
      >
        {{ submitError }}
      </p>

      <div class="flex flex-col-reverse gap-3 border-t border-border px-4 pt-6 sm:flex-row sm:items-center sm:justify-end sm:px-0">
        <p v-if="missing.length > 0" class="text-sm text-muted-foreground sm:mr-auto">
          Still needed: {{ missing.map((f) => f.title).join(", ") }}.
        </p>
        <div class="flex gap-2">
          <Button variant="ghost" :as="RouterLink" :to="`/store/${manifestId}`">Cancel</Button>
          <HealthGated blocks="apps">
            <Button :disabled="missing.length > 0 || pending || !!duplicateInfo" @click="onInstall">
              {{ pending ? "Starting…" : "Install" }}
            </Button>
          </HealthGated>
        </div>
      </div>
    </template>
  </div>
</template>
