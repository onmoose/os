// AI providers for the install setup page (docs/specs/INSTALL_SETUP.md).
//
// The provider list comes from the catalog (GET /api/v1/ai-providers, plan
// step 3), so it can change without an OS update. This module turns that list
// and an app's config fields into tiles and field values.
//
// TEMPORARY: the env-name lookup. aiSlots() guesses what a config field means
// from its app_env name (ANTHROPIC_API_KEY and so on). With manifest `role`s
// (plan step 4) the manifest says it, and NATIVE_ENV, CUSTOM_ENV and aiSlots()
// go. Keep every guess about env names in here, so nothing else in the
// dashboard learns them.
//
// The words follow the plan. A **slot** is one set of the app's fields that one
// provider fills together (a key, and maybe a base URL and a model). A slot is
// either **native** to one protocol (ANTHROPIC_API_KEY speaks the anthropic
// protocol) or **compatible**: it takes any provider with an OpenAI-compatible
// endpoint (the `<APP>_CUSTOM_*` triple, or OPENAI_API_KEY next to
// OPENAI_BASE_URL). A provider fills a native slot when its native_protocol
// matches, whatever its id.
import type { AIModel, AIProvider, InstallPlanConfigField } from "./api";

// OTHER is the "Other (OpenAI-compatible)" tile. It is the UI's own, not
// catalog data: it has no URL and no models, the user types the server's
// address, and the key is optional because a server the user runs may not
// ask for one. Its id is one no catalog provider is expected to use; if one
// did, Other would win the lookup.
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

// chatModels is what the model picker offers: the provider's chat models, with
// its suggested chat model first. The picker also takes a typed model id, so a
// model missing here never blocks the user.
export function chatModels(p: AIProvider): AIModel[] {
  const chat = (p.models ?? []).filter((m) => (m.types ?? []).includes("chat"));
  const first = p.defaults?.chat;
  const i = chat.findIndex((m) => m.id === first);
  if (i > 0) chat.unshift(...chat.splice(i, 1));
  return chat;
}

// suggestedModel is the model a required model field starts on.
export function suggestedModel(p: AIProvider): string {
  return chatModels(p)[0]?.id ?? "";
}

// keyLooksWrong is a soft check against the provider's key prefix. It is a
// hint for a pasted key that is cut short or from another provider, never a
// reason to block the save.
export function keyLooksWrong(p: AIProvider, key: string): boolean {
  const k = key.trim();
  return !!p.key_prefix && k !== "" && !k.startsWith(p.key_prefix);
}

export type AIAttr = "api_key" | "base_url" | "model";

export type AISlot = {
  // id is the protocol for a native slot, or "custom:<PREFIX>" for a
  // `<PREFIX>_CUSTOM_*` triple.
  id: string;
  // protocol is the native protocol this slot speaks, if any.
  protocol?: string;
  // compatible: the slot takes any OpenAI-compatible provider.
  compatible: boolean;
  fields: Partial<Record<AIAttr, InstallPlanConfigField>>;
};

// Exact env names we recognise, and the native protocol and part each one is.
// A name that is not here stays a plain field in the Settings section.
//
// A bare MODEL is left out on purpose: its value format is the app's own (one
// app wants "provider/model"), and a name alone cannot tell us which.
const NATIVE_ENV: Record<string, [protocol: string, attr: AIAttr]> = {
  ANTHROPIC_API_KEY: ["anthropic", "api_key"],
  OPENAI_API_KEY: ["openai", "api_key"],
  OPENAI_BASE_URL: ["openai", "base_url"],
  OPENAI_MODEL: ["openai", "model"],
  GEMINI_API_KEY: ["gemini", "api_key"],
  OPENROUTER_API_KEY: ["openrouter", "api_key"],
  OPENROUTER_MODEL: ["openrouter", "model"],
  GROQ_API_KEY: ["groq", "api_key"],
  MISTRAL_API_KEY: ["mistral", "api_key"],
  DEEPSEEK_API_KEY: ["deepseek", "api_key"],
  XAI_API_KEY: ["xai", "api_key"],
};

const CUSTOM_ENV = /^([A-Z0-9_]+)_CUSTOM_(BASE_URL|MODEL|API_KEY)$/;
const CUSTOM_ATTR: Record<string, AIAttr> = { BASE_URL: "base_url", MODEL: "model", API_KEY: "api_key" };

// aiSlots reads an app's config fields and returns the AI slots it recognises.
// A native slot counts only with a key field, and a custom triple only with a
// base URL field. Anything else is left for the Settings section.
export function aiSlots(fields: InstallPlanConfigField[]): AISlot[] {
  const byId = new Map<string, AISlot>();
  for (const f of fields) {
    const native = NATIVE_ENV[f.app_env];
    const custom = CUSTOM_ENV.exec(f.app_env);
    let id: string;
    let attr: AIAttr;
    let protocol: string | undefined;
    if (native) {
      [protocol, attr] = native;
      id = protocol;
    } else if (custom) {
      id = `custom:${custom[1]}`;
      attr = CUSTOM_ATTR[custom[2]!]!;
    } else {
      continue;
    }
    const slot = byId.get(id) ?? { id, protocol, compatible: !protocol, fields: {} };
    slot.fields[attr] = f;
    byId.set(id, slot);
  }
  const slots: AISlot[] = [];
  for (const s of byId.values()) {
    if (s.protocol && !s.fields.api_key) continue;
    if (!s.protocol && !s.fields.base_url) continue;
    // OPENAI_API_KEY next to OPENAI_BASE_URL is how many apps say "any
    // OpenAI-compatible server", so that slot takes other providers too.
    if (s.protocol === "openai" && s.fields.base_url) s.compatible = true;
    slots.push(s);
  }
  return slots;
}

// fillableSlots keeps the slots some tile can fill. A compatible slot can
// always take Other. A native slot needs a provider that speaks its protocol;
// without one its fields go back to the Settings section, so the user can
// still type them.
export function fillableSlots(slots: AISlot[], providers: AIProvider[]): AISlot[] {
  return slots.filter((s) => s.compatible || providers.some((p) => p.native_protocol === s.protocol));
}

// claimedEnvs is every app_env the AI section fills, so the Settings section
// can leave them out.
export function claimedEnvs(slots: AISlot[]): Set<string> {
  const out = new Set<string>();
  for (const s of slots) for (const f of Object.values(s.fields)) if (f) out.add(f.app_env);
  return out;
}

// slotFor picks where a provider goes, in the plan's order: a slot that speaks
// the provider's native protocol first, then a slot that takes any
// OpenAI-compatible provider, if the provider has such an endpoint (Other
// always does, since the user types it). A custom triple wins over OPENAI_*
// with a base URL, so picking Groq never takes over the app's OpenAI key.
// Undefined means the app cannot use this provider, and its tile is hidden.
export function slotFor(p: AIProvider, slots: AISlot[]): AISlot | undefined {
  if (p.native_protocol) {
    const native = slots.find((s) => s.protocol === p.native_protocol);
    if (native) return native;
  }
  if (!p.openai_base_url && !isOther(p)) return undefined;
  const compatible = slots.filter((s) => s.compatible);
  return compatible.find((s) => !s.protocol) ?? compatible[0];
}

// usedAsCompatible: the provider fills a slot that is not its native one, so
// the slot's base URL field must point at the provider.
export function usedAsCompatible(p: AIProvider, slot: AISlot): boolean {
  return !p.native_protocol || slot.protocol !== p.native_protocol;
}

// Other has no fixed endpoint, so it asks for the server's address.
export function asksForUrl(p: AIProvider, slot: AISlot): boolean {
  return usedAsCompatible(p, slot) && !p.openai_base_url;
}

// A custom triple needs a model: the apps that declare one say the model is
// required once a base URL is set. A native model field is optional, and blank
// means the app's own default.
export function modelRequired(slot: AISlot): boolean {
  return !slot.protocol && !!slot.fields.model;
}

export type AIChoice = {
  provider: string;
  key: string;
  baseUrl: string;
  model: string;
};

// fieldValues turns one slot's choice into app_env values. Every field of the
// slot gets a value, empty when unused, so switching a slot from one provider
// to another never leaves the old one's URL behind.
export function fieldValues(slot: AISlot, c: AIChoice, p: AIProvider | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  const { api_key, base_url, model } = slot.fields;
  if (api_key) out[api_key.app_env] = c.key.trim();
  if (base_url) {
    out[base_url.app_env] = p && usedAsCompatible(p, slot) ? (p.openai_base_url ?? c.baseUrl.trim()) : "";
  }
  if (model) out[model.app_env] = c.model.trim();
  return out;
}

// choiceComplete says whether a choice can be saved into its slot. The key is
// optional only for Other.
export function choiceComplete(slot: AISlot, c: AIChoice, p: AIProvider | undefined): boolean {
  if (!p) return false;
  if (slot.fields.api_key && !isOther(p) && !c.key.trim()) return false;
  if (asksForUrl(p, slot) && !/^https?:\/\/\S+$/.test(c.baseUrl.trim())) return false;
  if (modelRequired(slot) && !c.model.trim()) return false;
  return true;
}
