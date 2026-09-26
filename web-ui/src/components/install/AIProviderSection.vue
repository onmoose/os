<script setup lang="ts">
// The AI providers row of the install setup page (INSTALL_SETUP.md # 6). The
// user picks a provider tile, then one of their accounts for that provider (or
// adds one right here), then the models, and saves. Saving fills the slot the
// provider lands in; the parent sends it as a binding, and the brain fills the
// app's fields from the account. Which slot a provider lands in, and which
// tiles show at all, comes from aiProviders.ts.
//
// An app can take several providers at once (Anthropic and OpenAI in openclaw),
// one per slot. A compatible slot holds one provider, so saving a second one
// there replaces the first, and the editor says so before it happens.
//
// Adding an account needs no password re-prompt, like an email account. The
// account belongs to the user who adds it, and the list shows only their own.
import { computed, ref, watch } from "vue";
import { useMutation, useQuery, useQueryClient } from "@tanstack/vue-query";
import { Bot, ExternalLink, KeyRound, Plus, Server } from "lucide-vue-next";
import { api, ApiError, type AIAccount, type AIAccountBody, type AIProvider } from "../../api";
import Button from "../ui/Button.vue";
import OptionCards, { type Option } from "./OptionCards.vue";
import {
  withOther,
  findProvider,
  isOther,
  accountProviderId,
  accountsFor,
  modelsOfType,
  suggestedModels,
  modelProblem,
  modelIdProblem,
  keyLooksWrong,
  slotFor,
  choiceComplete,
  type AIChoice,
  type AISlot,
  type ModelSetting,
} from "../../aiProviders";

const props = defineProps<{ slots: AISlot[]; providers: AIProvider[]; appName: string }>();
// choices is keyed by slot id.
const choices = defineModel<Record<string, AIChoice>>({ required: true });

const qc = useQueryClient();

// The caller's own AI accounts. A failed read shows an empty list with the
// add form, so the user can still go on.
const accountsQuery = useQuery({
  queryKey: ["ai-accounts"],
  queryFn: () => api.get<{ accounts: AIAccount[] | null }>("/ai-accounts"),
  refetchOnWindowFocus: false,
});
const accounts = computed(() => accountsQuery.data.value?.accounts ?? []);

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
    const first = c ? Object.values(c.models).flat()[0] : undefined;
    return {
      id: p.id,
      label: p.name,
      description: c ? (first ? `Added, uses ${modelName(p, first)}` : "Added") : undefined,
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
  const own = accountsFor(provider, accounts.value);
  const draft: AIChoice =
    current?.provider === id
      ? { ...current, models: { ...current.models } }
      : {
          provider: id,
          // A sole account is the obvious intent, so it starts chosen.
          accountId: own.length === 1 ? (own[0]?.id ?? "") : "",
          models: suggestedModels(provider, slot),
        };
  editing.value = { slot, provider, draft };
  typed.value = {};
  cancelAdd();
  openAddIfNone();
}

// With no account yet, the add form is the only way on, so it starts open.
// That needs a list known to be empty: while it loads, or after it failed, the
// user may well have accounts, so the form waits (they can still open it).
function openAddIfNone() {
  if (!editing.value || !accountsQuery.isSuccess.value) return;
  // A form already open keeps what the user typed in it.
  if (editingAccounts.value.length === 0) {
    if (!adding.value) startAdd(editing.value.provider);
  }
  else if (autoAdd.value) cancelAdd();
}

// When the list arrives after the editor opened, open the form if it is
// empty, close a form this opened on its own if it is not, and preselect a
// sole account as pick() would have.
watch(
  () => accountsQuery.data.value,
  () => {
    const d = editing.value?.draft;
    if (!d) return;
    if (!d.accountId && editingAccounts.value.length === 1) d.accountId = editingAccounts.value[0]?.id ?? "";
    openAddIfNone();
  },
);

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

// withPending is the draft's models plus any id still typed in a list's text
// box but not yet added, so Save does not drop it.
function withPending(e: Editing): Record<string, string[]> {
  const out = { ...e.draft.models };
  for (const m of e.slot.models) {
    const id = (typed.value[m.key] ?? "").trim();
    if (m.multiple && id !== "" && !(out[m.key] ?? []).includes(id)) out[m.key] = [...(out[m.key] ?? []), id];
  }
  return out;
}

const canSave = computed(() => {
  const e = editing.value;
  if (!e) return false;
  return choiceComplete(e.slot, { ...e.draft, models: withPending(e) }, e.provider);
});

function save() {
  if (!editing.value || !canSave.value) return;
  const d = { ...editing.value.draft, models: withPending(editing.value) };
  typed.value = {};
  choices.value = { ...choices.value, [editing.value.slot.id]: d };
  editing.value = null;
  cancelAdd();
}

function remove() {
  if (!editing.value) return;
  const next = { ...choices.value };
  delete next[editing.value.slot.id];
  choices.value = next;
  editing.value = null;
  cancelAdd();
}

// ── Account picker ──────────────────────────────────────────────────────────
const editingAccounts = computed(() =>
  editing.value ? accountsFor(editing.value.provider, accounts.value) : [],
);
const accountOptions = computed<Option[]>(() =>
  editingAccounts.value.map((a) => ({
    id: a.id,
    label: a.label,
    description: a.base_url || (a.key_set ? "Key saved" : "No key"),
  })),
);
function pickAccount(id: string) {
  if (editing.value) editing.value.draft.accountId = id;
}

// ── Inline add ──────────────────────────────────────────────────────────────
const adding = ref(false);
// autoAdd: the form was opened by the page (an empty list), not by the user.
const autoAdd = ref(false);
const addLabel = ref("");
const addKey = ref("");
const addUrl = ref("");
const addError = ref("");

function startAdd(p: AIProvider, byUser = false) {
  adding.value = true;
  autoAdd.value = !byUser;
  // The provider's name is a fine first name for a first account.
  addLabel.value = accountsFor(p, accounts.value).length === 0 && !isOther(p) ? p.name : "";
  addKey.value = "";
  addUrl.value = "";
  addError.value = "";
}

function cancelAdd() {
  adding.value = false;
  autoAdd.value = false;
  addError.value = "";
}

const keyWarning = computed(() => !!editing.value && keyLooksWrong(editing.value.provider, addKey.value));

// The brain checks every rule again; this only keeps Add off until the form
// can pass. Other needs an address and no key; a listed provider a key.
const addValid = computed(() => {
  if (!editing.value || addLabel.value.trim() === "") return false;
  if (isOther(editing.value.provider)) return /^https?:\/\/\S+$/.test(addUrl.value.trim());
  return addKey.value.trim() !== "";
});

const create = useMutation({
  mutationFn: (body: AIAccountBody) => api.post<AIAccount>("/ai-accounts", body),
  onSuccess: async (created) => {
    await qc.invalidateQueries({ queryKey: ["ai-accounts"] });
    if (editing.value) editing.value.draft.accountId = created.id;
    cancelAdd();
  },
  onError: (e) => {
    addError.value = e instanceof ApiError ? e.message : "Something went wrong.";
  },
});

function addAccount() {
  if (!editing.value || !addValid.value) return;
  const body: AIAccountBody = {
    provider_id: accountProviderId(editing.value.provider),
    label: addLabel.value.trim(),
  };
  if (addKey.value.trim()) body.api_key = addKey.value.trim();
  if (addUrl.value.trim()) body.base_url = addUrl.value.trim();
  create.mutate(body);
}

// ── Model pickers ───────────────────────────────────────────────────────────
// One picker per model setting the slot declares: single choice for
// model.<type>, several for models.<type>. The cards are the provider's models
// of that type, suggested one first. A typed id is always possible, because
// providers ship new models faster than the list changes.

// typed holds the text box under each picker, by setting key.
const typed = ref<Record<string, string>>({});

function idsOf(m: ModelSetting): string[] {
  return editing.value?.draft.models[m.key] ?? [];
}

function setIds(m: ModelSetting, ids: string[]) {
  if (editing.value) editing.value.draft.models = { ...editing.value.draft.models, [m.key]: ids };
}

function modelOptions(m: ModelSetting): Option[] {
  if (!editing.value) return [];
  const p = editing.value.provider;
  const suggested = p.defaults?.[m.type];
  const opts: Option[] = modelsOfType(p, m.type).map((x) => {
    const notes = [x.id === suggested ? "Suggested" : "", x.name !== x.id ? x.id : ""].filter(Boolean);
    return { id: x.id, label: x.name, description: notes.join(" · ") || undefined };
  });
  // Typed ids in a list show as cards too, so they can be taken out again. For
  // Other, which has no list, they are the only cards.
  if (m.multiple) {
    for (const id of idsOf(m)) if (!opts.some((o) => o.id === id)) opts.push({ id, label: id });
  }
  return opts;
}

function pickModel(m: ModelSetting, id: string) {
  const ids = idsOf(m);
  if (!m.multiple) {
    setIds(m, [id]);
    typed.value = { ...typed.value, [m.key]: "" };
    return;
  }
  setIds(m, ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id]);
}

// For a single model, the text box is the choice itself when it holds a model
// that is not a card. For a list, it adds one id at a time.
function typedValue(m: ModelSetting): string {
  if (m.multiple) return typed.value[m.key] ?? "";
  const id = idsOf(m)[0] ?? "";
  return modelOptions(m).some((o) => o.id === id) ? "" : id;
}

function onTyped(m: ModelSetting, v: string) {
  if (m.multiple) typed.value = { ...typed.value, [m.key]: v };
  else setIds(m, v.trim() === "" ? [] : [v]);
}

function addTyped(m: ModelSetting) {
  const id = (typed.value[m.key] ?? "").trim();
  if (id === "" || typedProblem(m)) return;
  if (!idsOf(m).includes(id)) setIds(m, [...idsOf(m), id]);
  typed.value = { ...typed.value, [m.key]: "" };
}

// typedProblem checks the text box of a list before its id is added.
function typedProblem(m: ModelSetting): string {
  return m.multiple ? modelIdProblem((typed.value[m.key] ?? "").trim(), m.separator) : "";
}

function problemOf(m: ModelSetting): string {
  return editing.value ? modelProblem(m, idsOf(m), editing.value.provider) : "";
}

const inputClass =
  "block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 " +
  "outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 " +
  "focus:outline-accent sm:text-sm/6";
</script>

<template>
  <div class="space-y-4">
    <p class="text-sm text-muted-foreground">
      {{ appName }} uses an AI provider. Pick one, then pick or add your account for it.
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
    <div v-if="editing" class="space-y-5 rounded-lg border border-border bg-card p-4">
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

      <!-- Which account. -->
      <div class="space-y-2">
        <p class="text-sm/6 font-medium text-foreground">Account</p>
        <p v-if="accountsQuery.isPending.value && !accountsQuery.isError.value" class="text-sm text-muted-foreground">
          Loading your accounts…
        </p>
        <div v-else-if="accountsQuery.isError.value" class="flex flex-wrap items-center gap-2">
          <p class="text-sm text-destructive">Could not load your accounts.</p>
          <Button size="sm" variant="secondary" @click="accountsQuery.refetch()">Try again</Button>
          <Button v-if="!adding" size="sm" variant="ghost" @click="startAdd(editing.provider, true)">
            Add an account
          </Button>
        </div>
        <OptionCards
          v-else-if="editingAccounts.length > 0"
          label="Account"
          :options="accountOptions"
          :selected="editing.draft.accountId ? [editing.draft.accountId] : []"
          @pick="pickAccount"
        >
          <template #icon>
            <span class="flex size-10 items-center justify-center rounded-lg bg-muted text-muted-foreground">
              <KeyRound class="size-5 stroke-[1.5]" aria-hidden="true" />
            </span>
          </template>
          <template v-if="!adding" #extra>
            <button
              type="button"
              class="flex w-full cursor-pointer items-center gap-3 rounded-lg border border-dashed border-border px-4 py-3.5 text-left hover:border-olive-400 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
              @click="startAdd(editing.provider, true)"
            >
              <span class="flex size-10 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
                <Plus class="size-5" aria-hidden="true" />
              </span>
              <span class="text-sm font-medium text-foreground">Add an account</span>
            </button>
          </template>
        </OptionCards>

        <!-- Inline add: the account is saved for the user, then picked. -->
        <div v-if="adding" class="space-y-4 rounded-md border border-border p-4">
          <p class="text-sm text-muted-foreground">
            <template v-if="isOther(editing.provider)">
              Add a server that speaks the OpenAI API. It is saved as your account, so other apps can use it too.
            </template>
            <template v-else>
              Add your {{ editing.provider.name }} account. The key is saved on this box and never shown again.
            </template>
          </p>

          <div>
            <label for="ai-add-label" class="block text-sm/6 font-medium text-foreground">Account name</label>
            <input id="ai-add-label" v-model="addLabel" autocomplete="off" :class="[inputClass, 'mt-2']" />
            <p class="mt-2 text-sm text-muted-foreground">What you'll see when an app asks which account to use.</p>
          </div>

          <div v-if="isOther(editing.provider)">
            <label for="ai-add-url" class="block text-sm/6 font-medium text-foreground">Server address</label>
            <input
              id="ai-add-url"
              v-model="addUrl"
              type="url"
              placeholder="https://example.com/v1"
              autocomplete="off"
              :class="[inputClass, 'mt-2']"
            />
            <p class="mt-2 text-sm text-muted-foreground">It usually ends in /v1.</p>
          </div>

          <div>
            <label for="ai-add-key" class="block text-sm/6 font-medium text-foreground">
              API key<span v-if="isOther(editing.provider)" class="font-normal text-muted-foreground"> (optional)</span>
            </label>
            <input
              id="ai-add-key"
              v-model="addKey"
              type="password"
              autocomplete="new-password"
              :class="[inputClass, 'mt-2']"
            />
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

          <div class="flex flex-wrap items-center gap-2">
            <Button size="sm" :disabled="create.isPending.value || !addValid" @click="addAccount">
              {{ create.isPending.value ? "Adding…" : "Add account" }}
            </Button>
            <Button
              v-if="editingAccounts.length > 0 || !accountsQuery.isSuccess.value"
              size="sm"
              variant="ghost"
              @click="cancelAdd"
            >
              Cancel
            </Button>
          </div>
          <p v-if="addError" class="text-sm text-destructive">{{ addError }}</p>
        </div>
      </div>

      <!-- Models, one picker per model setting the app declares. -->
      <div v-for="m in editing.slot.models" :key="m.key" class="space-y-2">
        <p class="text-sm/6 font-medium text-foreground">
          {{ m.field.title }}
          <span v-if="m.multiple" class="font-normal text-muted-foreground"> (pick one or more)</span>
        </p>
        <OptionCards
          v-if="modelOptions(m).length > 0"
          :label="m.field.title"
          :options="modelOptions(m)"
          :selected="idsOf(m)"
          :multiple="m.multiple"
          @pick="(id) => pickModel(m, id)"
        />
        <div class="flex gap-2">
          <input
            :value="typedValue(m)"
            :aria-label="modelOptions(m).length > 0 ? `Other model for ${m.field.title}` : m.field.title"
            :placeholder="modelOptions(m).length > 0 ? 'Or type another model name' : 'Model name'"
            autocomplete="off"
            :class="inputClass"
            @input="onTyped(m, ($event.target as HTMLInputElement).value)"
            @keydown.enter.prevent="m.multiple && addTyped(m)"
          />
          <Button
            v-if="m.multiple"
            size="sm"
            variant="secondary"
            :disabled="!(typed[m.key] ?? '').trim() || !!typedProblem(m)"
            @click="addTyped(m)"
          >
            Add
          </Button>
        </div>
        <p v-if="typedProblem(m)" class="text-sm text-destructive">{{ typedProblem(m) }}</p>
        <p v-if="problemOf(m) && editing.draft.accountId" class="text-sm text-muted-foreground">{{ problemOf(m) }}</p>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <Button size="sm" :disabled="!canSave" @click="save">{{ isAdded ? "Save" : "Add" }}</Button>
        <Button v-if="isAdded" size="sm" variant="secondary" @click="remove">Remove</Button>
        <Button size="sm" variant="ghost" @click="editing = null; cancelAdd()">Cancel</Button>
      </div>
    </div>
  </div>
</template>
