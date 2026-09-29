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
// An AI need has its own pages (INSTALL_STEPS.md # 3): the key list, the
// service grid and the key form. Email has the same three (# 4): the account
// list, the email service grid and the add form. There is no Save inside any
// of them: Continue saves.
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
import { useMailPresets } from "../mailProviderForm";
import {
  aiSlots,
  bindingOf,
  findProvider,
  groupNeed,
  isOther,
  slotFor,
  unmetGroups,
  type AIChoice,
} from "../aiProviders";
import {
  STEP_AI_OPTIONAL,
  SUB_KEY,
  SUB_SERVICE,
  aiNeeds,
  choiceFor,
  needOfStep,
  needTiles,
  tileOf,
  usableAccounts,
  withNeedChoice,
  STEP_EMAIL,
  STEP_EMAIL_ADD,
  STEP_EMAIL_SERVICE,
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
  type AINeed,
  type Draft,
} from "../installSteps";
import AppGlyph from "../components/AppGlyph.vue";
import HealthGated from "../components/HealthGated.vue";
import Button from "../components/ui/Button.vue";
import Heading from "../components/ui/Heading.vue";
import ConfigFieldInput from "../components/install/ConfigFieldInput.vue";
import FolderChoices from "../components/install/FolderChoices.vue";
import InstallInfoBox from "../components/install/InstallInfoBox.vue";
import MailAddForm from "../components/install/MailAddForm.vue";
import MailProviderLogo from "../components/MailProviderLogo.vue";
import MailServiceGrid from "../components/install/MailServiceGrid.vue";
import AIKeyForm from "../components/install/AIKeyForm.vue";
import AccountList from "../components/install/AccountList.vue";
import AIProviderLogo from "../components/AIProviderLogo.vue";
import ServiceGrid from "../components/install/ServiceGrid.vue";

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

// The caller's AI accounts (the same query as Settings). They decide
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
// aiService is the service tile picked on a need's grid, by need step, so
// its key page knows which service it is for.
const aiService = ref<Record<string, string>>({});
// mailService is the email service picked on the email grid, for the add form.
const mailService = ref("");
const foldersReset = ref(false);
// warned is set once the first page has shown the duplicate warning, so the
// install may be sent with confirm: true.
const warned = ref(false);

// The email pages' state (see # Email pages below for the pages).
const mailAccounts = computed(() =>
  [...(plan.value?.mail?.providers ?? [])].sort((a, b) => b.created_at - a.created_at),
);
const onEmailPage = computed(() => [STEP_EMAIL, STEP_EMAIL_SERVICE, STEP_EMAIL_ADD].includes(step.value));
const presetsQuery = useMailPresets(computed(() => !!plan.value?.mail));
const mailPresets = computed(() => presetsQuery.data.value?.presets ?? []);
const mailPreset = computed(() => mailPresets.value.find((p) => p.id === mailService.value));
const emailMode = computed<"list" | "grid" | "add">(() => {
  if (step.value === STEP_EMAIL_ADD) return "add";
  if (step.value === STEP_EMAIL_SERVICE) return "grid";
  return mailAccounts.value.length > 0 ? "list" : "grid";
});
const pageMailService = ref("");
const mailForm = ref<InstanceType<typeof MailAddForm> | null>(null);


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

  aiService.value = { ...(saved?.aiService ?? {}) };
  mailService.value = typeof saved?.mailService === "string" ? saved.mailService : "";

  if (saved?.flow) {
    // A draft from before a change (the manifest dropped its required
    // fields, a service left the provider data) may name pages that are gone.
    // They are dropped, so target() can only send the user to a real page.
    flow.value = saved.flow.filter(stillThere);
    done.value = saved.done ?? [];
    warned.value = !!saved.warned;
    reviewed.value = !!saved.reviewed;
    foldersReset.value = !!saved.foldersReset;
  } else {
    // A first visit: a required AI need a saved key can fill is answered, and
    // that key is picked in advance. Any other required AI need asks for a
    // service (the grid) and then a key, or only a key when one service fits.
    // Then the required plain fields.
    const ask: string[] = [];
    for (const need of aiNeedList.value) {
      if (!need.required || need.slots.some((s) => choices[s.id])) continue;
      const pick = pickInAdvance(need, configFields.value, providers.value, accounts.value);
      if (pick) choices[pick.slotId] = pick.choice;
      else if (needTiles(need, providers.value).length > 1) ask.push(need.step, need.step + SUB_KEY);
      else ask.push(need.step);
    }
    if (needs.value.requiredFields.length > 0) ask.push(STEP_SETTINGS);
    flow.value = ask;
    warned.value = false;
    done.value = [];
    reviewed.value = false;
    foldersReset.value = false;
  }
  aiChoices.value = choices;
}


const draft = computed<Draft>(() => ({
  flow: flow.value,
  done: done.value,
  reviewed: reviewed.value,
  sources: folderSources.value,
  subfolders: folderSubfolders.value,
  mail: mailProviderId.value,
  values: configValues.value,
  ai: aiChoices.value,
  aiService: aiService.value,
  mailService: mailService.value,
  warned: warned.value,
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

// ── AI needs ────────────────────────────────────────────────────────────────
// One set of AI pages per need (installSteps.ts # AI needs): <need> shows the
// key list when the user has a usable key, else the service grid, else (one
// service fits) the key form; <need>-service is the grid; <need>-key the form.
const aiNeedList = computed(() => aiNeeds(needs.value));
const aiPage = computed(() => needOfStep(aiNeedList.value, step.value));

function needMet(need: AINeed): boolean {
  return !need.group || groupMet(need.group, configFields.value, plainValues.value, filled.value);
}

// tileOfId is the service of one of the user's AI accounts, for its logo.
function tileOfId(accountId: string): AIProvider | undefined {
  const a = accounts.value.find((x) => x.id === accountId);
  return a && tileOf(a, providers.value);
}

function usableFor(need: AINeed) {
  return usableAccounts(need, configFields.value, providers.value, accounts.value);
}

// aiMode is what an AI page shows.
function aiMode(need: AINeed, page: "base" | "service" | "key"): "list" | "grid" | "key" {
  if (page === "service") return "grid";
  if (page === "key") return "key";
  if (usableFor(need).length > 0) return "list";
  return needTiles(need, providers.value).length > 1 ? "grid" : "key";
}

// keyService is the service a need's key form is for: the one picked on the
// grid, else the only one that fits.
function keyService(need: AINeed): AIProvider | undefined {
  const tiles = needTiles(need, providers.value);
  if (tiles.length === 1) return tiles[0];
  return tiles.find((t) => t.id === aiService.value[need.step]);
}

// answered says whether a page's need has what it must have. Only required
// pages can be unanswered; the others always are. A required need's first
// page counts as answered once a service is picked on its grid, so the flow
// moves on to the key page.
function answered(name: string): boolean {
  const ai = needOfStep(aiNeedList.value, name);
  if (ai) {
    if (needMet(ai.need)) return true;
    if (ai.page === "service") return true;
    if (ai.page === "base" && aiMode(ai.need, "base") === "grid") return !!keyService(ai.need);
    return false;
  }
  if (name === STEP_SETTINGS) return requiredFieldsMet(needs.value, configFields.value, plainValues.value);
  return true;
}

const requiredSteps = computed(() => [
  ...needs.value.aiGroups.map((g) => g.step),
  ...(needs.value.requiredFields.length > 0 ? [STEP_SETTINGS] : []),
]);

// validSteps is every page this app has. A key page with no service to be
// for is not one.
const validSteps = computed(() => {
  const out = new Set(needs.value.requiredFields.length > 0 ? [STEP_SETTINGS] : []);
  for (const need of aiNeedList.value) {
    out.add(need.step);
    if (needTiles(need, providers.value).length > 1) out.add(need.step + SUB_SERVICE);
    if (keyService(need)) out.add(need.step + SUB_KEY);
  }
  if (plan.value?.mail) {
    out.add(STEP_EMAIL);
    out.add(STEP_EMAIL_SERVICE);
    // While the presets load, a saved service is trusted; once they are
    // loaded, only a preset that exists makes the add form a page.
    if (mailService.value && (presetsQuery.isPending.value || mailPreset.value)) out.add(STEP_EMAIL_ADD);
  }
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
  const valid = (n: string) => validSteps.value.has(n);
  const next = flow.value.filter(valid).find((n) => !done.value.includes(n) || !answered(n));
  if (next) return next;
  return requiredSteps.value.filter(valid).find((n) => !answered(n)) ?? "";
}

// stillThere says whether a first-time page from a saved draft still exists.
// A key page counts while its need still has a grid: it becomes valid again
// once the user picks a service there.
function stillThere(n: string): boolean {
  if (validSteps.value.has(n)) return true;
  const ai = needOfStep(aiNeedList.value, n);
  return !!ai && ai.page === "key" && needTiles(ai.need, providers.value).length > 1;
}

function redirect() {
  const t = target();
  if (t) router.replace(to(t));
  else reviewed.value = true;
}

// A step this app does not have (an old link, a changed manifest) goes to
// the last page, which redirects again if it must.
function checkStep() {
  if (validSteps.value.has(step.value)) return;
  // A key page whose service is not known yet goes back to its need, and an
  // email add form whose service is gone goes back to the email grid.
  const ai = needOfStep(aiNeedList.value, step.value);
  if (ai) router.replace(to(ai.need.step));
  else if (step.value === STEP_EMAIL_ADD) router.replace(to(STEP_EMAIL_SERVICE));
  else router.replace(to(""));
}

// A saved email service that names no preset (a stale draft, or a preset
// the brain dropped) is forgotten once the presets load, and an open add
// form for it goes back to the grid.
watch(
  () => presetsQuery.data.value,
  () => {
    if (!presetsQuery.data.value || !mailService.value || mailPreset.value) return;
    mailService.value = "";
    if (seededFor === key.value && step.value) checkStep();
  },
);

watch([step, scope], () => {
  if (seededFor !== key.value) return;
  if (step.value === "") redirect();
  else checkStep();
});

// next is where Continue goes: back to the last page once it was shown,
// else the next first-time page, else the last page.
function next(from: string): string {
  if (reviewed.value) return "";
  // A page that is not in the flow (the grid opened from the key list) goes
  // on from its need's place in the flow.
  let i = flow.value.indexOf(from);
  const ai = needOfStep(aiNeedList.value, from);
  if (i < 0 && ai) i = Math.max(flow.value.indexOf(ai.need.step + SUB_KEY), flow.value.indexOf(ai.need.step));
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
// The step counter counts the first-time pages and the last page; the App
// page is not a step. A page opened with Change from the last page has no
// number.
const stepTotal = computed(() => flow.value.length + 1);
const stepNumber = computed(() => {
  if (isLast.value) return stepTotal.value;
  const i = flow.value.indexOf(step.value);
  return i < 0 || reviewed.value ? 0 : i + 1;
});

const pageTitle = computed(() => {
  const name = appName.value;
  if (isLast.value) return `Ready to install ${name}`;
  const ai = aiPage.value;
  if (ai) {
    const mode = aiMode(ai.need, ai.page);
    if (mode === "list") return `Which key should ${name} use?`;
    if (mode === "grid") return `Which AI service should ${name} use?`;
    const service = keyService(ai.need);
    if (!service) return name;
    return isOther(service) ? "Your own server" : `Your ${service.name} key`;
  }
  switch (step.value) {
    case STEP_SETTINGS:
      return `${name} needs these to run`;
    case STEP_EMAIL:
    case STEP_EMAIL_SERVICE:
      return emailMode.value === "list"
        ? `Which email account should ${name} send from?`
        : `Which email should ${name} send from?`;
    case STEP_EMAIL_ADD:
      return `Your ${mailPreset.value?.id === "custom" ? "email server" : `${mailPreset.value?.account_name || mailPreset.value?.label} account`}`;
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
// pageValues is the "needs these to run" or Extra settings page's copy of its
// fields. It goes back to configValues only on Continue, so Back drops an
// edit. It lives in memory only, never in the draft.
const pageValues = ref<Record<string, string>>({});
// seedTick changes after each seeding, so the page copies below are taken
// from the seeded draft, not from the empty one before it.
const seedTick = ref(0);
const scopeOptions: Scope[] = ["personal", "household"];
const pageAccount = ref("");
const pageService = ref("");
const keyForm = ref<InstanceType<typeof AIKeyForm> | null>(null);
watch(
  [step, ready, seedTick],
  () => {
    if (step.value === STEP_SETTINGS || step.value === STEP_EXTRA) {
      const fields = step.value === STEP_SETTINGS ? needs.value.requiredFields : needs.value.extraFields;
      pageValues.value = Object.fromEntries(fields.map((f) => [f.app_env, configValues.value[f.app_env] ?? ""]));
    }
    if (step.value === STEP_EMAIL && ready.value) {
      // Set up opens with the newest saved account picked (INSTALL_STEPS.md
      // # Decisions); Change opens with the current one.
      const newest = [...(plan.value?.mail?.providers ?? [])].sort((a, b) => b.created_at - a.created_at)[0];
      pageMail.value = mailProviderId.value || newest?.id || "";
    }
    if (step.value === STEP_EMAIL || step.value === STEP_EMAIL_SERVICE) pageMailService.value = mailService.value;
    if (step.value === STEP_FOLDERS) {
      pageSources.value = { ...folderSources.value };
      pageSubfolders.value = { ...folderSubfolders.value };
    }
    if (step.value === STEP_FOR) pageScope.value = scope.value;
    const ai = aiPage.value;
    if (ai) {
      // The key list opens on the need's current key. With none, a required
      // need opens on the newest usable key, and so does Set up on an
      // optional one (INSTALL_STEPS.md # Decisions).
      const current = ai.need.slots.map((sl) => aiChoices.value[sl.id]).find(Boolean);
      pageAccount.value = current?.accountId ?? usableFor(ai.need)[0]?.id ?? "";
      const tiles = needTiles(ai.need, providers.value);
      pageService.value = aiService.value[ai.need.step] ?? current?.provider ?? "";
      if (!tiles.some((t) => t.id === pageService.value)) pageService.value = "";
    }
    // The "Folders reset" note is said once, on the last page.
    if (step.value !== "") foldersReset.value = false;
  },
  { immediate: true },
);

const canContinue = computed(() => {
  if (onEmailPage.value) {
    if (emailMode.value === "list") return true;
    if (emailMode.value === "grid") return pageMailService.value !== "";
    return !!mailForm.value?.valid && !mailForm.value?.pending;
  }
  const ai = aiPage.value;
  if (ai) {
    const mode = aiMode(ai.need, ai.page);
    if (mode === "list") return pageAccount.value !== "" || !ai.need.required;
    if (mode === "grid") return pageService.value !== "";
    return !!keyForm.value?.valid && !keyForm.value?.pending;
  }
  if (step.value === STEP_SETTINGS) return requiredFieldsMet(needs.value, configFields.value, pagePlain.value);
  return answered(step.value);
});

// pagePlain is the plain values as they would be after this page's Continue.
const pagePlain = computed(() => ({ ...plainValues.value, ...pageValues.value }));

// settingsNeeded names what the "needs these to run" page still misses.
const settingsNeeded = computed(() => [
  ...needs.value.requiredFields
    .filter((f) => f.required && (pagePlain.value[f.app_env] ?? "").trim() === "")
    .map((f) => f.title),
  ...unmetGroups(needs.value.plainGroups, configFields.value, pagePlain.value, new Set()).map((g) =>
    groupNeed(configFields.value, g),
  ),
]);

// continueAI saves an AI page: the picked key, the picked service, or a new
// key from the form. A new key is saved as the user's account right here, so
// there is no Save inside the page.
async function continueAI(need: AINeed, page: "base" | "service" | "key") {
  const name = step.value;
  const mode = aiMode(need, page);
  if (mode === "list") {
    const account = accounts.value.find((a) => a.id === pageAccount.value);
    const pick = account ? choiceFor(need, configFields.value, providers.value, account) : undefined;
    aiChoices.value = withNeedChoice(aiChoices.value, need, pick);
    markDone(name);
    router.push(to(next(name)));
    return;
  }
  if (mode === "grid") {
    aiService.value = { ...aiService.value, [need.step]: pageService.value };
    markDone(name);
    router.push(to(need.step + SUB_KEY));
    return;
  }
  const saved = await keyForm.value?.save();
  if (!saved) return;
  const pick = choiceFor(need, configFields.value, providers.value, saved.account, saved.models);
  if (!pick) {
    pageError.value = { step: name, message: `${appName.value} cannot use this key. Pick another service.` };
    return;
  }
  aiChoices.value = withNeedChoice(aiChoices.value, need, pick);
  markDone(name);
  markDone(need.step);
  router.push(to(next(name)));
}

// useOtherService opens the grid, or the key form when one service fits.
// ── Email pages ─────────────────────────────────────────────────────────────
// email: the account list when the user has accounts, else the grid;
// email-service: the grid; email-add: the add form for the picked service.
// Email is optional in v1, so it stays "not set up" until Continue.
function presetLabel(id: string): string {
  return mailPresets.value.find((p) => p.id === id)?.label ?? "";
}

// installWithoutEmail leaves email not set up and goes back to the last
// page. Email is optional in v1, so a failed preset list never blocks.
function installWithoutEmail() {
  mailProviderId.value = "";
  router.push(to(""));
}

async function continueEmail() {
  if (emailMode.value === "list") {
    mailProviderId.value = pageMail.value;
    markDone(STEP_EMAIL);
    router.push(to(next(STEP_EMAIL)));
    return;
  }
  if (emailMode.value === "grid") {
    mailService.value = pageMailService.value;
    router.push(to(STEP_EMAIL_ADD));
    return;
  }
  const created = await mailForm.value?.save();
  if (!created) return;
  mailProviderId.value = created.id;
  markDone(STEP_EMAIL);
  router.push(to(next(STEP_EMAIL)));
}

function useOtherService() {
  const ai = aiPage.value;
  if (!ai) return;
  const one = needTiles(ai.need, providers.value).length === 1;
  router.push(to(ai.need.step + (one ? SUB_KEY : SUB_SERVICE)));
}

function onContinue() {
  const name = step.value;
  if (!canContinue.value) return;
  if (pageError.value?.step === name) pageError.value = null;
  const ai = aiPage.value;
  if (ai) {
    void continueAI(ai.need, ai.page);
    return;
  }
  if (name === STEP_EMAIL || name === STEP_EMAIL_SERVICE || name === STEP_EMAIL_ADD) {
    void continueEmail();
    return;
  }
  if (name === STEP_SETTINGS || name === STEP_EXTRA) configValues.value = { ...configValues.value, ...pageValues.value };
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
  if (isLast.value) return `/store/${manifestId.value}`;
  const i = flow.value.indexOf(step.value);
  if (!reviewed.value && i > 0) return to(flow.value[i - 1]!);
  // The grid and a key form off the flow go back to their need's first page.
  const ai = aiPage.value;
  if (ai && ai.page !== "base" && i < 0) return to(ai.need.step);
  if (step.value === STEP_EMAIL_SERVICE || step.value === STEP_EMAIL_ADD) return to(STEP_EMAIL);
  return reviewed.value ? to("") : `/store/${manifestId.value}`;
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

// hasRows says whether the last page has any row at all. An app with no
// needs, no folders and no scope choice (Memos) has none.
const hasRows = computed(
  () =>
    canInstallHousehold.value ||
    needs.value.aiGroups.length > 0 ||
    needs.value.requiredFields.length > 0 ||
    folders.value.length > 0 ||
    needs.value.optionalSlots.length > 0 ||
    !!plan.value?.mail ||
    needs.value.extraFields.length > 0,
);

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
    ...unmet.map((g) => groupNeed(configFields.value, g)),
  ];
});

const { submit, confirmDuplicate, dismissDuplicate, submitError, submitLocation, duplicateInfo, pending } =
  useInstallSubmit(manifestId, () => {
    clearDrafts(userId.value, manifestId.value);
    seededFor = "";
  });

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
  // confirm only when this draft showed the duplicate warning. A user who
  // skipped the first page (a link straight to a later step) never saw it,
  // so the brain's 409 asks on the last page instead, as it does for a copy
  // made after the plan loaded.
  if ((p.existing ?? []).length > 0 && warned.value) req.confirm = true;
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

// The App page's direct install (an app that asks nothing) hands its
// failure over in the history state: an error, or a 409 from a copy made
// after the plan was read. The last page shows it, as if it had sent the
// install itself. The state is cleared, so a reload does not show it again.
{
  const st = (history.state ?? {}) as { installError?: string; installErrorLocation?: string; installDuplicate?: string };
  if (st.installDuplicate) duplicateInfo.value = st.installDuplicate;
  else if (st.installError) {
    submitLocation.value = st.installErrorLocation;
    submitError.value = st.installError;
  }
  if (st.installError || st.installDuplicate) {
    history.replaceState({ ...history.state, installError: undefined, installErrorLocation: undefined, installDuplicate: undefined }, "");
  }
}

// A 422 goes to the page that owns the field, with the error there
// (INSTALL_STEPS.md # 2, Errors). One the flow cannot place stays on the
// last page.
const pageError = ref<{ step: string; message: string } | null>(null);
watch(submitError, (message) => {
  if (!message || !plan.value) return;
  const owner = stepForError(submitLocation.value, needs.value, requires.value, !!plan.value.mail);
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
// showWarning is where the warning renders: the first page, or the last page
// when it is the only page.
const showWarning = computed(
  () => existing.value.length > 0 && (onFirstPage.value || (isLast.value && flow.value.length === 0)),
);
watch(
  [showWarning, ready],
  () => {
    if (showWarning.value && ready.value && seededFor === key.value) warned.value = true;
  },
  { immediate: true },
);

// The copy to offer as "Open it": the household one, else the user's own.
const openable = computed(() => {
  const i = householdInstance.value ?? ownPersonalInstance.value;
  return i && i.state !== "installing" && i.url ? i : undefined;
});

// Seeding runs last in setup, after every value it reads is declared: with
// the plan already cached, it runs at once.
watch(
  [ready, key],
  () => {
    if (!ready.value || !plan.value || !userId.value) return;
    if (seededFor === key.value) return;
    seededFor = key.value;
    seed(plan.value);
    seedTick.value++;
    route.query.step ? checkStep() : redirect();
    // The page may already be the one with the warning, with no route change
    // to follow (an app whose only page is the last page).
    if (showWarning.value) warned.value = true;
  },
  { immediate: true },
);

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
      <!-- Header: the app, with its info box right under the name on the
           first and last pages. The page's question comes after it, as
           the heading of the choice below it. -->
      <header class="space-y-3 px-4 sm:px-0">
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
        <InstallInfoBox
          v-if="isLast || onFirstPage"
          :app-name="plan.name"
          :permissions="plan.permissions"
          :footprint="plan.footprint"
        />
      </header>

      <!-- Warnings on the first page, before any question. -->
      <div
        v-if="showWarning"
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

      <Heading
        id="install-question"
        ref="heading"
        :level="2"
        tabindex="-1"
        class="px-4 pt-2 outline-none sm:px-0"
      >
        {{ pageTitle }}
      </Heading>

      <p
        v-if="pageError && pageError.step === step"
        class="mx-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive sm:mx-0"
        role="alert"
      >
        {{ pageError.message }}
      </p>

      <!-- ── Question pages ─────────────────────────────────────────────── -->
      <template v-if="!isLast">
        <div class="-mt-3 px-4 sm:px-0" role="group" aria-labelledby="install-question">
          <template v-if="aiPage">
            <AccountList
              v-if="aiMode(aiPage.need, aiPage.page) === 'list'"
              v-model="pageAccount"
              :rows="usableFor(aiPage.need).map((a) => ({ id: a.id, label: a.label, detail: tileOf(a, providers)?.name }))"
              :label="`Key for ${plan.name}`"
              :none-label="aiPage.need.required ? undefined : 'Don\'t use an AI service'"
              :other-label="needTiles(aiPage.need, providers).length > 1 ? 'Use a different AI service' : 'Add another key'"
              @other="useOtherService"
            >
              <template #logo="{ row }">
                <AIProviderLogo :provider="tileOfId(row.id)" />
              </template>
            </AccountList>
            <ServiceGrid
              v-else-if="aiMode(aiPage.need, aiPage.page) === 'grid'"
              v-model="pageService"
              :services="needTiles(aiPage.need, providers)"
              :label="`AI service for ${plan.name}`"
            />
            <AIKeyForm
              v-else-if="keyService(aiPage.need)"
              ref="keyForm"
              :service="keyService(aiPage.need)!"
              :ai-slot="slotFor(keyService(aiPage.need)!, aiPage.need.slots)!"
              :app-name="plan.name"
              :household="scope === 'household'"
              :labels="accounts.map((a) => a.label)"
              :only="needTiles(aiPage.need, providers).length === 1"
            />
          </template>
          <div v-else-if="step === 'settings'" class="space-y-6">
            <ConfigFieldInput
              v-for="f in needs.requiredFields"
              :key="f.app_env"
              v-model="pageValues[f.app_env]!"
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
              v-model="pageValues[f.app_env]!"
              :field="f"
            />
          </div>
          <template v-else-if="onEmailPage && plan.mail">
            <AccountList
              v-if="emailMode === 'list'"
              v-model="pageMail"
              :rows="mailAccounts.map((m) => ({ id: m.id, label: m.label, detail: presetLabel(m.provider_type) }))"
              :label="`Email account for ${plan.name}`"
              none-label="Don't send email"
              other-label="Use a different email service"
              @other="router.push(to(STEP_EMAIL_SERVICE))"
            >
              <template #logo="{ row }">
                <MailProviderLogo
                  :id="mailAccounts.find((m) => m.id === row.id)?.provider_type ?? 'custom'"
                  :label="row.label"
                  size="icon"
                />
              </template>
            </AccountList>
            <div
              v-else-if="presetsQuery.isError.value"
              class="space-y-3 rounded-md bg-destructive/10 px-3 py-3 text-sm text-destructive"
              role="alert"
            >
              <p>Could not load the list of email services.</p>
              <div class="flex flex-wrap gap-2">
                <Button size="sm" variant="secondary" @click="presetsQuery.refetch()">Try again</Button>
                <Button size="sm" variant="ghost" @click="installWithoutEmail">Don't send email</Button>
              </div>
            </div>
            <p v-else-if="presetsQuery.isPending.value" class="text-sm text-muted-foreground">Loading…</p>
            <MailServiceGrid
              v-else-if="emailMode === 'grid'"
              v-model="pageMailService"
              :presets="mailPresets"
              :label="`Email service for ${plan.name}`"
            />
            <MailAddForm
              v-else-if="mailPreset"
              ref="mailForm"
              :preset="mailPreset"
              :labels="mailAccounts.map((m) => m.label)"
              :manifest-id="manifestId"
            />
          </template>
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
          <p v-if="!canContinue && onEmailPage" class="text-sm text-muted-foreground sm:mr-auto">
            {{ emailMode === "grid" ? "Pick an email service to go on." : "Fill in the form to go on." }}
          </p>
          <p v-else-if="!canContinue && aiPage" class="text-sm text-muted-foreground sm:mr-auto">
            {{
              aiMode(aiPage.need, aiPage.page) === "grid"
                ? "Pick an AI service to go on."
                : aiMode(aiPage.need, aiPage.page) === "list"
                  ? "Pick a key to go on."
                  : "Fill in the form to go on."
            }}
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
        <!-- Only when there is a row: an empty list would draw as two
             lines with nothing between them. -->
        <div v-if="hasRows" class="border-t border-border">
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
            <Button size="sm" :disabled="pending" @click="confirmDuplicate(buildRequest(plan))">Install my own copy</Button>
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
