<script setup lang="ts">
// Settings → About: what moose is. The versions this box runs, and its
// updates, are on Settings → System (#488), so there is one version display,
// not two.
import { RouterLink } from "vue-router";
</script>

<template>
  <section class="space-y-3">
    <h2 class="text-xs font-medium uppercase tracking-wide text-muted-foreground">About</h2>
    <div class="space-y-2 rounded-xl border border-border bg-card px-4 py-4">
      <p class="text-base font-medium">moose</p>
      <p class="text-sm text-muted-foreground">
        A home-server OS for people who want to own their data without becoming sysadmins.
      </p>
      <RouterLink to="/settings/system" class="block text-sm text-accent hover:underline">
        Version and updates →
      </RouterLink>
      <a
        href="https://github.com/onmoose/os"
        target="_blank"
        rel="noopener noreferrer"
        class="inline-block text-sm text-accent hover:underline"
      >
        Project on GitHub →
      </a>
    </div>
  </section>
</template>
