<script setup lang="ts">
// Install progress page (/store/:id/install/:jobId), step 1 of INSTALL_SETUP.md.
// The setup page lands here once POST /api/v1/apps returns its job. The job id
// is in the URL, so a reload picks the install up again by polling
// GET /api/v1/jobs/:id. Jobs live in the brain's memory only, so a brain
// restart forgets the job; the page then says so instead of spinning.
//
// The brain reports a fine-grained `step` (internal/lifecycle/lifecycle.go).
// The page folds those ~15 steps into four phases, in the order the brain runs
// them, and draws them as the Tailwind Plus "circles with text" step list. It
// ends with an Open button, or a plain error with a way back.
import { computed, ref, watch } from "vue";
import { useRoute, RouterLink } from "vue-router";
import { useQuery, useQueryClient } from "@tanstack/vue-query";
import { ArrowLeft, Check, X } from "lucide-vue-next";
import { api, ApiError, type CatalogDetail, type Instance, type Job } from "../api";
import Button from "../components/ui/Button.vue";
import Heading from "../components/ui/Heading.vue";

const route = useRoute();
const qc = useQueryClient();

const manifestId = computed(() => String(route.params.id));
const jobId = computed(() => String(route.params.jobId));
// The setup page puts ?scope=household on this URL for a household install, so
// "Try again" returns to the same setup page rather than a personal one.
const retryTo = computed(() => ({
  path: `/store/${manifestId.value}/install`,
  query: route.query.scope === "household" ? { scope: "household" } : {},
}));

// Same key as the detail page, so the name is usually a cache read.
const detailQuery = useQuery({
  queryKey: computed(() => ["catalog", manifestId.value]),
  queryFn: () => api.get<CatalogDetail>(`/catalog/${encodeURIComponent(manifestId.value)}`),
});
const appName = computed(() => detailQuery.data.value?.name ?? "the app");

function isRunning(j: Job | undefined) {
  return !j || j.status === "running" || j.status === "cancelling";
}

const jobQuery = useQuery({
  queryKey: computed(() => ["job", jobId.value]),
  queryFn: () => api.get<Job>(`/jobs/${encodeURIComponent(jobId.value)}`),
  refetchInterval: (q) => (isRunning(q.state.data) && !q.state.error ? 600 : false),
  // A 404 means the brain no longer knows the job. Asking again won't help.
  retry: (count, err) => !(err instanceof ApiError && err.status === 404) && count < 3,
  refetchOnWindowFocus: false,
});
const job = computed(() => jobQuery.data.value);
const lost = computed(() => jobQuery.error.value instanceof ApiError && jobQuery.error.value.status === 404);

const done = computed(() => job.value?.status === "completed");
const failed = computed(() => !!job.value && !isRunning(job.value) && !done.value);

// ── Phases ──────────────────────────────────────────────────────────────────
// Listed in the order the brain runs its steps. resolving_digests pulls the
// images, so it is the download. Wording stays here, not in the brain.
const PHASES = [
  { label: "Preparing", hint: "Checking the app and making room for it." },
  { label: "Downloading", hint: "Getting the app's files. This can take a few minutes." },
  { label: "Setting up", hint: "Settings, folders, and the app's address." },
  { label: "Starting", hint: "Waiting for the app to answer." },
];
const STEP_PHASE: Record<string, number> = {
  admitting_compose: 0,
  checking_gpu: 0,
  allocating_slug: 0,
  writing_instance_dir: 0,
  resolving_digests: 1,
  generating_secrets: 2,
  provisioning_services: 2,
  binding_mail_provider: 2,
  binding_ai_accounts: 2,
  generating_override: 2,
  creating_network: 2,
  publishing_mdns: 2,
  registering_route: 2,
  compose_up: 3,
  waiting_healthy: 3,
  flipping_route: 3,
};

// reached never goes back. An unknown step (a new one the brain added) keeps
// the last known phase rather than jumping around.
const reached = ref(0);
watch(jobId, () => {
  reached.value = 0;
});
watch(
  () => job.value?.step,
  (step) => {
    const i = step ? STEP_PHASE[step] : undefined;
    if (i !== undefined && i > reached.value) reached.value = i;
  },
  { immediate: true },
);

type PhaseState = "complete" | "current" | "failed" | "upcoming";
function phaseState(i: number): PhaseState {
  if (done.value) return "complete";
  if (i < reached.value) return "complete";
  if (i === reached.value) return failed.value ? "failed" : "current";
  return "upcoming";
}

// ── The finished app ────────────────────────────────────────────────────────
// Refresh the apps list once the job ends, so the new instance (and its URL)
// is there for the Open button, and Home shows the right state.
watch(
  () => job.value?.status,
  (s) => {
    if (s && !isRunning(job.value)) qc.invalidateQueries({ queryKey: ["apps"] });
  },
);
const apps = useQuery({
  queryKey: ["apps"],
  queryFn: () => api.get<{ apps: Instance[] }>("/apps"),
  enabled: done,
});
const instance = computed<Instance | undefined>(() => {
  const id = job.value?.result?.instance_id;
  return typeof id === "string" ? apps.data.value?.apps.find((a) => a.id === id) : undefined;
});

const errorText = computed(() => {
  if (job.value?.status === "cancelled") return "The install was cancelled.";
  return job.value?.error?.message ?? "The install didn't finish.";
});
</script>

<template>
  <div class="mx-auto w-full max-w-xl space-y-8 px-4 pt-2 pb-10 sm:px-0">
    <RouterLink
      :to="`/store/${manifestId}`"
      class="inline-flex items-center gap-1 text-sm text-muted-foreground transition-colors hover:text-foreground"
    >
      <ArrowLeft class="size-4" aria-hidden="true" /> {{ detailQuery.data.value?.name ?? "Back" }}
    </RouterLink>

    <header>
      <Heading :level="2">
        {{ done ? `${appName} is ready` : failed ? `${appName} didn't install` : `Installing ${appName}` }}
      </Heading>
    </header>

    <!-- The brain forgot the job, most likely a restart. The instance may
         still exist, so point at Home rather than guess. -->
    <div v-if="lost" class="space-y-3">
      <p class="text-sm text-muted-foreground">
        This install is no longer being tracked. moose may have restarted while it ran. Check Home to see if the app is
        there.
      </p>
      <div class="flex flex-wrap gap-2">
        <Button :as="RouterLink" to="/">Go to Home</Button>
        <Button variant="secondary" :as="RouterLink" :to="`/store/${manifestId}`">Back to the app</Button>
      </div>
    </div>

    <p v-else-if="jobQuery.isError.value" class="text-sm text-destructive">
      Couldn't check on the install. {{ (jobQuery.error.value as Error)?.message }}
    </p>

    <template v-else>
      <nav aria-label="Install progress">
        <ol role="list" class="overflow-hidden">
          <li v-for="(p, i) in PHASES" :key="p.label" class="relative" :class="i < PHASES.length - 1 ? 'pb-10' : ''">
            <div
              v-if="i < PHASES.length - 1"
              aria-hidden="true"
              class="absolute top-4 left-4 mt-0.5 -ml-px h-full w-0.5"
              :class="phaseState(i) === 'complete' ? 'bg-accent' : 'bg-border'"
            />
            <div class="relative flex items-start" :aria-current="phaseState(i) === 'current' ? 'step' : undefined">
              <span class="flex h-9 items-center" aria-hidden="true">
                <span
                  v-if="phaseState(i) === 'complete'"
                  class="relative z-10 flex size-8 items-center justify-center rounded-full bg-accent"
                >
                  <Check class="size-4 text-accent-foreground" />
                </span>
                <span
                  v-else-if="phaseState(i) === 'failed'"
                  class="relative z-10 flex size-8 items-center justify-center rounded-full bg-destructive"
                >
                  <X class="size-4 text-white" />
                </span>
                <span
                  v-else-if="phaseState(i) === 'current'"
                  class="relative z-10 flex size-8 items-center justify-center rounded-full border-2 border-accent bg-background"
                >
                  <span class="size-2.5 animate-pulse rounded-full bg-accent" />
                </span>
                <span
                  v-else
                  class="relative z-10 flex size-8 items-center justify-center rounded-full border-2 border-border bg-background"
                />
              </span>
              <span class="ml-4 flex min-w-0 flex-col">
                <span
                  class="text-sm font-medium"
                  :class="{
                    'text-foreground': phaseState(i) === 'complete' || phaseState(i) === 'current',
                    'text-destructive': phaseState(i) === 'failed',
                    'text-muted-foreground': phaseState(i) === 'upcoming',
                  }"
                >{{ p.label }}</span>
                <span class="text-sm text-muted-foreground">{{ p.hint }}</span>
              </span>
            </div>
          </li>
        </ol>
      </nav>

      <!-- Screen readers hear the outcome once, not every poll. -->
      <div aria-live="polite">
        <div v-if="done" class="space-y-3">
          <p class="text-sm text-muted-foreground">{{ appName }} is installed and running.</p>
          <div class="flex flex-wrap gap-2">
            <Button v-if="instance" :as="'a'" :href="instance.url" target="_blank" rel="noopener">
              Open {{ appName }}
            </Button>
            <Button variant="secondary" :as="RouterLink" to="/">Go to Home</Button>
          </div>
        </div>

        <div v-else-if="failed" class="space-y-3">
          <p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive" role="alert">{{ errorText }}</p>
          <div class="flex flex-wrap gap-2">
            <Button :as="RouterLink" :to="retryTo">Try again</Button>
            <Button variant="secondary" :as="RouterLink" :to="`/store/${manifestId}`">Back to the app</Button>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>
