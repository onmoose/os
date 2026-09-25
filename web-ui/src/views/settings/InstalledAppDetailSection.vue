<script setup lang="ts">
// Settings → Installed apps → one instance. The per-app management page: header
// (logo, name, description), the action row (Open / Stop·Start / Uninstall),
// and the logs at the bottom. Rendered inside the Settings shell as a nested
// route (/settings/apps/:id), so the left nav stays put.
//
// Two data sources: GET /apps/{id} for the live instance (state, scope, url),
// and GET /catalog/{manifest_id} for the logo + description. The catalog lookup
// is best-effort: a Door-2 (custom) app has no catalog entry, so it falls back
// to the generic glyph and no description.
import { computed, ref, watch } from "vue";
import { useRoute, useRouter, RouterLink } from "vue-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/vue-query";
import { AppWindow, Check, ChevronDown, ChevronsUpDown, ExternalLink } from "lucide-vue-next";
import {
  SwitchRoot,
  SwitchThumb,
  SelectRoot,
  SelectTrigger,
  SelectPortal,
  SelectContent,
  SelectViewport,
  SelectItem,
  SelectItemText,
  SelectItemIndicator,
} from "reka-ui";
import { api, waitForJob, type Instance, type CatalogDetail, type Job, type MailProviderOption, type AppSecrets, type AppSecret, type AppConfig, type AppConfigField, type Exposure } from "@/api";
import { useAuth, isHosted } from "@/auth";
import MailProviderLogo from "@/components/MailProviderLogo.vue";
import AppLogs from "@/components/AppLogs.vue";
import Button from "@/components/ui/Button.vue";

const route = useRoute();
const router = useRouter();
const qc = useQueryClient();
const { currentUser } = useAuth();

const id = computed(() => String(route.params.id));

const appQuery = useQuery({
  queryKey: computed(() => ["apps", id.value]),
  queryFn: () => api.get<Instance>(`/apps/${id.value}`),
});
const app = computed(() => appQuery.data.value ?? null);

// Catalog lookup for logo + description. Best-effort: disabled until we know the
// manifest id, no retry (a Door-2 app legitimately 404s here), and any failure
// just leaves the glyph + "No description" fallback in place.
const catalogQuery = useQuery({
  queryKey: computed(() => ["catalog", app.value?.manifest_id]),
  queryFn: () => api.get<CatalogDetail>(`/catalog/${app.value!.manifest_id}`),
  enabled: computed(() => !!app.value?.manifest_id),
  retry: false,
});
const detail = computed(() => catalogQuery.data.value ?? null);

const brokenIcon = ref(false);
watch(id, () => { brokenIcon.value = false; });

// Authorization mirrors the brain: admins control any app; a member controls
// (and reads logs of) only their own personal instance. The list is already
// server-scoped, so a member never lands here for an app they can't see.
const canControl = computed(
  () => currentUser.value?.role === "admin" || app.value?.scope === "personal",
);

const running = computed(() => app.value?.state === "running");
const stopped = computed(() => app.value?.state === "stopped");
// `failed` shares the Start control as a click-to-retry (#154): the brain runs
// the identical Start transaction from `stopped` or `failed`. The logs below are
// the failure reason, so a retry from here isn't blind.
const failed = computed(() => app.value?.state === "failed");

function invalidate() {
  qc.invalidateQueries({ queryKey: ["apps"] });
  qc.invalidateQueries({ queryKey: ["apps", id.value] });
}

// awaitJob polls to terminal and throws on a failed job, so a job-level failure
// (e.g. compose up never goes healthy) surfaces via useMutation's isError.
// api.post only throws on the synchronous 4xx, not on a job that fails later.
async function awaitJob(job: Job): Promise<Job> {
  const done = await waitForJob(job.job_id);
  if (done.status === "failed") throw new Error(done.error?.message || "the operation failed");
  return done;
}

const stop = useMutation({
  mutationFn: async () => awaitJob(await api.post<Job>(`/apps/${id.value}/stop`)),
  onSettled: invalidate,
});

const start = useMutation({
  mutationFn: async () => awaitJob(await api.post<Job>(`/apps/${id.value}/start`)),
  onSettled: invalidate,
});

// Uninstall is destructive, so it's a two-step inline confirm rather than a bare
// button. On success we leave the now-dead detail page for the list.
const confirmingUninstall = ref(false);

// Logs start collapsed. They're a drill-down, not the first thing on the page.
const logsOpen = ref(false);
const uninstall = useMutation({
  mutationFn: async () => awaitJob(await api.del<Job>(`/apps/${id.value}`)),
  onSuccess: () => {
    qc.invalidateQueries({ queryKey: ["apps"] });
    router.push("/settings/apps");
  },
});

const busy = computed(
  () => stop.isPending.value || start.isPending.value || uninstall.isPending.value,
);

// ── Access mode (#306/#307, ENVIRONMENT.md # Per-app owner-only access) ───────
// Hosted-only per-app toggle between "Only me" (restricted: the box login gates
// the app) and "Public" (anyone with the link). Gated to the hosted profile
// (the endpoint 404s on the appliance, which has no public app subdomains) and to
// canControl (owner-or-admin, the same gate the brain re-checks). The PUT echoes
// the updated instance, so invalidate refreshes the detail + list without a job.
const exposure = computed<Exposure>(() => app.value?.exposure ?? "public");
const exposureOptions: { value: Exposure; label: string }[] = [
  { value: "restricted", label: "Only me" },
  { value: "public", label: "Public" },
];
const setExposure = useMutation({
  mutationFn: (next: Exposure) => api.put<Instance>(`/apps/${id.value}/exposure`, { exposure: next }),
  onSettled: invalidate,
});
// Paths the app's manifest keeps open to anyone even while the app is "Only me"
// (#415, an app whose API is token-authed and whose UI is session-authed). The
// label must say so: "Only me" would otherwise claim a narrower app than the box
// is really serving. It says *that* some paths are open, not which ones: the list
// is manifest detail nobody acts on from this page, and naming it turned one
// sentence into a wall. Empty for almost every app.
const publicPaths = computed<string[]>(() => app.value?.public_paths ?? []);
const accessSummary = computed(() => {
  if (exposure.value === "public") return "Anyone with the link can open it.";
  if (publicPaths.value.length > 0) {
    return "Only you can open the app. Visitors sign in to your box first. Some app paths are open to the public, so tools outside your box can reach them.";
  }
  return "Only you can open it. Visitors sign in to your box first.";
});

// ── Outgoing email (SERVICE_PROVISIONING.md # BYO outgoing mail) ─────────────
// Shown only for mail-capable apps (mail_supported comes from GET /apps/{id}).
// The options endpoint lists the caller's own accounts, the only ones the
// brain lets them bind: an admin rebinding a household app picks from the
// admin's own accounts. A rebind recreates the app's containers (env is read
// at container create), hence the job + hint below.
const mailOptions = useQuery({
  queryKey: ["mail-provider-options"],
  queryFn: () => api.get<{ providers: MailProviderOption[] }>("/mail-providers/options"),
  enabled: computed(() => !!app.value?.mail_supported && canControl.value),
});

const rebindMail = useMutation({
  mutationFn: async (providerId: string) =>
    awaitJob(await api.put<Job>(`/apps/${id.value}/mail-binding`, { provider_id: providerId })),
  onSettled: invalidate,
});

// "None" needs a value the picker can carry, and reka-ui reserves the empty
// string (it is how a Select is cleared), so the unbound state travels as this
// sentinel and turns back into "" on the way to the brain.
const NO_PROVIDER = "__none__";
const mailProviders = computed<MailProviderOption[]>(() => mailOptions.data.value?.providers ?? []);
const boundProvider = computed(() => mailProviders.value.find((p) => p.id === app.value?.mail_provider_id));
// The binding and the account list arrive from two different requests, so there
// is a window where the app IS bound and the list that names the account has not
// landed. The trigger must not fill that window with "None": it is a definite
// claim, and acting on it costs a rebind and an app restart.
//
// The picker opens only once that list has actually arrived: isSuccess, not
// "not loading". A failed request leaves the same empty list as a pending one,
// and an enabled picker over an empty list offers exactly one action: unbind.
const mailListReady = computed(() => mailOptions.isSuccess.value);
const mailLabel = computed(() => {
  if (boundProvider.value) return boundProvider.value.label;
  if (!app.value?.mail_provider_id) return "None (email features off)";
  if (mailOptions.isError.value) return "Account list unavailable";
  // Bound, the list has arrived, and the account is not in it: another user
  // added it (accounts are per user, and the list is only the caller's own).
  // The picker still works; choosing one replaces it with an account of the
  // caller's.
  return mailOptions.isLoading.value ? "Loading…" : "Someone else's account";
});

// ── Setup secrets (#152, SERVICE_PROVISIONING.md # Env-var injection) ─────────
// Owner-visible per-instance secrets a self-auth app declared `show: true`: the
// bootstrap credential the user reads to finish first sign-in. Gated to the same
// owner-or-admin rule as the controls (canControl); the brain re-checks. The
// section hides itself when the app declares none (the list comes back empty).
const secretsQuery = useQuery({
  queryKey: computed(() => ["app-secrets", id.value]),
  queryFn: () => api.get<AppSecrets>(`/apps/${id.value}/secrets`),
  enabled: computed(() => canControl.value),
});
const secrets = computed(() => secretsQuery.data.value?.secrets ?? []);

// Masked by default. Revealed per-secret on demand so the value isn't shoulder-
// surfaced just by opening the page. A reassigned Set keeps the template reactive.
const revealed = ref(new Set<string>());
function toggleReveal(name: string) {
  const next = new Set(revealed.value);
  if (next.has(name)) next.delete(name);
  else next.add(name);
  revealed.value = next;
}

// Copy is best-effort: navigator.clipboard is unavailable on the HTTP-only
// .local origin, so the value stays select-all on screen as the fallback.
const copied = ref<string | null>(null);
async function copySecret(s: AppSecret) {
  try {
    await navigator.clipboard.writeText(s.value);
    copied.value = s.name;
    setTimeout(() => {
      if (copied.value === s.name) copied.value = null;
    }, 1500);
  } catch {
    // No clipboard on an insecure context. The value is on screen to copy by hand.
  }
}

// ── Settings: user-supplied config (APP_MANIFEST.md # D4) ───────────────────
// Fields the app declared a `config:` block for (an API token, a model picker).
// GET never returns a secret's value (only `set`), so secrets show as set/not-set
// with a Replace affordance; non-secret values are editable inline. Save sends a
// PARTIAL update (only the fields the user actually changed), so an untouched
// secret is never resent (we don't have it) and never accidentally cleared. The
// PUT restarts the app as a job. Gated to canControl; the brain re-checks.
const configQuery = useQuery({
  queryKey: computed(() => ["app-config", id.value]),
  queryFn: () => api.get<AppConfig>(`/apps/${id.value}/config`),
  enabled: computed(() => canControl.value),
});
const configFields = computed<AppConfigField[]>(() => configQuery.data.value?.fields ?? []);

// edits is the local buffer: non-secret fields start at their stored value;
// secret fields start empty and only carry a value once the user hits Replace.
// replacing tracks which secrets are mid-edit (showing an input vs the set badge).
const edits = ref<Record<string, string>>({});
const replacing = ref<Set<string>>(new Set());

watch(
  configFields,
  (fields) => {
    const next: Record<string, string> = {};
    for (const f of fields) next[f.app_env] = f.secret ? "" : f.value;
    edits.value = next;
    replacing.value = new Set();
  },
  { immediate: true },
);

function startReplace(appEnv: string) {
  const next = new Set(replacing.value);
  next.add(appEnv);
  replacing.value = next;
  edits.value[appEnv] = "";
}
function cancelReplace(appEnv: string) {
  const next = new Set(replacing.value);
  next.delete(appEnv);
  replacing.value = next;
  edits.value[appEnv] = "";
}

// changedFields is the partial-update payload: a non-secret field whose buffer
// differs from its stored value, and a secret only when the user typed a new,
// non-empty value (a blank Replace box is ignored; we never blank a secret here).
function changedFields(): Record<string, string> {
  const out: Record<string, string> = {};
  for (const f of configFields.value) {
    const v = edits.value[f.app_env] ?? "";
    if (f.secret) {
      if (replacing.value.has(f.app_env) && v !== "") out[f.app_env] = v;
    } else if (v !== f.value) {
      out[f.app_env] = v;
    }
  }
  return out;
}
const dirty = computed(() => Object.keys(changedFields()).length > 0);

// Block Save if a required NON-secret field has been cleared, because the brain would
// 422 it. A required secret is already set (install enforced it) and can only be
// replaced, never blanked from here, so it never gates Save.
const configValid = computed(() =>
  configFields.value.every(
    (f) => !f.required || f.secret || (edits.value[f.app_env] ?? "").trim() !== "",
  ),
);

const saveConfig = useMutation({
  mutationFn: async () => awaitJob(await api.put<Job>(`/apps/${id.value}/config`, { fields: changedFields() })),
  onSettled: () => {
    invalidate();
    qc.invalidateQueries({ queryKey: ["app-config", id.value] });
  },
});
</script>

<template>
  <div class="flex flex-1 flex-col gap-8 pt-2">
    <RouterLink to="/settings/apps" class="inline-block text-sm text-muted-foreground hover:text-foreground">
      ← Installed apps
    </RouterLink>

    <p v-if="appQuery.isLoading.value" class="text-sm text-muted-foreground">Loading…</p>
    <p v-else-if="appQuery.isError.value" class="text-sm text-destructive">
      Couldn't load this app. {{ (appQuery.error.value as Error)?.message }}
    </p>

    <template v-else-if="app">
      <!-- Header: logo · name/description · state -->
      <header class="flex flex-col gap-5 sm:flex-row sm:items-center">
        <div
          class="grid size-20 shrink-0 place-items-center overflow-hidden rounded-3xl border border-border bg-card text-muted-foreground"
        >
          <img
            v-if="detail?.icon_url && !brokenIcon"
            :src="detail.icon_url"
            :alt="`${app.name} icon`"
            class="size-full object-cover"
            @error="brokenIcon = true"
          />
          <AppWindow v-else class="size-9" />
        </div>

        <div class="min-w-0 flex-1">
          <h1 class="text-xl font-semibold">{{ app.name }}</h1>
          <p v-if="detail?.short_description" class="mt-0.5 text-sm text-muted-foreground">
            {{ detail.short_description }}
          </p>
          <!-- The app's address, written out. The Open button below is a one-click
               path, but it hides the URL and only exists while the app runs. This
               shows the link itself, so it can be opened in a new tab, copied with
               a right-click, or read out to another device. Clipboard copy is not
               offered: navigator.clipboard is unavailable on the HTTP-only .local
               origin, so right-click copy is the reliable path. -->
          <a
            v-if="app.url"
            :href="app.url"
            target="_blank"
            rel="noopener"
            class="mt-1 inline-flex max-w-full items-center gap-1.5 text-sm text-muted-foreground underline underline-offset-2 hover:text-foreground"
          >
            <span class="truncate">{{ app.url }}</span>
            <ExternalLink class="size-3.5 shrink-0" />
          </a>
          <p class="mt-1 text-xs uppercase tracking-wide text-muted-foreground">{{ app.state }}</p>
        </div>
      </header>

      <!-- Action row -->
      <section class="flex flex-wrap items-center gap-2">
        <!-- A real anchor, so it opens a new tab, but the same pill at the same
             size as the buttons beside it. -->
        <Button v-if="running" as="a" size="sm" :href="app.url" target="_blank" rel="noopener">
          Open
        </Button>

        <Button v-if="canControl && running" variant="secondary" size="sm" :disabled="busy" @click="stop.mutate()">
          {{ stop.isPending.value ? "Stopping…" : "Stop service" }}
        </Button>
        <Button
          v-else-if="canControl && (stopped || failed)"
          variant="secondary"
          size="sm"
          :disabled="busy"
          @click="start.mutate()"
        >
          <template v-if="failed">{{ start.isPending.value ? "Retrying…" : "Retry" }}</template>
          <template v-else>{{ start.isPending.value ? "Starting…" : "Start service" }}</template>
        </Button>

        <template v-if="canControl">
          <template v-if="confirmingUninstall">
            <span class="text-sm text-muted-foreground">Uninstall {{ app.name }}? This deletes its data.</span>
            <Button
              variant="secondary"
              size="sm"
              class="border-destructive text-destructive hover:bg-destructive/10"
              :disabled="busy"
              @click="uninstall.mutate()"
            >
              {{ uninstall.isPending.value ? "Uninstalling…" : "Confirm uninstall" }}
            </Button>
            <Button variant="ghost" size="sm" :disabled="busy" @click="confirmingUninstall = false">Cancel</Button>
          </template>
          <Button
            v-else
            variant="ghost"
            size="sm"
            class="text-destructive hover:bg-destructive/10"
            :disabled="busy"
            @click="confirmingUninstall = true"
          >
            Uninstall
          </Button>
        </template>
      </section>

      <!-- Action error surface (job failure / 409 / host 5xx) -->
      <p v-if="stop.isError.value" class="text-sm text-destructive">
        Couldn't stop: {{ (stop.error.value as Error)?.message }}
      </p>
      <p v-if="start.isError.value" class="text-sm text-destructive">
        Couldn't start: {{ (start.error.value as Error)?.message }}
      </p>
      <p v-if="uninstall.isError.value" class="text-sm text-destructive">
        Couldn't uninstall: {{ (uninstall.error.value as Error)?.message }}
      </p>

      <!-- Access mode: hosted-only Only-me / Public toggle. Hidden on the
           appliance (no public app subdomains) and for viewers who can't control
           the app. Switching re-writes the app's Caddy route via the brain. -->
      <section v-if="isHosted() && canControl" class="space-y-2">
        <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Access</h2>
        <div class="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card px-4 py-3">
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium">Who can open this app</div>
            <div class="text-xs text-muted-foreground">{{ accessSummary }}</div>
          </div>
          <div role="group" aria-label="Access mode" class="inline-flex shrink-0 rounded-lg border border-border p-0.5 text-sm">
            <button
              v-for="opt in exposureOptions"
              :key="opt.value"
              type="button"
              class="cursor-pointer rounded-md px-3 py-1 transition-colors disabled:cursor-default"
              :class="exposure === opt.value ? 'bg-accent text-accent-foreground' : 'text-muted-foreground hover:text-foreground'"
              :aria-pressed="exposure === opt.value"
              :disabled="setExposure.isPending.value || busy || opt.value === exposure"
              @click="setExposure.mutate(opt.value)"
            >
              {{ opt.label }}
            </button>
          </div>
          <!-- The manifest's own open paths (#415), listed only while the app is
               Only me: on a Public app every path is open, so naming a few would
               read as a limit that isn't there. The summary line above says THAT
               some paths are open; this says which. Badge shape ported from
               Tailwind Plus (elements/badges 13-small-with-border) with the palette
               mapped to moose's tokens, and set in mono because a path is a literal
               string someone types, like the secrets and env fields below. -->
          <div
            v-if="exposure === 'restricted' && publicPaths.length > 0"
            class="w-full border-t border-border pt-3"
          >
            <div class="text-xs text-muted-foreground">These paths stay open to anyone:</div>
            <ul class="mt-1.5 flex flex-wrap gap-1.5">
              <li
                v-for="path in publicPaths"
                :key="path"
                class="inline-flex items-center rounded-md bg-muted px-1.5 py-0.5 font-mono text-xs font-medium text-muted-foreground inset-ring inset-ring-border"
              >
                {{ path }}
              </li>
            </ul>
          </div>
        </div>
        <p v-if="setExposure.isError.value" class="text-sm text-destructive">
          Couldn't change access: {{ (setExposure.error.value as Error)?.message }}
        </p>
      </section>

      <!-- Outgoing email: provider binding for mail-capable apps. -->
      <section v-if="app.mail_supported && canControl" class="space-y-2">
        <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Outgoing email</h2>
        <div class="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card px-4 py-3">
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium">Send email as</div>
            <div class="text-xs text-muted-foreground">
              {{ rebindMail.isPending.value ? "Applying. The app restarts briefly." : "Changing this restarts the app briefly." }}
            </div>
          </div>
          <!-- A listbox rather than a native <select>, because the choice is
               "which of my accounts", and an account is recognised by its
               provider's logo faster than by a name someone typed. Shape ported
               from Tailwind Plus (forms/select-menus 05-custom-with-avatar) with
               the palette on moose's tokens and the Headless UI primitives mapped
               to reka-ui, which is what this project already ships. -->
          <SelectRoot
            :model-value="app.mail_provider_id || NO_PROVIDER"
            :disabled="rebindMail.isPending.value || busy || !mailListReady"
            @update:model-value="(v) => rebindMail.mutate(v === NO_PROVIDER ? '' : String(v))"
          >
            <SelectTrigger
              aria-label="Email account"
              class="inline-flex w-56 max-w-full cursor-pointer items-center justify-between gap-2 rounded-lg border border-border bg-background px-2 py-1 text-left text-sm outline-none focus:border-accent disabled:cursor-default disabled:opacity-50"
            >
              <span class="flex min-w-0 items-center gap-2">
                <span v-if="boundProvider" class="flex size-5 shrink-0 items-center justify-center">
                  <MailProviderLogo :id="boundProvider.provider_type" :label="boundProvider.label" size="inline" />
                </span>
                <span class="truncate">{{ mailLabel }}</span>
              </span>
              <ChevronsUpDown class="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
            </SelectTrigger>

            <SelectPortal>
              <SelectContent
                position="popper"
                :side-offset="4"
                class="z-50 max-h-56 w-[var(--reka-select-trigger-width)] overflow-auto rounded-xl border border-border bg-card py-1 shadow-lg"
              >
                <SelectViewport>
                  <SelectItem
                    :value="NO_PROVIDER"
                    class="relative flex cursor-pointer items-center gap-2 py-2 pl-3 pr-9 text-sm outline-none data-[highlighted]:bg-muted"
                  >
                    <span class="size-5 shrink-0" aria-hidden="true" />
                    <SelectItemText>None (email features off)</SelectItemText>
                    <SelectItemIndicator class="absolute inset-y-0 right-0 flex items-center pr-3">
                      <Check class="size-4" aria-hidden="true" />
                    </SelectItemIndicator>
                  </SelectItem>
                  <SelectItem
                    v-for="p in mailProviders"
                    :key="p.id"
                    :value="p.id"
                    class="relative flex cursor-pointer items-center gap-2 py-2 pl-3 pr-9 text-sm outline-none data-[highlighted]:bg-muted"
                  >
                    <span class="flex size-5 shrink-0 items-center justify-center">
                      <MailProviderLogo :id="p.provider_type" :label="p.label" size="inline" />
                    </span>
                    <SelectItemText>{{ p.label }}</SelectItemText>
                    <SelectItemIndicator class="absolute inset-y-0 right-0 flex items-center pr-3">
                      <Check class="size-4" aria-hidden="true" />
                    </SelectItemIndicator>
                  </SelectItem>
                </SelectViewport>
              </SelectContent>
            </SelectPortal>
          </SelectRoot>
        </div>
        <p v-if="rebindMail.isError.value" class="text-sm text-destructive">
          Couldn't change the email account: {{ (rebindMail.error.value as Error)?.message }}
        </p>
      </section>

      <!-- Setup secrets: owner-visible bootstrap credentials for self-auth apps.
           Shown only when the app declared one (`show: true`); masked until the
           owner reveals it. -->
      <p v-if="secretsQuery.isError.value" class="text-sm text-destructive">
        Couldn't load secrets: {{ (secretsQuery.error.value as Error)?.message }}
      </p>
      <section v-if="canControl && secrets.length" class="space-y-2">
        <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Setup secrets</h2>
        <p class="text-xs text-muted-foreground">
          Use these to finish signing in to {{ app.name }} the first time. Keep them private.
        </p>
        <ul class="space-y-2">
          <li
            v-for="s in secrets"
            :key="s.name"
            class="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-card px-4 py-3"
          >
            <div class="min-w-0 flex-1">
              <div class="text-xs font-medium text-muted-foreground">{{ s.name }}</div>
              <div class="mt-0.5 break-all font-mono text-sm" :class="{ 'select-all': revealed.has(s.name) }">
                {{ revealed.has(s.name) ? s.value : "••••••••••••" }}
              </div>
            </div>
            <Button type="button" variant="secondary" size="sm" @click="toggleReveal(s.name)">
              {{ revealed.has(s.name) ? "Hide" : "Reveal" }}
            </Button>
            <Button type="button" variant="secondary" size="sm" @click="copySecret(s)">
              {{ copied === s.name ? "Copied" : "Copy" }}
            </Button>
          </li>
        </ul>
      </section>

      <!-- Settings: user-supplied config (APP_MANIFEST.md # D4). Hidden when the
           app declares no config: block. Non-secret values edit inline; secrets
           show set/not-set with a Replace box. Save sends only what changed and
           restarts the app. -->
      <p v-if="configQuery.isError.value" class="text-sm text-destructive">
        Couldn't load settings: {{ (configQuery.error.value as Error)?.message }}
      </p>
      <section v-if="canControl && configFields.length" class="space-y-2">
        <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">Settings</h2>
        <p class="text-xs text-muted-foreground">Changing these restarts {{ app.name }} briefly.</p>
        <div class="space-y-2">
          <div
            v-for="f in configFields"
            :key="f.app_env"
            class="space-y-2 rounded-xl border border-border bg-card px-4 py-3"
          >
            <div>
              <div class="text-sm font-medium">
                {{ f.title }}<span v-if="f.required" class="text-destructive"> *</span>
              </div>
              <div class="text-xs text-muted-foreground">{{ f.description }}</div>
              <div class="mt-0.5 font-mono text-xs text-muted-foreground">Sets {{ f.app_env }}</div>
            </div>

            <!-- secret: set/not-set badge + replace affordance -->
            <template v-if="f.secret">
              <div v-if="!replacing.has(f.app_env)" class="flex items-center gap-3">
                <span class="text-sm text-muted-foreground">{{ f.set ? "•••••••• (set)" : "Not set" }}</span>
                <Button type="button" variant="secondary" size="sm" @click="startReplace(f.app_env)">
                  {{ f.set ? "Replace" : "Set" }}
                </Button>
              </div>
              <div v-else class="flex items-center gap-2">
                <input
                  v-model="edits[f.app_env]"
                  type="password"
                  autocomplete="off"
                  placeholder="Enter a new value"
                  class="w-full rounded-lg border border-border bg-background px-3 py-1.5 text-sm outline-none focus:border-accent"
                />
                <Button type="button" variant="ghost" size="sm" @click="cancelReplace(f.app_env)">Cancel</Button>
              </div>
            </template>

            <!-- non-secret enum: select of declared options -->
            <select
              v-else-if="f.type === 'enum'"
              v-model="edits[f.app_env]"
              class="w-full rounded-lg border border-border bg-background px-3 py-1.5 text-sm outline-none focus:border-accent"
            >
              <option v-if="!f.required" value="">None</option>
              <option v-for="opt in (f.options ?? [])" :key="opt" :value="opt">{{ opt }}</option>
            </select>

            <!-- non-secret bool: toggle; value travels as "true"/"false" -->
            <SwitchRoot
              v-else-if="f.type === 'bool'"
              :model-value="edits[f.app_env] === 'true'"
              class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors data-[state=checked]:border-accent data-[state=checked]:bg-accent"
              @update:model-value="(on: boolean) => (edits[f.app_env] = on ? 'true' : 'false')"
            >
              <SwitchThumb
                class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
              />
            </SwitchRoot>

            <!-- non-secret text -->
            <input
              v-else
              v-model="edits[f.app_env]"
              type="text"
              class="w-full rounded-lg border border-border bg-background px-3 py-1.5 text-sm outline-none focus:border-accent"
            />
          </div>
        </div>

        <div class="flex items-center gap-3">
          <Button
            type="button"
            size="sm"
            :disabled="!dirty || !configValid || saveConfig.isPending.value"
            @click="saveConfig.mutate()"
          >
            {{ saveConfig.isPending.value ? "Saving…" : "Save changes" }}
          </Button>
          <span v-if="saveConfig.isPending.value" class="text-xs text-muted-foreground">
            The app restarts briefly.
          </span>
        </div>
        <p v-if="saveConfig.isError.value" class="text-sm text-destructive">
          Couldn't save settings: {{ (saveConfig.error.value as Error)?.message }}
        </p>
      </section>

      <!-- Logs: collapsed by default; a full-width accordion row (styled like
           the Installed apps list rows) that expands the log panel on click. The
           chevron at the end rotates to signal expansion. -->
      <section v-if="canControl" class="flex flex-col gap-2">
        <button
          type="button"
          class="flex w-full shrink-0 cursor-pointer items-center justify-between gap-3 rounded-xl border border-border bg-card px-4 py-3 text-sm hover:bg-muted"
          :aria-expanded="logsOpen"
          @click="logsOpen = !logsOpen"
        >
          <span class="font-medium">Logs</span>
          <ChevronDown class="size-4 shrink-0 text-muted-foreground transition-transform" :class="{ 'rotate-180': logsOpen }" />
        </button>
        <!-- Bounded scroll box (not a viewport-fill): at least 400px so there is
             always readable output, capped at 70vh so it stays in normal page
             flow. The page scrolls around it and the AppShell spacer clears the
             dock. A flex-1 fill would instead pin it to the viewport and, on a
             short screen, overflow its last rows behind the dock. -->
        <AppLogs v-if="logsOpen" :id="app.id" fill class="min-h-[400px] max-h-[70vh]" />
      </section>
    </template>
  </div>
</template>
