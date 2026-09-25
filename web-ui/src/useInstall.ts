// The catalog-app install flow (DASHBOARD.md # Install authorization), shared by
// the three pages it spans: the app detail page (Install button), the setup
// page (/store/:id/install), and the progress page (/store/:id/install/:jobId).
//
// Two parts:
//   - useAppInstances: which copies of this app the caller already has, which
//     drives the detail page's Install / Open / "Installing…" button.
//   - useInstallSubmit: the POST with its 409-duplicate and 422 branches. A
//     202 hands over to the progress page, which owns the job from then on.
//
// All wording stays in the pages.
import { computed, ref, watch, type Ref } from "vue";
import { useRouter } from "vue-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/vue-query";
import { useAuth } from "./auth";
import { api, ApiError, type Instance, type Job, type InstallRequest } from "./api";

export function useAppInstances(manifestId: Ref<string>) {
  const { currentUser, singleUserMode } = useAuth();

  const apps = useQuery({
    queryKey: ["apps"],
    queryFn: () => api.get<{ apps: Instance[] }>("/apps"),
  });

  // The caller-relevant instances for this app: a shared (household) copy anyone
  // permitted can open, and the caller's own personal copy.
  const householdInstance = computed<Instance | undefined>(() =>
    (apps.data.value?.apps ?? []).find((a) => a.manifest_id === manifestId.value && a.scope === "household"),
  );
  const ownPersonalInstance = computed<Instance | undefined>(() =>
    (apps.data.value?.apps ?? []).find(
      (a) =>
        a.manifest_id === manifestId.value &&
        a.scope === "personal" &&
        a.owner_user_id === currentUser.value?.id,
    ),
  );

  // The brain creates the instance row in the "installing" state at the very
  // start of the job and emits app.state_changed (internal/lifecycle), so the
  // SSE refetch of ["apps"] shows it while the job still runs. That is what
  // keeps the detail page on "Installing…" rather than a dead Open link, and it
  // holds across a reload because it needs no local state at all.
  const installing = computed(
    () => householdInstance.value?.state === "installing" || ownPersonalInstance.value?.state === "installing",
  );

  // Admins outside single-user mode may install for the whole household; the
  // brain enforces the same rule on POST /apps.
  const canInstallHousehold = computed(() => currentUser.value?.role === "admin" && !singleUserMode.value);

  return { householdInstance, ownPersonalInstance, installing, canInstallHousehold };
}

export function useInstallSubmit(manifestId: Ref<string>) {
  const qc = useQueryClient();
  const router = useRouter();

  const submitError = ref<string | null>(null); // 422 and other POST failures, shown inline
  const duplicateInfo = ref<string | null>(null); // 409 duplicate-install, warn-don't-block
  const lastRequest = ref<InstallRequest | null>(null); // kept for the confirm retry

  const mutation = useMutation({
    mutationFn: (req: InstallRequest) => api.post<Job>("/apps", req),
    onSuccess: (job, req) => {
      // The instance row appears early in the job; refetch so the detail page
      // shows "Installing…" if the user goes back to it.
      qc.invalidateQueries({ queryKey: ["apps"] });
      // replace, not push: Back from the progress page should not land on a
      // setup form for an install that has already started.
      //
      // The scope rides along in the URL. The job does not say which scope it
      // had, and the progress page's "Try again" must go back to the same
      // setup page, household or personal.
      router.replace({
        path: `/store/${encodeURIComponent(manifestId.value)}/install/${encodeURIComponent(job.job_id)}`,
        query: req.scope === "household" ? { scope: "household" } : {},
      });
    },
    onError: (err: unknown) => {
      if (err instanceof ApiError && err.code === "duplicate-install") {
        duplicateInfo.value = err.message;
      } else {
        submitError.value = err instanceof Error ? err.message : "The install could not start.";
      }
    },
  });

  function submit(req: InstallRequest) {
    submitError.value = null;
    duplicateInfo.value = null;
    lastRequest.value = req;
    mutation.mutate(req);
  }

  // confirmDuplicate retries the last request past the duplicate warning.
  function confirmDuplicate() {
    if (!lastRequest.value) return;
    submit({ ...lastRequest.value, confirm: true });
  }

  function dismissDuplicate() {
    duplicateInfo.value = null;
  }

  // The setup view is reused across /store/:id/install navigations, so clear
  // what belonged to the previous app.
  watch(manifestId, () => {
    submitError.value = null;
    duplicateInfo.value = null;
    lastRequest.value = null;
  });

  return {
    submit,
    confirmDuplicate,
    dismissDuplicate,
    submitError,
    duplicateInfo,
    pending: mutation.isPending,
  };
}
