// Vue Router 4, history mode (WEB_UI.md: Caddy serves index.html for unmatched
// routes). The four destinations mirror the dock in DASHBOARD.md # global
// navigation. Settings is a left-nav shell (SettingsLayout) whose sections
// (Account, Installed apps, AI services, Email, Notifications, Activity,
// Users, About) are nested child routes; the right pane is the shell's <RouterView>. Activity is open to
// all signed-in users; Users is admin-only (the section guards the role, and the
// shell hides its nav item from members). Role gating per AUTH.md # Roles.
import { createRouter, createWebHistory, type RouteRecordRaw } from "vue-router";

const routes: RouteRecordRaw[] = [
  { path: "/", name: "home", component: () => import("@/views/HomeView.vue") },
  { path: "/files", name: "files", component: () => import("@/views/FilesView.vue") },
  { path: "/store", name: "store", component: () => import("@/views/StoreView.vue") },
  // Door-2 custom-container install — a dedicated full-screen form, admin-only
  // (the view guards the role; the Store affordance is hidden from members).
  // Declared before /store/:id; Vue Router also ranks the static segment higher,
  // so "custom" never matches the app-detail param.
  { path: "/store/custom", name: "store-custom", component: () => import("@/views/CustomInstallView.vue") },
  // App detail page (APP_STORE.md # Catalog schema) — the browse grid links here;
  // it's where the description, screenshots, and the Install flow live.
  { path: "/store/:id", name: "store-app", component: () => import("@/views/AppDetailView.vue") },
  // Installing a catalog app is two pages (INSTALL_SETUP.md # 6): the setup form,
  // then the progress of the job it starts. ?scope=household on the setup page
  // is the split-button's household install. The job id is in the progress URL,
  // so a reload picks the job up again.
  { path: "/store/:id/install", name: "store-install", component: () => import("@/views/InstallSetupView.vue") },
  {
    path: "/store/:id/install/:jobId",
    name: "store-install-progress",
    component: () => import("@/views/InstallProgressView.vue"),
  },
  // Settings shell + its sections (DASHBOARD.md # global navigation, AUTH.md #
  // Roles). The bare /settings redirects to Account, the default landing. The old
  // /settings/users and /settings/activity paths are preserved here as the same
  // nested children, so existing links keep working.
  {
    path: "/settings",
    component: () => import("@/views/settings/SettingsLayout.vue"),
    children: [
      { path: "", redirect: "/settings/account" },
      { path: "account", name: "settings-account", component: () => import("@/views/settings/AccountSection.vue") },
      { path: "ssh", name: "settings-ssh", component: () => import("@/views/settings/SshSection.vue") },
      { path: "notifications", name: "settings-notifications", component: () => import("@/views/settings/NotificationsSection.vue") },
      { path: "apps", name: "settings-apps", component: () => import("@/views/settings/InstalledAppsSection.vue") },
      { path: "apps/:id", name: "settings-app", component: () => import("@/views/settings/InstalledAppDetailSection.vue") },
      { path: "activity", name: "settings-activity", component: () => import("@/views/settings/ActivitySection.vue") },
      { path: "users", name: "settings-users", component: () => import("@/views/settings/UsersSection.vue") },
      // Integrations (INSTALL_SETUP.md, 2026-09-26): the user's own accounts
      // that connect their apps to outside services.
      // The UI says "AI services" (INSTALL_STEPS.md # 6). The screen was LLM
      // providers at /settings/llm, and old links still land on it.
      { path: "ai", name: "settings-ai", component: () => import("@/views/settings/LLMProvidersSection.vue") },
      { path: "llm", redirect: "/settings/ai" },
      { path: "email", name: "settings-email", component: () => import("@/views/settings/EmailSection.vue") },
      // Adding an account is two steps, and each is its own URL so the browser
      // Back button walks form → picker → list.
      { path: "email/add", name: "settings-email-add", component: () => import("@/views/settings/EmailAddSection.vue") },
      { path: "email/add/:preset", name: "settings-email-new", component: () => import("@/views/settings/EmailAddSection.vue") },
      // The screen was Outgoing email at /settings/mail until it moved under
      // Integrations. Old links and bookmarks still land on it.
      { path: "mail", redirect: "/settings/email" },
      { path: "mail/add", redirect: "/settings/email/add" },
      { path: "mail/add/:preset", redirect: (to) => `/settings/email/add/${String(to.params.preset)}` },
      { path: "about", name: "settings-about", component: () => import("@/views/settings/AboutSection.vue") },
    ],
  },
  // Recovery is reachable while logged OUT (AUTH.md # Using the recovery code).
  // It's registered here so the catch-all below doesn't redirect the path away;
  // App.vue renders RecoverView directly in its logged-out branch, since the
  // router-view this route would feed only mounts inside AppShell once signed in.
  { path: "/recover", name: "recover", component: () => import("@/RecoverView.vue") },
  // Unknown paths fall back to Home — the SPA never 404s its own chrome.
  { path: "/:pathMatch(.*)*", redirect: "/" },
];

export const router = createRouter({
  history: createWebHistory(),
  routes,
  // Scroll to a #hash target when the destination carries one (the degraded-mode
  // banner links to #health-issues on Home); other navigations keep the default
  // (no forced scroll), so existing behavior is unchanged.
  scrollBehavior(to) {
    if (to.hash) return { el: to.hash, behavior: "smooth" };
  },
});
