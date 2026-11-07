<script setup lang="ts">
// NotificationBell is the dashboard-chrome bell (NOTIFICATIONS.md # Surfaces):
// an unread-count badge plus a click-to-open dropdown inbox — reverse-chron,
// severity-colored, grouped unread/read, with relative time, an action link
// to the notification's route, and per-row dismiss. Live updates ride the global SSE channel via useEvents
// (wired in Dashboard.vue), which invalidates the queries this composable owns.
import { ref, computed, onMounted, onUnmounted } from "vue";
import { useRouter } from "vue-router";
import { useNotifications } from "./useNotifications";
import { relativeTime } from "./utils";
import type { Notification } from "./api";

const { list, unreadCount, markRead, markAllRead, dismiss } = useNotifications();

const open = ref(false);
const root = ref<HTMLElement | null>(null);

const notifications = computed(() => list.data.value?.notifications ?? []);
const badge = computed(() => unreadCount.data.value?.count ?? 0);
// Two groups, unread first; the list is already newest-first per group.
const groups = computed(() =>
  [
    { label: "Unread", items: notifications.value.filter((n) => !n.read) },
    { label: "Earlier", items: notifications.value.filter((n) => n.read) },
  ].filter((g) => g.items.length > 0),
);

function toggle() {
  open.value = !open.value;
}

const router = useRouter();

function onRowClick(n: Notification) {
  if (!n.read) markRead.mutate(n.id);
}

// The action opens the notification's route (an app's settings page, the
// Storage page, ...), marks it read and closes the dropdown.
function onAction(n: Notification) {
  if (!n.read) markRead.mutate(n.id);
  open.value = false;
  if (n.action_route) router.push(n.action_route);
}

// Click-outside closes the dropdown (no modal — the bell waits to be looked at).
function onDocClick(e: MouseEvent) {
  if (open.value && root.value && !root.value.contains(e.target as Node)) {
    open.value = false;
  }
}
onMounted(() => document.addEventListener("click", onDocClick));
onUnmounted(() => document.removeEventListener("click", onDocClick));

</script>

<template>
  <div ref="root" class="bell-wrap">
    <button
      class="bell"
      :class="{ active: open }"
      :aria-label="badge > 0 ? `Notifications, ${badge} unread` : 'Notifications'"
      @click="toggle"
    >
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor"
        stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
        <path d="M13.73 21a2 2 0 0 1-3.46 0" />
      </svg>
      <span v-if="badge > 0" class="badge">{{ badge > 99 ? "99+" : badge }}</span>
    </button>

    <div v-if="open" class="inbox">
      <div class="inbox-head">
        <strong>Notifications</strong>
        <button v-if="badge > 0" class="link" @click="markAllRead.mutate()">Mark all read</button>
      </div>

      <p v-if="list.isLoading.value" class="empty">Loading…</p>
      <p v-else-if="notifications.length === 0" class="empty">You're all caught up.</p>

      <template v-else>
        <div v-for="g in groups" :key="g.label" class="group">
          <div class="group-label">{{ g.label }}</div>
          <div
            v-for="n in g.items"
            :key="n.id"
            class="row"
            :class="{ unread: !n.read }"
            role="button"
            tabindex="0"
            @click="onRowClick(n)"
            @keydown.enter="onRowClick(n)"
            @keydown.space.prevent="onRowClick(n)"
          >
            <span class="dot" :data-sev="n.severity"></span>
            <div class="row-main">
              <div class="row-summary">{{ n.summary }}</div>
              <div v-if="n.body" class="row-body">{{ n.body }}</div>
              <div class="row-meta">
                <span class="time">{{ relativeTime(n.ts) }}</span>
                <span v-if="n.resolved_at" class="resolved">resolved</span>
                <button
                  v-if="n.action_label && n.action_route"
                  type="button"
                  class="action"
                  @click.stop="onAction(n)"
                >
                  {{ n.action_label }}
                </button>
              </div>
            </div>
            <button class="dismiss" title="Dismiss" @click.stop="dismiss.mutate(n.id)">×</button>
          </div>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.bell-wrap { position: relative; }
.bell {
  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 2rem;
  height: 2rem;
  padding: 0;
  color: var(--color-muted-foreground);
  border: 1px solid var(--color-border);
  background: var(--color-card);
  border-radius: var(--radius);
  cursor: pointer;
}
.bell:hover { background: var(--color-muted); }
.bell.active { background: var(--color-muted); border-color: var(--color-olive-300); }
.badge {
  position: absolute;
  top: -6px;
  right: -6px;
  min-width: 1rem;
  height: 1rem;
  padding: 0 3px;
  font-size: 0.62rem;
  font-weight: 600;
  line-height: 1rem;
  text-align: center;
  color: var(--color-olive-50);
  background: var(--color-destructive);
  border: 1px solid var(--color-background);
  border-radius: 999px;
}

.inbox {
  position: absolute;
  top: calc(100% + 8px);
  right: 0;
  width: 340px;
  max-height: 460px;
  overflow-y: auto;
  background: var(--color-card);
  border: 1px solid var(--color-border);
  border-radius: var(--radius);
  box-shadow: 0 8px 28px rgba(0, 0, 0, 0.12);
  z-index: 50;
}
.inbox-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.6rem 0.85rem;
  border-bottom: 1px solid var(--color-border);
  position: sticky;
  top: 0;
  background: var(--color-card);
}
.inbox-head strong { font-size: 0.85rem; }
.link {
  border: none;
  background: none;
  padding: 0;
  color: var(--color-accent);
  font-size: 0.78rem;
  cursor: pointer;
}
.link:hover { text-decoration: underline; }
.empty { color: var(--color-muted-foreground); font-size: 0.85rem; text-align: center; padding: 1.4rem 1rem; margin: 0; }

.group-label {
  font-size: 0.65rem;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--color-muted-foreground);
  padding: 0.5rem 0.85rem 0.25rem;
}
.row {
  display: flex;
  gap: 0.55rem;
  align-items: flex-start;
  padding: 0.6rem 0.85rem;
  border-top: 1px solid var(--color-border);
  cursor: pointer;
}
.row:hover { background: var(--color-muted); }
/* Unread lacks a distinct hue in the monochrome palette; the bold summary and
   colored severity dot carry the signal, with a light olive resting tint. */
.row.unread { background: var(--color-muted); }
.row.unread:hover { background: var(--color-olive-200); }
.row.unread .row-summary { font-weight: 600; }
.dot {
  flex: 0 0 8px;
  width: 8px;
  height: 8px;
  margin-top: 0.35rem;
  border-radius: 999px;
  background: var(--color-muted-foreground);
}
.dot[data-sev="info"] { background: var(--color-info); }
.dot[data-sev="warning"] { background: var(--color-warning); }
.dot[data-sev="error"] { background: var(--color-destructive); }
.dot[data-sev="critical"] { background: var(--color-destructive); }
.row-main { flex: 1; min-width: 0; }
.row-summary { font-size: 0.85rem; color: var(--color-foreground); }
.row-body { font-size: 0.78rem; color: var(--color-muted-foreground); margin-top: 2px; }
.row-meta { display: flex; gap: 0.5rem; align-items: center; margin-top: 4px; font-size: 0.72rem; color: var(--color-muted-foreground); }
.resolved { color: var(--color-success); }
.action {
  padding: 0;
  border: none;
  background: none;
  color: var(--color-accent);
  font: inherit;
  cursor: pointer;
}
.action:hover { text-decoration: underline; }
.dismiss {
  flex: 0 0 auto;
  border: none;
  background: none;
  color: var(--color-muted-foreground);
  font-size: 1.1rem;
  line-height: 1;
  padding: 0 0.15rem;
  cursor: pointer;
}
.dismiss:hover { color: var(--color-foreground); }
</style>
