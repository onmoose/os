// useEvents subscribes once to the brain's global SSE stream
// (BRAIN_UI_PROTOCOL.md Pattern C) and invalidates Query caches on relevant
// events — the push/pull-share-one-cache pattern from WEB_UI.md.
import { onMounted, onUnmounted } from "vue";
import { useQueryClient } from "@tanstack/vue-query";

export function useEvents() {
  const qc = useQueryClient();
  let es: EventSource | null = null;

  // An app starting, stopping, installing or going away also changes whether
  // other apps fit next to it (#599), so the store lists, the App page and the
  // install plan re-read their fit too.
  const invalidateApps = () => {
    qc.invalidateQueries({ queryKey: ["apps"] });
    qc.invalidateQueries({ queryKey: ["catalog"] });
    qc.invalidateQueries({ queryKey: ["install-plan"] });
  };
  // Notification events are advisory refetch triggers (NOTIFICATIONS.md #
  // Surfaces): re-read the caller's audience-scoped list/badge rather than
  // merging payloads off the shared, unfiltered bus.
  const invalidateNotifications = () => qc.invalidateQueries({ queryKey: ["notifications"] });
  // Health-issue transitions are advisory too (HEALTH.md # Display; issue #12):
  // the {id, instance_key} payload just says "the set changed," so re-read
  // GET /api/v1/health and let useHealth derive the banner / blocks_* gates.
  const invalidateHealth = () => qc.invalidateQueries({ queryKey: ["health/issues"] });

  onMounted(() => {
    es = new EventSource("/api/v1/events", { withCredentials: true });
    for (const kind of ["app.state_changed", "app.installed", "app.uninstalled"]) {
      es.addEventListener(kind, invalidateApps);
    }
    for (const kind of ["notification.created", "notification.updated"]) {
      es.addEventListener(kind, invalidateNotifications);
    }
    for (const kind of ["health.issue_raised", "health.issue_cleared"]) {
      es.addEventListener(kind, invalidateHealth);
    }
  });

  onUnmounted(() => es?.close());
}
