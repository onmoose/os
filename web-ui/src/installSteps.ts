// The install flow as pages (docs/specs/INSTALL_STEPS.md): one question per
// page, then a last page that shows every answer with a Change link.
//
// This module is the pure part. It turns the install plan (flat `config`
// fields and `requires` groups) and the AI provider data into the needs the
// pages ask about, says which need is answered, keeps the draft in the tab's
// session storage, and says which page owns a 422. The view
// (views/InstallSetupView.vue) holds the state and draws the pages.
//
// The mapping from plan to pages is INSTALL_STEPS.md # Build rules, rule 1:
//   - a requires group of AI members only: required AI pages, one per group;
//   - AI slots in no group: the optional "AI service" row on the last page;
//   - the mail block: the optional "Email" row;
//   - required plain fields, and the fields of a plain group: one page,
//     "<App> needs these to run";
//   - optional plain fields: the "Extra settings" row.
// A mixed group and a required field with a role are lint errors. When the
// box meets one anyway, the fields of that group or slot become plain fields
// on the "needs these" page.
import type {
  AIAccount,
  AIProvider,
  InstallPlanConfigField,
  InstallPlanFootprint,
  RequiresGroup,
} from "./api";
import {
  COMPATIBLE,
  OTHER,
  withOther,
  aiSlots,
  choiceComplete,
  claimedEnvs,
  filledEnvs,
  fillableSlots,
  groupFields,
  slotFor,
  suggestedModels,
  unmetGroups,
  type AIChoice,
  type AISlot,
} from "./aiProviders";

// ── Step names ──────────────────────────────────────────────────────────────
// A question page is /store/:id/install?step=<name>. The last page has no
// step. AI group pages are "ai", "ai-2", "ai-3" in requires order.
export const STEP_SETTINGS = "settings";
export const STEP_AI_OPTIONAL = "ai-optional";
export const STEP_EMAIL = "email";
export const STEP_EMAIL_SERVICE = "email-service";
export const STEP_EMAIL_ADD = "email-add";
export const STEP_EXTRA = "extra";
export const STEP_FOLDERS = "folders";
export const STEP_FOR = "for";

function aiStep(i: number): string {
  return i === 0 ? "ai" : `ai-${i + 1}`;
}

// ── Needs ───────────────────────────────────────────────────────────────────

export type AIGroupNeed = { step: string; group: RequiresGroup; slots: AISlot[] };

export type Needs = {
  // slots is every AI slot drawn as tiles, on any page.
  slots: AISlot[];
  aiGroups: AIGroupNeed[];
  // optionalSlots are the drawn slots that no AI group covers.
  optionalSlots: AISlot[];
  // requiredFields are the plain fields of the "needs these to run" page:
  // required ones, and the members of a plain group, in manifest order.
  requiredFields: InstallPlanConfigField[];
  // plainGroups gate that page's Continue.
  plainGroups: RequiresGroup[];
  // extraFields are the optional plain fields of the "Extra settings" page.
  extraFields: InstallPlanConfigField[];
};

function isAIMember(m: string): boolean {
  return m === "ai" || /^ai\.[a-z0-9_]+$/.test(m);
}

// planNeeds sorts the plan's fields into the pages. providers empty (no
// provider data) means no slot is drawn and every field is a plain field.
export function planNeeds(
  fields: InstallPlanConfigField[],
  requires: RequiresGroup[],
  providers: AIProvider[],
): Needs {
  let drawn = providers.length > 0 ? fillableSlots(aiSlots(fields), providers) : [];

  // A slot with a required field is shown as plain fields: the brain would
  // refuse a binding next to a typed value for the same slot.
  drawn = drawn.filter((s) => !s.fields.some((f) => f.required));

  // A group that is not AI only, or whose AI members match no drawn slot, is a
  // plain group. Its fields go on the "needs these" page, so a slot that
  // holds one of them is shown as plain fields too.
  const slotsOf = (g: RequiresGroup, from: AISlot[]) => {
    const members = new Set(groupFields(fields, g).map((f) => f.app_env));
    return from.filter((s) => s.fields.some((f) => members.has(f.app_env)));
  };
  const aiOnly = (g: RequiresGroup) => (g.one_of ?? []).length > 0 && (g.one_of ?? []).every(isAIMember);
  const plainGroups: RequiresGroup[] = [];
  for (const g of requires) {
    if (aiOnly(g) && slotsOf(g, drawn).length > 0) continue;
    plainGroups.push(g);
    const demote = new Set(slotsOf(g, drawn).map((s) => s.id));
    drawn = drawn.filter((s) => !demote.has(s.id));
  }
  // A demotion can leave an AI group with no slot. It is a plain group then.
  const aiGroups: AIGroupNeed[] = [];
  for (const g of requires) {
    if (plainGroups.includes(g)) continue;
    const slots = slotsOf(g, drawn);
    if (slots.length === 0) plainGroups.push(g);
    else aiGroups.push({ step: aiStep(aiGroups.length), group: g, slots });
  }

  const inGroup = new Set(aiGroups.flatMap((g) => g.slots.map((s) => s.id)));
  const claimed = claimedEnvs(drawn);
  const plainGroupEnvs = new Set(plainGroups.flatMap((g) => groupFields(fields, g).map((f) => f.app_env)));
  const plain = fields.filter((f) => !claimed.has(f.app_env));
  return {
    slots: drawn,
    aiGroups,
    optionalSlots: drawn.filter((s) => !inGroup.has(s.id)),
    requiredFields: plain.filter((f) => f.required || plainGroupEnvs.has(f.app_env)),
    plainGroups,
    extraFields: plain.filter((f) => !f.required && !plainGroupEnvs.has(f.app_env)),
  };
}

// filledBy is every field the bound slots will really get a value for, by the
// brain's rule (filledEnvs).
export function filledBy(slots: AISlot[], choices: Record<string, AIChoice>, accounts: AIAccount[]): Set<string> {
  const out = new Set<string>();
  for (const s of slots) {
    const c = choices[s.id];
    if (!c) continue;
    for (const env of filledEnvs(s, accounts.find((a) => a.id === c.accountId))) out.add(env);
  }
  return out;
}

export function groupMet(
  group: RequiresGroup,
  fields: InstallPlanConfigField[],
  values: Record<string, string>,
  filled: Set<string>,
): boolean {
  return unmetGroups([group], fields, values, filled).length === 0;
}

// requiredFieldsMet: every required field on the "needs these" page has a
// value, and every plain group has a filled member.
export function requiredFieldsMet(
  needs: Needs,
  fields: InstallPlanConfigField[],
  values: Record<string, string>,
): boolean {
  if (needs.requiredFields.some((f) => f.required && (values[f.app_env] ?? "").trim() === "")) return false;
  return unmetGroups(needs.plainGroups, fields, values, new Set()).length === 0;
}

// ── AI needs ────────────────────────────────────────────────────────────────
// An AI need is one set of AI pages: a required group, or the optional row
// for the slots no group covers. One AI service fills one need
// (INSTALL_STEPS.md # Decisions): picking a key for a need replaces the
// choice of every slot of that need.
//
// Each need has three pages, named after its step: <step> (the key list, or
// the service grid when the user has no usable key, or the key form when
// only one service fits), <step>-service (the grid) and <step>-key (the key
// form).

export type AINeed = { step: string; slots: AISlot[]; required: boolean; group?: RequiresGroup };

export const SUB_SERVICE = "-service";
export const SUB_KEY = "-key";

export function aiNeeds(needs: Needs): AINeed[] {
  const out: AINeed[] = needs.aiGroups.map((g) => ({ step: g.step, slots: g.slots, required: true, group: g.group }));
  if (needs.optionalSlots.length > 0) out.push({ step: STEP_AI_OPTIONAL, slots: needs.optionalSlots, required: false });
  return out;
}

// needOfStep finds the need a step belongs to, and which of its pages it is.
export function needOfStep(
  needs: AINeed[],
  step: string,
): { need: AINeed; page: "base" | "service" | "key" } | undefined {
  for (const need of needs) {
    if (step === need.step) return { need, page: "base" };
    if (step === need.step + SUB_SERVICE) return { need, page: "service" };
    if (step === need.step + SUB_KEY) return { need, page: "key" };
  }
  return undefined;
}

// tileOf is the service tile an account belongs to: My own server for an
// OpenAI-compatible account, else its listed provider.
export function tileOf(account: AIAccount, providers: AIProvider[]): AIProvider | undefined {
  return account.provider_id === COMPATIBLE ? OTHER : providers.find((p) => p.id === account.provider_id);
}

// needTiles are the services that can fill a need, in the provider data's
// order (popularity order), with My own server last.
export function needTiles(need: AINeed, providers: AIProvider[]): AIProvider[] {
  return withOther(providers).filter((p) => !!slotFor(p, need.slots));
}

// choiceFor is the choice one account gives a need, with the provider's
// default models, or undefined when the account cannot fill the need.
export function choiceFor(
  need: AINeed,
  fields: InstallPlanConfigField[],
  providers: AIProvider[],
  account: AIAccount,
  models?: Record<string, string[]>,
): { slotId: string; choice: AIChoice } | undefined {
  const provider = tileOf(account, providers);
  if (!provider) return undefined;
  const slot = slotFor(provider, need.slots);
  if (!slot) return undefined;
  const choice: AIChoice = { provider: provider.id, accountId: account.id, models: models ?? suggestedModels(provider, slot) };
  if (!choiceComplete(slot, choice, provider)) return undefined;
  if (need.group && !groupMet(need.group, fields, {}, new Set(filledEnvs(slot, account)))) return undefined;
  return { slotId: slot.id, choice };
}

// usableAccounts are the user's accounts that can fill a need, newest first.
export function usableAccounts(
  need: AINeed,
  fields: InstallPlanConfigField[],
  providers: AIProvider[],
  accounts: AIAccount[],
): AIAccount[] {
  return [...accounts]
    .sort((a, b) => b.created_at - a.created_at)
    .filter((a) => !!choiceFor(need, fields, providers, a));
}

// pickInAdvance is the choice a saved account gives a need: the newest usable
// account (INSTALL_STEPS.md # Build rules, rule 3). Undefined when none can.
export function pickInAdvance(
  need: AINeed,
  fields: InstallPlanConfigField[],
  providers: AIProvider[],
  accounts: AIAccount[],
): { slotId: string; choice: AIChoice } | undefined {
  const first = usableAccounts(need, fields, providers, accounts)[0];
  return first && choiceFor(need, fields, providers, first);
}

// withNeedChoice replaces a need's choice: every slot of the need is cleared,
// then the one slot is set (or none, to remove the need's choice).
export function withNeedChoice(
  choices: Record<string, AIChoice>,
  need: AINeed,
  pick: { slotId: string; choice: AIChoice } | undefined,
): Record<string, AIChoice> {
  const next = { ...choices };
  for (const s of need.slots) delete next[s.id];
  if (pick) next[pick.slotId] = pick.choice;
  return next;
}

// defaultKeyLabel is the name a new key gets when the user gives none: the
// base ("Anthropic key"), then "Anthropic key 2" and on, so a name is never
// needed to go on.
export function defaultKeyLabel(base: string, labels: string[]): string {
  const taken = new Set(labels.map((l) => l.trim().toLowerCase()));
  if (!taken.has(base.toLowerCase())) return base;
  for (let n = 2; ; n++) if (!taken.has(`${base} ${n}`.toLowerCase())) return `${base} ${n}`;
}

// ── Draft in session storage ────────────────────────────────────────────────
// INSTALL_STEPS.md # Build rules, rule 5. One key per user, app and scope.
// Only non-secret answers are stored: secret fields stay in memory, and a
// reload asks for them again. Unsaved keys never reach this module at all:
// they live in the AI and email forms until the account is saved.

export type Draft = {
  // flow is the question pages this user was shown when the install started,
  // in order. It is fixed then, so the step counter does not jump when a page
  // is answered.
  flow: string[];
  // done is the pages the user pressed Continue on.
  done: string[];
  // reviewed is set once the last page was shown. From then on Continue goes
  // back to the last page (the "Check your answers" pattern).
  reviewed: boolean;
  sources: Record<string, string>;
  subfolders: Record<string, string>;
  // mail is the picked email account id, "" when not set up.
  mail: string;
  values: Record<string, string>;
  ai: Record<string, AIChoice>;
  // aiService is the service picked on a need's grid, by need step.
  aiService: Record<string, string>;
  // mailService is the email service picked on the email grid.
  mailService: string;
  // warned is set once the duplicate warning was shown, so the install is
  // sent with confirm: true only then.
  warned: boolean;
  // foldersReset is set when a change of scope put the folders back on their
  // defaults, so the last page can say so once.
  foldersReset: boolean;
};

const PREFIX = "moose.install.v1";

export function draftKey(userId: string, manifestId: string, scope: string): string {
  return `${PREFIX}.${userId}.${manifestId}.${scope}`;
}

export function loadDraft(key: string): Partial<Draft> | null {
  try {
    const raw = sessionStorage.getItem(key);
    if (!raw) return null;
    const d = JSON.parse(raw) as unknown;
    return d && typeof d === "object" ? (d as Partial<Draft>) : null;
  } catch {
    return null;
  }
}

// saveDraft stores the draft without the values of secret fields.
export function saveDraft(key: string, d: Draft, secret: Set<string>) {
  const values: Record<string, string> = {};
  for (const [k, v] of Object.entries(d.values)) if (!secret.has(k)) values[k] = v;
  try {
    sessionStorage.setItem(key, JSON.stringify({ ...d, values }));
  } catch {
    // Storage full or blocked: the draft still lives in memory.
  }
}

// clearDrafts removes the app's drafts for both scopes, after an install
// starts or when the user cancels.
export function clearDrafts(userId: string, manifestId: string) {
  for (const scope of ["personal", "household"]) {
    try {
      sessionStorage.removeItem(draftKey(userId, manifestId, scope));
    } catch {
      // Nothing to clear.
    }
  }
}

// ── Errors ──────────────────────────────────────────────────────────────────

// stepForError names the page that owns a 422 from POST /api/v1/apps, so the
// error shows there and not as red text on the last page. It reads the
// location the brain puts on the error (BRAIN_UI_PROTOCOL.md # POST
// /api/v1/apps): config.fields.<APP_ENV>, config.ai_bindings.<slot>,
// config.requires[<i>], config.mail_provider_id or config.folders.<name>.
// The message is never read, only shown. "" means the last page keeps it.
export function stepForError(
  location: string | undefined,
  needs: Needs,
  requires: RequiresGroup[],
  hasMail: boolean,
): string {
  if (!location) return "";
  const stepOfSlot = (slotId: string) =>
    needs.aiGroups.find((g) => g.slots.some((s) => s.id === slotId))?.step ??
    (needs.optionalSlots.some((s) => s.id === slotId) ? STEP_AI_OPTIONAL : "");

  if (location.startsWith("config.folders.")) return STEP_FOLDERS;
  if (location === "config.mail_provider_id") return hasMail ? STEP_EMAIL : "";
  if (location.startsWith("config.ai_bindings.")) {
    return stepOfSlot(location.slice("config.ai_bindings.".length)) || needs.aiGroups[0]?.step || "";
  }
  const req = /^config\.requires\[(\d+)\]$/.exec(location);
  if (req) {
    const group = requires[Number(req[1])];
    if (!group) return "";
    const ai = needs.aiGroups.find((g) => g.group === group);
    if (ai) return ai.step;
    return needs.plainGroups.includes(group) ? STEP_SETTINGS : "";
  }
  if (location.startsWith("config.fields.")) {
    const env = location.slice("config.fields.".length);
    const slot = needs.slots.find((s) => s.fields.some((x) => x.app_env === env));
    if (slot) return stepOfSlot(slot.id);
    if (needs.requiredFields.some((f) => f.app_env === env)) return STEP_SETTINGS;
    if (needs.extraFields.some((f) => f.app_env === env)) return STEP_EXTRA;
  }
  return "";
}

// ── Info box ────────────────────────────────────────────────────────────────

// spaceTight warns, never blocks, when the projected need nears the free
// space. 90% is a UI judgement of "nears"; free_bytes 0 means the brain could
// not measure.
export function spaceTight(fp: InstallPlanFootprint | undefined): boolean {
  if (!fp) return false;
  const need = fp.image_disk_bytes + (fp.estimated_state_bytes ?? 0);
  return fp.free_bytes > 0 && need >= fp.free_bytes * 0.9;
}

// ── Folder words ────────────────────────────────────────────────────────────

export function folderName(folder: string): string {
  return folder.charAt(0).toUpperCase() + folder.slice(1);
}

export function sourceLabel(folder: string, source: string, singleUser: boolean): string {
  const name = folderName(folder);
  if (source === "shared") {
    return singleUser ? `Shared ${name} (accessible from your other devices)` : `The household's shared ${name}`;
  }
  return `Your ${name}`;
}
