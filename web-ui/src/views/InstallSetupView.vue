<script setup lang="ts">
// The install flow (/store/:id/install), one need per step
// (docs/specs/INSTALL_STEPS.md). The App page's Install button lands here for
// an app that has at least one step, or something to warn about.
//
//   App page ──▶ step 1 ──▶ step 2 ──▶ … ──▶ last step ──Install──▶ progress
//
// A step is ?step=<name>. The scope (?scope=household) rides on every page and
// comes from the App page's button: there is no scope step. The steps are, in
// order (installSteps.ts stepList): each AI need, email, the required plain
// fields, the folders. A step with saved accounts lists them with the newest
// picked, so the user sees and confirms the account there; a user with a
// saved key still sees the step. Optional plain fields have no step: they
// keep their defaults and are changed later on the app's settings screen.
//
// An AI need and email have sub-pages under their step, which share its
// number: <need>-service and email-service (the service grid) and <need>-key
// and email-add (the form for a new account). Their bottom bar has only Back
// and Continue: Continue on the form saves the account and goes back to the
// step's list with it picked, and the step's own button then moves on (on
// the last step, Install). Every page's bottom bar is Back and Continue or
// Install; the one Cancel, which ends the install, is in the header.
//
// The bare path goes to the first step. For an app with no steps (the App
// page sends it here only when there is a warning), the bare path is a page
// with only the warning and an Install button. Opening a later step while an
// earlier required step has no answer goes to that step. The draft lives in
// the tab's session storage (non-secret answers only), so a reload or a trip
// to a provider's site in another tab loses nothing but a typed secret.
//
// Driven by GET /api/v1/catalog/:id/install-plan (advisory; the brain checks
// everything again on POST /api/v1/apps). The UI owns all wording.
import { computed, nextTick, onBeforeUnmount, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { useQuery } from "@tanstack/vue-query";
import { ArrowLeft, TriangleAlert } from "lucide-vue-next";
import {
  api,
  type AIAccount,
  type AIProvider,
  type CatalogDetail,
  type FolderElection,
  type InstallPlan,
  type InstallRequest,
  type Scope,
} from "../api";
import { useAuth } from "../auth";
import { useAppInstances, useInstallSubmit } from "../useInstall";
import { useMailPresets } from "../mailProviderForm";
import { formatSize } from "../utils";
import {
  aiSlots,
  bindingOf,
  groupNeed,
  isOther,
  modelIdProblem,
  slotFor,
  unmetGroups,
  type AIChoice,
  type AISlot,
} from "../aiProviders";
import {
  STEP_EMAIL,
  STEP_EMAIL_ADD,
  STEP_EMAIL_SERVICE,
  STEP_FOLDERS,
  STEP_SETTINGS,
  SUB_KEY,
  SUB_SERVICE,
  aiNeeds,
  choiceFor,
  clearDrafts,
  draftKey,
  filledBy,
  folderName,
  groupMet,
  loadDraft,
  needOfStep,
  needTiles,
  planNeeds,
  requiredFieldsMet,
  serverSlot,
  saveDraft,
  spaceTight,
  stepForError,
  stepList,
  tileOf,
  usableAccounts,
  withNeedChoice,
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
import OptionalOffer from "../components/install/OptionalOffer.vue";

const route = useRoute();
const router = useRouter();
const { currentUser } = useAuth();

const manifestId = computed(() => String(route.params.id));
const scope = computed<Scope>(() => (route.query.scope === "household" ? "household" : "personal"));
const step = computed(() => (typeof route.query.step === "string" ? route.query.step : ""));
const userId = computed(() => currentUser.value?.id ?? "");

const { householdInstance, ownPersonalInstance } = useAppInstances(manifestId);

// ── Data ────────────────────────────────────────────────────────────────────
const planQuery = useQuery({
  queryKey: computed(() => ["install-plan", manifestId.value]),
  queryFn: () => api.get<InstallPlan>(`/catalog/${encodeURIComponent(manifestId.value)}/install-plan`),
  staleTime: 0,
  // A refetch on focus would swap the plan under a half-filled form. The page
  // refetches on purpose only after an email account is added.
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

// The caller's AI accounts (the same query as Settings).
const aiAccountsQuery = useQuery({
  queryKey: ["ai-accounts"],
  queryFn: () => api.get<{ accounts: AIAccount[] | null }>("/ai-accounts"),
  enabled: computed(() => needs.value.slots.length > 0),
  refetchOnWindowFocus: false,
});
const accounts = computed(() => aiAccountsQuery.data.value?.accounts ?? []);

// Wait for everything the flow depends on before choosing a page, so the
// fields do not jump between steps and the redirect does not guess.
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
const folderSources = ref<Record<string, string>>({});
const folderSubfolders = ref<Record<string, string>>({});
const mailProviderId = ref(""); // "" is no email
const configValues = ref<Record<string, string>>({});
const aiChoices = ref<Record<string, AIChoice>>({});
// aiService is the service tile picked on a need's grid, by need step, so
// its key form knows which service it is for.
const aiService = ref<Record<string, string>>({});
// mailService is the email service picked on the email grid, for the add form.
const mailService = ref("");
// serverModels are the model names typed for each My own server account
// (it has no model list), by account id and model setting.
const serverModels = ref<Record<string, Record<string, string[]>>>({});
// declined are the optional steps answered with "Don't use", so a return to
// the step shows that choice, not the newest account.
const declined = ref<string[]>([]);
// warned is set once the first step has shown the duplicate warning, so the
// install may be sent with confirm: true.
const warned = ref(false);

// The email step's data.
const mailAccounts = computed(() =>
  [...(plan.value?.mail?.providers ?? [])].sort((a, b) => b.created_at - a.created_at),
);
const onEmailPage = computed(() => [STEP_EMAIL, STEP_EMAIL_SERVICE, STEP_EMAIL_ADD].includes(step.value));
const presetsQuery = useMailPresets(computed(() => !!plan.value?.mail));
const mailPresets = computed(() => presetsQuery.data.value?.presets ?? []);
const mailPreset = computed(() => mailPresets.value.find((p) => p.id === mailService.value));
// emailMode is what the email step shows: the saved accounts, or, with none,
// a small offer ("Not now" or "Set up email") that opens the grid on Set up.
// A required email need would skip the offer and start at the grid.
const emailMode = computed<"list" | "offer" | "grid" | "add">(() => {
  if (step.value === STEP_EMAIL_ADD) return "add";
  if (step.value === STEP_EMAIL_SERVICE) return "grid";
  if (mailAccounts.value.length > 0) return "list";
  return plan.value?.mail?.optional === false ? "grid" : "offer";
});
const mailForm = ref<InstanceType<typeof MailAddForm> | null>(null);

const secretEnvs = computed(() => new Set(configFields.value.filter((f) => f.secret).map((f) => f.app_env)));
const key = computed(() => draftKey(userId.value, manifestId.value, scope.value));

// Seed the draft once per app and scope, from the tab's saved draft if there
// is one, else from the plan's defaults. Saved accounts are picked on each
// step's own page, where the user sees them, not here. A later refetch (after
// an email account is added) must not wipe the draft.
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

  const values: Record<string, string> = {};
  for (const f of p.config ?? []) values[f.app_env] = saved?.values?.[f.app_env] ?? f.default ?? "";
  configValues.value = values;

  const slotIds = new Set(needs.value.slots.map((s) => s.id));
  const choices: Record<string, AIChoice> = {};
  for (const [slot, c] of Object.entries(saved?.ai ?? {})) if (slotIds.has(slot) && c?.accountId) choices[slot] = c;
  aiChoices.value = choices;

  aiService.value = { ...(saved?.aiService ?? {}) };
  mailService.value = typeof saved?.mailService === "string" ? saved.mailService : "";
  serverModels.value = { ...(saved?.serverModels ?? {}) };
  declined.value = Array.isArray(saved?.declined) ? saved.declined : [];
  warned.value = !!saved?.warned;
}

const draft = computed<Draft>(() => ({
  sources: folderSources.value,
  subfolders: folderSubfolders.value,
  mail: mailProviderId.value,
  values: configValues.value,
  ai: aiChoices.value,
  aiService: aiService.value,
  mailService: mailService.value,
  serverModels: serverModels.value,
  declined: declined.value,
  warned: warned.value,
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

// ── Steps ───────────────────────────────────────────────────────────────────
const steps = computed(() => (plan.value ? stepList(needs.value, plan.value) : []));

// ── AI needs ────────────────────────────────────────────────────────────────
// One step per need (installSteps.ts # AI needs): <need> shows the key list
// when the user has a usable key, else the service grid, else (one service
// fits) the key form; <need>-service is the grid; <need>-key the form.
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

// aiMode is what an AI page shows: the usable keys, or, with none, for an
// optional need a small offer ("Not now" or "Set up an AI service"), and for
// a required one the service grid (or the key form when one service fits).
function aiMode(need: AINeed, page: "base" | "service" | "key"): "list" | "offer" | "grid" | "key" {
  if (page === "service") return "grid";
  if (page === "key") return "key";
  if (usableFor(need).length > 0) return "list";
  if (!need.required) return "offer";
  return needTiles(need, providers.value).length > 1 ? "grid" : "key";
}

// keyService is the service a need's key form is for: the one picked on the
// grid, else the only one that fits.
function keyService(need: AINeed): AIProvider | undefined {
  const tiles = needTiles(need, providers.value);
  if (tiles.length === 1) return tiles[0];
  return tiles.find((t) => t.id === aiService.value[need.step]);
}

// answered says whether a step has what it must have. Only required steps can
// be unanswered.
function answered(name: string): boolean {
  const ai = needOfStep(aiNeedList.value, name);
  if (ai) return needMet(ai.need);
  if (name === STEP_SETTINGS) return requiredFieldsMet(needs.value, configFields.value, plainValues.value);
  return true;
}

// baseOf is the step a page belongs to: a sub-page's need or email step.
function baseOf(name: string): string {
  const ai = needOfStep(aiNeedList.value, name);
  if (ai) return ai.need.step;
  if (name === STEP_EMAIL_SERVICE || name === STEP_EMAIL_ADD) return STEP_EMAIL;
  return name;
}

// validSteps is every page this app has. A key form with no service to be for
// is not one, and neither is an email add form whose service is gone.
const validSteps = computed(() => {
  const out = new Set(steps.value);
  for (const need of aiNeedList.value) {
    if (needTiles(need, providers.value).length > 1) out.add(need.step + SUB_SERVICE);
    if (keyService(need)) out.add(need.step + SUB_KEY);
  }
  if (plan.value?.mail) {
    out.add(STEP_EMAIL_SERVICE);
    // While the presets load, a saved service is trusted; once they are
    // loaded, only a preset that exists makes the add form a page.
    if (mailService.value && (presetsQuery.isPending.value || mailPreset.value)) out.add(STEP_EMAIL_ADD);
  }
  return out;
});

// ── Navigation ──────────────────────────────────────────────────────────────
function to(name: string) {
  const query: Record<string, string> = {};
  if (scope.value === "household") query.scope = "household";
  if (name) query.step = name;
  return { path: `/store/${encodeURIComponent(manifestId.value)}/install`, query };
}

// checkStep keeps the URL on a page that exists and may be opened now. The
// bare path goes to the first step (an app with no steps stays there). A page
// the app does not have goes to its step, or to the first step. A step after
// a required step with no answer goes to that step.
function checkStep() {
  const name = step.value;
  if (name === "") {
    if (steps.value.length > 0) router.replace(to(steps.value[0]!));
    return;
  }
  if (!validSteps.value.has(name)) {
    const base = baseOf(name);
    if (name === STEP_EMAIL_ADD && plan.value?.mail) router.replace(to(STEP_EMAIL_SERVICE));
    else router.replace(to(steps.value.includes(base) ? base : ""));
    return;
  }
  const i = steps.value.indexOf(baseOf(name));
  const missing = steps.value.slice(0, i).find((n) => !answered(n));
  if (missing) router.replace(to(missing));
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
  if (seededFor === key.value) checkStep();
});

// ── Pages ───────────────────────────────────────────────────────────────────
const appName = computed(() => plan.value?.name ?? "");
const noSteps = computed(() => steps.value.length === 0);
const stepIndex = computed(() => steps.value.indexOf(baseOf(step.value)));
const onFirstStep = computed(() => (noSteps.value ? step.value === "" : stepIndex.value === 0 && step.value === steps.value[0]));
const onLastStep = computed(() => noSteps.value || stepIndex.value === steps.value.length - 1);
// The step counter counts the steps only; a sub-page has its step's number.
const stepNumber = computed(() => (stepIndex.value < 0 ? 0 : stepIndex.value + 1));

const pageTitle = computed(() => {
  const name = appName.value;
  if (noSteps.value) return `Install ${name}`;
  const ai = aiPage.value;
  if (ai) {
    const mode = aiMode(ai.need, ai.page);
    if (mode === "list") return `Which key should ${name} use?`;
    if (mode === "offer") return `${name} can use an AI service`;
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
      if (emailMode.value === "offer") return `${name} can send email`;
      return emailMode.value === "list"
        ? `Which email account should ${name} send from?`
        : `Which email should ${name} send from?`;
    case STEP_EMAIL_ADD:
      return `Your ${mailPreset.value?.id === "custom" ? "email server" : `${mailPreset.value?.account_name || mailPreset.value?.label} account`}`;
    case STEP_FOLDERS:
      return folders.value.length === 1
        ? `${name} will use your ${folderName(folders.value[0]!.folder)}`
        : `${name} will use these folders`;
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

// Page-local copies. A step saves its answer on Continue, not as the user
// clicks, so leaving it with Back changes nothing.
const pageMail = ref("");
const pageMailService = ref("");
const pageSources = ref<Record<string, string>>({});
const pageSubfolders = ref<Record<string, string>>({});
// pageValues is the "needs these to run" step's copy of its fields. It goes
// back to configValues only on Continue, so Back drops an edit. It lives in
// memory only, never in the draft.
const pageValues = ref<Record<string, string>>({});
// seedTick changes after each seeding, so the page copies below are taken
// from the seeded draft, not from the empty one before it.
const seedTick = ref(0);
const pageAccount = ref("");
// pageModels are the model name boxes shown under a picked My own server
// account whose model names are not known yet, by model setting.
const pageModels = ref<Record<string, string>>({});
// pageOffer is the choice on an optional step's offer: "later" (Not now,
// picked in advance) or "setup".
const pageOffer = ref<"later" | "setup">("later");
const pageService = ref("");
const keyForm = ref<InstanceType<typeof AIKeyForm> | null>(null);
watch(
  [step, ready, seedTick],
  () => {
    if (step.value === STEP_SETTINGS) {
      pageValues.value = Object.fromEntries(
        needs.value.requiredFields.map((f) => [f.app_env, configValues.value[f.app_env] ?? ""]),
      );
    }
    pageOffer.value = "later";
    if (step.value === STEP_EMAIL && ready.value) {
      // The account list opens on the current account, else the newest.
      pageMail.value = declined.value.includes(STEP_EMAIL)
        ? ""
        : mailProviderId.value || mailAccounts.value[0]?.id || "";
    }
    if (step.value === STEP_EMAIL || step.value === STEP_EMAIL_SERVICE) pageMailService.value = mailService.value;
    if (step.value === STEP_FOLDERS) {
      pageSources.value = { ...folderSources.value };
      pageSubfolders.value = { ...folderSubfolders.value };
    }
    pageModels.value = {};
    const ai = aiPage.value;
    if (ai) {
      // The key list opens on the need's current key, else the newest usable
      // one (INSTALL_STEPS.md # Build rules, rule 3).
      const current = ai.need.slots.map((sl) => aiChoices.value[sl.id]).find(Boolean);
      pageAccount.value =
        current?.accountId ?? (declined.value.includes(ai.need.step) ? "" : (usableFor(ai.need)[0]?.id ?? ""));
      const tiles = needTiles(ai.need, providers.value);
      pageService.value = aiService.value[ai.need.step] ?? current?.provider ?? "";
      if (!tiles.some((t) => t.id === pageService.value)) pageService.value = "";
    }
  },
  { immediate: true },
);

// pagePlain is the plain values as they would be after this step's Continue.
const pagePlain = computed(() => ({ ...plainValues.value, ...pageValues.value }));

// settingsNeeded names what the "needs these to run" step still misses.
const settingsNeeded = computed(() => [
  ...needs.value.requiredFields
    .filter((f) => f.required && (pagePlain.value[f.app_env] ?? "").trim() === "")
    .map((f) => f.title),
  ...unmetGroups(needs.value.plainGroups, configFields.value, pagePlain.value, new Set()).map((g) =>
    groupNeed(configFields.value, g),
  ),
]);

// ── Continue ────────────────────────────────────────────────────────────────
const { submit, confirmDuplicate, dismissDuplicate, submitError, submitLocation, duplicateInfo, pending } =
  useInstallSubmit(manifestId, () => {
    clearDrafts(userId.value, manifestId.value);
    seededFor = "";
  });

const canContinue = computed(() => {
  if (pending.value) return false;
  if (onEmailPage.value) {
    if (emailMode.value === "list" || emailMode.value === "offer") return true;
    if (emailMode.value === "grid") return pageMailService.value !== "";
    return !!mailForm.value?.valid && !mailForm.value?.pending;
  }
  const ai = aiPage.value;
  if (ai) {
    const mode = aiMode(ai.need, ai.page);
    if (mode === "list") {
      if (pageAccount.value === "") return !ai.need.required;
      return !serverNames.value || serverNames.value.every((m) => modelNameOk(m, pageModels.value[m.key] ?? ""));
    }
    if (mode === "offer") return true;
    if (mode === "grid") return pageService.value !== "";
    return !!keyForm.value?.valid && !keyForm.value?.pending;
  }
  if (step.value === STEP_SETTINGS) return requiredFieldsMet(needs.value, configFields.value, pagePlain.value);
  return true;
});

// opensSubPage says whether Continue on this page opens a sub-page of the
// same step (a grid opens its form) instead of finishing the step.
const opensSubPage = computed(() => {
  if (onEmailPage.value) return emailMode.value === "grid" || (emailMode.value === "offer" && pageOffer.value === "setup");
  const ai = aiPage.value;
  if (!ai) return false;
  const mode = aiMode(ai.need, ai.page);
  return mode === "grid" || (mode === "offer" && pageOffer.value === "setup");
});
// onAddPage says whether this page is part of adding a new account: a
// service grid or a key or account form. Its buttons act on that sub-flow
// only (Back and Continue), never on the whole install.
const onAddPage = computed(() => {
  if (onEmailPage.value) return emailMode.value === "grid" || emailMode.value === "add";
  const ai = aiPage.value;
  if (!ai) return false;
  const mode = aiMode(ai.need, ai.page);
  return mode === "grid" || mode === "key";
});
// The primary button installs on the last step's own page. A grid or a form
// always says Continue: after a save the user comes back to the step's list.
const installsHere = computed(() => onLastStep.value && !onAddPage.value && !opensSubPage.value);

// finishStep moves on from a step whose answer is saved: to the next step,
// or, from the last one, to the install.
function finishStep(base: string) {
  const i = steps.value.indexOf(base);
  if (i >= 0 && i < steps.value.length - 1) {
    router.push(to(steps.value[i + 1]!));
    return;
  }
  install();
}

// continueAI saves an AI page: the picked key, the picked service, or a new
// key from the form. A new key is saved as the user's account right here, so
// there is no Save inside the page.
// ── My own server ───────────────────────────────────────────────────────────
// A My own server account has no model list, so its choice needs the model
// names the user typed for it. They are kept per account (serverModels).

// storedModels are the known model names of an account for a slot, when
// every model setting of the slot has one.
function storedModels(accountId: string, slot: AISlot): Record<string, string[]> | undefined {
  // The names typed for the account, else the ones its current choice holds
  // (a draft from before names were kept per account).
  const known =
    serverModels.value[accountId] ?? Object.values(aiChoices.value).find((c) => c.accountId === accountId)?.models;
  if (!known || !slot.models.every((m) => (known[m.key] ?? []).length > 0)) return undefined;
  return Object.fromEntries(slot.models.map((m) => [m.key, known[m.key]!]));
}

// modelsOf are the models to build a choice with: the known names for a My
// own server account, else the provider's defaults (undefined).
function modelsOf(need: AINeed, account: AIAccount): Record<string, string[]> | undefined {
  const slot = serverSlot(need, configFields.value, account);
  return slot ? storedModels(account.id, slot) : undefined;
}

// modelNameOk says whether a typed model name can be used for a setting.
function modelNameOk(m: AISlot["models"][number], v: string): boolean {
  const id = v.trim();
  return id !== "" && !modelIdProblem(id, m.multiple ? m.separator : undefined);
}

// serverNames are the model settings the step still has to ask for: the
// picked account is a My own server whose names for this slot are unknown.
const serverNames = computed(() => {
  const ai = aiPage.value;
  if (!ai) return null;
  const account = accounts.value.find((a) => a.id === pageAccount.value);
  const slot = account && serverSlot(ai.need, configFields.value, account);
  if (!slot || slot.models.length === 0 || storedModels(account.id, slot)) return null;
  return slot.models;
});

async function continueAI(need: AINeed, page: "base" | "service" | "key") {
  const mode = aiMode(need, page);
  if (mode === "list") {
    const account = accounts.value.find((a) => a.id === pageAccount.value);
    if (account && serverNames.value) {
      const typed = Object.fromEntries(serverNames.value.map((m) => [m.key, [(pageModels.value[m.key] ?? "").trim()]]));
      serverModels.value = { ...serverModels.value, [account.id]: { ...(serverModels.value[account.id] ?? {}), ...typed } };
    }
    const pick = account ? choiceFor(need, configFields.value, providers.value, account, modelsOf(need, account)) : undefined;
    // An account that cannot be built into a choice now never clears the
    // choice it already is.
    const current = need.slots.map((sl) => aiChoices.value[sl.id]).find(Boolean);
    if (account && !pick && current?.accountId === account.id) {
      finishStep(need.step);
      return;
    }
    aiChoices.value = withNeedChoice(aiChoices.value, need, pick);
    declined.value = pick ? declined.value.filter((n) => n !== need.step) : [...new Set([...declined.value, need.step])];
    finishStep(need.step);
    return;
  }
  if (mode === "offer") {
    if (pageOffer.value === "setup") useOtherService();
    else {
      aiChoices.value = withNeedChoice(aiChoices.value, need, undefined);
      declined.value = [...new Set([...declined.value, need.step])];
      finishStep(need.step);
    }
    return;
  }
  if (mode === "grid") {
    aiService.value = { ...aiService.value, [need.step]: pageService.value };
    router.push(to(need.step + SUB_KEY));
    return;
  }
  const saved = await keyForm.value?.save();
  if (!saved) return;
  const pick = choiceFor(need, configFields.value, providers.value, saved.account, saved.models);
  if (!pick) {
    pageError.value = { step: step.value, message: `${appName.value} cannot use this key. Pick another service.` };
    return;
  }
  // The new key is picked, and the user goes back to the step's list to see
  // it there; the step's own button then moves on.
  if (saved.models) serverModels.value = { ...serverModels.value, [saved.account.id]: saved.models };
  aiChoices.value = withNeedChoice(aiChoices.value, need, pick);
  declined.value = declined.value.filter((n) => n !== need.step);
  pageAccount.value = saved.account.id;
  returnToStep(need.step);
}

// useOtherService opens the grid, or the key form when one service fits.
function useOtherService() {
  const ai = aiPage.value;
  if (!ai) return;
  const one = needTiles(ai.need, providers.value).length === 1;
  router.push(to(ai.need.step + (one ? SUB_KEY : SUB_SERVICE)));
}

function presetLabel(id: string): string {
  return mailPresets.value.find((p) => p.id === id)?.label ?? "";
}

async function continueEmail() {
  if (emailMode.value === "list") {
    mailProviderId.value = pageMail.value;
    declined.value = pageMail.value
      ? declined.value.filter((n) => n !== STEP_EMAIL)
      : [...new Set([...declined.value, STEP_EMAIL])];
    finishStep(STEP_EMAIL);
    return;
  }
  if (emailMode.value === "offer") {
    if (pageOffer.value === "setup") router.push(to(STEP_EMAIL_SERVICE));
    else {
      mailProviderId.value = "";
      declined.value = [...new Set([...declined.value, STEP_EMAIL])];
      finishStep(STEP_EMAIL);
    }
    return;
  }
  if (emailMode.value === "grid") {
    mailService.value = pageMailService.value;
    router.push(to(STEP_EMAIL_ADD));
    return;
  }
  const created = await mailForm.value?.save();
  if (!created) return;
  // The new account is picked on the step's list, where the user sees it.
  mailProviderId.value = created.id;
  declined.value = declined.value.filter((n) => n !== STEP_EMAIL);
  pageMail.value = created.id;
  returnToStep(STEP_EMAIL);
}

function onContinue() {
  const name = step.value;
  if (!canContinue.value) return;
  if (pageError.value?.step === name) pageError.value = null;
  submitError.value = null;
  const ai = aiPage.value;
  if (ai) {
    void continueAI(ai.need, ai.page);
    return;
  }
  if (onEmailPage.value) {
    void continueEmail();
    return;
  }
  if (name === STEP_SETTINGS) configValues.value = { ...configValues.value, ...pageValues.value };
  if (name === STEP_FOLDERS) {
    folderSources.value = { ...pageSources.value };
    folderSubfolders.value = { ...pageSubfolders.value };
  }
  finishStep(name);
}

// ── Back ────────────────────────────────────────────────────────────────────
// Back goes one page back, the same as the browser's Back: form → grid → the
// page the user came from (the step's list or offer), and a step's first page
// → the previous step, or the App page from the first step. The history
// position Vue Router keeps in history.state tells whether the page before
// is one of the flow's own; a page opened by a link has none before it, and
// then backTarget says where Back goes.
function historyPos(): number | undefined {
  const p = (history.state as { position?: unknown } | null)?.position;
  return typeof p === "number" ? p : undefined;
}
const entryPos = historyPos() ?? 0;
// stepPos is the history position of each step's own page, so a save on a
// sub-page can go back to it as the browser's Back would.
const stepPos = new Map<string, number>();
watch(
  () => route.fullPath,
  () => {
    const pos = historyPos();
    if (pos !== undefined && steps.value.includes(step.value)) stepPos.set(step.value, pos);
  },
  { immediate: true },
);

function backTarget() {
  const name = step.value;
  const ai = aiPage.value;
  if (ai && ai.page === "key") {
    const grid = needTiles(ai.need, providers.value).length > 1 && aiMode(ai.need, "base") !== "grid";
    return to(grid ? ai.need.step + SUB_SERVICE : ai.need.step);
  }
  if (ai && ai.page === "service") return to(ai.need.step);
  if (name === STEP_EMAIL_ADD) return to(plan.value?.mail?.optional === false && mailAccounts.value.length === 0 ? STEP_EMAIL : STEP_EMAIL_SERVICE);
  if (name === STEP_EMAIL_SERVICE) return to(STEP_EMAIL);
  const i = steps.value.indexOf(name);
  return i > 0 ? to(steps.value[i - 1]!) : `/store/${manifestId.value}`;
}

function goBack() {
  const pos = historyPos();
  if (pos !== undefined && pos > entryPos) {
    router.back();
    return;
  }
  // Leaving the flow from the page it was entered at: the App page is the
  // previous entry when the user came from it, so go back; otherwise
  // replace, so the browser's Back does not return here.
  const target = backTarget();
  const path = typeof target === "string" ? target : router.resolve(target).fullPath;
  if ((history.state as { back?: unknown } | null)?.back === path) router.back();
  else router.replace(target);
}

// returnToStep goes back from a sub-page to its step's own page after a save,
// through the browser history when the step's page is in it, so Back from
// there does not return to the form.
function returnToStep(base: string) {
  if (step.value === base) return; // the form was the step's first view
  const pos = historyPos();
  const at = stepPos.get(base);
  if (pos !== undefined && at !== undefined && at < pos && at >= entryPos) router.go(at - pos);
  else router.replace(to(base));
}

function cancel() {
  clearDrafts(userId.value, manifestId.value);
  seededFor = "";
  router.push(`/store/${manifestId.value}`);
}

const cancelClass = "shrink-0 cursor-pointer text-sm text-muted-foreground transition-colors hover:text-foreground";

// ── Install ─────────────────────────────────────────────────────────────────
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
  // skipped the first step (a link straight to a later step) never saw it,
  // so the brain's 409 asks on the last step instead, as it does for a copy
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

// install sends the install from the last step. A required step with no
// answer (an answer removed on the way back) is opened instead.
function install() {
  if (!plan.value || pending.value) return;
  const missing = steps.value.find((n) => !answered(n));
  if (missing) {
    router.push(to(missing));
    return;
  }
  pageError.value = null;
  submit(buildRequest(plan.value));
}

// lastPage is the page where a 409 or an error no step owns shows: the last
// step, or the bare path for an app with no steps.
const lastPage = computed(() => steps.value[steps.value.length - 1] ?? "");
const onLastPage = computed(() => step.value === lastPage.value);

// The App page's direct install (an app that asks nothing) hands its
// failure over in the history state: an error, or a 409 from a copy made
// after the plan was read. The page shows it, as if it had sent the install
// itself. The state is cleared, so a reload does not show it again.
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

// A 422 goes to the step that owns the field, with the error there
// (INSTALL_STEPS.md # 1, Errors). One no step owns, and a 409, show on the
// last step above its Install button.
const pageError = ref<{ step: string; message: string } | null>(null);
watch(submitError, (message) => {
  if (!message || !plan.value) return;
  const owner = stepForError(submitLocation.value, needs.value, requires.value, !!plan.value.mail);
  const target = owner && validSteps.value.has(owner) ? owner : lastPage.value;
  pageError.value = { step: target, message };
  submitError.value = null;
  if (step.value !== target) router.push(to(target));
});
watch(duplicateInfo, (dup) => {
  if (dup && !onLastPage.value) router.push(to(lastPage.value));
});

// ── Warnings on the first step ──────────────────────────────────────────────
// The duplicate and not-enough-space warnings show at the top of the first
// step, before any question (INSTALL_STEPS.md # Decisions). For an app with
// no steps, the bare path shows only the warning.
const existing = computed(() => plan.value?.existing ?? []);
const tight = computed(() => spaceTight(plan.value?.footprint));
const duplicateLines = computed(() =>
  existing.value.map((c) => {
    if (c.scope === "household") return `${c.name} is already installed for everyone at home.`;
    if (c.mine) return `You already have your own copy of ${c.name}.`;
    return `Someone else on this box has their own copy of ${c.name}.`;
  }),
);
const showWarning = computed(() => existing.value.length > 0 && onFirstStep.value);
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
    checkStep();
    // The page may already be the one with the warning, with no route change
    // to follow (an app with no steps).
    if (showWarning.value) warned.value = true;
  },
  { immediate: true },
);
</script>

<template>
  <div class="mx-auto w-full max-w-3xl space-y-6 pt-2 pb-10">
    <!-- Before the plan loads there is no header yet, so Cancel is here. -->
    <div v-if="!plan" class="flex justify-end px-4 sm:px-0">
      <button type="button" :class="cancelClass" @click="cancel">Cancel</button>
    </div>

    <p v-if="planQuery.isLoading.value || (plan && !ready)" class="text-sm text-muted-foreground">Loading…</p>
    <div v-else-if="planQuery.isError.value" class="space-y-2">
      <p class="text-sm text-destructive">
        Couldn't load what this app needs. {{ (planQuery.error.value as Error)?.message }}
      </p>
      <Button variant="secondary" size="sm" @click="planQuery.refetch()">Try again</Button>
    </div>

    <template v-else-if="plan">
      <!-- Header: the app, with its info box right under the name on the
           first step. The step's question comes after it, as the heading of
           the choice below it. -->
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
          <p class="min-w-0 flex-1 text-sm text-muted-foreground">
            <span class="font-medium text-foreground">{{ plan.name }}</span>
            <template v-if="stepNumber > 0 && steps.length > 1"> · Step {{ stepNumber }} of {{ steps.length }}</template>
          </p>
          <!-- The one way to end the install, on every page: it clears the
               draft and goes back to the App page. -->
          <button type="button" :class="cancelClass" @click="cancel">Cancel</button>
        </div>
        <InstallInfoBox
          v-if="onFirstStep && !noSteps"
          :app-name="plan.name"
          :permissions="plan.permissions"
          :footprint="plan.footprint"
        />
      </header>

      <!-- Warnings on the first step, before any question. -->
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
      <!-- For an app with no steps, the space warning is the page (its info
           box would carry it on a first step). -->
      <p
        v-if="noSteps && tight"
        class="mx-4 flex gap-2 rounded-lg border border-warning/40 bg-warning/10 px-4 py-3 text-sm sm:mx-0"
        role="status"
      >
        <TriangleAlert class="mt-0.5 size-4 shrink-0 text-warning" aria-hidden="true" />
        This might not fit. Only about {{ formatSize(plan.footprint.free_bytes) }} is free on your box. You can still
        install.
      </p>

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

      <div v-if="!noSteps" class="-mt-3 px-4 sm:px-0" role="group" aria-labelledby="install-question">
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
          <!-- A picked My own server account whose model names are not known
               yet: the names, required, right under the list. -->
          <div v-if="aiMode(aiPage.need, aiPage.page) === 'list' && serverNames" class="mt-4 space-y-4">
            <div v-for="m in serverNames" :key="m.key">
              <label :for="`server-model-${m.key}`" class="block text-sm/6 font-medium text-foreground">
                {{ serverNames.length > 1 ? m.field.title : "Model name" }}
              </label>
              <input
                :id="`server-model-${m.key}`"
                v-model="pageModels[m.key]"
                autocomplete="off"
                class="mt-2 block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 focus:outline-accent sm:text-sm/6"
              />
              <p class="mt-2 text-sm text-muted-foreground">The name your server gives the model.</p>
            </div>
          </div>
          <OptionalOffer
            v-else-if="aiMode(aiPage.need, aiPage.page) === 'offer'"
            v-model="pageOffer"
            setup-label="Set up an AI service"
            :label="`AI service for ${plan.name}`"
          />
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
          <OptionalOffer
            v-else-if="emailMode === 'offer'"
            v-model="pageOffer"
            setup-label="Set up email"
            :label="`Email for ${plan.name}`"
          />
          <div
            v-else-if="presetsQuery.isError.value"
            class="space-y-3 rounded-md bg-destructive/10 px-3 py-3 text-sm text-destructive"
            role="alert"
          >
            <p>Could not load the list of email services.</p>
            <Button size="sm" variant="secondary" @click="presetsQuery.refetch()">Try again</Button>
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
        <!-- The folder step: the consent screen for folder access, for every
             folder the app uses. -->
        <FolderChoices
          v-else-if="step === 'folders'"
          v-model:sources="pageSources"
          v-model:subfolders="pageSubfolders"
          :folders="folders"
          :scope="scope"
          :app-name="plan.name"
        />
      </div>

      <!-- 409 and errors no step owns: on the last step, above Install. -->
      <template v-if="onLastPage">
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
      </template>

      <div
        class="flex flex-col-reverse gap-3 border-t border-border px-4 pt-6 sm:flex-row sm:items-center sm:justify-end sm:px-0"
      >
        <p v-if="!canContinue && onEmailPage && !pending" class="text-sm text-muted-foreground sm:mr-auto">
          {{ emailMode === "grid" ? "Pick an email service to go on." : "Fill in the form to go on." }}
        </p>
        <p v-else-if="!canContinue && aiPage && !pending" class="text-sm text-muted-foreground sm:mr-auto">
          {{
            aiMode(aiPage.need, aiPage.page) === "grid"
              ? "Pick an AI service to go on."
              : aiMode(aiPage.need, aiPage.page) === "list"
                ? serverNames && pageAccount
                  ? "Type the model name to go on."
                  : "Pick a key to go on."
                : "Fill in the form to go on."
          }}
        </p>
        <p v-else-if="!canContinue && step === 'settings' && !pending" class="text-sm text-muted-foreground sm:mr-auto">
          Still needed: {{ settingsNeeded.join("; ") }}.
        </p>
        <!-- Back and Continue (or Install) only. On a grid or a form they act
             on adding the account, never on the whole install. -->
        <div class="flex flex-wrap justify-end gap-2">
          <Button variant="ghost" @click="goBack">
            <ArrowLeft class="size-4" aria-hidden="true" /> Back
          </Button>
          <HealthGated v-if="installsHere" blocks="apps">
            <Button :disabled="!canContinue || !!duplicateInfo" @click="noSteps ? install() : onContinue()">
              {{ pending ? "Starting…" : existing.length > 0 && noSteps ? "Install my own copy" : "Install" }}
            </Button>
          </HealthGated>
          <Button v-else :disabled="!canContinue" @click="onContinue">Continue</Button>
        </div>
      </div>
    </template>
  </div>
</template>
