<script setup lang="ts">
// "Go back to <version>" (UPDATES.md # 4 Update mechanics per app, # 6, #634).
// For 7 days after a good update, the owner (or an admin for a household app)
// can put the app back on the version it ran before, with the data it had
// then. The brain says when it can (update.can_rollback); this button asks
// first, because data saved since the update is lost, then runs the rollback
// job and refreshes the app.
//
// After a rollback the brain turns the app's automatic update off, so the
// window does not apply the same version again the next night (DECISIONS.md
// 2026-11-07). The dialog says so.
//
// Used on the app's settings page and in Settings → Updates.
import { computed, ref } from "vue";
import { useMutation, useQueryClient } from "@tanstack/vue-query";
import { api, waitForJob, type Instance, type Job } from "@/api";
import Button from "@/components/ui/Button.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";

const props = defineProps<{ app: Instance }>();
const qc = useQueryClient();

const from = computed(() => props.app.update.updated_from ?? "");
const updatedOn = computed(() => {
  const at = props.app.update.updated_at;
  return at ? new Date(at).toLocaleString() : "the update";
});

const confirm = ref<InstanceType<typeof ConfirmDialog> | null>(null);

const rollback = useMutation({
  mutationFn: async () => {
    const job = await api.post<Job>(`/apps/${props.app.id}/rollback`);
    const done = await waitForJob(job.job_id);
    if (done.status === "failed") throw new Error(done.error?.message || "moose could not go back");
    return done;
  },
  onSettled: () => {
    qc.invalidateQueries({ queryKey: ["apps"] });
    qc.invalidateQueries({ queryKey: ["apps", props.app.id] });
  },
});
</script>

<template>
  <div class="space-y-1">
    <Button
      variant="secondary"
      size="sm"
      :disabled="rollback.isPending.value || app.state === 'updating'"
      @click="confirm?.open()"
    >
      {{ rollback.isPending.value ? "Going back…" : `Go back to ${from}` }}
    </Button>
    <p v-if="rollback.isError.value" class="text-sm text-destructive">
      Couldn't go back: {{ (rollback.error.value as Error)?.message }}
    </p>
    <ConfirmDialog
      ref="confirm"
      :title="`Go back to version ${from}?`"
      :confirm-label="`Go back to ${from}`"
      @confirm="rollback.mutate()"
    >
      <p>
        {{ app.name }} goes back to version {{ from }}, with the data it had on {{ updatedOn }}, just before the update.
      </p>
      <p class="font-medium text-destructive">
        Anything saved in {{ app.name }} since then is lost.
      </p>
      <p>
        moose then stops updating {{ app.name }} on its own, so it stays on {{ from }}. You can update it again from its
        settings.
      </p>
    </ConfirmDialog>
  </div>
</template>
