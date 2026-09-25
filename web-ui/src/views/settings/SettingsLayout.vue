<script setup lang="ts">
// Settings shell: a left nav panel plus a routed content pane (DASHBOARD.md #
// global navigation: Settings is "box + account settings, and the home for gated
// routes"). The internal IA is a two-pane layout: a bordered nav panel on the
// left, the active section's <RouterView> filling the rest. Each section is its
// own nested route under /settings, so deep-links and the avatar-menu links keep
// working.
//
// Role gating mirrors AUTH.md: Users is admin-only (hidden from members here;
// the section view also redirects, defence in depth).
// Everything else is open to every signed-in user. A group left with zero
// visible items is dropped rather than rendered with a bare label.
//
// Responsive: the nav is a bordered panel with grouped sections on md+ and
// collapses to a flat, horizontally scrollable tab strip above the content on
// narrow screens. Mobile keeps its own off-canvas nav via the Dock
// (components/Dock.vue), so this doesn't duplicate that with a second slide-over.
import { computed } from "vue";
import { RouterLink, RouterView } from "vue-router";
import { User, Bell, LayoutGrid, Mail, ScrollText, Terminal, Users, Info, type LucideIcon } from "lucide-vue-next";
import { useAuth } from "@/auth";

const { currentUser } = useAuth();

type NavItem = { to: string; label: string; icon: LucideIcon; adminOnly?: boolean };
type NavGroup = { label: string; items: NavItem[] };

// Group and item order is the menu order. "You" holds what the signed-in user
// owns: their account, the apps they have installed, and the email accounts
// they added (each belongs to one user, INSTALL_SETUP.md # 5). "System" holds the
// box-wide items. SSH sits between Notifications and Activity: it is per-account
// like Notifications, and both of those read as settings you carry, while
// Activity is the record of what happened on this box.
const groups: NavGroup[] = [
  {
    label: "You",
    items: [
      { to: "/settings/account", label: "Account", icon: User },
      { to: "/settings/apps", label: "Installed apps", icon: LayoutGrid },
      { to: "/settings/mail", label: "Outgoing email", icon: Mail },
    ],
  },
  {
    label: "System",
    items: [
      { to: "/settings/users", label: "Users", icon: Users, adminOnly: true },
      { to: "/settings/notifications", label: "Notifications", icon: Bell },
      { to: "/settings/ssh", label: "SSH", icon: Terminal },
      { to: "/settings/activity", label: "Activity", icon: ScrollText },
      { to: "/settings/about", label: "About", icon: Info },
    ],
  },
];

const isAdmin = computed(() => currentUser.value?.role === "admin");

const visibleGroups = computed(() =>
  groups
    .map((g) => ({ ...g, items: g.items.filter((i) => !i.adminOnly || isAdmin.value) }))
    .filter((g) => g.items.length > 0),
);

// Flat list for the mobile tab strip: same filter, no grouping.
const flatItems = computed(() => visibleGroups.value.flatMap((g) => g.items));
</script>

<template>
  <!-- flex-1 (not h-full) so the shell fills the AppShell content wrapper on
       short pages. Because there is no min-h-0, it grows past the viewport on
       tall ones so the page scrolls (the AppShell spacer then clears the dock).
       min-h-0 is deliberately absent: it would let a tall section collapse the
       column and overflow its content behind the fixed dock instead of scrolling. -->
  <div class="flex flex-1 flex-col gap-6 pt-2 md:flex-row md:gap-8">
    <!-- Mobile: flat, horizontally scrollable tab strip. -->
    <nav class="-mx-1 flex shrink-0 gap-1 overflow-x-auto px-1 md:hidden">
      <RouterLink
        v-for="item in flatItems"
        :key="item.to"
        :to="item.to"
        active-class="bg-muted text-foreground"
        class="flex items-center gap-2.5 whitespace-nowrap rounded-xl px-3 py-2 text-sm text-muted-foreground hover:bg-muted hover:text-foreground"
      >
        <component :is="item.icon" class="size-4 shrink-0" />
        <span>{{ item.label }}</span>
      </RouterLink>
    </nav>

    <!-- Desktop: bordered nav panel with grouped sections. -->
    <nav class="hidden shrink-0 flex-col gap-5 rounded-xl border border-border bg-card p-3 md:flex md:w-64">
      <div v-for="group in visibleGroups" :key="group.label">
        <div class="px-3 pb-1 text-xs font-semibold text-muted-foreground">{{ group.label }}</div>
        <ul class="space-y-1">
          <li v-for="item in group.items" :key="item.to">
            <RouterLink
              :to="item.to"
              active-class="bg-muted text-accent"
              class="flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-semibold text-muted-foreground hover:bg-muted hover:text-accent"
            >
              <component :is="item.icon" class="size-6 shrink-0" />
              {{ item.label }}
            </RouterLink>
          </li>
        </ul>
      </div>
    </nav>

    <!-- Content pane: the active section. min-w-0 so long content (e.g. log
         lines) can shrink rather than force horizontal overflow; no min-h-0, so a
         tall section grows the page (scrolls) instead of overflowing the dock. -->
    <div class="flex min-w-0 flex-1 flex-col">
      <RouterView />
    </div>
  </div>
</template>
