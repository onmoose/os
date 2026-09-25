<script setup lang="ts">
// The AI providers row of the install setup page (INSTALL_SETUP.md # 6). The
// user picks a provider tile, types its key (and a model or URL where the app
// has a field for it), and saves. Saving fills every field of the slot the
// provider lands in; the parent turns the choices into app_env values with
// fieldValues(). Which slot a provider lands in, and which tiles show at all,
// comes from aiProviders.ts, which is temporary until manifest roles land.
//
// An app can take several providers at once (Anthropic and OpenAI in openclaw),
// one per slot. A compatible slot holds one provider, so saving a second one
// there replaces the first, and the editor says so before it happens.
import { computed, ref } from "vue";
import { ExternalLink } from "lucide-vue-next";
import Button from "../ui/Button.vue";
import OptionCards, { type Option } from "./OptionCards.vue";
import {
  PROVIDERS,
  slotFor,
  asksForUrl,
  modelRequired,
  choiceComplete,
  type AIChoice,
  type AIProvider,
  type AISlot,
} from "../../aiProviders";

const props = defineProps<{ slots: AISlot[]; appName: string }>();
// choices is keyed by slot id.
const choices = defineModel<Record<string, AIChoice>>({ required: true });

// Only providers this app can use get a tile.
const offered = computed(() => PROVIDERS.filter((p) => slotFor(p, props.slots)));

function providerOf(id: string): AIProvider | undefined {
  return PROVIDERS.find((p) => p.id === id);
}

const added = computed(() => Object.values(choices.value).map((c) => c.provider));

const tileOptions = computed(() =>
  offered.value.map((p) => {
    const c = Object.values(choices.value).find((x) => x.provider === p.id);
    return {
      id: p.id,
      label: p.label,
      description: c ? (c.model ? `Added, uses ${c.model}` : "Added") : undefined,
    };
  }),
);

// ── Editor ──────────────────────────────────────────────────────────────────
type Editing = { slot: AISlot; provider: AIProvider; draft: AIChoice };
const editing = ref<Editing | null>(null);

function pick(id: string) {
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
          // A required model starts on the provider's first suggestion. An
          // optional one starts blank, which leaves the app on its default.
          model: slot.fields.model && modelRequired(slot) ? (provider.models[0] ?? "") : "",
        };
  editing.value = { slot, provider, draft };
}

// replaces names the provider a save would push out of a shared slot.
const replaces = computed(() => {
  if (!editing.value) return null;
  const current = choices.value[editing.value.slot.id];
  if (!current || current.provider === editing.value.provider.id) return null;
  return providerOf(current.provider)?.label ?? null;
});

const isAdded = computed(
  () => !!editing.value && choices.value[editing.value.slot.id]?.provider === editing.value.provider.id,
);

const canSave = computed(() => !!editing.value && choiceComplete(editing.value.slot, editing.value.draft));

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

// The model picker uses a sentinel id for "the app's own default", because the
// real value for it is an empty string.
const APP_DEFAULT = "__app_default";
const modelOptions = computed<Option[]>(() => {
  if (!editing.value) return [];
  const opts: Option[] = editing.value.provider.models.map((m) => ({ id: m, label: m }));
  // A typed model stays visible as a card of its own once chosen.
  const m = editing.value.draft.model;
  if (m && !opts.some((o) => o.id === m)) opts.push({ id: m, label: m });
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

    <OptionCards label="AI provider" :options="tileOptions" :selected="added" multiple @pick="pick">
      <template #icon="{ option }">
        <span class="flex size-10 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <img
            v-if="providerOf(option.id)?.logo"
            :src="providerOf(option.id)!.logo"
            :alt="option.label"
            class="size-6 object-contain"
          />
          <component :is="providerOf(option.id)?.icon" v-else class="size-5 stroke-[1.5]" aria-hidden="true" />
        </span>
      </template>
    </OptionCards>

    <!-- Editor for the picked provider. -->
    <div v-if="editing" class="space-y-4 rounded-lg border border-border bg-card p-4">
      <div class="flex items-center gap-2.5">
        <span class="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <component :is="editing.provider.icon" class="size-4 stroke-[1.5]" aria-hidden="true" />
        </span>
        <h4 class="text-sm font-semibold text-foreground">{{ editing.provider.label }}</h4>
      </div>

      <p v-if="replaces" class="rounded-md bg-warning/10 px-3 py-2 text-sm text-warning">
        {{ appName }} takes only one provider of this kind. Saving replaces {{ replaces }}.
      </p>

      <div v-if="editing.slot.fields.api_key">
        <label for="ai-key" class="block text-sm/6 font-medium text-foreground">
          API key<span v-if="!editing.provider.needsKey" class="font-normal text-muted-foreground"> (optional)</span>
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
        <p class="mt-2 text-sm text-muted-foreground">
          <a
            v-if="editing.provider.keyUrl"
            :href="editing.provider.keyUrl"
            target="_blank"
            rel="noopener noreferrer"
            class="inline-flex items-center gap-1 underline hover:text-foreground"
          >Get a key from {{ editing.provider.label }} <ExternalLink class="size-3.5" aria-hidden="true" /></a>
          <template v-else>Only needed if your server asks for one.</template>
        </p>
      </div>

      <div v-if="asksForUrl(editing.provider, editing.slot)">
        <label for="ai-url" class="block text-sm/6 font-medium text-foreground">Server address</label>
        <div class="mt-2">
          <input
            id="ai-url"
            v-model="editing.draft.baseUrl"
            type="url"
            :placeholder="editing.provider.id === 'ollama' ? 'http://192.168.1.20:11434/v1' : 'https://example.com/v1'"
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
          v-if="editing.provider.models.length > 0"
          label="Model"
          :options="modelOptions"
          :selected="selectedModel"
          allow-typed
          @pick="pickModel"
        />
        <input
          v-else
          v-model="editing.draft.model"
          aria-label="Model"
          placeholder="Model name, for example llama3.2"
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
