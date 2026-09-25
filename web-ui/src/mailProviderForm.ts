// Shared shape and helpers for the outgoing-email provider form
// (SERVICE_PROVISIONING.md # BYO outgoing mail). Three places consume it: the
// add flow at /settings/mail/add/:preset, the inline edit form on the account
// list, and the inline add on the install setup page. They must agree field for
// field: the same preset rules decide what is shown and what is sent on all.
import type { Ref } from "vue";
import { useQuery } from "@tanstack/vue-query";
import { api, ApiError, type MailPreset } from "@/api";
import { isHosted } from "@/auth";

export type ProviderForm = {
  label: string;
  host: string;
  port: number;
  username: string;
  password: string;
  from_address: string;
  encryption: "none" | "starttls" | "tls";
  provider_type: string;
  region: string;
};

// The preset table is static server-side data, so it never needs refetching
// within a session. Every caller shares it; Query dedupes them onto one request.
// The endpoint is admin-only, so the install page passes `enabled` and fetches
// only once an admin opens its add flow.
export function useMailPresets(enabled?: Ref<boolean>) {
  return useQuery({
    queryKey: ["mail-presets"],
    queryFn: () => api.get<{ presets: MailPreset[] }>("/mail-presets"),
    staleTime: Infinity,
    enabled: enabled ?? true,
  });
}

export function emptyForm(): ProviderForm {
  return {
    label: "",
    host: "",
    port: 587,
    username: "",
    password: "",
    from_address: "",
    encryption: "starttls",
    provider_type: "custom",
    region: "",
  };
}

// hostFor resolves the SMTP host for a preset: the static one, or the host the
// chosen region option names (SES and Mailgun are the only two, and Mailgun's
// EU host is a prefix rather than a region code — hence a host per option).
export function hostFor(p: MailPreset, region: string): string {
  if (!p.region) return p.host;
  const opts = p.region.options ?? [];
  const opt = opts.find((o) => o.value === region) ?? opts[0];
  return opt?.host ?? "";
}

// formFromPreset seeds a blank form from the preset the admin picked. The label
// defaults to the provider name so it stops being a field they must invent; a
// duplicate surfaces the existing 409.
export function formFromPreset(p: MailPreset): ProviderForm {
  const f = emptyForm();
  f.provider_type = p.id;
  f.label = p.id === "custom" ? "" : p.label;
  f.port = p.port;
  f.encryption = p.encryption as ProviderForm["encryption"];
  f.region = p.region?.default ?? "";
  f.host = hostFor(p, f.region);
  if (p.username_mode === "fixed") f.username = p.username_fixed;
  else if (p.username_mode === "user") f.username = p.username_prefill ?? "";
  return f;
}

// Postmark wants the same server token as username and password, so the form
// keeps the two in step and shows one field.
//
// A blank password is never copied across. On edit it means "keep the stored
// credential", so the paired username has to be kept too — copying the empty
// value over it would wipe the stored username while leaving the password
// intact, and the account would fail AUTH on its next send with nothing in the
// UI to say why.
export function syncSameAsPassword(f: ProviderForm, p: MailPreset | undefined) {
  if (p?.username_mode === "same_as_password" && f.password !== "") f.username = f.password;
}

export function formValid(f: ProviderForm): boolean {
  return (
    !!f.label.trim() && !!f.host.trim() && f.port >= 1 && f.port <= 65535 && f.from_address.includes("@")
  );
}

// A hosted box reaches 587 and 2525 only — 25 and 465 get no SYN-ACK, so a
// provider saved on implicit TLS never delivers (SERVICE_PROVISIONING.md # BYO
// outgoing mail). Warn rather than forbid: an appliance on a home LAN reaches
// 465 fine, and some providers prefer it.
export function portWarning(f: ProviderForm): string {
  if (!isHosted()) return "";
  if (f.port !== 25 && f.port !== 465) return "";
  return `This box cannot reach port ${f.port}. Use port 587 with STARTTLS. Every provider in the list supports it.`;
}

// bodyOf drops the UI-only region field: the brain stores the resolved host, not
// the region that produced it (the server never re-derives host from a preset,
// so an advanced override survives — DECISIONS.md 2026-08-27 D3).
export function bodyOf(f: ProviderForm) {
  const { region: _region, ...body } = f;
  return body;
}

export function errorMessage(e: unknown): string {
  // A dismissed elevation prompt is a deliberate no-op, not a failure.
  if (e instanceof ApiError && e.code === "elevation_cancelled") return "";
  return e instanceof ApiError ? e.message : "Something went wrong.";
}

// One field idiom across both views, matching CustomInstallView: a labelled
// stack, one field per line, inset fill on the card with an olive focus ring.
export const fieldClass =
  "w-full rounded-xl border border-border bg-background px-3 py-2 text-sm outline-none focus:border-accent " +
  "disabled:cursor-not-allowed disabled:opacity-60";
