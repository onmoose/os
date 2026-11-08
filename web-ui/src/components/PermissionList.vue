<script setup lang="ts">
// One line per permission, in the install screen's words (permissionLines,
// wantLines in installSteps.ts). Write access to a folder is red, because that
// is the one line people must notice (APP_ISOLATION.md # User content). The
// install flow's info box, the update prompt, the app's settings page and
// Settings → Users all draw their lists with it, so they look the same.
import type { PermissionLine } from "@/installSteps";

defineProps<{ lines: PermissionLine[] }>();
</script>

<template>
  <ul class="list-disc space-y-0.5 pl-5">
    <li
      v-for="l in lines"
      :key="l.key"
      :class="l.danger ? 'font-medium text-destructive marker:text-destructive' : ''"
    >
      {{ l.text }}
    </li>
  </ul>
</template>
