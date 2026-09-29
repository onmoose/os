<script setup lang="ts">
// An AI service's logo in a rounded square, for Settings → Integrations →
// AI services. The logo URL is a box route that proxies the catalog's image
// (INSTALL_SETUP.md # 4). A logo that fails to load, a provider with none, or
// no provider at all (it left the provider data) falls back to an icon: a
// server for Other, a bot for the rest.
//
// The dashboard has a light theme only today. When a dark theme lands it puts
// the `dark` class on <html>, and the dark logo is used then, as on the setup
// page (AISlotPicker).
import { computed, ref, watch } from "vue";
import { Bot, Server } from "lucide-vue-next";
import type { AIProvider } from "../api";
import { isOther } from "../aiProviders";

const props = defineProps<{ provider?: AIProvider; other?: boolean }>();

const broken = ref(false);
watch(
  () => props.provider?.id,
  () => (broken.value = false),
);

const darkTheme = document.documentElement.classList.contains("dark");
const src = computed(() => {
  const p = props.provider;
  if (!p || broken.value) return undefined;
  return (darkTheme && p.logo_dark_url) || p.logo_url;
});
const icon = computed(() => (props.other || (props.provider && isOther(props.provider)) ? Server : Bot));
</script>

<template>
  <span class="flex size-10 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
    <img v-if="src" :src="src" :alt="provider?.name ?? ''" class="size-6 object-contain" @error="broken = true" />
    <component :is="icon" v-else class="size-5 stroke-[1.5]" aria-hidden="true" />
  </span>
</template>
