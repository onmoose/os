<script setup lang="ts">
// "Your Anthropic key" (INSTALL_STEPS.md # 3): the form for a new key. It has
// no Save of its own. The page's Continue calls save(), which saves the key
// as the user's account and returns the account, and the page then picks it.
// A key typed here stays in this component's memory until then: it never
// reaches the URL or session storage.
//
// For a listed service, top to bottom: the password box with the soft prefix
// warning, the optional name (a quiet underlined box; empty means the default
// name), the household line on a household install, and at the bottom the
// numbered steps from the provider data (`key_url` and `help`, with an "Open
// <service>" button) folded under "Where do I find my API key?". There is no
// cost line: what a service costs depends too much on the model. For My own
// server:
// the address, an optional key, and a model name box for each model type the
// app's slot needs, saved on the account, since such a server has no model
// list.
import { computed, ref, watch } from "vue";
import { useMutation, useQueryClient } from "@tanstack/vue-query";
import { ExternalLink } from "lucide-vue-next";
import { api, ApiError, type AIAccount, type AIAccountBody, type AIProvider } from "../../api";
import { accountProviderId, isOther, keyLooksWrong, modelIdProblem, type AISlot } from "../../aiProviders";
import { defaultKeyLabel } from "../../installSteps";
import Button from "../ui/Button.vue";

// The same form adds an account in Settings → Integrations → AI services.
// There it has no app: no slot, no app name and no household line, and My
// own server asks one model name, for chat.
//
// With `account`, the form edits that saved account instead (a My own server
// account saved before its model names were kept on it): the address and name
// are filled, an empty key box keeps the stored key, and the model names the
// slot needs are asked.
const props = defineProps<{
  service: AIProvider;
  aiSlot?: AISlot;
  account?: AIAccount;
  appName?: string;
  household?: boolean;
  // labels are the names of the user's accounts, so the default name is free.
  labels: string[];
  // only: this is the one service the app works with, so there was no grid.
  only?: boolean;
}>();

// modelTypes are the model types My own server is asked a name for, saved on
// the account (its `models`, by type): the ones the app's slot needs, or chat
// in Settings. A server has no model list, so the name is the only way to
// pick one; the install flow then never asks again.
const modelTypes = computed(() =>
  props.aiSlot ? [...new Set(props.aiSlot.models.map((m) => m.type))] : ["chat"],
);
// separatorOf is the list separator a type's name must not hold, when the
// slot takes a list of that type.
function separatorOf(type: string): string | undefined {
  return props.aiSlot?.models.find((m) => m.type === type && m.multiple)?.separator;
}
function modelTitle(type: string): string {
  if (modelTypes.value.length === 1) return "Model name";
  return props.aiSlot?.models.find((m) => m.type === type)?.field.title ?? `Model name (${type})`;
}

const qc = useQueryClient();

const key = ref("");
const address = ref("");
const name = ref("");
const models = ref<Record<string, string>>({});
const error = ref("");

// A new service (or account) starts the form fresh; an account being edited
// fills it.
watch(
  () => [props.service.id, props.account?.id],
  () => {
    key.value = "";
    address.value = props.account?.base_url ?? "";
    name.value = props.account?.label ?? "";
    models.value = Object.fromEntries(
      Object.entries(props.account?.models ?? {}).map(([t, ids]) => [t, (ids ?? [])[0] ?? ""]),
    );
    error.value = "";
  },
  { immediate: true },
);

const other = computed(() => isOther(props.service));
const defaultName = computed(() =>
  defaultKeyLabel(other.value ? "My server" : `${props.service.name} key`, props.labels),
);
const keyWarning = computed(() => keyLooksWrong(props.service, key.value));
const keyLink = computed(() => (props.service.key_url && /^https?:\/\//i.test(props.service.key_url) ? props.service.key_url : ""));

function modelProblemOf(type: string): string {
  return modelIdProblem((models.value[type] ?? "").trim(), separatorOf(type));
}

// valid keeps Continue off until the brain can accept the form. The brain
// checks every rule again.
const valid = computed(() => {
  if (other.value) {
    if (!/^https?:\/\/\S+$/.test(address.value.trim())) return false;
    return modelTypes.value.every((t) => (models.value[t] ?? "").trim() !== "" && !modelProblemOf(t));
  }
  return key.value.trim() !== "";
});

const create = useMutation({
  mutationFn: (body: AIAccountBody) =>
    props.account
      ? api.put<AIAccount>(`/ai-accounts/${encodeURIComponent(props.account.id)}`, body)
      : api.post<AIAccount>("/ai-accounts", body),
});

// save stores the key as the user's account (or saves the account being
// edited), with a My own server's model names on it. It returns the account,
// or null when it failed (the form then shows why).
async function save(): Promise<{ account: AIAccount } | null> {
  if (!valid.value || create.isPending.value) return null;
  error.value = "";
  const body: AIAccountBody = {
    provider_id: accountProviderId(props.service),
    label: name.value.trim() || defaultName.value,
  };
  // On an edit an empty key keeps the stored one.
  if (key.value.trim()) body.api_key = key.value.trim();
  if (other.value) {
    body.base_url = address.value.trim();
    const own: Record<string, string[]> = {};
    for (const [t, ids] of Object.entries(props.account?.models ?? {})) if (ids?.length) own[t] = [...ids];
    for (const t of modelTypes.value) own[t] = [(models.value[t] ?? "").trim()];
    body.models = own;
  }
  try {
    const created = await create.mutateAsync(body);
    // Put the new account in the list at once, so the page can pick it even
    // if the refetch below is slow.
    qc.setQueryData<{ accounts: AIAccount[] | null }>(["ai-accounts"], (old) => ({
      accounts: [...(old?.accounts ?? []).filter((a) => a.id !== created.id), created],
    }));
    void qc.invalidateQueries({ queryKey: ["ai-accounts"] });
    key.value = "";
    return { account: created };
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : "The key could not be saved. Try again.";
    return null;
  }
}

defineExpose({ valid, save, pending: create.isPending });

const inputClass =
  "mt-2 block w-full rounded-md bg-card px-3 py-1.5 text-base text-foreground outline-1 -outline-offset-1 " +
  "outline-border placeholder:text-muted-foreground focus:outline-2 focus:-outline-offset-2 " +
  "focus:outline-accent sm:text-sm/6";
</script>

<template>
  <div class="space-y-6">
    <p v-if="only && appName" class="text-sm text-foreground">{{ appName }} works with {{ service.name }}.</p>

    <!-- My own server -->
    <template v-if="other">
      <p class="text-sm text-muted-foreground">
        Use a server that speaks the OpenAI API, on your network or on the internet.
      </p>
      <div>
        <label for="ai-key-address" class="block text-sm/6 font-medium text-foreground">Server address</label>
        <input
          id="ai-key-address"
          v-model="address"
          type="url"
          placeholder="https://example.com/v1"
          autocomplete="off"
          :class="inputClass"
        />
        <p class="mt-2 text-sm text-muted-foreground">It usually ends in /v1.</p>
      </div>
      <div>
        <label for="ai-key-secret" class="block text-sm/6 font-medium text-foreground">
          Key <span class="font-normal text-muted-foreground">(optional)</span>
        </label>
        <input id="ai-key-secret" v-model="key" type="password" autocomplete="new-password" :class="inputClass" />
        <p class="mt-2 text-sm text-muted-foreground">
          {{ account ? "Leave it empty to keep the saved key." : "Only needed if your server asks for one." }}
        </p>
      </div>
      <div v-for="t in modelTypes" :key="t">
        <label :for="`ai-key-model-${t}`" class="block text-sm/6 font-medium text-foreground">
          {{ modelTitle(t) }}
        </label>
        <input :id="`ai-key-model-${t}`" v-model="models[t]" autocomplete="off" :class="inputClass" />
        <p v-if="modelProblemOf(t)" class="mt-2 text-sm text-destructive">{{ modelProblemOf(t) }}</p>
        <p v-else class="mt-2 text-sm text-muted-foreground">
          The name your server gives the model. It is saved with the server, so apps do not ask again.
        </p>
      </div>
    </template>

    <!-- A listed service: the key box first. -->
    <template v-else>
      <div>
        <label for="ai-key-secret" class="block text-sm/6 font-medium text-foreground">{{ service.name }} key</label>
        <input
          id="ai-key-secret"
          v-model="key"
          type="password"
          autocomplete="new-password"
          :class="[inputClass, 'py-2.5 text-base']"
        />
        <p v-if="keyWarning" class="mt-2 text-sm text-warning">
          Keys from {{ service.name }} usually start with {{ service.key_prefix }}. Check that you copied the whole key.
        </p>
      </div>
    </template>

    <!-- The name: always there, a quiet underlined box. Empty means the
         default name ("Anthropic key"). -->
    <input
      id="ai-key-name"
      v-model="name"
      :placeholder="other ? 'Name this server (optional)' : 'Name this key (optional)'"
      :aria-label="other ? 'Name this server (optional)' : 'Name this key (optional)'"
      autocomplete="off"
      class="block w-full border-0 border-b border-border bg-transparent px-0 py-1.5 text-base text-foreground placeholder:text-muted-foreground focus:border-accent focus:outline-none sm:text-sm/6"
    />

    <p v-if="household && appName" class="text-sm text-muted-foreground">
      Your key pays for everyone at home who uses {{ appName }}.
    </p>

    <!-- The steps, folded, at the bottom: most users only need the box. -->
    <details v-if="!other" class="rounded-md border border-border px-4 py-3">
      <summary class="cursor-pointer text-sm font-medium text-foreground">Where do I find my API key?</summary>
      <ol class="mt-3 list-decimal space-y-3 pl-5 text-sm text-foreground marker:text-muted-foreground">
        <li>
          <span>Open {{ service.name }} and sign in, or make an account.</span>
          <div v-if="keyLink" class="mt-2">
            <Button size="sm" variant="secondary" as="a" :href="keyLink" target="_blank" rel="noopener noreferrer">
              Open {{ service.name }} <ExternalLink class="size-3.5" aria-hidden="true" />
            </Button>
          </div>
        </li>
        <li v-if="service.help">{{ service.help }}</li>
        <li>Copy the key, and paste it into the {{ service.name }} key box.</li>
      </ol>
    </details>

    <p v-if="error" class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive" role="alert">{{ error }}</p>
  </div>
</template>
