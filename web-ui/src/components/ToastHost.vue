<script setup lang="ts">
// ToastHost renders the app-wide ephemeral toast stack (WEB_UI.md's toast
// model). Mounted once in AppShell; reads the singleton list from toasts.ts and
// renders it bottom-right, clear of the centered dock. Toasts carry a variant
// (error / success) that colors the accent; the region stays assertive so
// screen readers announce failures promptly.
import { RouterLink } from "vue-router";
import { useToasts, dismissToast } from "../toasts";

const { toasts } = useToasts();
</script>

<template>
  <div class="toast-host" role="alert" aria-live="assertive">
    <div v-for="t in toasts" :key="t.id" class="toast" :data-variant="t.variant">
      <span class="toast-msg">
        {{ t.message }}
        <RouterLink v-if="t.link" :to="t.link.to" class="toast-link" @click="dismissToast(t.id)">{{ t.link.label }}</RouterLink>
      </span>
      <button class="toast-close" aria-label="Dismiss" @click="dismissToast(t.id)">×</button>
    </div>
  </div>
</template>

<style scoped>
/* Empty region ignores pointer events so it never blocks the UI behind it;
   each toast re-enables them so its dismiss button stays clickable. */
.toast-host {
  position: fixed;
  right: 1rem;
  bottom: 1rem;
  z-index: 100;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  width: max-content;
  max-width: min(92vw, 22rem);
  pointer-events: none;
}
.toast {
  display: flex;
  align-items: flex-start;
  gap: 0.6rem;
  padding: 0.6rem 0.85rem;
  border-radius: var(--radius);
  box-shadow: 0 8px 28px rgba(0, 0, 0, 0.12);
  pointer-events: auto;
}
/* Tinted panels: the muted status token mixed into the card surface for the fill
   and border, the solid token for the accent stripe and text — so the toasts sit
   in the olive palette while error/success stay legible signals. */
.toast[data-variant="error"] {
  background: color-mix(in oklch, var(--color-destructive) 12%, var(--color-card));
  border: 1px solid color-mix(in oklch, var(--color-destructive) 35%, var(--color-card));
  border-left: 3px solid var(--color-destructive);
}
.toast[data-variant="success"] {
  background: color-mix(in oklch, var(--color-success) 12%, var(--color-card));
  border: 1px solid color-mix(in oklch, var(--color-success) 35%, var(--color-card));
  border-left: 3px solid var(--color-success);
}
.toast-msg {
  flex: 1;
  font-size: 0.82rem;
  line-height: 1.3;
}
.toast-link {
  margin-left: 0.35rem;
  text-decoration: underline;
  text-underline-offset: 2px;
}
.toast[data-variant="error"] .toast-msg { color: var(--color-destructive); }
.toast[data-variant="success"] .toast-msg { color: var(--color-success); }
.toast-close {
  flex: 0 0 auto;
  padding: 0 0.1rem;
  border: none;
  background: none;
  font-size: 1.1rem;
  line-height: 1;
  cursor: pointer;
}
.toast[data-variant="error"] .toast-close { color: color-mix(in oklch, var(--color-destructive) 55%, var(--color-card)); }
.toast[data-variant="error"] .toast-close:hover { color: var(--color-destructive); }
.toast[data-variant="success"] .toast-close { color: color-mix(in oklch, var(--color-success) 55%, var(--color-card)); }
.toast[data-variant="success"] .toast-close:hover { color: var(--color-success); }
</style>
