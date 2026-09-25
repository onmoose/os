<script setup lang="ts">
// The AI providers row of the install setup page (INSTALL_SETUP.md # 6). The
// user picks a provider tile, types its key (and a model or URL where the app
// has a field for it), and saves. Saving fills every field of the slot the
// provider lands in; the parent turns the choices into app_env values with
// fieldValues(). The providers come from the catalog (the parent fetches
// them); which slot a provider lands in, and which tiles show at all, comes
// from aiProviders.ts.
//
// An app can take several providers at once (Anthropic and OpenAI in openclaw),
// one per slot. A compatible slot holds one provider, so saving a second one
// there replaces the first, and the editor says so before it happens.
import { computed, ref } from "vue";
import { Bot, ExternalLink, Server } from "lucide-vue-next";
import type { AIProvider } from "../../api";
import Button from "../ui/Button.vue";
import OptionCards, { type Option } from "./OptionCards.vue";
import {
  withOther,
  findProvider,
  isOther,
  chatModels,
  suggestedModel,
  keyLooksWrong,
  slotFor,
  asksForUrl,
  modelRequired,
  choiceComplete,
  type AIChoice,
  type AISlot,
} from "../../aiProviders";

const props = defineProps<{ slots: AISlot[]; providers: AIProvider[]; appName: string }>();
// choices is keyed by slot id.
const choices = defineModel<Record<string, AIChoice>>({ required: true });

// Only providers this app can use get a tile, in the catalog's order, with
// Other last.
const offered = computed(() => withOther(props.providers).filter((p) => slotFor(p, props.slots)));

function providerOf(id: string): AIProvider | undefined {
  return findProvider(props.providers, id);
}

// ── Logos ───────────────────────────────────────────────────────────────────
// The dashboard has a light theme only today. When a dark theme lands it will
// put the `dark` class on <html> (the Tailwind convention), and the dark logo
// is used then. Following the OS setting instead would draw a light logo on
// the light page for anyone whose OS is dark.
const darkTheme = document.documentElement.classList.contains("dark");
// A logo that fails to load falls back to the icon, like an app icon does.
const brokenLogos = ref(new Set<string>());
function logoOf(p: AIProvider | undefined): string | undefined {
  if (!p || brokenLogos.value.has(p.id)) return undefined;
  return (darkTheme && p.logo_dark_url) || p.logo_url;
}
function logoFailed(id: string) {
  brokenLogos.value = new Set(brokenLogos.value).add(id);
}
function fallbackIcon(p: AIProvider | undefined) {
  return p && isOther(p) ? Server : Bot;
}

// The key link is catalog data, so only a plain web address becomes a link.
function isWebLink(u: string | undefined): boolean {
  return !!u && /^https?:\/\//i.test(u);
}

function modelName(p: AIProvider | undefined, id: string): string {
  return p?.models?.find((m) => m.id === id)?.name ?? id;
}

const added = computed(() => Object.values(choices.value).map((c) => c.provider));

const tileOptions = computed(() =>
  offered.value.map((p) => {
    const c = Object.values(choices.value).find((x) => x.provider === p.id);
    return {
      id: p.id,
      label: p.name,
      description: c ? (c.model ? `Added, uses ${modelName(p, c.model)}` : "Added") : undefined,
    };
  }),
);

// ── Editor ──────────────────────────────────────────────────────────────────
type Editing = { slot: AISlot; provider: AIProvider; draft: AIChoice };
const editing = ref<Editing | null>(null);

// A tile shows as selected once its provider is added, and also while its
// editor is open, so the user sees which tile they are filling in.
const selectedTiles = computed(() => {
  const open = editing.value?.provider.id;
  return open && !added.value.includes(open) ? [...added.value, open] : added.value;
});

function pick(id: string) {
  // A second click on the provider being edited closes its editor.
  if (editing.value?.provider.id === id) {
    editing.value = null;
    return;
  }
  const provider = providerOf(id);
  const slot = provider && slotFor(provider, props.slots);
  if (!provider || !slot) return;
  const current = choices.value[slot.id];
  const draft: AIChoice =
    current?.provider === id
      ? { ...current }
      : {
          provider: id,
          key: "",
          baseUrl: "",
          // A required model starts on the provider's suggested chat model. An
          // optional one starts blank, which leaves the app on its default.
          model: slot.fields.model && modelRequired(slot) ? suggestedModel(provider) : "",
        };
  editing.value = { slot, provider, draft };
}

// replaces names the provider a save would push out of a shared slot.
const replaces = computed(() => {
  if (!editing.value) return null;
  const current = choices.value[editing.value.slot.id];
  if (!current || current.provider === editing.value.provider.id) return null;
  return providerOf(current.provider)?.name ?? null;
});

const isAdded = computed(
  () => !!editing.value && choices.value[editing.value.slot.id]?.provider === editing.value.provider.id,
);

const canSave = computed(
  () => !!editing.value && choiceComplete(editing.value.slot, editing.value.draft, editing.value.provider),
);

const keyWarning = computed(() => !!editing.value && keyLooksWrong(editing.value.provider, editing.value.draft.key));

function save() {
  if (!editing.value || !canSave.value) return;
  choices.value = { ...choices.value, [editing.value.slot.id]: { ...editing.value.draft } };
  editing.value = null;
}

function remove() {
  if (!editing.value) return;
  const next = { ...choices.value };
  delete next[editing.value.slot.id];
  choices.value = next;
  editing.value = null;
}

// The chat models the picker offers, suggested one first.
const editingModels = computed(() => (editing.value ? chatModels(editing.value.provider) : []));

// The model picker uses a sentinel id for "the app's own default", because the
// real value for it is an empty string.
const APP_DEFAULT = "__app_default";
const modelOptions = computed<Option[]>(() => {
  if (!editing.value) return [];
  const suggested = editing.value.provider.defaults?.chat;
  const opts: Option[] = editingModels.value.map((m) => {
    const notes = [m.id === suggested ? "Suggested" : "", m.name !== m.id ? m.id : ""].filter(Boolean);
    return { id: m.id, label: m.name, description: notes.join(" · ") || undefined };
  });
  if (!modelRequired(editing.value.slot)) {
    opts.unshift({ id: APP_DEFAULT, label: "App default", description: `Let ${props.appName} choose` });
  }
  return opts;
});
const selectedModel = computed(() => {
  const m = editing.value?.draft.model ?? "";
  return [m === "" ? APP_DEFAULT : m];
});
function pickModel(id: string) {
  if (editing.value) editing.value.draft.model = id === APP_DEFAULT ? "" : id;
}

// otherModel is the typed field under the model cards: a model id that is not
// in our list. Providers ship new models faster than the list changes, so the
// field is always there, never hidden behind "More". It shows the current
// model only when that model is not one of the cards.
const otherModel = computed({
  get: () => {
    const m = editing.value?.draft.model ?? "";
    return editingModels.value.some((x) => x.id === m) ? "" : m;
  },
  set: (v: string) => {
    if (editing.value) editing.value.draft.model = v;
  },
});

const inputClass =
  "block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 " +
  "outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 " +
  "focus:outline-accent sm:text-sm/6";
</script>

<template>
  <div class="space-y-4">
    <p class="text-sm text-muted-foreground">
      {{ appName }} uses an AI provider. Pick one and paste its key.
      <template v-if="slots.length > 1">You can add more than one.</template>
    </p>

    <OptionCards label="AI provider" :options="tileOptions" :selected="selectedTiles" multiple @pick="pick">
      <template #icon="{ option }">
        <span class="flex size-10 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <img
            v-if="logoOf(providerOf(option.id))"
            :src="logoOf(providerOf(option.id))"
            :alt="option.label"
            class="size-6 object-contain"
            @error="logoFailed(option.id)"
          />
          <component :is="fallbackIcon(providerOf(option.id))" v-else class="size-5 stroke-[1.5]" aria-hidden="true" />
        </span>
      </template>
    </OptionCards>

    <!-- Editor for the picked provider. -->
    <div v-if="editing" class="space-y-4 rounded-lg border border-border bg-card p-4">
      <div class="flex items-center gap-2.5">
        <span class="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <img
            v-if="logoOf(editing.provider)"
            :src="logoOf(editing.provider)"
            alt=""
            class="size-5 object-contain"
            @error="logoFailed(editing.provider.id)"
          />
          <component :is="fallbackIcon(editing.provider)" v-else class="size-4 stroke-[1.5]" aria-hidden="true" />
        </span>
        <h4 class="text-sm font-semibold text-foreground">{{ editing.provider.name }}</h4>
      </div>

      <p v-if="replaces" class="rounded-md bg-warning/10 px-3 py-2 text-sm text-warning">
        {{ appName }} takes only one provider of this kind. Saving replaces {{ replaces }}.
      </p>

      <div v-if="editing.slot.fields.api_key">
        <label for="ai-key" class="block text-sm/6 font-medium text-foreground">
          API key<span v-if="isOther(editing.provider)" class="font-normal text-muted-foreground"> (optional)</span>
        </label>
        <div class="mt-2">
          <input
            id="ai-key"
            v-model="editing.draft.key"
            type="password"
            autocomplete="new-password"
            :class="inputClass"
          />
        </div>
        <p v-if="keyWarning" class="mt-2 text-sm text-warning">
          Keys from {{ editing.provider.name }} usually start with {{ editing.provider.key_prefix }}. Check that you
          copied the whole key.
        </p>
        <p class="mt-2 text-sm text-muted-foreground">
          <template v-if="isOther(editing.provider)">Only needed if your server asks for one.</template>
          <template v-else>
            <template v-if="editing.provider.help">{{ editing.provider.help }} </template>
            <a
              v-if="isWebLink(editing.provider.key_url)"
              :href="editing.provider.key_url"
              target="_blank"
              rel="noopener noreferrer"
              class="inline-flex items-center gap-1 underline hover:text-foreground"
            >Get a key from {{ editing.provider.name }} <ExternalLink class="size-3.5" aria-hidden="true" /></a>
          </template>
        </p>
      </div>

      <div v-if="asksForUrl(editing.provider, editing.slot)">
        <label for="ai-url" class="block text-sm/6 font-medium text-foreground">Server address</label>
        <div class="mt-2">
          <input
            id="ai-url"
            v-model="editing.draft.baseUrl"
            type="url"
            placeholder="https://example.com/v1"
            autocomplete="off"
            :class="inputClass"
          />
        </div>
        <p class="mt-2 text-sm text-muted-foreground">
          The address of a server that speaks the OpenAI API. It usually ends in /v1.
        </p>
      </div>

      <div v-if="editing.slot.fields.model" class="space-y-2">
        <p class="text-sm/6 font-medium text-foreground">
          Model<span v-if="modelRequired(editing.slot)" class="text-destructive"> *</span>
        </p>
        <OptionCards
          v-if="editingModels.length > 0"
          label="Model"
          :options="modelOptions"
          :selected="selectedModel"
          @pick="pickModel"
        />
        <input
          v-model="otherModel"
          :aria-label="editingModels.length > 0 ? 'Other model' : 'Model'"
          :placeholder="editingModels.length > 0 ? 'Or type another model name' : 'Model name'"
          autocomplete="off"
          :class="inputClass"
        />
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <Button size="sm" :disabled="!canSave" @click="save">{{ isAdded ? "Save" : "Add" }}</Button>
        <Button v-if="isAdded" size="sm" variant="secondary" @click="remove">Remove</Button>
        <Button size="sm" variant="ghost" @click="editing = null">Cancel</Button>
      </div>
    </div>
  </div>
</template>
