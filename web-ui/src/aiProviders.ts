// AI providers for the install setup page (docs/specs/INSTALL_SETUP.md # 1 to
// # 3, and # 6).
//
// This is the one module that turns an app's roles and the provider data into
// tiles, model pickers and bindings. The provider list comes from the catalog
// (GET /api/v1/ai-providers), so it can change without an OS update. The app's
// fields say what they mean with a `role` (`ai.anthropic.api_key`,
// `ai.openai_compatible.models.chat`), sent on the install plan.
//
// The words follow the plan. A **slot** is the set of an app's fields that
// share `kind.protocol` (`ai.anthropic`): one provider account fills all of
// them at once. A slot is **native** to one protocol, or **compatible**
// (`openai_compatible`): it takes any provider with an OpenAI-compatible
// endpoint. A provider fills a native slot when its native_protocol matches,
// whatever its id.
//
// The page sends one binding per filled slot (account and model ids), and the
// brain turns it into the app's values. The page never sees a key again after
// the account is saved.
import type { AIAccount, AIBinding, AIModel, AIProvider, InstallPlanConfigField, RequiresGroup } from "./api";

// COMPATIBLE is the reserved protocol of the generic slot, and the provider id
// of an account made with the Other tile.
export const COMPATIBLE = "openai_compatible";

// OTHER is the "Other (OpenAI-compatible)" tile. It is the UI's own, not
// catalog data: it has no URL and no models, the user types the server's
// address, and the key is optional because a server the user runs may not
// ask for one. Its accounts are saved with provider id `openai_compatible`.
export const OTHER_ID = "__other";
export const OTHER: AIProvider = { id: OTHER_ID, name: "Other (OpenAI-compatible)", models: [] };

export function isOther(p: AIProvider): boolean {
  return p.id === OTHER_ID;
}

// withOther is the tile list: the catalog's providers in their order, then
// Other.
export function withOther(providers: AIProvider[]): AIProvider[] {
  return [...providers, OTHER];
}

export function findProvider(providers: AIProvider[], id: string): AIProvider | undefined {
  return id === OTHER_ID ? OTHER : providers.find((p) => p.id === id);
}

// accountProviderId is the provider id an account for this tile carries.
export function accountProviderId(p: AIProvider): string {
  return isOther(p) ? COMPATIBLE : p.id;
}

// accountsFor lists the user's accounts a tile can use.
export function accountsFor(p: AIProvider, accounts: AIAccount[]): AIAccount[] {
  const id = accountProviderId(p);
  return accounts.filter((a) => a.provider_id === id);
}

// keyLooksWrong is a soft check against the provider's key prefix. It is a
// hint for a pasted key that is cut short or from another provider, never a
// reason to block the save.
export function keyLooksWrong(p: AIProvider, key: string): boolean {
  const k = key.trim();
  return !!p.key_prefix && k !== "" && !k.startsWith(p.key_prefix);
}

// ── Roles and slots ─────────────────────────────────────────────────────────

type Role = { kind: string; protocol: string; attribute: string; modelType?: string };

// parseRole splits a role the brain sent. The brain sends a role only when it
// can fill the field, so the shape is already checked there.
function parseRole(role: string | undefined): Role | undefined {
  if (!role) return undefined;
  const [kind, protocol, attribute, modelType] = role.split(".");
  if (!kind || !protocol || !attribute) return undefined;
  return { kind, protocol, attribute, modelType };
}

// A model setting the slot declares: `model.chat` takes one id, and
// `models.chat` a list joined with the field's separator.
export type ModelSetting = {
  key: string; // model.chat, models.embedding: the key in a binding's models
  type: string;
  multiple: boolean;
  separator: string;
  field: InstallPlanConfigField;
};

export type AISlot = {
  id: string; // kind.protocol, e.g. ai.anthropic
  protocol: string;
  compatible: boolean;
  fields: InstallPlanConfigField[]; // every field of the slot, in manifest order
  hasKey: boolean;
  models: ModelSetting[];
};

// aiSlots groups an app's AI role fields into slots, in manifest order. Only
// kind `ai` is drawn on the setup page. A field without a role is a plain
// field.
export function aiSlots(fields: InstallPlanConfigField[]): AISlot[] {
  const byId = new Map<string, AISlot>();
  for (const f of fields) {
    const r = parseRole(f.role);
    if (!r || r.kind !== "ai") continue;
    const id = `${r.kind}.${r.protocol}`;
    let slot = byId.get(id);
    if (!slot) {
      slot = { id, protocol: r.protocol, compatible: r.protocol === COMPATIBLE, fields: [], hasKey: false, models: [] };
      byId.set(id, slot);
    }
    slot.fields.push(f);
    if (r.attribute === "api_key") slot.hasKey = true;
    if ((r.attribute === "model" || r.attribute === "models") && r.modelType) {
      slot.models.push({
        key: `${r.attribute}.${r.modelType}`,
        type: r.modelType,
        multiple: r.attribute === "models",
        separator: f.separator || ",",
        field: f,
      });
    }
  }
  return [...byId.values()];
}

// modelsOfType is what a model picker offers: the provider's models of that
// type, with its suggested one first. The picker also takes a typed id, so a
// model missing here never blocks the user.
export function modelsOfType(p: AIProvider, type: string): AIModel[] {
  const list = (p.models ?? []).filter((m) => (m.types ?? []).some((t) => t === type));
  const first = p.defaults?.[type];
  const i = list.findIndex((m) => m.id === first);
  if (i > 0) list.unshift(...list.splice(i, 1));
  return list;
}

// hasModelTypes: a listed provider can fill a slot only when it has a model of
// every type the slot declares. Otherwise the user would pick it and the app
// would have no model to use. Other has no list; the user types the ids.
export function hasModelTypes(p: AIProvider, slot: AISlot): boolean {
  return isOther(p) || slot.models.every((m) => modelsOfType(p, m.type).length > 0);
}

// fits says whether a provider can fill a slot at all: a native slot takes a
// provider whose native_protocol matches, and the compatible slot takes one
// with an OpenAI-compatible endpoint, or Other.
export function fits(p: AIProvider, slot: AISlot): boolean {
  if (!hasModelTypes(p, slot)) return false;
  if (slot.compatible) return isOther(p) || !!p.openai_base_url;
  return !isOther(p) && p.native_protocol === slot.protocol;
}

// slotFor picks where a provider's tile goes, in the plan's order (# 1): the
// app's slot for the provider's native protocol first, then the compatible
// slot. Undefined means the app cannot use this provider, and its tile is
// hidden.
export function slotFor(p: AIProvider, slots: AISlot[]): AISlot | undefined {
  const native = slots.find((s) => !s.compatible && fits(p, s));
  return native ?? slots.find((s) => s.compatible && fits(p, s));
}

// fillableSlots keeps the slots some tile can fill. The compatible slot can
// always take Other. A native slot needs a listed provider that fits it;
// without one its fields stay plain fields, so the user can still type them.
export function fillableSlots(slots: AISlot[], providers: AIProvider[]): AISlot[] {
  return slots.filter((s) => s.compatible || providers.some((p) => fits(p, s)));
}

// claimedEnvs is every app_env the AI row fills, so the Settings row can
// leave them out.
export function claimedEnvs(slots: AISlot[]): Set<string> {
  const out = new Set<string>();
  for (const s of slots) for (const f of s.fields) out.add(f.app_env);
  return out;
}

// ── Choices and bindings ────────────────────────────────────────────────────

// AIChoice is what the user picked for one slot: a tile, one of their accounts
// for it, and model ids per model setting.
export type AIChoice = {
  provider: string; // tile id: a provider id, or OTHER_ID
  accountId: string;
  models: Record<string, string[]>;
};

// suggestedModels is where the pickers start: each setting on the provider's
// default for its type. Other has no defaults, so it starts empty.
export function suggestedModels(p: AIProvider, slot: AISlot): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const m of slot.models) {
    const d = modelsOfType(p, m.type)[0]?.id;
    out[m.key] = d ? [d] : [];
  }
  return out;
}

// modelIdProblem says why one model id cannot be used, or "": the brain's rule
// for an id. No line breaks or control characters, and in a list no
// separator, since the app would split the id in two.
export function modelIdProblem(id: string, separator?: string): string {
  if (/[\u0000-\u001f\u007f-\u009f]/.test(id)) return "A model name cannot contain line breaks or control characters.";
  if (separator && id.includes(separator)) return `A model name cannot contain "${separator}" here.`;
  return "";
}

// modelProblem says why one model setting cannot be saved yet, or "". A
// listed provider with a default may leave it empty, and the brain then uses
// the default; the pickers start on it anyway.
export function modelProblem(m: ModelSetting, ids: string[], p: AIProvider): string {
  const clean = ids.map((x) => x.trim()).filter((x) => x !== "");
  if (clean.length === 0) {
    return !isOther(p) && p.defaults?.[m.type] ? "" : `Pick a model for ${m.field.title}.`;
  }
  if (!m.multiple && clean.length > 1) return `Pick one model for ${m.field.title}.`;
  for (const id of clean) {
    const problem = modelIdProblem(id, m.multiple ? m.separator : undefined);
    if (problem) return problem;
  }
  return "";
}

// choiceComplete says whether a choice can be saved into its slot: an account
// is picked, and every model setting has what it needs.
export function choiceComplete(slot: AISlot, c: AIChoice, p: AIProvider | undefined): boolean {
  if (!p || !c.accountId) return false;
  return slot.models.every((m) => modelProblem(m, c.models[m.key] ?? [], p) === "");
}

// bindingOf is the POST /api/v1/apps binding for one saved choice. Settings
// left empty are left out, so the brain uses the provider's default.
export function bindingOf(slot: AISlot, c: AIChoice): AIBinding {
  const models: Record<string, string[]> = {};
  for (const m of slot.models) {
    const seen = new Set<string>();
    const ids = (c.models[m.key] ?? []).map((x) => x.trim()).filter((x) => x !== "" && !seen.has(x) && seen.add(x));
    if (ids.length > 0) models[m.key] = ids;
  }
  const b: AIBinding = { slot: slot.id, account_id: c.accountId };
  if (Object.keys(models).length > 0) b.models = models;
  return b;
}

// filledEnvs lists the fields of a bound slot that the brain will really give
// a value, the same rule as its resolution: the key only when the account has
// one; a compatible base URL always (the account's, or the provider's
// OpenAI-compatible address); a native base URL only from the account's own
// base URL. Model fields are left out, because they never count for
// `requires`. Without the account (the list has not loaded) nothing counts.
export function filledEnvs(slot: AISlot, account: AIAccount | undefined): string[] {
  if (!account) return [];
  const out: string[] = [];
  for (const f of slot.fields) {
    const attr = parseRole(f.role)?.attribute;
    if (attr === "api_key" && account.key_set) out.push(f.app_env);
    if (attr === "base_url" && (slot.compatible || account.base_url)) out.push(f.app_env);
  }
  return out;
}

// ── Requires ────────────────────────────────────────────────────────────────

// fieldCounts mirrors the brain's rule for a requires member: a kind (`ai`)
// or slot (`ai.acme`) matches a field whose role starts with it, and any other
// member is an app_env. A model field never counts.
function fieldCounts(f: InstallPlanConfigField, member: string): boolean {
  const r = parseRole(f.role);
  if (r && (r.attribute === "model" || r.attribute === "models")) return false;
  if (member === f.app_env) return true;
  return !!f.role && f.role.startsWith(`${member}.`);
}

// groupFields lists the fields that can satisfy a requires group.
export function groupFields(fields: InstallPlanConfigField[], group: RequiresGroup): InstallPlanConfigField[] {
  const members = group.one_of ?? [];
  return fields.filter((f) => members.some((m) => fieldCounts(f, m)));
}

// unmetGroups returns the requires groups nothing fills yet. A field counts as
// filled when it has a typed value, or when a binding fills it (filledEnvs).
// The brain checks the same groups again on install.
export function unmetGroups(
  groups: RequiresGroup[],
  fields: InstallPlanConfigField[],
  values: Record<string, string>,
  filled: Set<string>,
): RequiresGroup[] {
  return groups.filter(
    (g) => !groupFields(fields, g).some((f) => filled.has(f.app_env) || (values[f.app_env] ?? "").trim() !== ""),
  );
}

// groupNeed names what one unmet group needs, for the "Still needed" line.
export function groupNeed(fields: InstallPlanConfigField[], group: RequiresGroup): string {
  const members = group.one_of ?? [];
  // A group of AI kinds or slots is met by any tile the page shows for them.
  if (members.every((m) => m === "ai" || /^ai\.[a-z0-9_]+$/.test(m))) return "an AI provider";
  const titles = groupFields(fields, group).map((f) => f.title);
  if (titles.length === 0) return members.join(", ");
  if (titles.length === 1) return titles[0]!;
  return `one of ${titles.slice(0, -1).join(", ")} or ${titles[titles.length - 1]}`;
}
