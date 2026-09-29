<script setup lang="ts">
// The install flow (/store/:id/install), one question per page
// (docs/specs/INSTALL_STEPS.md). The App page's Install button lands here.
// A question page is ?step=<name>; the last page has no step. The scope
// (?scope=household) rides on every page.
//
//   App page ──▶ [first-time questions] ──▶ last page ──Install──▶ progress
//                                             ▲
//                                             └── Change opens one page, then comes back
//
// The first-time questions are only the needs with no saved answer: an AI
// group with no usable account, and the "needs these to run" page of required
// plain fields. The last page shows every answer with a Change link, then the
// optional rows (AI service, Email, Extra settings) with Set up. installSteps.ts
// sorts the plan into these pages (INSTALL_STEPS.md # Build rules).
//
// Opening the last page while a required need has no answer redirects to the
// first page that owns one, so the last page is only reached with its answers
// in place. The draft lives in the tab's session storage (non-secret answers
// only), so a reload or a trip to a provider's site in another tab loses
// nothing but a typed secret.
//
// Step 1 of the plan's build keeps today's pickers behind these pages
// (AISlotPicker, MailAccountSection). Steps 2 and 3 replace them with the key
// list, the service grid and the key form.
//
// Driven by GET /api/v1/catalog/:id/install-plan (advisory; the brain checks
// everything again on POST /api/v1/apps). The UI owns all wording.
import { computed, nextTick, onBeforeUnmount, ref, watch } from "vue";
import { useRoute, useRouter, RouterLink } from "vue-router";
import { useQuery } from "@tanstack/vue-query";
import { ArrowLeft, TriangleAlert } from "lucide-vue-next";
import {
  api,
  type AIAccount,
  type AIProvider,
  type CatalogDetail,
  type FolderElection,
  type InstallPlan,
  type InstallPlanFolder,
  type InstallRequest,
  type Scope,
} from "../api";
import { useAuth } from "../auth";
import { useAppInstances, useInstallSubmit } from "../useInstall";
import { aiSlots, bindingOf, findProvider, groupNeed, unmetGroups, type AIChoice } from "../aiProviders";
import {
  STEP_AI_OPTIONAL,
  STEP_EMAIL,
  STEP_EXTRA,
  STEP_FOLDERS,
  STEP_FOR,
  STEP_SETTINGS,
  clearDrafts,
  draftKey,
  filledBy,
  folderName,
  groupMet,
  loadDraft,
  pickInAdvance,
  planNeeds,
  requiredFieldsMet,
  saveDraft,
  sourceLabel,
  stepForError,
  type AIGroupNeed,
  type Draft,
} from "../installSteps";
import AppGlyph from "../components/AppGlyph.vue";
import HealthGated from "../components/HealthGated.vue";
import Button from "../components/ui/Button.vue";
import Heading from "../components/ui/Heading.vue";
import ConfigFieldInput from "../components/install/ConfigFieldInput.vue";
import FolderChoices from "../components/install/FolderChoices.vue";
import InstallInfoBox from "../components/install/InstallInfoBox.vue";
import MailAccountSection from "../components/install/MailAccountSection.vue";
import AISlotPicker from "../components/AISlotPicker.vue";

const route = useRoute();
const router = useRouter();
const { currentUser, singleUserMode } = useAuth();

const manifestId = computed(() => String(route.params.id));
const scope = computed<Scope>(() => (route.query.scope === "household" ? "household" : "personal"));
const step = computed(() => (typeof route.query.step === "string" ? route.query.step : ""));
const userId = computed(() => currentUser.value?.id ?? "");

const { canInstallHousehold, householdInstance, ownPersonalInstance } = useAppInstances(manifestId);

// ── Data ────────────────────────────────────────────────────────────────────
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

// AI provider data from the catalog (INSTALL_SETUP.md # 4). A failed fetch is
// not an error: the page falls back to plain fields, so an install is never
// blocked by it.
const providersQuery = useQuery({
  queryKey: ["ai-providers"],
  queryFn: () => api.get<{ providers: AIProvider[] | null }>("/ai-providers"),
  staleTime: 5 * 60_000,
  retry: 1,
  refetchOnWindowFocus: false,
});
const providers = computed(() => providersQuery.data.value?.providers ?? []);

const configFields = computed(() => plan.value?.config ?? []);
const requires = computed(() => plan.value?.requires ?? []);
const folders = computed(() => plan.value?.permissions.folders ?? []);
const needs = computed(() => planNeeds(configFields.value, requires.value, providers.value));

// The caller's AI accounts, the same query AISlotPicker uses. They decide
// which AI group is already answered, and what a binding fills.
const aiAccountsQuery = useQuery({
  queryKey: ["ai-accounts"],
  queryFn: () => api.get<{ accounts: AIAccount[] | null }>("/ai-accounts"),
  enabled: computed(() => needs.value.slots.length > 0),
  refetchOnWindowFocus: false,
});
const accounts = computed(() => aiAccountsQuery.data.value?.accounts ?? []);

// Wait for everything the flow depends on before choosing a page, so the
// fields do not jump between pages and the redirect does not guess.
// isPending, not isLoading, for the accounts: in the tick after the query is
// enabled it is not fetching yet, and the page would decide on no accounts.
const hasAIFields = computed(() => aiSlots(configFields.value).length > 0);
const ready = computed(
  () =>
    !!plan.value &&
    !(hasAIFields.value && providersQuery.isPending.value) &&
    !(needs.value.slots.length > 0 && aiAccountsQuery.isPending.value),
);

// ── Draft ───────────────────────────────────────────────────────────────────
const flow = ref<string[]>([]);
const done = ref<string[]>([]);
const reviewed = ref(false);
const folderSources = ref<Record<string, string>>({});
const folderSubfolders = ref<Record<string, string>>({});
const mailProviderId = ref(""); // "" is not set up
const configValues = ref<Record<string, string>>({});
const aiChoices = ref<Record<string, AIChoice>>({});
const foldersReset = ref(false);

const secretEnvs = computed(() => new Set(configFields.value.filter((f) => f.secret).map((f) => f.app_env)));
const key = computed(() => draftKey(userId.value, manifestId.value, scope.value));

// Seed the draft once per app and scope, from the tab's saved draft if there
// is one, else from the plan's defaults and the user's saved accounts. A
// later refetch (after an inline email account add) must not wipe it.
let seededFor = "";
function seed(p: InstallPlan) {
  const saved = loadDraft(key.value);
  const sources: Record<string, string> = {};
  const subs: Record<string, string> = {};
  for (const f of p.permissions.folders ?? []) {
    const menu = f.sources[scope.value];
    const s = saved?.sources?.[f.folder];
    sources[f.folder] = s && (menu.options ?? []).includes(s) ? s : menu.default;
    if (f.scope === "pick-subfolder") subs[f.folder] = saved?.subfolders?.[f.folder] ?? f.subfolder_default ?? "";
  }
  folderSources.value = sources;
  folderSubfolders.value = subs;

  const mailIds = new Set((p.mail?.providers ?? []).map((m) => m.id));
  mailProviderId.value = saved?.mail && mailIds.has(saved.mail) ? saved.mail : "";

  // A secret typed before a scope change stays: it is still in memory.
  const values: Record<string, string> = {};
  for (const f of p.config ?? []) {
    const kept = secretEnvs.value.has(f.app_env) ? configValues.value[f.app_env] : saved?.values?.[f.app_env];
    values[f.app_env] = kept ?? f.default ?? "";
  }
  configValues.value = values;

  const slotIds = new Set(needs.value.slots.map((s) => s.id));
  const choices: Record<string, AIChoice> = {};
  for (const [slot, c] of Object.entries(saved?.ai ?? {})) if (slotIds.has(slot) && c?.accountId) choices[slot] = c;

  if (saved?.flow) {
    flow.value = saved.flow;
    done.value = saved.done ?? [];
    reviewed.value = !!saved.reviewed;
    foldersReset.value = !!saved.foldersReset;
  } else {
    // A first visit: an AI group a saved account can fill is answered, and
    // its key is picked in advance. The other groups, and the required plain
    // fields, are the first-time pages.
    const ask: string[] = [];
    for (const g of needs.value.aiGroups) {
      if (g.slots.some((s) => choices[s.id])) continue;
      const pick = pickInAdvance(g, configFields.value, providers.value, accounts.value);
      if (pick) choices[pick.slotId] = pick.choice;
      else ask.push(g.step);
    }
    if (needs.value.requiredFields.length > 0) ask.push(STEP_SETTINGS);
    flow.value = ask;
    done.value = [];
    reviewed.value = false;
    foldersReset.value = false;
  }
  aiChoices.value = choices;
}

watch(
  [ready, key],
  () => {
    if (!ready.value || !plan.value || !userId.value) return;
    if (seededFor === key.value) return;
    seededFor = key.value;
    seed(plan.value);
    route.query.step ? checkStep() : redirect();
  },
  { immediate: true },
);

const draft = computed<Draft>(() => ({
  flow: flow.value,
  done: done.value,
  reviewed: reviewed.value,
  sources: folderSources.value,
  subfolders: folderSubfolders.value,
  mail: mailProviderId.value,
  values: configValues.value,
  ai: aiChoices.value,
  foldersReset: foldersReset.value,
}));
watch(
  draft,
  (d) => {
    if (seededFor === key.value) saveDraft(key.value, d, secretEnvs.value);
  },
  { deep: true },
);

// ── Answers ─────────────────────────────────────────────────────────────────
const plainValues = computed(() => {
  const claimed = new Set(needs.value.slots.flatMap((s) => s.fields.map((f) => f.app_env)));
  const out: Record<string, string> = {};
  for (const f of configFields.value) if (!claimed.has(f.app_env)) out[f.app_env] = configValues.value[f.app_env] ?? "";
  return out;
});
const filled = computed(() => filledBy(needs.value.slots, aiChoices.value, accounts.value));

function aiGroupOf(name: string): AIGroupNeed | undefined {
  return needs.value.aiGroups.find((g) => g.step === name);
}

// answered says whether a page's need has what it must have. Only required
// pages can be unanswered; the others always are.
function answered(name: string): boolean {
  const g = aiGroupOf(name);
  if (g) return groupMet(g.group, configFields.value, plainValues.value, filled.value);
  if (name === STEP_SETTINGS) return requiredFieldsMet(needs.value, configFields.value, plainValues.value);
  return true;
}

const requiredSteps = computed(() => [
  ...needs.value.aiGroups.map((g) => g.step),
  ...(needs.value.requiredFields.length > 0 ? [STEP_SETTINGS] : []),
]);

// validSteps is every page this app has.
const validSteps = computed(() => {
  const out = new Set(requiredSteps.value);
  if (needs.value.optionalSlots.length > 0) out.add(STEP_AI_OPTIONAL);
  if (plan.value?.mail) out.add(STEP_EMAIL);
  if (needs.value.extraFields.length > 0) out.add(STEP_EXTRA);
  if (folders.value.some((f) => folderHasChoice(f))) out.add(STEP_FOLDERS);
  if (canInstallHousehold.value) out.add(STEP_FOR);
  return out;
});

// ── Navigation ──────────────────────────────────────────────────────────────
function to(name: string, s: Scope = scope.value) {
  const query: Record<string, string> = {};
  if (s === "household") query.scope = "household";
  if (name) query.step = name;
  return { path: `/store/${encodeURIComponent(manifestId.value)}/install`, query };
}

// target is the page the last page sends the user to, or "" to stay: the
// first first-time page not yet done or answered, else the first required
// page left unanswered.
function target(): string {
  const next = flow.value.find((n) => !done.value.includes(n) || !answered(n));
  if (next) return next;
  return requiredSteps.value.find((n) => !answered(n)) ?? "";
}

function redirect() {
  const t = target();
  if (t) router.replace(to(t));
  else reviewed.value = true;
}

// A step this app does not have (an old link, a changed manifest) goes to
// the last page, which redirects again if it must.
function checkStep() {
  if (!validSteps.value.has(step.value)) router.replace(to(""));
}

watch([step, scope], () => {
  if (seededFor !== key.value) return;
  if (step.value === "") redirect();
  else checkStep();
});

// next is where Continue goes: back to the last page once it was shown,
// else the next first-time page, else the last page.
function next(from: string): string {
  if (reviewed.value) return "";
  const i = flow.value.indexOf(from);
  return flow.value.slice(i + 1).find((n) => !done.value.includes(n) || !answered(n)) ?? "";
}

function markDone(name: string) {
  if (!done.value.includes(name)) done.value = [...done.value, name];
}

// ── Pages ───────────────────────────────────────────────────────────────────
const appName = computed(() => plan.value?.name ?? "");

const isLast = computed(() => step.value === "");
const firstPage = computed(() => flow.value[0] ?? "");
const onFirstPage = computed(() => step.value === firstPage.value);
// The step counter counts the App page, the first-time pages, and the last
// page. A page opened with Change from the last page has no number.
const stepTotal = computed(() => flow.value.length + 2);
const stepNumber = computed(() => {
  if (isLast.value) return stepTotal.value;
  const i = flow.value.indexOf(step.value);
  return i < 0 || reviewed.value ? 0 : i + 2;
});

const pageTitle = computed(() => {
  const name = appName.value;
  if (isLast.value) return `Ready to install ${name}`;
  if (aiGroupOf(step.value) || step.value === STEP_AI_OPTIONAL) return `Which AI service should ${name} use?`;
  switch (step.value) {
    case STEP_SETTINGS:
      return `${name} needs these to run`;
    case STEP_EMAIL:
      return `Which email should ${name} send from?`;
    case STEP_EXTRA:
      return "Extra settings";
    case STEP_FOLDERS:
      return `Which folders should ${name} use?`;
    case STEP_FOR:
      return `Who is ${name} for?`;
  }
  return name;
});

// Each page moves focus to its heading and names the tab after it
// (INSTALL_STEPS.md # 6).
const heading = ref<InstanceType<typeof Heading> | null>(null);
const originalTitle = document.title;
watch(
  [pageTitle, () => route.fullPath, ready],
  async () => {
    if (!ready.value || !plan.value) return;
    document.title = pageTitle.value;
    await nextTick();
    (heading.value?.$el as HTMLElement | undefined)?.focus();
  },
  { immediate: true },
);
onBeforeUnmount(() => {
  document.title = originalTitle;
});

// Page-local copies. A page that is not required saves its answer on
// Continue, not as the user clicks, so leaving it with Back changes nothing.
// That keeps an optional need "not set up" until the user means it.
const pageMail = ref("");
const pageSources = ref<Record<string, string>>({});
const pageSubfolders = ref<Record<string, string>>({});
const pageScope = ref<Scope>("personal");
const scopeOptions: Scope[] = ["personal", "household"];
watch(
  [step, ready],
  () => {
    if (step.value === STEP_EMAIL) {
      // Set up opens with the newest saved account picked (INSTALL_STEPS.md
      // # Decisions); Change opens with the current one.
      const newest = [...(plan.value?.mail?.providers ?? [])].sort((a, b) => b.created_at - a.created_at)[0];
      pageMail.value = mailProviderId.value || newest?.id || "";
    }
    if (step.value === STEP_FOLDERS) {
      pageSources.value = { ...folderSources.value };
      pageSubfolders.value = { ...folderSubfolders.value };
    }
    if (step.value === STEP_FOR) pageScope.value = scope.value;
    // The "Folders reset" note is said once, on the last page.
    if (step.value !== "") foldersReset.value = false;
  },
  { immediate: true },
);

const canContinue = computed(() => answered(step.value));

// settingsNeeded names what the "needs these to run" page still misses.
const settingsNeeded = computed(() => [
  ...needs.value.requiredFields
    .filter((f) => f.required && (plainValues.value[f.app_env] ?? "").trim() === "")
    .map((f) => f.title),
  ...unmetGroups(needs.value.plainGroups, configFields.value, plainValues.value, new Set()).map((g) =>
    groupNeed(configFields.value, g),
  ),
]);

function onContinue() {
  const name = step.value;
  if (!canContinue.value) return;
  if (pageError.value?.step === name) pageError.value = null;
  if (name === STEP_EMAIL) mailProviderId.value = pageMail.value;
  if (name === STEP_FOLDERS) {
    folderSources.value = { ...pageSources.value };
    folderSubfolders.value = { ...pageSubfolders.value };
  }
  if (name === STEP_FOR) {
    changeScope(pageScope.value);
    return;
  }
  markDone(name);
  router.push(to(next(name)));
}

// changeScope moves the draft to the other scope's key with the folders back
// on that scope's defaults, since folder sources differ per scope, and says
// so on the last page.
function changeScope(s: Scope) {
  if (s === scope.value) {
    router.push(to(""));
    return;
  }
  saveDraft(
    draftKey(userId.value, manifestId.value, s),
    { ...draft.value, sources: {}, subfolders: {}, reviewed: true, foldersReset: folders.value.length > 0 },
    secretEnvs.value,
  );
  router.push(to("", s));
}

function backLink() {
  if (isLast.value || reviewed.value) return isLast.value ? `/store/${manifestId.value}` : to("");
  const i = flow.value.indexOf(step.value);
  return i > 0 ? to(flow.value[i - 1]!) : `/store/${manifestId.value}`;
}

function cancel() {
  clearDrafts(userId.value, manifestId.value);
  seededFor = "";
  router.push(`/store/${manifestId.value}`);
}

// ── Last page rows ──────────────────────────────────────────────────────────
function folderHasChoice(f: InstallPlanFolder): boolean {
  return (f.sources[scope.value].options ?? []).length > 1 || f.scope === "pick-subfolder";
}

function folderValue(f: InstallPlanFolder): string {
  const src = sourceLabel(f.folder, folderSources.value[f.folder] ?? f.sources[scope.value].default, singleUserMode.value);
  const sub = folderSubfolders.value[f.folder];
  return f.scope === "pick-subfolder" && sub ? `${src}, subfolder ${sub}` : src;
}

// aiSummary names what fills a set of slots: "Anthropic, key 'Work key'".
function aiSummary(slots: { id: string }[]): string {
  const parts: string[] = [];
  for (const s of slots) {
    const c = aiChoices.value[s.id];
    if (!c) continue;
    const provider = findProvider(providers.value, c.provider)?.name ?? "AI service";
    const account = accounts.value.find((a) => a.id === c.accountId);
    parts.push(account ? `${provider}, key '${account.label}'` : provider);
  }
  return parts.join("; ");
}

const mailLabel = computed(
  () => plan.value?.mail?.providers?.find((m) => m.id === mailProviderId.value)?.label ?? "",
);

const extraCount = computed(
  () =>
    needs.value.extraFields.filter((f) => {
      const v = (configValues.value[f.app_env] ?? "").trim();
      return v !== "" && v !== (f.default ?? "");
    }).length,
);

// ── Install gate ────────────────────────────────────────────────────────────
// Install stays disabled until every required field has a value and every
// requires group has a filled member, as before; the redirect rule means
// this only happens after a Change removed an answer. The brain checks both
// again.
const stillNeeded = computed(() => {
  const missing = configFields.value.filter(
    (f) => f.required && !filled.value.has(f.app_env) && (plainValues.value[f.app_env] ?? "").trim() === "",
  );
  const unmet = unmetGroups(requires.value, configFields.value, plainValues.value, filled.value);
  return [
    ...missing.map((f) => f.title),
    ...unmet.map((g) => groupNeed(configFields.value, g).replace("an LLM provider", "an AI service")),
  ];
});

const { submit, confirmDuplicate, dismissDuplicate, submitError, duplicateInfo, pending } = useInstallSubmit(
  manifestId,
  () => {
    clearDrafts(userId.value, manifestId.value);
    seededFor = "";
  },
);

function buildRequest(p: InstallPlan): InstallRequest {
  const elections: FolderElection[] = folders.value.map((f) => {
    const e: FolderElection = { folder: f.folder };
    if ((f.sources[scope.value].options ?? []).length > 1) e.source = folderSources.value[f.folder];
    if (f.scope === "pick-subfolder") {
      const sub = folderSubfolders.value[f.folder];
      if (sub) e.subfolder = sub;
    }
    return e;
  });
  const req: InstallRequest = { manifest_id: p.manifest_id, scope: scope.value, config: { folders: elections } };
  // The first page showed the copies the plan listed, so the user has seen
  // the warning. A copy made after the plan loaded still answers 409.
  if ((p.existing ?? []).length > 0) req.confirm = true;
  if (p.mail && mailProviderId.value) req.config!.mail_provider_id = mailProviderId.value;
  // An optional field left blank is omitted, so the app keeps its own default.
  // A bool always carries "true" or "false".
  const fields: Record<string, string> = {};
  for (const [k, v] of Object.entries(plainValues.value)) if (v !== "") fields[k] = v;
  if (Object.keys(fields).length > 0) req.config!.fields = fields;
  const bindings = needs.value.slots
    .filter((s) => aiChoices.value[s.id])
    .map((s) => bindingOf(s, aiChoices.value[s.id]!));
  if (bindings.length > 0) req.config!.ai_bindings = bindings;
  return req;
}

function onInstall() {
  if (!plan.value || stillNeeded.value.length > 0 || pending.value) return;
  pageError.value = null;
  submit(buildRequest(plan.value));
}

// A 422 goes to the page that owns the field, with the error there
// (INSTALL_STEPS.md # 2, Errors). One the flow cannot place stays on the
// last page.
const pageError = ref<{ step: string; message: string } | null>(null);
watch(submitError, (message) => {
  if (!message || !plan.value) return;
  const owner = stepForError(message, needs.value, configFields.value, !!plan.value.mail);
  if (!owner || !validSteps.value.has(owner)) return;
  pageError.value = { step: owner, message };
  submitError.value = null;
  router.push(to(owner));
});

// ── Warnings on the first page ──────────────────────────────────────────────
// The duplicate warning shows on the first page, before any question
// (INSTALL_STEPS.md # Decisions). The copies come with the plan.
const existing = computed(() => plan.value?.existing ?? []);
const duplicateLines = computed(() =>
  existing.value.map((c) => {
    if (c.scope === "household") return `${c.name} is already installed for everyone at home.`;
    if (c.mine) return `You already have your own copy of ${c.name}.`;
    return `Someone else on this box has their own copy of ${c.name}.`;
  }),
);
// The copy to offer as "Open it": the household one, else the user's own.
const openable = computed(() => {
  const i = householdInstance.value ?? ownPersonalInstance.value;
  return i && i.state !== "installing" && i.url ? i : undefined;
});

const rowClass = "px-4 py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-0";
const dtClass = "text-sm/6 font-medium text-foreground";
const ddClass = "mt-1 flex items-start justify-between gap-4 text-sm/6 text-foreground sm:col-span-2 sm:mt-0";
const changeClass = "shrink-0 font-medium text-accent hover:underline";
</script>

<template>
  <div class="mx-auto w-full max-w-3xl space-y-6 pt-2 pb-10">
    <RouterLink
      :to="backLink()"
      class="inline-flex items-center gap-1 text-sm text-muted-foreground transition-colors hover:text-foreground"
    >
      <ArrowLeft class="size-4" aria-hidden="true" /> Back
    </RouterLink>

    <p v-if="planQuery.isLoading.value || (plan && !ready)" class="text-sm text-muted-foreground">Loading…</p>
    <div v-else-if="planQuery.isError.value" class="space-y-2">
      <p class="text-sm text-destructive">
        Couldn't load what this app needs. {{ (planQuery.error.value as Error)?.message }}
      </p>
      <Button variant="secondary" size="sm" @click="planQuery.refetch()">Try again</Button>
    </div>

    <template v-else-if="plan">
      <!-- Header: the app, then the page's question. -->
      <header class="space-y-4 px-4 sm:px-0">
        <div class="flex items-center gap-3">
          <div
            class="grid size-10 shrink-0 place-items-center overflow-hidden rounded-xl border border-border bg-card text-muted-foreground"
          >
            <img
              v-if="detailQuery.data.value?.icon_url && !brokenIcon"
              :src="detailQuery.data.value.icon_url"
              alt=""
              class="size-full object-cover"
              @error="brokenIcon = true"
            />
            <AppGlyph v-else :name="detailQuery.data.value?.icon_glyph" class="size-5" />
          </div>
          <p class="text-sm text-muted-foreground">
            <span class="font-medium text-foreground">{{ plan.name }}</span>
            <template v-if="stepNumber > 0"> · Step {{ stepNumber }} of {{ stepTotal }}</template>
          </p>
        </div>
        <Heading ref="heading" :level="2" tabindex="-1" class="outline-none">{{ pageTitle }}</Heading>
        <InstallInfoBox
          v-if="isLast || onFirstPage"
          :permissions="plan.permissions"
          :footprint="plan.footprint"
        />
      </header>

      <!-- Warnings on the first page, before any question. -->
      <div
        v-if="existing.length > 0 && (onFirstPage || (isLast && flow.length === 0))"
        class="mx-4 space-y-3 rounded-lg border border-warning/40 bg-warning/10 px-4 py-3 text-sm sm:mx-0"
        role="status"
      >
        <p v-for="line in duplicateLines" :key="line" class="flex gap-2">
          <TriangleAlert class="mt-0.5 size-4 shrink-0 text-warning" aria-hidden="true" />
          {{ line }}
        </p>
        <p>You can open it, or go on to install another copy.</p>
        <Button v-if="openable" size="sm" variant="secondary" as="a" :href="openable.url" target="_blank" rel="noopener">
          Open {{ openable.name }}
        </Button>
      </div>

      <p
        v-if="pageError && pageError.step === step"
        class="mx-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive sm:mx-0"
        role="alert"
      >
        {{ pageError.message }}
      </p>

      <!-- ── Question pages ─────────────────────────────────────────────── -->
      <template v-if="!isLast">
        <div class="px-4 sm:px-0">
          <AISlotPicker
            v-if="aiGroupOf(step)"
            v-model="aiChoices"
            :slots="aiGroupOf(step)!.slots"
            :providers="providers"
            :app-name="plan.name"
          />
          <AISlotPicker
            v-else-if="step === 'ai-optional'"
            v-model="aiChoices"
            :slots="needs.optionalSlots"
            :providers="providers"
            :app-name="plan.name"
          />
          <div v-else-if="step === 'settings'" class="space-y-6">
            <ConfigFieldInput
              v-for="f in needs.requiredFields"
              :key="f.app_env"
              v-model="configValues[f.app_env]!"
              :field="f"
            />
          </div>
          <div v-else-if="step === 'extra'" class="space-y-6">
            <p class="text-sm text-muted-foreground">
              {{ plan.name }} runs without these. Fill in only what you need.
            </p>
            <ConfigFieldInput
              v-for="f in needs.extraFields"
              :key="f.app_env"
              v-model="configValues[f.app_env]!"
              :field="f"
            />
          </div>
          <MailAccountSection
            v-else-if="step === 'email' && plan.mail"
            v-model="pageMail"
            :manifest-id="manifestId"
            :app-name="plan.name"
            :providers="plan.mail.providers ?? []"
          />
          <FolderChoices
            v-else-if="step === 'folders'"
            v-model:sources="pageSources"
            v-model:subfolders="pageSubfolders"
            :folders="folders"
            :scope="scope"
          />
          <fieldset v-else-if="step === 'for'" :aria-label="`Who ${plan.name} is for`" class="space-y-2">
            <label
              v-for="opt in scopeOptions"
              :key="opt"
              class="relative flex cursor-pointer items-center gap-3 rounded-lg border border-border bg-card px-4 py-2.5 text-sm hover:border-olive-400 has-checked:border-accent has-checked:outline-1 has-checked:-outline-offset-2 has-checked:outline-accent"
            >
              <input v-model="pageScope" type="radio" name="install-for" :value="opt" class="accent-accent" />
              {{ opt === "personal" ? "Just you" : "Everyone at home" }}
            </label>
            <p v-if="folders.length > 0" class="pt-2 text-sm text-muted-foreground">
              Changing this puts the folders back on their defaults.
            </p>
          </fieldset>
        </div>

        <div
          class="flex flex-col-reverse gap-3 border-t border-border px-4 pt-6 sm:flex-row sm:items-center sm:justify-end sm:px-0"
        >
          <p v-if="!canContinue && aiGroupOf(step)" class="text-sm text-muted-foreground sm:mr-auto">
            Pick an AI service and add it to go on.
          </p>
          <p v-else-if="!canContinue" class="text-sm text-muted-foreground sm:mr-auto">
            Still needed: {{ settingsNeeded.join("; ") }}.
          </p>
          <div class="flex gap-2">
            <Button variant="ghost" @click="cancel">Cancel</Button>
            <Button :disabled="!canContinue" @click="onContinue">Continue</Button>
          </div>
        </div>
      </template>

      <!-- ── Last page ──────────────────────────────────────────────────── -->
      <template v-else>
        <div class="border-t border-border">
          <dl class="divide-y divide-border">
            <div v-if="canInstallHousehold" :class="rowClass">
              <dt :class="dtClass">For</dt>
              <dd :class="ddClass">
                <span>{{ scope === "household" ? "Everyone at home" : "Just you" }}</span>
                <RouterLink :to="to(STEP_FOR)" :class="changeClass">Change</RouterLink>
              </dd>
            </div>

            <div v-for="g in needs.aiGroups" :key="g.step" :class="rowClass">
              <dt :class="dtClass">AI service</dt>
              <dd :class="ddClass">
                <span v-if="aiSummary(g.slots)">{{ aiSummary(g.slots) }}</span>
                <span v-else class="text-destructive">Needed</span>
                <RouterLink :to="to(g.step)" :class="changeClass">Change</RouterLink>
              </dd>
            </div>

            <div v-if="needs.requiredFields.length > 0" :class="rowClass">
              <dt :class="dtClass">Settings</dt>
              <dd :class="ddClass">
                <span v-if="answered(STEP_SETTINGS)">Filled in</span>
                <span v-else class="text-destructive">Needed</span>
                <RouterLink :to="to(STEP_SETTINGS)" :class="changeClass">Change</RouterLink>
              </dd>
            </div>

            <div v-for="f in folders" :key="f.folder" :class="rowClass">
              <dt :class="dtClass">{{ folderName(f.folder) }}</dt>
              <dd :class="ddClass">
                <span>{{ folderValue(f) }}</span>
                <RouterLink v-if="folderHasChoice(f)" :to="to(STEP_FOLDERS)" :class="changeClass">Change</RouterLink>
              </dd>
            </div>

            <div v-if="needs.optionalSlots.length > 0" :class="rowClass">
              <dt :class="dtClass">AI service</dt>
              <dd :class="ddClass">
                <span v-if="aiSummary(needs.optionalSlots)">{{ aiSummary(needs.optionalSlots) }}</span>
                <span v-else class="text-muted-foreground">Not set up</span>
                <RouterLink :to="to(STEP_AI_OPTIONAL)" :class="changeClass">
                  {{ aiSummary(needs.optionalSlots) ? "Change" : "Set up" }}
                </RouterLink>
              </dd>
            </div>

            <div v-if="plan.mail" :class="rowClass">
              <dt :class="dtClass">Email</dt>
              <dd :class="ddClass">
                <span v-if="mailLabel">{{ mailLabel }}</span>
                <span v-else class="text-muted-foreground">Not set up</span>
                <RouterLink :to="to(STEP_EMAIL)" :class="changeClass">{{ mailLabel ? "Change" : "Set up" }}</RouterLink>
              </dd>
            </div>

            <div v-if="needs.extraFields.length > 0" :class="rowClass">
              <dt :class="dtClass">Extra settings</dt>
              <dd :class="ddClass">
                <span v-if="extraCount > 0">{{ extraCount }} filled in</span>
                <span v-else class="text-muted-foreground">Not set up</span>
                <RouterLink :to="to(STEP_EXTRA)" :class="changeClass">{{ extraCount > 0 ? "Change" : "Set up" }}</RouterLink>
              </dd>
            </div>
          </dl>
        </div>

        <p v-if="foldersReset" class="mx-4 text-sm text-muted-foreground sm:mx-0" role="status">
          Folders reset for {{ scope === "household" ? "everyone at home" : "just you" }}.
        </p>

        <!-- 409: a copy appeared after the plan loaded (a second tab, a race).
             Warn, don't block. -->
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

        <div
          class="flex flex-col-reverse gap-3 border-t border-border px-4 pt-6 sm:flex-row sm:items-center sm:justify-end sm:px-0"
        >
          <p v-if="stillNeeded.length > 0" class="text-sm text-muted-foreground sm:mr-auto">
            Still needed: {{ stillNeeded.join("; ") }}.
          </p>
          <div class="flex gap-2">
            <Button variant="ghost" @click="cancel">Cancel</Button>
            <HealthGated blocks="apps">
              <Button :disabled="stillNeeded.length > 0 || pending || !!duplicateInfo" @click="onInstall">
                {{ pending ? "Starting…" : "Install" }}
              </Button>
            </HealthGated>
          </div>
        </div>
      </template>
    </template>
  </div>
</template>
