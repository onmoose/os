// toasts.ts — the app-wide ephemeral toast channel (WEB_UI.md's toast model:
// ephemeral, in-tab, non-blocking). A module-singleton list plus an imperative
// push, the same shape as auth.ts's singleton state: any module can call
// pushErrorToast / pushSuccessToast and <ToastHost> renders the live list once
// in the signed-in shell. No per-call-site UI (issue #44).
//
// Two variants: error (a mutation rolled back silently and this is the only
// "something failed" feedback) and success (a confirmation/recovery notice —
// e.g. a health issue cleared, HEALTH.md # Clear "no silent auto-recovery";
// issue #12). The 409 "View" toast is still unbuilt — it extends this channel
// when it lands.
import { reactive } from "vue";

export type ToastVariant = "error" | "success";

// ToastLink is an optional link on a toast: the overnight-updates toast links
// to the app's page, or the apps list (UPDATES.md # 6 Post-update toast, #634).
export interface ToastLink {
  label: string;
  to: string;
}

export interface Toast {
  id: number;
  message: string;
  variant: ToastVariant;
  link?: ToastLink;
}

// Live toasts, newest last. Reactive so <ToastHost> re-renders on push/dismiss.
const toasts = reactive<Toast[]>([]);

// How long a toast lingers before auto-dismissing — long enough to read a short
// failure line, short enough to stay out of the way.
const TOAST_TTL_MS = 6000;

let seq = 0;

function push(message: string, variant: ToastVariant, link?: ToastLink) {
  const id = ++seq;
  toasts.push({ id, message, variant, link });
  setTimeout(() => dismissToast(id), TOAST_TTL_MS);
}

export function pushErrorToast(message: string) {
  push(message, "error");
}

// pushSuccessToast: a confirmation/recovery notice (a health issue cleared,
// HEALTH.md # Clear). <ToastHost> styles it green and announces it politely.
export function pushSuccessToast(message: string, link?: ToastLink) {
  push(message, "success", link);
}

export function dismissToast(id: number) {
  const i = toasts.findIndex((t) => t.id === id);
  if (i !== -1) toasts.splice(i, 1);
}

export function useToasts() {
  return { toasts };
}
