<script setup lang="ts">
// Door-2 custom-container install (DASHBOARD.md # Door-2 custom container install
// flow). A dedicated full-screen form — distinct from the catalog consent dialog
// — that authors a synthetic manifest from a pasted docker-compose.yml. Admin-only.
//
// Flow: paste/upload compose → POST /apps/custom/inspect (read-only) drives the
// service dropdown + best-effort main-port prefill → author the permission block
// (internet/LAN/GPU toggles + folder grants, or the Edit-as-YAML escape hatch) →
// submit to POST /apps/custom. Admission runs on submit; its field-named 422 is
// coached inline against the compose. Scope follows the store row's split-button
// convention (silent personal on a single-user box).
import { ref, computed, watch } from "vue";
import { useRouter } from "vue-router";
import { useMutation, useQueryClient } from "@tanstack/vue-query";
import { SwitchRoot, SwitchThumb } from "reka-ui";
import { ArrowLeft, Upload, Plus, Trash2, TriangleAlert } from "lucide-vue-next";
import { useAuth } from "../auth";
import {
  api,
  waitForJob,
  ApiError,
  type Job,
  type Scope,
  type CustomInspectResult,
  type CustomInstallRequest,
  type CustomPermissions,
  type CustomFolderGrant,
  type CustomOverlayRenderResult,
  type CustomOverlayParseResult,
} from "../api";
import SplitButton from "../components/SplitButton.vue";
import Heading from "@/components/ui/Heading.vue";
import Button from "@/components/ui/Button.vue";

const router = useRouter();
const qc = useQueryClient();
const { currentUser, singleUserMode } = useAuth();

// Door 2 is admin-only. The Store hides the affordance from members, but guard
// the route directly too (deep-link, or a role change mid-session).
const isAdmin = computed(() => currentUser.value?.role === "admin");
watch(
  currentUser,
  (u) => {
    if (u && u.role !== "admin") router.replace("/store");
  },
  { immediate: true },
);

// ── form state ────────────────────────────────────────────────────────────────
const compose = ref("");
const name = ref("");
const services = ref<string[]>([]);
const mainService = ref("");
const mainPort = ref<number | undefined>(undefined);
// Once the admin edits the port, inspect stops overwriting their value.
const portTouched = ref(false);

// Permissions (DASHBOARD.md # Permissions): internet defaults on, LAN/GPU off,
// folder grants empty. `devices` has no form control — it's the long tail
// reachable only via the Edit-as-YAML escape hatch — but we hold any the admin
// added there so it survives flipping back to the form.
const internet = ref(true);
const lan = ref(false);
const gpu = ref(false);
const folders = ref<CustomFolderGrant[]>([]);
const devices = ref<string[]>([]);

// Edit-as-YAML escape hatch: "form" renders the toggles/rows; "yaml" replaces
// them with a raw permissions-overlay editor. The brain owns all YAML
// (render/parse endpoints) so the frontend ships no YAML dependency.
const editMode = ref<"form" | "yaml">("form");
const overlayText = ref("");
const overlayError = ref<string | null>(null);

// 422 synthesize/admission rejection (field-named), shown inline against the compose.
const submitError = ref<string | null>(null);

const USE_CASE_FOLDERS = ["photos", "documents", "movies", "music", "notes", "downloads"];
const folderLabel = (f: string) => f.charAt(0).toUpperCase() + f.slice(1);

// Shared field chrome — one source of truth for the form inputs/selects/textarea
// so every field reads the same (inset olive-background fill on the card, olive
// focus ring). Small inline controls (folder rows) use a compact variant below.
const fieldClass =
  "w-full rounded-xl border border-border bg-background px-3 py-2 text-sm outline-none focus:border-accent";

const slug = computed(() =>
  name.value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, ""),
);

// ── inspect (advisory, debounced) ───────────────────────────────────────────────
let inspectTimer: ReturnType<typeof setTimeout> | undefined;

async function runInspect() {
  if (compose.value.trim() === "") {
    services.value = [];
    return;
  }
  try {
    const res = await api.post<CustomInspectResult>("/apps/custom/inspect", {
      compose: compose.value,
      main_service: mainService.value || undefined,
    });
    services.value = res.services;
    if (res.services.length === 1) {
      mainService.value = res.services[0] ?? "";
    } else if (!res.services.includes(mainService.value)) {
      mainService.value = "";
    }
    if (!portTouched.value && res.main_port > 0) {
      mainPort.value = res.main_port;
    }
  } catch {
    // Inspect is advisory: an incomplete or not-yet-valid paste just leaves the
    // dropdown empty and the port asked. Real errors surface on submit.
    services.value = [];
  }
}

watch(compose, () => {
  clearTimeout(inspectTimer);
  inspectTimer = setTimeout(runInspect, 350);
});

// Picking a service re-infers the port from that service's `expose:`/`ports:`.
function onServiceChange() {
  if (!portTouched.value) runInspect();
}

function onFile(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0];
  if (!file) return;
  file.text().then((t) => {
    compose.value = t;
  });
}

// ── permissions ─────────────────────────────────────────────────────────────────
function addFolder() {
  folders.value.push({ folder: "photos", mode: "read", target: "" });
}
function removeFolder(i: number) {
  folders.value.splice(i, 1);
}

// buildPerms projects the form state to the structured election the backend takes.
function buildPerms(): CustomPermissions {
  return {
    internet: internet.value,
    lan: lan.value,
    gpu: gpu.value,
    folders: folders.value
      .filter((f) => f.folder)
      .map((f) => ({ folder: f.folder, mode: f.mode, target: f.target?.trim() || undefined })),
    devices: devices.value.length ? devices.value : undefined,
  };
}

// Flip to YAML: the brain renders the current election as the overlay text.
async function flipToYaml() {
  overlayError.value = null;
  try {
    const res = await api.post<CustomOverlayRenderResult>("/apps/custom/overlay/render", {
      permissions: buildPerms(),
    });
    overlayText.value = res.overlay;
    editMode.value = "yaml";
  } catch (err) {
    overlayError.value = err instanceof ApiError ? err.message : (err as Error).message;
  }
}

// Flip to form: the brain parses + validates the overlay back to form fields. A
// bad target / unknown key / malformed YAML keeps us in YAML mode with the error
// shown inline, so the edit isn't silently lost.
async function flipToForm() {
  overlayError.value = null;
  try {
    const res = await api.post<CustomOverlayParseResult>("/apps/custom/overlay/parse", {
      overlay: overlayText.value,
    });
    const p = res.permissions;
    internet.value = p.internet;
    lan.value = p.lan;
    gpu.value = p.gpu;
    folders.value = (p.folders ?? []).map((f) => ({ folder: f.folder, mode: f.mode ?? "read", target: f.target ?? "" }));
    devices.value = p.devices ?? [];
    editMode.value = "form";
  } catch (err) {
    overlayError.value = err instanceof ApiError ? err.message : (err as Error).message;
  }
}

// ── install ─────────────────────────────────────────────────────────────────────
const install = useMutation({
  mutationFn: async (scope: Scope) => {
    submitError.value = null;
    const body: CustomInstallRequest = {
      name: name.value,
      compose: compose.value,
      main_service: mainService.value || undefined,
      main_port: Number(mainPort.value),
      scope,
    };
    // YAML mode submits the raw overlay (the admin may have edited it past what
    // the form can express); form mode submits the structured election.
    if (editMode.value === "yaml") body.overlay = overlayText.value;
    else body.permissions = buildPerms();
    const job = await api.post<Job>("/apps/custom", body);
    const done = await waitForJob(job.job_id);
    if (done.status !== "completed") {
      throw new Error(done.error?.message ?? "The install didn't finish.");
    }
    return done;
  },
  onSuccess: async () => {
    await qc.invalidateQueries({ queryKey: ["apps"] });
    router.push("/store");
  },
  onError: (err: unknown) => {
    // A 422 carries the brain's field-named synthesize/admission message — coach
    // it inline against the compose, never an opaque toast.
    submitError.value = err instanceof ApiError ? err.message : (err as Error).message;
  },
});

const canSubmit = computed(
  () =>
    compose.value.trim() !== "" &&
    name.value.trim() !== "" &&
    // A multi-service compose forces a service choice; one-or-unknown is fine —
    // the brain infers/validates the rest.
    (services.value.length <= 1 || mainService.value !== "") &&
    Number(mainPort.value) > 0 &&
    !install.isPending.value,
);

// Scope mirrors the store row's split-button convention: the primary action is
// personal; an admin on a multi-user box gets a dropdown to install for the whole
// household. Single-user boxes silently install personal (# Single-user).
const scopeItems = computed(() =>
  isAdmin.value && !singleUserMode.value
    ? [{ label: "Install for the whole household", action: () => install.mutate("household") }]
    : [],
);

const submitLabel = computed(() => (install.isPending.value ? "Installing…" : "Install"));
</script>

<template>
  <div v-if="isAdmin" class="mx-auto w-full max-w-2xl space-y-6 pt-2">
    <!-- Page heading -->
    <header class="space-y-2">
      <RouterLink
        to="/store"
        class="inline-flex items-center gap-1 text-sm text-muted-foreground transition-colors hover:text-foreground"
      >
        <ArrowLeft class="size-4" aria-hidden="true" /> Store
      </RouterLink>
      <Heading :level="2">Install a custom app</Heading>
      <p class="text-sm text-muted-foreground">
        Paste a <code>docker-compose.yml</code> to run an app that isn't in the catalog. It installs into the same
        sandbox as a store app.
      </p>
    </header>

    <!-- 1. Compose: paste or upload -->
    <section class="space-y-4 rounded-2xl border border-border bg-card p-5 sm:p-6">
      <div>
        <h3 class="text-sm font-semibold text-foreground">Compose file</h3>
        <p class="mt-1 text-xs text-muted-foreground">Paste your <code>docker-compose.yml</code>, or upload a file.</p>
      </div>
      <textarea
        id="compose"
        v-model="compose"
        rows="10"
        spellcheck="false"
        placeholder="Paste a docker-compose.yml…"
        :class="[fieldClass, 'font-mono text-xs']"
      />
      <label
        class="inline-flex cursor-pointer items-center gap-1.5 rounded-full border border-border bg-card px-3 py-1 text-sm/7 font-medium text-foreground transition-colors hover:bg-muted"
      >
        <Upload class="size-4" aria-hidden="true" /> Upload file
        <input type="file" accept=".yml,.yaml,text/yaml,text/plain" class="sr-only" @change="onFile" />
      </label>
      <!-- 422 synthesize/admission coaching, inline against the offending compose -->
      <p
        v-if="submitError"
        class="flex gap-2 rounded-xl border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
      >
        <TriangleAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <span>{{ submitError }}</span>
      </p>
    </section>

    <!-- 2. Details: name + main service + port -->
    <section class="space-y-4 rounded-2xl border border-border bg-card p-5 sm:p-6">
      <h3 class="text-sm font-semibold text-foreground">Details</h3>

      <!-- App name + live URL preview -->
      <div class="space-y-1.5">
        <label class="text-sm font-medium" for="name">App name</label>
        <input id="name" v-model="name" placeholder="My App" :class="fieldClass" />
        <p class="text-xs text-muted-foreground">
          It'll be reachable at <code>{{ slug || "your-app" }}.local</code>
        </p>
      </div>

      <!-- Main service — auto when one, required dropdown when several -->
      <div v-if="services.length > 1" class="space-y-1.5">
        <label class="text-sm font-medium" for="main-service">Main service</label>
        <select id="main-service" v-model="mainService" :class="fieldClass" @change="onServiceChange">
          <option value="" disabled>Choose the service the dashboard opens…</option>
          <option v-for="s in services" :key="s" :value="s">{{ s }}</option>
        </select>
        <p class="text-xs text-muted-foreground">This compose has several services — pick the one users open.</p>
      </div>
      <p v-else-if="services.length === 1" class="text-xs text-muted-foreground">
        Main service: <code>{{ services[0] }}</code> (auto-detected)
      </p>

      <!-- Main port -->
      <div class="space-y-1.5">
        <label class="text-sm font-medium" for="main-port">Main port</label>
        <input
          id="main-port"
          v-model.number="mainPort"
          type="number"
          min="1"
          max="65535"
          placeholder="e.g. 8080"
          :class="[fieldClass, 'w-40']"
          @input="portTouched = true"
        />
        <p class="text-xs text-muted-foreground">
          The port your app listens on <em>inside</em> the container — check the image's docs. We prefill it from the
          compose when we can; always double-check.
        </p>
      </div>
    </section>

    <!-- 3. Permissions — form toggles/rows, or the Edit-as-YAML escape hatch -->
    <section class="space-y-4 rounded-2xl border border-border bg-card p-5 sm:p-6">
      <div class="flex items-center justify-between">
        <h3 class="text-sm font-semibold text-foreground">Permissions</h3>
        <button
          type="button"
          class="text-xs text-muted-foreground transition-colors hover:text-foreground"
          @click="editMode === 'form' ? flipToYaml() : flipToForm()"
        >
          {{ editMode === "form" ? "Edit as YAML" : "Back to form" }}
        </button>
      </div>

      <p
        v-if="overlayError"
        class="flex gap-2 rounded-xl border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
      >
        <TriangleAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <span>{{ overlayError }}</span>
      </p>

      <!-- Form view -->
      <template v-if="editMode === 'form'">
        <!-- Access toggles as a bordered list (Tailwind toggle-list idiom). -->
        <div class="divide-y divide-border overflow-hidden rounded-xl border border-border">
          <!-- Internet -->
          <div class="flex items-start justify-between gap-4 p-4">
            <div class="min-w-0">
              <div class="text-sm font-medium">Internet access</div>
              <div class="text-xs text-muted-foreground">Let this app reach the internet. On by default for custom apps.</div>
            </div>
            <SwitchRoot
              v-model="internet"
              aria-label="Internet access"
              class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors data-[state=checked]:border-accent data-[state=checked]:bg-accent"
            >
              <SwitchThumb
                class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
              />
            </SwitchRoot>
          </div>

          <!-- LAN / mDNS -->
          <div class="flex items-start justify-between gap-4 p-4">
            <div class="min-w-0">
              <div class="text-sm font-medium">Local network access</div>
              <div class="text-xs text-muted-foreground">Let this app reach other devices on your LAN. Off by default.</div>
            </div>
            <SwitchRoot
              v-model="lan"
              aria-label="Local network access"
              class="relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border border-border bg-muted outline-none transition-colors data-[state=checked]:border-accent data-[state=checked]:bg-accent"
            >
              <SwitchThumb
                class="pointer-events-none block size-4 translate-x-0.5 rounded-full bg-card shadow transition-transform data-[state=checked]:translate-x-[1.125rem]"
              />
            </SwitchRoot>
          </div>
          <!-- GPU access is intentionally not offered yet — GPU passthrough isn't
               supported (issue #125, blocked). The `gpu` election stays false; the
               YAML escape hatch remains the only way to set it for tinkerers. -->
        </div>

        <!-- Folder grants -->
        <div class="space-y-3">
          <div>
            <div class="text-sm font-medium">Folder access</div>
            <div class="mt-1 text-xs text-muted-foreground">
              Give the app one of your content folders. <strong>Source</strong> is the folder on your box;
              <strong>destination</strong> is where the app reads it inside the container (check the image's docs).
            </div>
          </div>
          <div v-for="(f, i) in folders" :key="i" class="flex items-center gap-2">
            <select
              v-model="f.folder"
              aria-label="Source folder"
              class="rounded-lg border border-border bg-background px-2 py-1.5 text-sm outline-none focus:border-accent"
            >
              <option v-for="uc in USE_CASE_FOLDERS" :key="uc" :value="uc">{{ folderLabel(uc) }}</option>
            </select>
            <span class="text-xs text-muted-foreground">→</span>
            <input
              v-model="f.target"
              placeholder="/path/in/container"
              aria-label="Destination path"
              class="min-w-0 flex-1 rounded-lg border border-border bg-background px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
            />
            <select
              v-model="f.mode"
              aria-label="Mode"
              class="rounded-lg border border-border bg-background px-2 py-1.5 text-sm outline-none focus:border-accent"
            >
              <option value="read">read</option>
              <option value="write">write</option>
            </select>
            <Button
              variant="ghost"
              size="icon"
              type="button"
              aria-label="Remove folder"
              class="text-muted-foreground hover:text-destructive"
              @click="removeFolder(i)"
            >
              <Trash2 class="size-4" aria-hidden="true" />
            </Button>
          </div>
          <Button variant="secondary" size="sm" type="button" @click="addFolder">
            <Plus class="size-4" aria-hidden="true" /> Add a folder
          </Button>
        </div>

        <!-- GPU has no form control yet (unsupported, #125) but round-trips through
             the YAML overlay; surface it read-only so a YAML-set flag isn't silently
             submitted with no confirmation in the form (mirrors devices below). -->
        <p v-if="gpu" class="text-xs text-muted-foreground">GPU access (set via YAML): <code>enabled</code></p>

        <!-- Devices: no form control (the long tail); shown read-only when set via YAML -->
        <p v-if="devices.length" class="text-xs text-muted-foreground">
          Devices (set via YAML): <code>{{ devices.join(", ") }}</code>
        </p>
      </template>

      <!-- YAML view: a raw editor over the permissions overlay only (the compose
           keeps its own textarea above — the two are never merged). -->
      <template v-else>
        <textarea
          v-model="overlayText"
          rows="10"
          spellcheck="false"
          aria-label="Permissions overlay (YAML)"
          :class="[fieldClass, 'font-mono text-xs']"
        />
        <p class="text-xs text-muted-foreground">
          The manifest overlay moose wraps around your compose. Edit fields the form doesn't show (like
          <code>devices</code>); the compose above is untouched. Admission still runs on install.
        </p>
      </template>
    </section>

    <!-- TOFU / no-auto-update honesty note -->
    <p class="flex gap-2 rounded-xl border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
      <TriangleAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      <span>
        moose pins the <strong>exact image it pulls now</strong> and won't change it underneath you — a custom app
        <strong>does not auto-update</strong>. To move to a newer image, uninstall and paste again.
      </span>
    </p>

    <!-- 4. Scope + submit (store split-button convention) -->
    <div class="flex items-center gap-3">
      <SplitButton
        :label="submitLabel"
        :disabled="!canSubmit"
        :items="scopeItems"
        @click="install.mutate('personal')"
      />
      <RouterLink to="/store" class="text-sm text-muted-foreground transition-colors hover:text-foreground">
        Cancel
      </RouterLink>
    </div>
  </div>
</template>
