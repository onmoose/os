<script setup lang="ts">
// Provider logo for the outgoing-email picker (Settings → Outgoing email).
//
// Files live in assets/mail-providers/ and are matched by preset id, so adding
// a provider logo is adding a file — no change here. See the README in that
// folder for the naming rule and what makes a good file. They are bundled
// rather than hotlinked because a box may have no internet, and the dashboard
// must not call a CDN when it renders.
//
// A provider with no file falls back to a lettermark tile, which reads as a
// logo slot rather than as a missing image. "Custom SMTP server" is not a brand
// at all, so it gets the Lucide server glyph.
import { computed } from "vue";
import { Server } from "lucide-vue-next";

const props = withDefaults(
  defineProps<{
    id: string;
    label: string;
    // card: the picker tile, logo above the name. inline: next to a heading or
    // in a list row, where a wide wordmark has to stay narrow. icon: the 40px
    // icon square of an OptionCards card; the logo is scaled down to fit inside
    // it, so a wide wordmark gets small rather than spilling out.
    size?: "card" | "inline" | "icon";
  }>(),
  { size: "card" },
);

// Eagerly globbed so every logo is hashed into the build and resolvable by id.
// The key is the filename without its extension, which is the preset id.
const files = import.meta.glob<string>("../assets/mail-providers/*.{svg,png,jpg,jpeg,webp}", {
  eager: true,
  query: "?url",
  import: "default",
});

const byID: Record<string, string> = Object.fromEntries(
  Object.entries(files).map(([path, url]) => [path.split("/").pop()!.replace(/\.[^.]+$/, ""), url]),
);

// Initials for a provider with no file. Two characters at most — "S2" reads as
// a mark, "SMTP2GO" reads as truncated text.
const letters: Record<string, string> = {
  ses: "SE",
  sendgrid: "SG",
  mailgun: "MG",
  postmark: "P",
  brevo: "B",
  resend: "R",
  smtp2go: "S2",
  google_workspace: "W",
};

const src = computed(() => byID[props.id]);
const letter = computed(() => letters[props.id]);

// A fixed-height box with object-contain, never a fixed square: the folder
// holds both square icons and wide wordmarks, and whatever gets dropped in
// later has to render without being stretched or cropped.
const boxClass = computed(() => {
  if (props.size === "card") return "h-9 max-w-[8rem] object-contain";
  if (props.size === "icon") return "max-h-10 max-w-10 object-contain";
  return "h-6 max-w-20 object-contain";
});
const tileClass = computed(() => {
  if (props.size === "card" || props.size === "icon") {
    return "flex h-9 w-9 items-center justify-center rounded-lg bg-muted text-xs font-semibold text-muted-foreground";
  }
  return "flex h-6 w-6 items-center justify-center rounded-md bg-muted text-[0.625rem] font-semibold text-muted-foreground";
});
</script>

<template>
  <img v-if="src" :src="src" :alt="label" :class="boxClass" loading="lazy" decoding="async" />
  <span v-else-if="letter" :class="tileClass" role="img" :aria-label="label">{{ letter }}</span>
  <Server v-else :class="size === 'inline' ? 'size-5 stroke-[1.5]' : 'size-8 stroke-[1.5]'" aria-hidden="true" />
</template>
