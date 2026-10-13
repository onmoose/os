<script setup lang="ts">
// A yellow alert with a title and a description (Tailwind UI # Alerts # With
// description). The description is the default slot. role is "status" for a
// warning the user may go past, "alert" for one that stops them.
import { TriangleAlert } from "lucide-vue-next";

withDefaults(defineProps<{ title: string; role?: "status" | "alert" }>(), { role: "status" });
</script>

<template>
  <div class="rounded-md bg-yellow-50 p-4" :role="role">
    <div class="flex">
      <div class="shrink-0">
        <TriangleAlert class="size-5 text-yellow-400" aria-hidden="true" />
      </div>
      <div class="ml-3">
        <h3 class="text-sm font-medium text-yellow-800">{{ title }}</h3>
        <div class="mt-2 space-y-1 text-sm text-yellow-700">
          <slot />
        </div>
      </div>
    </div>
  </div>
</template>
