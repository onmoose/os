<script setup lang="ts">
// The post-update toast (UPDATES.md # 6 Post-update toast, #634). On the
// first dashboard load after a night with app updates: "3 apps updated
// overnight", with a link to the list in Settings → Updates. Not modal, and it
// goes away by itself.
//
// It shows once: the brain keeps when the person last saw it (updates_seen_at
// on /me, POST /me/updates-seen), so it does not show again on the next page
// load or on another device. The apps are the ones the nightly window updated
// since then, in the last day at most, and only the person's own personal
// apps and the household apps (updates.ts # overnightUpdates). The list comes
// from the apps list the dashboard already loads.
import { ref, watch } from "vue";
import { useQuery } from "@tanstack/vue-query";
import { api, type Instance } from "@/api";
import { markUpdatesSeen, useAuth } from "@/auth";
import { pushSuccessToast } from "@/toasts";
import { overnightUpdates } from "@/updates";

const { currentUser } = useAuth();
const apps = useQuery({
  queryKey: ["apps"],
  queryFn: () => api.get<{ apps: Instance[] }>("/apps"),
});

// Once per page load at most; the seen mark covers the rest.
const done = ref(false);

watch(
  [() => apps.data.value, currentUser],
  async ([data, user]) => {
    if (done.value || !data || !user) return;
    done.value = true;
    const list = overnightUpdates(data.apps, user);
    if (list.length === 0) return;
    const [first] = list;
    const message = list.length === 1 && first ? `${first.name} updated overnight` : `${list.length} apps updated overnight`;
    pushSuccessToast(message, { label: "See what changed", to: "/settings/updates" });
    try {
      await api.post("/me/updates-seen");
      markUpdatesSeen(Math.floor(Date.now() / 1000));
    } catch {
      // Not stored: the toast shows again on the next load. That is the
      // safe side.
    }
  },
  { immediate: true },
);
</script>

<template>
  <span class="hidden" aria-hidden="true" />
</template>
