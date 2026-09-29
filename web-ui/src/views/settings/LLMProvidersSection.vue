<script setup lang="ts">
// Settings → Integrations → AI services: the signed-in user's own AI
// provider accounts (INSTALL_SETUP.md # 5 and piece 4, SERVICE_PROVISIONING.md
// # AI provider accounts). Every user has this screen, and it lists only the
// accounts they added. The UI says "AI service"; the API says ai-accounts.
//
// A row shows the account, its provider, and the apps that use it (used_by).
// Add and edit need no password re-prompt: the account is the user's own.
// Delete keeps it (withElevation), because it cannot be undone.
//
// An account reaches the apps that use it. Saving a new key or address
// updates and restarts them, and deleting the account takes the key out of
// them and restarts them; the brain does that in a job whose id it returns.
// The edit form and the delete confirmation name those apps before the user
// confirms (DECISIONS.md 2026-09-26). A name change restarts nothing. The
// provider of an account cannot be changed here: the user adds a new account
// instead, because the apps' slots may not fit the other provider.
import { computed, ref } from "vue";
import { useMutation, useQuery, useQueryClient } from "@tanstack/vue-query";
import { ArrowLeft, ExternalLink, Plus } from "lucide-vue-next";
import {
  api,
  appNames,
  waitForJobOk,
  type AccountDeleted,
  type AIAccount,
  type AIAccountBody,
  type AIAccountSaved,
  type AIProvider,
} from "@/api";
import { withElevation } from "@/elevate";
import { COMPATIBLE, OTHER, accountProviderId, findProvider, isOther, keyLooksWrong, withOther } from "@/aiProviders";
import { errorMessage, fieldClass } from "@/mailProviderForm";
import Button from "@/components/ui/Button.vue";
import AIProviderLogo from "@/components/AIProviderLogo.vue";
import OptionCards from "@/components/install/OptionCards.vue";

const qc = useQueryClient();

const accountsQuery = useQuery({
  queryKey: ["ai-accounts"],
  queryFn: () => api.get<{ accounts: AIAccount[] | null }>("/ai-accounts"),
});
const accounts = computed(() => accountsQuery.data.value?.accounts ?? []);
const isEmpty = computed(() => accountsQuery.isSuccess.value && accounts.value.length === 0);

// The provider list comes from the catalog. Without it only "Other" can be
// added, and a row names its provider by id.
const providersQuery = useQuery({
  queryKey: ["ai-providers"],
  queryFn: () => api.get<{ providers: AIProvider[] | null }>("/ai-providers"),
  staleTime: 5 * 60_000,
  retry: 1,
});
const providers = computed(() => providersQuery.data.value?.providers ?? []);

// providerOf is the tile an account belongs to: Other for openai_compatible,
// else the listed provider, or undefined when it left the provider data.
function providerOf(a: AIAccount): AIProvider | undefined {
  return a.provider_id === COMPATIBLE ? OTHER : findProvider(providers.value, a.provider_id);
}

function providerName(a: AIAccount): string {
  return providerOf(a)?.name ?? a.provider_id;
}

function detailLine(a: AIAccount): string {
  const parts = [providerName(a)];
  if (a.base_url) parts.push(a.base_url);
  else if (!a.key_set) parts.push("No key");
  return parts.join(" · ");
}

// ── Notices ─────────────────────────────────────────────────────────────────
const rowError = ref<Record<string, string>>({});
const rowNotice = ref<Record<string, string>>({});
const pageNotice = ref("");

function setRow(map: typeof rowError, id: string, msg: string) {
  const next = { ...map.value };
  if (msg) next[id] = msg;
  else delete next[id];
  map.value = next;
}

// followJob waits for the job that updates the apps and says how it went.
// The account change is already saved, so a failure here is about the apps.
async function followJob(jobId: string, count: number, say: (msg: string) => void) {
  say(count === 1 ? "Updating the app that uses this account…" : "Updating the apps that use this account…");
  try {
    await waitForJobOk(jobId);
    say(count === 1 ? "The app was updated and restarted." : "The apps were updated and restarted.");
  } catch (e) {
    say(`Saved, but ${e instanceof Error ? e.message : "some apps could not be updated"}.`);
  } finally {
    qc.invalidateQueries({ queryKey: ["apps"] });
    qc.invalidateQueries({ queryKey: ["app-config"] });
  }
}

// ── Add ─────────────────────────────────────────────────────────────────────
// Two steps on one screen: pick the provider, then fill in the account.
const adding = ref(false);
const addProvider = ref<AIProvider | null>(null);
const addLabel = ref("");
const addKey = ref("");
const addUrl = ref("");
const addError = ref("");

const tiles = computed(() => withOther(providers.value).map((p) => ({ id: p.id, label: p.name })));

function startAdd() {
  adding.value = true;
  addProvider.value = null;
  editFor.value = null;
  confirmDeleteFor.value = null;
}

function pickAddProvider(id: string) {
  const p = findProvider(providers.value, id);
  if (!p) return;
  addProvider.value = p;
  // The provider's name is a fine first name for a first account of it.
  const has = accounts.value.some((a) => a.provider_id === accountProviderId(p));
  addLabel.value = !isOther(p) && !has ? p.name : "";
  addKey.value = "";
  addUrl.value = "";
  addError.value = "";
}

function cancelAdd() {
  adding.value = false;
  addProvider.value = null;
  addError.value = "";
}

// The brain checks every rule again; this only keeps Add off until the form
// can pass. Other needs an address and no key; a listed provider a key.
const addValid = computed(() => {
  const p = addProvider.value;
  if (!p || addLabel.value.trim() === "") return false;
  if (isOther(p)) return /^https?:\/\/\S+$/.test(addUrl.value.trim());
  return addKey.value.trim() !== "";
});

const create = useMutation({
  mutationFn: (body: AIAccountBody) => api.post<AIAccount>("/ai-accounts", body),
  onSuccess: () => {
    qc.invalidateQueries({ queryKey: ["ai-accounts"] });
    cancelAdd();
  },
  onError: (e) => (addError.value = errorMessage(e)),
});

function submitAdd() {
  const p = addProvider.value;
  if (!p || !addValid.value) return;
  const body: AIAccountBody = { provider_id: accountProviderId(p), label: addLabel.value.trim() };
  if (addKey.value.trim()) body.api_key = addKey.value.trim();
  if (addUrl.value.trim()) body.base_url = addUrl.value.trim();
  create.mutate(body);
}

// ── Edit ────────────────────────────────────────────────────────────────────
const editFor = ref<string | null>(null);
const editLabel = ref("");
const editKey = ref("");
const editUrl = ref("");

function startEdit(a: AIAccount) {
  confirmDeleteFor.value = null;
  adding.value = false;
  if (editFor.value === a.id) {
    editFor.value = null;
    return;
  }
  editFor.value = a.id;
  editLabel.value = a.label;
  editKey.value = "";
  editUrl.value = a.base_url;
}

// restartsApps: would saving restart the apps? The brain's rule: a new key or
// a new address does, a name change does not.
function restartsApps(a: AIAccount): boolean {
  return editKey.value.trim() !== "" || editUrl.value.trim() !== a.base_url;
}

function editValid(a: AIAccount): boolean {
  if (editLabel.value.trim() === "") return false;
  if (a.provider_id === COMPATIBLE) return /^https?:\/\/\S+$/.test(editUrl.value.trim());
  const url = editUrl.value.trim();
  return url === "" || /^https?:\/\/\S+$/.test(url);
}

const update = useMutation({
  mutationFn: (a: AIAccount) => {
    // An empty key keeps the stored one.
    const body: AIAccountBody = { provider_id: a.provider_id, label: editLabel.value.trim() };
    if (editKey.value.trim()) body.api_key = editKey.value.trim();
    if (editUrl.value.trim()) body.base_url = editUrl.value.trim();
    return api.put<AIAccountSaved>(`/ai-accounts/${a.id}`, body);
  },
  onSuccess: (saved, a) => {
    setRow(rowError, a.id, "");
    editFor.value = null;
    qc.invalidateQueries({ queryKey: ["ai-accounts"] });
    if (saved.job_id) {
      void followJob(saved.job_id, saved.used_by?.length ?? 0, (msg) => setRow(rowNotice, a.id, msg));
    }
  },
  onError: (e, a) => setRow(rowError, a.id, errorMessage(e)),
});

// ── Delete ──────────────────────────────────────────────────────────────────
const confirmDeleteFor = ref<string | null>(null);

const remove = useMutation({
  mutationFn: (a: AIAccount) => withElevation(() => api.del<AccountDeleted | undefined>(`/ai-accounts/${a.id}`)),
  onSuccess: (done, a) => {
    setRow(rowError, a.id, "");
    confirmDeleteFor.value = null;
    qc.invalidateQueries({ queryKey: ["ai-accounts"] });
    if (done?.job_id) {
      void followJob(done.job_id, a.used_by?.length ?? 0, (msg) => (pageNotice.value = msg));
    }
  },
  onError: (e, a) => setRow(rowError, a.id, errorMessage(e)),
});

// The key link is catalog data, so only a plain web address becomes a link.
function isWebLink(u: string | undefined): boolean {
  return !!u && /^https?:\/\//i.test(u);
}

function fid(name: string, id = ""): string {
  return `llm-${name}${id ? `-${id}` : ""}`;
}
</script>

<template>
  <div class="space-y-6">
    <section class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">AI services</h2>
      <p class="text-sm text-muted-foreground">
        Your accounts with AI services, such as OpenAI or Anthropic, or a server on your network. Apps you install use
        them to answer questions, write and search. Only you can see and use the accounts you add here.
      </p>
      <Button v-if="!isEmpty && !adding" @click="startAdd"><Plus class="size-4" /> Add account</Button>
    </section>

    <p v-if="pageNotice" class="text-sm text-muted-foreground">{{ pageNotice }}</p>

    <!-- Add: pick the provider, then fill in the account. -->
    <section v-if="adding" class="space-y-4 rounded-2xl border border-border bg-card p-5 sm:p-6">
      <template v-if="!addProvider">
        <h3 class="text-sm font-semibold text-foreground">Which provider?</h3>
        <p v-if="providersQuery.isPending.value" class="text-sm text-muted-foreground">Loading…</p>
        <OptionCards v-else label="AI service" :options="tiles" :selected="[]" @pick="pickAddProvider">
          <template #icon="{ option }">
            <AIProviderLogo :provider="findProvider(providers, option.id)" />
          </template>
        </OptionCards>
        <p v-if="providersQuery.isError.value" class="text-sm text-muted-foreground">
          The list of providers did not load, so only a server of your own can be added now.
        </p>
        <Button variant="ghost" @click="cancelAdd">Cancel</Button>
      </template>

      <template v-else>
        <button
          type="button"
          class="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
          @click="addProvider = null"
        >
          <ArrowLeft class="size-4" /> Choose a different provider
        </button>
        <div class="flex items-center gap-2.5">
          <AIProviderLogo :provider="addProvider" />
          <h3 class="text-sm font-semibold text-foreground">{{ addProvider.name }}</h3>
        </div>
        <p class="text-sm text-muted-foreground">
          <template v-if="isOther(addProvider)">Add a server that speaks the OpenAI API.</template>
          <template v-else>The key is saved on this box and never shown again.</template>
        </p>

        <div class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('add-label')">Account name</label>
          <input :id="fid('add-label')" v-model="addLabel" :class="fieldClass" autocomplete="off" />
          <p class="text-xs text-muted-foreground">What you'll see when an app asks which account to use.</p>
        </div>

        <div v-if="isOther(addProvider)" class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('add-url')">Server address</label>
          <input
            :id="fid('add-url')"
            v-model="addUrl"
            type="url"
            placeholder="https://example.com/v1"
            :class="fieldClass"
            autocomplete="off"
          />
          <p class="text-xs text-muted-foreground">It usually ends in /v1.</p>
        </div>

        <div class="space-y-1.5">
          <label class="text-sm font-medium" :for="fid('add-key')">
            API key<span v-if="isOther(addProvider)" class="font-normal text-muted-foreground"> (optional)</span>
          </label>
          <input :id="fid('add-key')" v-model="addKey" type="password" :class="fieldClass" autocomplete="new-password" />
          <p v-if="keyLooksWrong(addProvider, addKey)" class="text-xs text-warning">
            Keys from {{ addProvider.name }} usually start with {{ addProvider.key_prefix }}. Check that you copied the
            whole key.
          </p>
          <p class="text-xs text-muted-foreground">
            <template v-if="isOther(addProvider)">Only needed if your server asks for one.</template>
            <template v-else>
              <template v-if="addProvider.help">{{ addProvider.help }} </template>
              <a
                v-if="isWebLink(addProvider.key_url)"
                :href="addProvider.key_url"
                target="_blank"
                rel="noopener noreferrer"
                class="inline-flex items-center gap-1 underline hover:text-foreground"
              >Get a key from {{ addProvider.name }} <ExternalLink class="size-3.5" aria-hidden="true" /></a>
            </template>
          </p>
        </div>

        <div class="flex gap-2">
          <Button :disabled="create.isPending.value || !addValid" @click="submitAdd">
            {{ create.isPending.value ? "Adding…" : "Add account" }}
          </Button>
          <Button variant="ghost" @click="cancelAdd">Cancel</Button>
        </div>
        <p v-if="addError" class="text-xs text-destructive">{{ addError }}</p>
      </template>
    </section>

    <!-- Empty state -->
    <section
      v-if="isEmpty && !adding"
      class="flex min-h-[20rem] items-center justify-center rounded-2xl border border-border bg-card px-6 py-12"
    >
      <div class="text-center">
        <h3 class="text-sm font-semibold text-foreground">No AI service accounts</h3>
        <p class="mx-auto mt-1 max-w-sm text-sm text-muted-foreground">
          Add an account, and the apps that need an AI service can use it. You can also add one while you install an
          app.
        </p>
        <div class="mt-6">
          <Button @click="startAdd"><Plus class="size-4" /> Add account</Button>
        </div>
      </div>
    </section>

    <!-- Account list -->
    <section v-else-if="!isEmpty" class="space-y-3">
      <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Your accounts</h2>
      <p v-if="accountsQuery.isPending.value" class="text-sm text-muted-foreground">Loading…</p>
      <div v-else-if="accountsQuery.isError.value" class="flex items-center gap-2">
        <p class="text-sm text-destructive">Could not load your accounts.</p>
        <Button size="sm" variant="secondary" @click="accountsQuery.refetch()">Try again</Button>
      </div>
      <ul v-else class="space-y-2">
        <li v-for="a in accounts" :key="a.id" class="space-y-3 rounded-2xl border border-border bg-card p-5 sm:p-6">
          <div class="flex flex-wrap items-center gap-3">
            <AIProviderLogo :provider="providerOf(a)" :other="a.provider_id === COMPATIBLE" />
            <div class="min-w-0 flex-1">
              <div class="text-sm font-medium">{{ a.label }}</div>
              <div class="truncate text-xs text-muted-foreground">{{ detailLine(a) }}</div>
              <div class="text-xs text-muted-foreground">
                {{ a.used_by?.length ? `Used by ${appNames(a.used_by)}` : "No app uses it yet" }}
              </div>
            </div>
            <Button variant="secondary" size="sm" @click="startEdit(a)">Edit</Button>
            <Button
              variant="ghost"
              size="sm"
              class="text-destructive hover:bg-destructive/10"
              :disabled="remove.isPending.value"
              @click="editFor = null; confirmDeleteFor = confirmDeleteFor === a.id ? null : a.id"
            >
              Delete
            </Button>
          </div>

          <!-- Delete confirmation: names the apps that lose the account. -->
          <div
            v-if="confirmDeleteFor === a.id"
            class="flex flex-wrap items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/5 px-3 py-2"
          >
            <span v-if="a.used_by?.length" class="text-sm">
              Delete <strong>{{ a.label }}</strong>? {{ appNames(a.used_by) }} will lose this key and restart. Until you
              pick another account for {{ a.used_by.length === 1 ? "it" : "them" }}, {{ a.used_by.length === 1 ? "it" : "they" }}
              may not work.
            </span>
            <span v-else class="text-sm">Delete <strong>{{ a.label }}</strong>? No app uses it.</span>
            <Button
              variant="secondary"
              size="sm"
              class="border-destructive text-destructive hover:bg-destructive/10"
              :disabled="remove.isPending.value"
              @click="remove.mutate(a)"
            >
              Delete
            </Button>
            <Button variant="ghost" size="sm" @click="confirmDeleteFor = null">Cancel</Button>
          </div>

          <!-- Inline edit. The provider stays; a new key or address restarts
               the apps that use the account, and the form names them. -->
          <div v-if="editFor === a.id" class="space-y-3">
            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('label', a.id)">Account name</label>
              <input :id="fid('label', a.id)" v-model="editLabel" :class="fieldClass" autocomplete="off" />
            </div>
            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('url', a.id)">
                {{ a.provider_id === COMPATIBLE ? "Server address" : "Address" }}
                <span v-if="a.provider_id !== COMPATIBLE" class="font-normal text-muted-foreground">(optional)</span>
              </label>
              <input
                :id="fid('url', a.id)"
                v-model="editUrl"
                type="url"
                placeholder="https://example.com/v1"
                :class="fieldClass"
                autocomplete="off"
              />
              <p v-if="a.provider_id !== COMPATIBLE" class="text-xs text-muted-foreground">
                Leave empty to use {{ providerName(a) }}'s own address.
              </p>
            </div>
            <div class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('key', a.id)">API key</label>
              <input :id="fid('key', a.id)" v-model="editKey" type="password" :class="fieldClass" autocomplete="new-password" />
              <p class="text-xs text-muted-foreground">Leave blank to keep the one you saved.</p>
            </div>
            <p v-if="a.used_by?.length && restartsApps(a)" class="rounded-md bg-warning/10 px-3 py-2 text-xs text-warning">
              Saving restarts {{ appNames(a.used_by) }}, so {{ a.used_by.length === 1 ? "it uses" : "they use" }} the new
              {{ editKey.trim() ? "key" : "address" }}.
            </p>
            <div class="flex gap-2">
              <Button :disabled="update.isPending.value || !editValid(a)" @click="update.mutate(a)">
                {{ update.isPending.value ? "Saving…" : "Save" }}
              </Button>
              <Button variant="ghost" @click="editFor = null">Cancel</Button>
            </div>
          </div>

          <p v-if="rowNotice[a.id]" class="text-xs text-muted-foreground">{{ rowNotice[a.id] }}</p>
          <p v-if="rowError[a.id]" class="text-xs text-destructive">{{ rowError[a.id] }}</p>
        </li>
      </ul>
    </section>
  </div>
</template>
