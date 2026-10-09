// The catalog-app install flow (DASHBOARD.md # Install authorization), shared by
// the pages it spans: the app detail page and the pack page (Install
// buttons), the setup page (/store/:id/install), and the progress page
// (/store/:id/install/:jobId).
//
// Three parts:
//   - useAppInstances: which copies of this app the caller already has, which
//     drives the Install / Open / "Installing…" button.
//   - useStartInstall: what an Install button does. It starts the install at
//     once for an app that asks nothing, and opens the install pages for any
//     other app.
//   - useInstallSubmit: the POST with its 409-duplicate and 422 branches. A
//     202 hands over to the progress page, which owns the job from then on.
//
// All wording stays in the pages.
import { computed, ref, watch, type Ref } from "vue";
import { useRouter } from "vue-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/vue-query";
import { useAuth } from "./auth";
import { api, ApiError, type Instance, type Job, type InstallRequest, type InstallPlan } from "./api";
import { defaultFieldValues, installWarnings, needsNoPages, unavailableText } from "./installSteps";

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

// useStartInstall is what an Install button does, on the app detail page and
// on the pack page. It reads the install plan, the same query the install
// pages read. goInstall starts the install at once for an app that asks
// nothing and has nothing to warn about (INSTALL_STEPS.md # Build rules, rule
// 8): the 202 goes straight to the progress page. Anything else opens the
// install pages. The button waits for the plan. If the plan fails, it opens
// the pages, which show the error and a retry.
//
// unavailable is set when this box cannot install the app at all, as one plain
// sentence; the page shows it in place of the button.
export function useStartInstall(manifestId: Ref<string>) {
  const router = useRouter();

  const planQuery = useQuery({
    queryKey: computed(() => ["install-plan", manifestId.value]),
    queryFn: () => api.get<InstallPlan>(`/catalog/${encodeURIComponent(manifestId.value)}/install-plan`),
    refetchOnWindowFocus: false,
  });
  const plan = computed(() => planQuery.data.value ?? null);
  const unavailable = computed(() => unavailableText(plan.value));

  const { submit, submitError, submitLocation, duplicateInfo, pending } = useInstallSubmit(manifestId);
  const directScope = ref<"personal" | "household">("personal");

  function pagesPath(household: boolean) {
    return {
      path: `/store/${encodeURIComponent(manifestId.value)}/install`,
      query: household ? { scope: "household" } : undefined,
    };
  }

  function goInstall(household = false) {
    const p = plan.value;
    if (pending.value) return;
    if (!p) {
      if (planQuery.isError.value) router.push(pagesPath(household));
      return;
    }
    const scope = household ? "household" : "personal";
    if (!needsNoPages(p) || installWarnings(p)) {
      router.push(pagesPath(household));
      return;
    }
    directScope.value = scope;
    // Optional plain fields have no step, so they go with their defaults, as
    // they would from the install pages.
    const fields = defaultFieldValues(p);
    submit({
      manifest_id: p.manifest_id,
      scope,
      config: { folders: [], ...(Object.keys(fields).length > 0 ? { fields } : {}) },
    });
  }

  // A direct install that fails opens the install pages with the error there, so
  // the user is never left on the button's page with nothing. A 409 (a copy made
  // after the plan was read) shows the duplicate box there.
  watch([submitError, duplicateInfo], ([err, dup]) => {
    if (!err && !dup) return;
    router.push({
      ...pagesPath(directScope.value === "household"),
      state: { installError: err ?? undefined, installErrorLocation: submitLocation.value, installDuplicate: dup ?? undefined },
    });
  });

  return { planQuery, plan, unavailable, goInstall, pending };
}

// onStarted runs when the brain accepted the install (202), before the page
// moves on. The setup flow clears its draft there.
export function useInstallSubmit(manifestId: Ref<string>, onStarted?: () => void) {
  const qc = useQueryClient();
  const router = useRouter();

  const submitError = ref<string | null>(null); // 422 and other POST failures, shown inline
  // submitLocation is the part of the request a 422 blames (errors[0].location),
  // so the setup flow can show it on the page that owns that part.
  const submitLocation = ref<string | undefined>(undefined);
  const duplicateInfo = ref<string | null>(null); // 409 duplicate-install, warn-don't-block
  const lastRequest = ref<InstallRequest | null>(null); // kept for the confirm retry

  const mutation = useMutation({
    mutationFn: (req: InstallRequest) => api.post<Job>("/apps", req),
    onSuccess: (job, req) => {
      onStarted?.();
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
        submitLocation.value = err instanceof ApiError ? err.location : undefined;
        submitError.value = err instanceof Error ? err.message : "The install could not start.";
      }
    },
  });

  function submit(req: InstallRequest) {
    submitError.value = null;
    submitLocation.value = undefined;
    duplicateInfo.value = null;
    lastRequest.value = req;
    mutation.mutate(req);
  }

  // confirmDuplicate retries the last request past the duplicate warning.
  // fallback is the request to send when this page did not send the first
  // one (the App page's direct install got the 409 and opened this page).
  function confirmDuplicate(fallback?: InstallRequest) {
    const req = lastRequest.value ?? fallback;
    if (!req) return;
    submit({ ...req, confirm: true });
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
    submitLocation,
    duplicateInfo,
    pending: mutation.isPending,
  };
}
