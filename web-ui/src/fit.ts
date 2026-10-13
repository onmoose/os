// The words for an app's fit on this box (#599, APP_ISOLATION.md # Resource
// limits). The brain sends the state and the figures (AppFit); the sentences
// live here, so the store card, the App page and the install pages say the
// same thing.
import type { AppFit } from "./api";
import { formatSize } from "./utils";

// cores writes a CPU core count: "1 core", "1.5 cores", "2 cores".
export function cores(n: number): string {
  return n === 1 ? "1 core" : `${n} cores`;
}

// recommendedMemory and recommendedCPU are the App page's Information rows,
// or "" when the app recommends nothing for that resource.
export function recommendedMemory(fit: AppFit | undefined): string {
  return fit?.recommended_memory_bytes ? formatSize(fit.recommended_memory_bytes) : "";
}
export function recommendedCPU(fit: AppFit | undefined): string {
  return fit?.recommended_cpu ? cores(fit.recommended_cpu) : "";
}

// fitBadge is the short label on a store card, or "" when the app fits.
export function fitBadge(fit: AppFit | undefined): string {
  if (fit?.state === "too-big") return "Too big for this box";
  if (fit?.state === "tight") return "May not fit right now";
  return "";
}

// tooBigText says why this box can't run the app, with both figures for each
// resource that is short.
export function tooBigText(fit: AppFit): string {
  const out = ["This box can't run this app as it is."];
  if (fit.too_big_memory) {
    out.push(
      `It recommends ${formatSize(fit.recommended_memory_bytes ?? 0)} of memory, and this box has ${formatSize(fit.box_memory_bytes)}.`,
    );
  }
  if (fit.too_big_cpu) {
    out.push(`It recommends ${cores(fit.recommended_cpu ?? 0)}, and this box has ${cores(fit.box_cores ?? 0)}.`);
  }
  return out.join(" ");
}

// tightText says the app may not fit next to the apps that are running. The
// running apps themselves are listed by the caller, so each can be a line.
export function tightText(fit: AppFit): string {
  return (
    `This box has enough memory for this app, but not next to the apps that are running now. ` +
    `It recommends ${formatSize(fit.recommended_memory_bytes ?? 0)}, and about ${formatSize(fit.free_memory_bytes)} is free.`
  );
}

// runningLine is one running app and the memory it recommends.
export function runningLine(r: { name: string; recommended_memory_bytes: number }): string {
  return `${r.name} (${formatSize(r.recommended_memory_bytes)})`;
}
