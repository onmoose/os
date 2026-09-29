<script setup lang="ts">
// The info box of the install flow (INSTALL_STEPS.md # 2). It belongs to the
// app header: it sits right under the app's name on the first page and the
// last page only. It is quiet on purpose: grey, small text, no controls.
//
// The size comes first, as its own line ("Takes about 2.0 GB."), with the
// not-enough-space warning under it. Then a title ("What OpenClaw can do")
// and one line per permission. Write access to a folder stays red, because
// that is the one line people must notice (APP_ISOLATION.md # User content).
// An app with no permissions says so in one line, with no empty list, so the
// box is never empty: the size is 0 when the app's images are already on the
// box, and then the size line is left out.
import { computed } from "vue";
import { TriangleAlert } from "lucide-vue-next";
import type { InstallPlanFootprint, InstallPlanPermissions } from "../../api";
import { formatSize } from "../../utils";
import { permissionLines, spaceTight } from "../../installSteps";

const props = defineProps<{ appName: string; permissions: InstallPlanPermissions; footprint?: InstallPlanFootprint }>();

// The words are shared with the App page's Permissions group.
const lines = computed(() => permissionLines(props.permissions));
const size = computed(() => props.footprint?.image_disk_bytes ?? 0);
const tight = computed(() => spaceTight(props.footprint));
</script>

<template>
  <div class="space-y-2 rounded-lg bg-muted px-4 py-3 text-sm text-muted-foreground">
    <p v-if="size > 0">Takes about {{ formatSize(size) }}.</p>
    <p v-if="tight && footprint" class="flex gap-2 text-destructive">
      <TriangleAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      This might not fit. Only about {{ formatSize(footprint.free_bytes) }} is free on your box. You can still install.
    </p>
    <template v-if="lines.length > 0">
      <p class="font-medium text-foreground">What {{ appName }} can do</p>
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
    <p v-else>{{ appName }} needs no special permissions.</p>
  </div>
</template>
