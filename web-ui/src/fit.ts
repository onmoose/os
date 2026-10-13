// The words for an app's fit on this box (#599, APP_ISOLATION.md # Resource
// limits). The brain sends the state and the figures (AppFit); the sentences
// live here, so the App page and the install pages say the same thing.
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

// tooBigText says why this box can't run the app, with both figures for each
// resource that is short. The brain's install refusal says the same words
// (lifecycle.TooBigError).
export function tooBigText(fit: AppFit): string {
  return [TOO_BIG_TITLE, tooBigDetails(fit)].join(" ");
}

// TOO_BIG_TITLE and tooBigDetails are the two halves of tooBigText: the alert
// on the App page shows the first as its title and the second under it.
export const TOO_BIG_TITLE = "This app can't run on this box as it is.";
export const TIGHT_TITLE = "This app may not fit next to the apps that are running.";

export function tooBigDetails(fit: AppFit): string {
  const out: string[] = [];
  if (fit.too_big_memory) {
    out.push(
      `It requires ${formatSize(fit.recommended_memory_bytes ?? 0)} of memory, and this box only has ${formatSize(fit.box_memory_bytes)}.`,
    );
  }
  if (fit.too_big_cpu) {
    out.push(`It requires ${cores(fit.recommended_cpu ?? 0)}, and this box only has ${cores(fit.box_cores ?? 0)}.`);
  }
  return out.join(" ");
}

// tightText is the tight alert's description: the box has the memory in
// total, but not free now. The running apps follow (runningText).
export function tightText(fit: AppFit): string {
  return (
    `This box has enough memory in total. ` +
    `It requires ${formatSize(fit.recommended_memory_bytes ?? 0)}, and only about ${formatSize(fit.free_memory_bytes)} is free right now.`
  );
}

// othersLine counts the running apps the caller may not see by name: other
// people's own apps. They still take memory.
export function othersLine(n: number): string {
  return n === 1 ? "1 app that someone else on this box uses" : `${n} apps that other people on this box use`;
}

// runningLine is one running app and the memory it recommends.
export function runningLine(r: { name: string; recommended_memory_bytes: number }): string {
  return `${r.name} (${formatSize(r.recommended_memory_bytes)})`;
}

// runningText is the tight notice's second sentence: the running apps in one
// line, then what to do. "" when there are none to name or count.
export function runningText(running: { name: string; recommended_memory_bytes: number }[], others: number): string {
  const parts = running.map(runningLine);
  if (others > 0) parts.push(othersLine(others));
  if (parts.length === 0) return "";
  const todo = running.length > 0 ? " To make room, stop or uninstall one of them." : "";
  return `Running now: ${parts.join(", ")}.${todo}`;
}

// SPACE_TITLE and spaceText are the App page's not-enough-disk warning, from
// the install plan's footprint (spaceTight). A warning only: the install
// still goes ahead.
export const SPACE_TITLE = "This might not fit.";

export function spaceText(need: number, free: number): string {
  return `It needs about ${formatSize(need)}, and only about ${formatSize(free)} is free on your box. You can still install.`;
}
