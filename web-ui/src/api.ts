// Thin fetch wrapper (WEB_UI.md): prepends /api/v1, sends credentials, parses
// {code,message} errors into a typed ApiError. The wire types below are
// generated from the brain's OpenAPI schema; this wrapper stays hand-written —
// it keeps the ApiError mapping + 401 handling that openapi-fetch's
// {data,error} client would not provide. Regenerate the types with
// `npm run gen:api` after a brain DTO change.
import type { components } from "./generated/openapi";

export class ApiError extends Error {
  code: string;
  status: number;
  // location is the field the brain blamed, when it named one (huma's
  // errors[0].location, e.g. "body.keys[2].public_key"). Lets a form show the
  // message next to the input at fault instead of only at its submit button.
  location?: string;
  constructor(code: string, message: string, status: number, location?: string) {
    super(message);
    this.code = code;
    this.status = status;
    this.location = location;
  }
}

// onUnauthenticated is called whenever any request returns 401. The auth
// composable wires this to drop currentUser so the router falls back to the
// login view without each call site having to handle 401 itself.
let onUnauthenticated: (() => void) | null = null;
export function setUnauthenticatedHandler(fn: () => void) {
  onUnauthenticated = fn;
}

// RequestOpts.suppressAuthHandler skips the global drop-to-login on a 401 for
// this one call. Needed where a 401 means "bad input" rather than "session
// expired": POST /me/password returns 401 when the *current* password is wrong
// (the caller is still authenticated), so firing onUnauthenticated would clear
// currentUser and bounce the user to the login screen instead of showing the
// inline "Incorrect password." error. The rare genuinely-expired-session 401
// here self-corrects on the next API call, which still drops to login.
export interface RequestOpts {
  suppressAuthHandler?: boolean;
}

async function request<T>(method: string, path: string, body?: unknown, opts?: RequestOpts): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    credentials: "include",
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({}));
    if (res.status === 401 && onUnauthenticated && !opts?.suppressAuthHandler) onUnauthenticated();
    // The brain uses huma's default error model: { detail, title, errors:[{message}] }.
    // Some endpoints (jobs) carry an explicit { code, message }. Accept both, and
    // surface the first per-field message (e.g. the duplicate-install summary).
    const code = err.code ?? err.detail ?? "unknown";
    const message =
      err.message ?? err.errors?.[0]?.message ?? err.detail ?? err.title ?? res.statusText;
    throw new ApiError(code, message, res.status, err.errors?.[0]?.location);
  }
  return res.status === 204 ? (undefined as T) : ((await res.json()) as T);
}

export const api = {
  get: <T>(p: string) => request<T>("GET", p),
  post: <T>(p: string, b?: unknown, opts?: RequestOpts) => request<T>("POST", p, b, opts),
  put: <T>(p: string, b?: unknown) => request<T>("PUT", p, b),
  patch: <T>(p: string, b?: unknown) => request<T>("PATCH", p, b),
  del: <T>(p: string) => request<T>("DELETE", p),
};

// --- wire types (generated from the brain's OpenAPI schema) ----------------
// Backed by api/openapi.json via `npm run gen:api` (openapi-typescript): the
// brain's huma handler structs are the single source of truth. These aliases
// keep the export names the dashboard already imports, so call sites are
// unchanged. CI's `make openapi-check` keeps the committed spec fresh, so these
// types cannot silently drift from the Go DTOs.
type Schemas = components["schemas"];

export type User = Schemas["UserDTO"];
export type AuthState = Schemas["Auth-stateResponse"];
export type SetupResult = Schemas["SetupResponse"];
export type CatalogEntry = Schemas["Entry"];
export type CatalogDetail = Schemas["Detail"];
export type CatalogHome = Schemas["Home"];
// HomeGroupView is one rendered category row of the landing page (a spotlight
// app plus ordered groups, authored in store's home.yml). Exported on its own
// so the packing helper (lib/storeLayout.ts) can type against it directly.
export type HomeGroupView = Schemas["HomeGroupView"];
export type CatalogCategory = Schemas["CategoryPage"];
// CatalogCategoryRef is one entry of the authored category vocabulary: a category
// id with the display label the store renders. Carried on the landing payload.
export type CatalogCategoryRef = Schemas["Category"];
export type Instance = Schemas["InstanceDTO"];
// Exposure is the per-app access mode (#306, hosted only): "restricted" is the
// box-login-gated owner-only default, "public" is anonymous. Unlike Scope below,
// the brain DOES declare this one as an enum, so the generated schema carries the
// literal union — derive from it rather than re-typing the values here.
export type Exposure = Instance["exposure"];
export type Notification = Schemas["NotificationDTO"];
export type AuditEvent = Schemas["AuditEventDTO"];
// HealthIssue mirrors health.Issue (HEALTH.md # Issue shape) — the degraded-mode
// banner's source of truth (issue #12). severity/category are free strings in the
// schema (same as scope/status); the UI compares them against the known values.
export type HealthIssue = Schemas["Issue"];
export type Job = Schemas["Job"];
export type FolderElection = Schemas["FolderElection"];
export type InstallRequest = Schemas["Install-appRequest"];
export type InstallPlan = Schemas["InstallPlanDTO"];
export type InstallPlanFolder = Schemas["InstallPlanFolder"];
export type InstallPlanPermissions = Schemas["InstallPlanPermissions"];
export type InstallPlanConfigField = Schemas["InstallPlanConfigField"];
export type AppConfig = Schemas["AppConfigDTO"];
export type AppConfigField = Schemas["AppConfigFieldDTO"];
export type SourceMenu = Schemas["SourceMenu"];
export type FolderSources = Schemas["FolderSources"];
export type MailProvider = Schemas["MailProviderDTO"];
export type MailProviderOption = Schemas["MailProviderOption"];
export type MailPreset = Schemas["MailPresetDTO"];
export type MailPresetRegionOption = Schemas["MailPresetRegionOptionDTO"];
export type SystemStorage = Schemas["SystemStorageDTO"];
export type SystemVersion = Schemas["SystemVersionDTO"];
export type DiskSpace = Schemas["DiskSpaceDTO"];
export type AppSecrets = Schemas["AppSecretsDTO"];
export type AppSecret = Schemas["AppSecretDTO"];
// SSHAccess is the whole Settings -> SSH screen state for the signed-in user:
// the on/off flag, whether the account also demands the moose password, the
// account's keys, and key_required, the server's answer to "does this box make
// a public key the mandatory factor?" (true on hosted, false on the appliance).
// The screen reads key_required rather than checking the profile itself, so the
// rule lives only in the brain (AUTH.md # Device access).
export type SSHAccess = Schemas["SSHAccessDTO"];
export type SSHKey = Schemas["SSHKeyDTO"];

// Scope is a UI-side literal union, intentionally NOT generated. The brain
// serves scope (like severity / status / state) as a free string — the huma
// structs don't declare enums (filed as a follow-up in
// docs/progress/openapi-codegen.md). The dashboard only ever sets/compares the
// two real values, and the install setup page indexes FolderSources by scope, which
// needs the literal union.
export type Scope = "household" | "personal";

// --- Door-2 hand-maintained types (not in the OpenAPI spec) -----------------
// These types back the custom-container install endpoints which bypass huma
// and are therefore not generated. See DASHBOARD.md # Door-2.

// Door-2 inspect: admin-only parse of a pasted compose for service dropdown
// and main-port prefill (main_port 0 = could not infer; the form asks).
export interface CustomInspectResult {
  services: string[];
  main_port: number;
}

// CustomFolderGrant is one Door-2 folder grant: a use-case folder (Source
// picker), the in-container destination the admin types (target — Door-2 has no
// author to map MOOSE_FOLDER_<NAME>), and read/write.
export interface CustomFolderGrant {
  folder: string;
  mode?: "read" | "write";
  target?: string;
}

// CustomPermissions is the structured Door-2 permission election (form mode).
// internet defaults on server-side when omitted.
export interface CustomPermissions {
  internet?: boolean;
  lan?: boolean;
  gpu?: boolean;
  folders?: CustomFolderGrant[];
  devices?: string[];
}

// CustomPermissionsResolved is a parsed/normalized permission set (the parse
// endpoint result the form repopulates from) — internet is concrete here.
export interface CustomPermissionsResolved {
  internet: boolean;
  lan: boolean;
  gpu: boolean;
  folders: CustomFolderGrant[];
  devices: string[];
}

export interface CustomOverlayRenderResult {
  overlay: string;
}
export interface CustomOverlayParseResult {
  permissions: CustomPermissionsResolved;
}

// CustomInstallRequest is the POST /api/v1/apps/custom body. The form sends
// `permissions`; the Edit-as-YAML toggle sends a raw `overlay` instead, which
// wins server-side. scope follows the store convention (household for admins by
// choice, forced/silent personal otherwise).
export interface CustomInstallRequest {
  name: string;
  compose: string;
  main_service?: string;
  main_port: number;
  scope?: Scope;
  permissions?: CustomPermissions;
  overlay?: string;
}

// Poll a job to a terminal state (Pattern B). A useJob() composable with
// refetchInterval is the real shape; this is enough for the skeleton. onPoll, if
// given, is called with each non-terminal poll so callers can surface the job's
// live `step` (e.g. the install button's phase label, #150) instead of discarding
// every intermediate state.
export async function waitForJob(jobId: string, onPoll?: (job: Job) => void): Promise<Job> {
  for (;;) {
    const job = await api.get<Job>(`/jobs/${jobId}`);
    if (job.status !== "running" && job.status !== "cancelling") return job;
    onPoll?.(job);
    await new Promise((r) => setTimeout(r, 600));
  }
}
