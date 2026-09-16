<script setup lang="ts">
// Settings → About: what this box is, and what it is running. Reads
// GET /api/v1/system/version (#375): the brain's version + commit, the
// host-agent version, and the UI image the box was launched with.
//
// Two levels on purpose (UPDATES.md # 6). The version alone is the fact a
// non-technical owner might read out when asking for help, so it sits on the
// front of the card. Commit, host-agent and UI image are for someone reading a
// support thread, so they hide behind a closed disclosure and the card stays
// calm.
//
// The endpoint answers 200 with parts missing when a source can't be read (a
// version report missing one of three parts is still useful; see internal/api/
// system.go). So each part degrades to "Unknown" on its own; one unreadable
// source never blanks the card.
import { computed } from "vue";
import { useQuery } from "@tanstack/vue-query";
import { api, type SystemVersion } from "@/api";

const version = useQuery({
  queryKey: ["system-version"],
  queryFn: () => api.get<SystemVersion>("/system/version"),
});

const v = computed<SystemVersion | undefined>(() => version.data.value);

// The brain's own version is the box's version (one version for the whole
// monorepo, see BUILD.md # Versioning).
const boxVersion = computed(() => v.value?.version || "");

// A dev build stamps "dev" rather than a SemVer; show it as-is, it is the truth.
const commitShort = computed(() => {
  const c = v.value?.commit ?? "";
  return c.length > 12 ? c.slice(0, 12) : c;
});

// The image ref is long and its digest is the part that identifies it. Keep the
// whole string in a title attribute and show the tail here.
const uiImage = computed(() => v.value?.ui_image ?? "");
</script>

<template>
  <section class="space-y-3">
    <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">About</h2>
    <div class="space-y-2 rounded-xl border border-border bg-card px-4 py-4">
      <div class="flex items-baseline gap-2">
        <span class="text-base font-medium">moose</span>
        <span v-if="version.isLoading.value" class="text-sm text-muted-foreground">Checking…</span>
        <span v-else-if="boxVersion" class="text-sm text-muted-foreground">{{ boxVersion }}</span>
        <span v-else class="text-sm text-muted-foreground">Version unknown</span>
      </div>
      <p class="text-sm text-muted-foreground">
        A home-server OS for people who want to own their data without becoming sysadmins.
      </p>

      <p v-if="version.isError.value" class="text-sm text-destructive">
        Couldn't read what this box is running.
      </p>

      <details v-else-if="!version.isLoading.value" class="group">
        <summary class="cursor-pointer text-sm text-muted-foreground hover:text-foreground">
          Technical details
        </summary>
        <dl class="mt-2 space-y-1 text-sm">
          <div class="flex gap-2">
            <dt class="w-28 shrink-0 text-muted-foreground">Commit</dt>
            <dd class="font-mono" :title="v?.commit || ''">{{ commitShort || "Unknown" }}</dd>
          </div>
          <div class="flex gap-2">
            <dt class="w-28 shrink-0 text-muted-foreground">Host agent</dt>
            <dd>{{ v?.host_agent_version || "Unknown" }}</dd>
          </div>
          <div class="flex gap-2">
            <dt class="w-28 shrink-0 text-muted-foreground">Dashboard</dt>
            <dd class="break-all font-mono" :title="uiImage">{{ uiImage || "Unknown" }}</dd>
          </div>
        </dl>
      </details>

      <a
        href="https://github.com/onmoose/moose"
        target="_blank"
        rel="noopener noreferrer"
        class="inline-block text-sm text-accent hover:underline"
      >
        Project on GitHub →
      </a>
    </div>
  </section>
</template>
