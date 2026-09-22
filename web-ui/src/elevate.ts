// Elevation flow (USERS_AND_GROUPS.md # Elevation in the UI). Destructive /
// far-reaching admin operations re-prompt for the password — the brain marks
// the session "elevated" for a 5-minute window (POST /api/v1/auth/elevate) and
// the user-mutation endpoints reject with `elevation_required` (403) until then.
//
// Shape: a single ElevateDialog instance (mounted in AppShell) renders the
// password prompt; this module owns the singleton state and the `withElevation`
// helper that wraps a mutation, catches the 403, drives the prompt, and retries
// once. Within a live window the prompt never shows — the first call elevates,
// the rest pass straight through.
//
// The hosted box owner has no password to re-type, so their confirm step is a
// portal round-trip rather than a prompt (issue #469; ENVIRONMENT.md # Owner
// sign-in & seed ingestion). See requestElevation below.
import { ref } from "vue";
import { api, ApiError } from "./api";
import { isHosted, isBoxOwner, redirectToPortalConfirm } from "./auth";

// elevationCancelled is thrown when the user dismisses the prompt. Callers map
// it to "no message" so a cancel reads as a no-op, not an error.
export const elevationCancelled = new ApiError("elevation_cancelled", "", 0);

export const elevateVisible = ref(false);
export const elevateSubmitting = ref(false);
export const elevateError = ref("");

let pending: { resolve: () => void; reject: (e: unknown) => void } | null = null;

// requestElevation drives the confirm step and resolves once the session is
// elevated, or rejects with elevationCancelled if the user backs out.
//
// Two proofs, picked by who is asking. The account password is the normal one, so
// this shows the prompt. The hosted box OWNER is the exception: they have no box
// password — the portal signed them in and the box gave their account a random
// one nobody has seen — so their proof is a portal round-trip instead (issue
// #469): mint a one-time challenge, then hand the browser to the portal with the
// current page as the return path. The portal signs a fresh ownership assertion
// and sends the browser back, and the box elevates the session it mints. A box
// user the owner created on a hosted box does have a password, so they keep the
// prompt like everyone on an appliance.
function requestElevation(): Promise<void> {
  if (isHosted() && isBoxOwner()) return confirmViaPortal();
  elevateError.value = "";
  elevateVisible.value = true;
  return new Promise((resolve, reject) => {
    pending = { resolve, reject };
  });
}

// leavingForConfirm is true once confirmViaPortal has started sending the page to
// the portal. A screen that warns before unload about unsaved changes reads it and
// stays quiet for this one leave: the redirect is part of saving, not a way out,
// and the screen has already put its draft somewhere that survives the trip.
let leavingForConfirm = false;
export function isLeavingForConfirm() {
  return leavingForConfirm;
}

// confirmViaPortal leaves the page, so its promise never settles — the returned
// Promise is a way of saying "this call ends here". The pending mutation is not
// resumed on the way back: the user lands on the page they were on, with the
// window open, and clicks the action again. Losing a half-finished action is the
// honest cost of a full-page round-trip, and it keeps us from replaying a
// destructive write the user may have changed their mind about.
//
// A form that holds a draft keeps it across the trip itself, in sessionStorage
// (the SSH screen does, issue #494). That is not a replay: the user gets their
// form back and presses Save again.
async function confirmViaPortal(): Promise<never> {
  const { challenge } = await api.post<{ challenge: string; expires_at: number }>(
    "/auth/elevate/challenge",
    {},
  );
  const params = new URLSearchParams(window.location.search);
  params.set("confirm", challenge);
  const query = params.toString();
  leavingForConfirm = true;
  redirectToPortalConfirm(
    `${window.location.pathname}${query ? `?${query}` : ""}${window.location.hash}`,
  );
  // Navigation is asynchronous; block the caller until the page goes away rather
  // than letting it fall through and report a failure that never happened.
  return new Promise<never>(() => {});
}

export async function submitElevation(password: string) {
  if (!password) return;
  elevateSubmitting.value = true;
  elevateError.value = "";
  try {
    await api.post("/auth/elevate", { password });
    elevateVisible.value = false;
    pending?.resolve();
    pending = null;
  } catch (e) {
    elevateError.value = e instanceof ApiError ? e.message : "Incorrect password.";
  } finally {
    elevateSubmitting.value = false;
  }
}

export function cancelElevation() {
  elevateVisible.value = false;
  pending?.reject(elevationCancelled);
  pending = null;
}

// withElevation runs fn; if the brain rejects with `elevation_required`, it
// drives the password prompt and retries fn exactly once. Any other error (and
// a cancelled prompt) propagates to the caller's onError.
export async function withElevation<T>(fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (e) {
    if (e instanceof ApiError && e.code === "elevation_required") {
      await requestElevation();
      return await fn();
    }
    throw e;
  }
}
