// updates.ts: the words the dashboard uses for app update outcomes (#634,
// UPDATES.md # 6), in one place, so the tile banner, the overnight toast and
// the app's settings page say the same thing.
import type { Instance, User } from "@/api";

// failedStep names the step of an update that did not go through, in plain
// words: the failure mode the app's page shows (UPDATES.md # 6 Failure
// signaling: image pull, health check, snapshot restore).
export function failedStep(kind: string | undefined): string {
  switch (kind) {
    case "pull":
      return "The new version could not be downloaded.";
    case "snapshot":
      return "moose could not keep a copy of the app's data first.";
    case "start":
      return "The new version did not start.";
    case "health":
      return "The new version did not pass its health check.";
    case "refused":
      return "The new version did not pass moose's safety checks.";
    case "snapshot_restore":
      return "moose could not put the app's data back.";
    case "interrupted":
      return "The update was cut off before it finished.";
    default:
      return "The update did not go through.";
  }
}

// The paused text from #632, the same on the tile and on the app's page.
export const PAUSED_TEXT = "Update is failing repeatedly. Paused.";

// failureBanner is the tile's banner sentence for a failed update that was
// rolled back (UPDATES.md # 6 Failure signaling).
export function failureBanner(app: Instance): string {
  const u = app.update;
  if (u.paused) return PAUSED_TEXT;
  const when = u.failed_overnight ? " last night" : "";
  return `${app.name} couldn't update to ${u.failed_version}${when}. Rolled back to ${app.version}.`;
}

// showFailureBanner: the banner stays until a person closes it, for an app
// that runs again after the failure. An app that did not start again shows
// as failed instead.
export function showFailureBanner(app: Instance): boolean {
  return !!app.update.failed_version && !app.update.failed_acked && app.state !== "update_failed" && app.state !== "updating";
}

// isMine is the apps whose updates a person hears about: their own personal
// apps and the household apps (UPDATES.md # 6 Notification center).
export function isMine(app: Instance, user: User | null | undefined): boolean {
  return app.scope === "household" || app.owner_user_id === user?.id;
}

const DAY_MS = 24 * 60 * 60 * 1000;

// overnightUpdates is the apps the nightly window updated since the person
// last saw the toast, and in the last day at most, so a first sign-in does not
// report a week of updates.
export function overnightUpdates(apps: Instance[], user: User | null | undefined, now = Date.now()): Instance[] {
  const seen = (user?.updates_seen_at ?? 0) * 1000;
  const since = Math.max(seen, now - DAY_MS);
  return apps.filter((a) => {
    const at = a.update.updated_at ? Date.parse(a.update.updated_at) : 0;
    return a.update.updated_overnight && at > since && isMine(a, user);
  });
}

