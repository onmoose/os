// AI providers for the install setup page (docs/specs/INSTALL_SETUP.md).
//
// TEMPORARY. Everything in this file is a stand-in until plan step 4 lands:
// manifest `role`s on config fields, and provider data published by the
// catalog service. Until then:
//
//   - PROVIDERS is a hand-written list in the UI. It will come from the catalog
//     service, so it can change without an OS update.
//   - aiSlots() guesses what a config field means from its app_env name
//     (ANTHROPIC_API_KEY and so on). With roles, the manifest says it.
//
// Step 4 replaces this whole module. Keep every guess about env names in here,
// so nothing else in the dashboard learns them.
//
// The words follow the plan. A **slot** is one set of the app's fields that one
// provider fills together (a key, and maybe a base URL and a model). A slot is
// either **native** to one provider (ANTHROPIC_API_KEY is Anthropic's) or
// **compatible**: it takes any provider with an OpenAI-compatible endpoint (the
// `<APP>_CUSTOM_*` triple, or OPENAI_API_KEY next to OPENAI_BASE_URL).
import type { Component } from "vue";
import { Bot, BrainCircuit, Cpu, Flame, Gauge, Globe, Server, Sparkles, Waypoints, Wind, Zap } from "lucide-vue-next";
import type { InstallPlanConfigField } from "./api";

export type AIProvider = {
  id: string;
  label: string;
  // logo is a bundled image URL. None ship yet, so every tile draws `icon`.
  logo?: string;
  icon: Component;
  // keyUrl is where the user makes a key. Absent for a server that needs none.
  keyUrl?: string;
  // baseUrl is the provider's OpenAI-compatible endpoint. A provider without
  // one (a self-hosted server) asks the user to type it.
  baseUrl?: string;
  // needsKey is false for a local server (Ollama) that takes no key.
  needsKey: boolean;
  // models are suggestions, first one is the default. The picker also takes a
  // typed model id, so a model missing here never blocks the user.
  models: string[];
};

// Order is the featured order: the setup page shows the first five, and the
// rest behind "More".
export const PROVIDERS: AIProvider[] = [
  {
    id: "anthropic",
    label: "Anthropic",
    icon: Sparkles,
    keyUrl: "https://console.anthropic.com/settings/keys",
    baseUrl: "https://api.anthropic.com/v1/",
    needsKey: true,
    models: ["claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5"],
  },
  {
    id: "openai",
    label: "OpenAI",
    icon: BrainCircuit,
    keyUrl: "https://platform.openai.com/api-keys",
    baseUrl: "https://api.openai.com/v1",
    needsKey: true,
    models: ["gpt-5", "gpt-5-mini", "gpt-4.1", "gpt-4o-mini"],
  },
  {
    id: "gemini",
    label: "Google Gemini",
    icon: Globe,
    keyUrl: "https://aistudio.google.com/apikey",
    baseUrl: "https://generativelanguage.googleapis.com/v1beta/openai/",
    needsKey: true,
    models: ["gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite"],
  },
  {
    id: "openrouter",
    label: "OpenRouter",
    icon: Waypoints,
    keyUrl: "https://openrouter.ai/keys",
    baseUrl: "https://openrouter.ai/api/v1",
    needsKey: true,
    models: [
      "anthropic/claude-sonnet-4.5",
      "openai/gpt-5",
      "google/gemini-2.5-pro",
      "meta-llama/llama-3.3-70b-instruct",
      "deepseek/deepseek-chat",
    ],
  },
  {
    id: "groq",
    label: "Groq",
    icon: Zap,
    keyUrl: "https://console.groq.com/keys",
    baseUrl: "https://api.groq.com/openai/v1",
    needsKey: true,
    models: ["llama-3.3-70b-versatile", "llama-3.1-8b-instant", "openai/gpt-oss-120b", "qwen/qwen3-32b"],
  },
  {
    id: "mistral",
    label: "Mistral",
    icon: Wind,
    keyUrl: "https://console.mistral.ai/api-keys",
    baseUrl: "https://api.mistral.ai/v1",
    needsKey: true,
    models: ["mistral-large-latest", "mistral-medium-latest", "mistral-small-latest", "codestral-latest"],
  },
  {
    id: "deepseek",
    label: "DeepSeek",
    icon: Bot,
    keyUrl: "https://platform.deepseek.com/api_keys",
    baseUrl: "https://api.deepseek.com/v1",
    needsKey: true,
    models: ["deepseek-chat", "deepseek-reasoner"],
  },
  {
    id: "xai",
    label: "xAI",
    icon: Bot,
    keyUrl: "https://console.x.ai",
    baseUrl: "https://api.x.ai/v1",
    needsKey: true,
    models: ["grok-4", "grok-3-mini"],
  },
  {
    id: "together",
    label: "Together AI",
    icon: Cpu,
    keyUrl: "https://api.together.ai/settings/api-keys",
    baseUrl: "https://api.together.xyz/v1",
    needsKey: true,
    models: ["meta-llama/Llama-3.3-70B-Instruct-Turbo", "deepseek-ai/DeepSeek-V3", "Qwen/Qwen2.5-72B-Instruct-Turbo"],
  },
  {
    id: "fireworks",
    label: "Fireworks AI",
    icon: Flame,
    keyUrl: "https://fireworks.ai/account/api-keys",
    baseUrl: "https://api.fireworks.ai/inference/v1",
    needsKey: true,
    models: ["accounts/fireworks/models/llama-v3p3-70b-instruct", "accounts/fireworks/models/deepseek-v3"],
  },
  {
    id: "cerebras",
    label: "Cerebras",
    icon: Gauge,
    keyUrl: "https://cloud.cerebras.ai",
    baseUrl: "https://api.cerebras.ai/v1",
    needsKey: true,
    models: ["llama-3.3-70b", "qwen-3-32b"],
  },
  {
    id: "ollama",
    label: "Ollama",
    icon: Server,
    needsKey: false,
    models: ["llama3.2", "qwen2.5", "mistral"],
  },
  {
    id: "custom",
    label: "Other (OpenAI-compatible)",
    icon: Server,
    needsKey: false,
    models: [],
  },
];

export type AIAttr = "api_key" | "base_url" | "model";

export type AISlot = {
  // id is the provider id for a native slot, or "custom:<PREFIX>" for a
  // `<PREFIX>_CUSTOM_*` triple.
  id: string;
  // native is the provider this slot belongs to, if any.
  native?: string;
  // compatible: the slot takes any OpenAI-compatible provider.
  compatible: boolean;
  fields: Partial<Record<AIAttr, InstallPlanConfigField>>;
};

// Exact env names we recognise, and what they mean. A name that is not here
// stays a plain field in the Settings section.
//
// A bare MODEL is left out on purpose: its value format is the app's own (one
// app wants "provider/model"), and a name alone cannot tell us which.
const NATIVE_ENV: Record<string, [provider: string, attr: AIAttr]> = {
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
    let nativeId: string | undefined;
    if (native) {
      [nativeId, attr] = native;
      id = nativeId;
    } else if (custom) {
      id = `custom:${custom[1]}`;
      attr = CUSTOM_ATTR[custom[2]!]!;
    } else {
      continue;
    }
    const slot = byId.get(id) ?? { id, native: nativeId, compatible: !nativeId, fields: {} };
    slot.fields[attr] = f;
    byId.set(id, slot);
  }
  const slots: AISlot[] = [];
  for (const s of byId.values()) {
    if (s.native && !s.fields.api_key) continue;
    if (!s.native && !s.fields.base_url) continue;
    // OPENAI_API_KEY next to OPENAI_BASE_URL is how many apps say "any
    // OpenAI-compatible server", so that slot takes other providers too.
    if (s.native === "openai" && s.fields.base_url) s.compatible = true;
    slots.push(s);
  }
  return slots;
}

// claimedEnvs is every app_env the AI section fills, so the Settings section
// can leave them out.
export function claimedEnvs(slots: AISlot[]): Set<string> {
  const out = new Set<string>();
  for (const s of slots) for (const f of Object.values(s.fields)) if (f) out.add(f.app_env);
  return out;
}

// slotFor picks where a provider goes, in the plan's order: the provider's own
// native slot first, then a slot that takes any OpenAI-compatible provider. A
// custom triple wins over OPENAI_* with a base URL, so picking Groq never
// takes over the app's OpenAI key. Undefined means the app cannot use this
// provider, and its tile is hidden.
export function slotFor(p: AIProvider, slots: AISlot[]): AISlot | undefined {
  const native = slots.find((s) => s.native === p.id);
  if (native) return native;
  const compatible = slots.filter((s) => s.compatible);
  return compatible.find((s) => !s.native) ?? compatible[0];
}

// usedAsCompatible: the provider fills a slot that is not its own, so the
// slot's base URL field must point at the provider.
export function usedAsCompatible(p: AIProvider, slot: AISlot): boolean {
  return slot.native !== p.id;
}

// A provider without a fixed endpoint (Ollama, other) asks for its URL.
export function asksForUrl(p: AIProvider, slot: AISlot): boolean {
  return usedAsCompatible(p, slot) && !p.baseUrl;
}

// A custom triple needs a model: the apps that declare one say the model is
// required once a base URL is set. A native model field is optional, and blank
// means the app's own default.
export function modelRequired(slot: AISlot): boolean {
  return !slot.native && !!slot.fields.model;
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
export function fieldValues(slot: AISlot, c: AIChoice): Record<string, string> {
  const p = PROVIDERS.find((x) => x.id === c.provider);
  const out: Record<string, string> = {};
  const { api_key, base_url, model } = slot.fields;
  if (api_key) out[api_key.app_env] = c.key.trim();
  if (base_url) {
    out[base_url.app_env] = p && usedAsCompatible(p, slot) ? (p.baseUrl ?? c.baseUrl.trim()) : "";
  }
  if (model) out[model.app_env] = c.model.trim();
  return out;
}

// choiceComplete says whether a choice can be saved into its slot.
export function choiceComplete(slot: AISlot, c: AIChoice): boolean {
  const p = PROVIDERS.find((x) => x.id === c.provider);
  if (!p) return false;
  if (slot.fields.api_key && p.needsKey && !c.key.trim()) return false;
  if (asksForUrl(p, slot) && !/^https?:\/\/\S+$/.test(c.baseUrl.trim())) return false;
  if (modelRequired(slot) && !c.model.trim()) return false;
  return true;
}
