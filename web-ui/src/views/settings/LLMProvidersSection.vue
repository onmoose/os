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
import { ArrowLeft, Plus } from "lucide-vue-next";
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
import { COMPATIBLE, OTHER, findProvider, isOther, modelIdProblem, serverModelsBody, withOther } from "@/aiProviders";
import { errorMessage, fieldClass } from "@/mailProviderForm";
import Button from "@/components/ui/Button.vue";
import AIProviderLogo from "@/components/AIProviderLogo.vue";
import AIKeyForm from "@/components/install/AIKeyForm.vue";
import ServiceGrid from "@/components/install/ServiceGrid.vue";

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

// ── Add ──────────────────────────────────────────────────────────────────
// The same two views as the install flow (INSTALL_STEPS.md # 3): the service
// grid, then the key form. The form is the install flow's AIKeyForm, so
// the key box, the folded steps and the default name match. Saving goes back
// to the account list.
const adding = ref(false);
const addService = ref(""); // the tile picked on the grid
const addProvider = ref<AIProvider | null>(null); // set once Continue opens the form
const keyForm = ref<InstanceType<typeof AIKeyForm> | null>(null);

// Every service, in the provider data's order, with My own server last.
const services = computed(() => withOther(providers.value));

function startAdd() {
  adding.value = true;
  addService.value = "";
  addProvider.value = null;
  editFor.value = null;
  confirmDeleteFor.value = null;
}

function openForm() {
  addProvider.value = findProvider(providers.value, addService.value) ?? null;
}

function cancelAdd() {
  adding.value = false;
  addProvider.value = null;
}

async function submitAdd() {
  const saved = await keyForm.value?.save();
  if (!saved) return;
  pageNotice.value = `Added ${saved.account.label}.`;
  cancelAdd();
}

// ── Edit ────────────────────────────────────────────────────────────────────
const editFor = ref<string | null>(null);
const editLabel = ref("");
const editKey = ref("");
const editUrl = ref("");
// editModel is a server's chat model name, saved on the account, so apps
// that bind it do not ask for it. Only My own server accounts have it.
const editModel = ref("");

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
  editModel.value = (a.models?.chat ?? [])[0] ?? "";
}

// restartsApps: would saving restart the apps? The brain's rule: a new key or
// a new address does, a name change does not.
function restartsApps(a: AIAccount): boolean {
  return editKey.value.trim() !== "" || editUrl.value.trim() !== a.base_url;
}

function editValid(a: AIAccount): boolean {
  if (editLabel.value.trim() === "") return false;
  if (a.provider_id === COMPATIBLE) {
    // The model name is required: the brain refuses a server without one.
    const model = editModel.value.trim();
    return /^https?:\/\/\S+$/.test(editUrl.value.trim()) && model !== "" && !modelIdProblem(model);
  }
  const url = editUrl.value.trim();
  return url === "" || /^https?:\/\/\S+$/.test(url);
}

const update = useMutation({
  mutationFn: (a: AIAccount) => {
    // An empty key keeps the stored one.
    const body: AIAccountBody = { provider_id: a.provider_id, label: editLabel.value.trim() };
    if (editKey.value.trim()) body.api_key = editKey.value.trim();
    if (editUrl.value.trim()) body.base_url = editUrl.value.trim();
    if (a.provider_id === COMPATIBLE) {
      body.models = serverModelsBody(a.models, { chat: editModel.value });
    }
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

    <!-- Add: the service grid, then the key form, as in the install flow. -->
    <section v-if="adding" class="space-y-5 rounded-2xl border border-border bg-card p-5 sm:p-6">
      <template v-if="!addProvider">
        <h3 id="add-ai-service" class="text-sm font-semibold text-foreground">Which AI service?</h3>
        <p v-if="providersQuery.isPending.value" class="text-sm text-muted-foreground">Loading…</p>
        <template v-else>
          <ServiceGrid v-model="addService" :services="services" label="AI service" />
          <p v-if="providersQuery.isError.value" class="text-sm text-muted-foreground">
            The list of AI services did not load, so only a server of your own can be added now.
          </p>
        </template>
        <div class="flex gap-2">
          <Button variant="ghost" @click="cancelAdd"><ArrowLeft class="size-4" /> Back</Button>
          <Button :disabled="!addService" @click="openForm">Continue</Button>
        </div>
      </template>

      <template v-else>
        <div class="flex items-center gap-2.5">
          <AIProviderLogo :provider="addProvider" />
          <h3 class="text-sm font-semibold text-foreground">
            {{ isOther(addProvider) ? "Your own server" : `Your ${addProvider.name} key` }}
          </h3>
        </div>
        <AIKeyForm ref="keyForm" :service="addProvider" :labels="accounts.map((a) => a.label)" />
        <!-- Back and Continue only, as in the install flow: Back goes to the
             grid, Continue saves and goes back to the list. -->
        <div class="flex gap-2">
          <Button variant="ghost" @click="addProvider = null"><ArrowLeft class="size-4" /> Back</Button>
          <Button :disabled="!keyForm?.valid || keyForm?.pending" @click="submitAdd">
            {{ keyForm?.pending ? "Adding…" : "Continue" }}
          </Button>
        </div>
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
            <div v-if="a.provider_id === COMPATIBLE" class="space-y-1.5">
              <label class="text-sm font-medium" :for="fid('model', a.id)">Model name</label>
              <input :id="fid('model', a.id)" v-model="editModel" :class="fieldClass" autocomplete="off" />
              <p class="text-xs text-muted-foreground">
                The name your server gives the model. Apps you install later use it; apps already installed keep theirs.
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
