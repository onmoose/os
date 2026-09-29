<script setup lang="ts">
// The info box of the install flow (INSTALL_STEPS.md # 2): what the app can
// do, as plain sentences, and how much space it takes. It is quiet on
// purpose: grey, small text, no controls. It shows on the first page and the
// last page only. Write access to a folder stays red, because that is the one
// line people must notice (APP_ISOLATION.md # User content). The
// not-enough-space line is in it on both pages.
import { computed } from "vue";
import { TriangleAlert } from "lucide-vue-next";
import type { InstallPlanFootprint, InstallPlanPermissions } from "../../api";
import { formatSize } from "../../utils";
import { folderName, spaceTight } from "../../installSteps";

const props = defineProps<{ permissions: InstallPlanPermissions; footprint?: InstallPlanFootprint }>();

const devices = computed(() => props.permissions.devices ?? []);
const folders = computed(() => props.permissions.folders ?? []);
const nothing = computed(
  () =>
    !props.permissions.internet &&
    !props.permissions.lan &&
    !props.permissions.gpu &&
    devices.value.length === 0 &&
    folders.value.length === 0,
);
const size = computed(() => props.footprint?.image_disk_bytes ?? 0);
const tight = computed(() => spaceTight(props.footprint));
</script>

<template>
  <div class="space-y-2 rounded-lg bg-muted px-4 py-3 text-sm text-muted-foreground">
    <p v-if="nothing">It needs no special permissions.</p>
    <ul v-else class="list-disc space-y-0.5 pl-5">
      <li v-if="permissions.internet">It can connect to the internet.</li>
      <li v-if="permissions.lan">It can reach other devices on your network.</li>
      <li v-if="permissions.gpu">It can use the graphics card.</li>
      <li v-for="d in devices" :key="d">It can use the device {{ d }}.</li>
      <li
        v-for="f in folders"
        :key="f.folder"
        :class="f.mode === 'write' ? 'font-medium text-destructive marker:text-destructive' : ''"
      >
        <template v-if="f.mode === 'write'">It can add, change, and delete files in {{ folderName(f.folder) }}.</template>
        <template v-else>It can read files in {{ folderName(f.folder) }}.</template>
      </li>
    </ul>
    <p v-if="size > 0">Takes about {{ formatSize(size) }}.</p>
    <p v-if="tight && footprint" class="flex gap-2 text-destructive">
      <TriangleAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
      This might not fit. Only about {{ formatSize(footprint.free_bytes) }} is free on your box. You can still install.
    </p>
  </div>
</template>
