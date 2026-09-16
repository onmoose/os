package api

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/moose/internal/profile"
	"github.com/onmoose/moose/internal/store"
)

// This file is the brain side of the first-run wizard's non-account steps
// (FIRST_RUN.md # Phase 2, ENVIRONMENT.md # Provisioning — "Setup wizard,
// trimmed"): set the system time zone, record the telemetry choice, and mark
// first-run complete. The account step reuses POST /setup (auth.go); these three
// run after it, as the freshly-created admin, so each is admin-gated. They are
// also the endpoints the later Settings → System → Time and Settings → Privacy
// surfaces reuse, so they are callable any time an admin is signed in, not only
// during first-run.

// registerFirstRun registers the wizard's box-configuration endpoints.
func (s *Server) registerFirstRun(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "set-timezone", Method: "POST", Path: "/api/v1/system/timezone",
		Summary: "Set the system time zone via host-agent timedatectl (admin)",
	}, s.setTimezone)

	huma.Register(api, huma.Operation{
		OperationID: "set-telemetry", Method: "POST", Path: "/api/v1/system/telemetry",
		Summary: "Record the box-wide telemetry consent choice (admin)",
	}, s.setTelemetry)

	huma.Register(api, huma.Operation{
		OperationID: "complete-first-run", Method: "POST", Path: "/api/v1/system/first-run-complete",
		Summary: "Mark the first-run wizard complete so it never reappears (admin)",
	}, s.completeFirstRun)
}

// tzRe bounds an accepted IANA tz database name (area[/location...], e.g.
// "Europe/Stockholm", "UTC"). The privileged host-agent setter re-validates the
// same shape at the timedatectl boundary; this is the brain-side gate so a bad
// zone is a clean 422 rather than a host 502.
var tzRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(/[A-Za-z0-9_+\-]+)*$`)

// setTimezone applies the system time zone (TIME.md # System TZ). Admin-only
// (FIRST_RUN.md # Roles — "Configure box"). The brain holds no time-zone state
// of its own; timedatectl on the host is the source of truth, so this is a pure
// pass-through to host-agent after validation.
func (s *Server) setTimezone(ctx context.Context, in *struct {
	Body struct {
		Timezone string `json:"timezone"`
	}
}) (*struct {
	Body struct {
		Timezone string `json:"timezone"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	zone := strings.TrimSpace(in.Body.Timezone)
	if !tzRe.MatchString(zone) {
		return nil, huma.Error422UnprocessableEntity("a valid IANA time zone is required")
	}
	if err := s.host.SetTimezone(ctx, zone); err != nil {
		return nil, huma.Error502BadGateway("host-agent set-timezone failed", err)
	}
	out := &struct {
		Body struct {
			Timezone string `json:"timezone"`
		}
	}{}
	out.Body.Timezone = zone
	return out, nil
}

// setTelemetry records the box-wide telemetry consent (TELEMETRY.md # Locked:
// off by default — the first-run prompt is the founding admin making the
// box-level choice once). Admin-only; persisted in box_meta. Idempotent.
func (s *Server) setTelemetry(ctx context.Context, in *struct {
	Body struct {
		Enabled bool `json:"enabled"`
	}
}) (*struct {
	Body struct {
		Enabled bool `json:"enabled"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	val := "false"
	if in.Body.Enabled {
		val = "true"
	}
	if err := s.store.SetBoxMeta(store.BoxMetaTelemetryConsent, val); err != nil {
		return nil, huma.Error500InternalServerError("persist telemetry consent", err)
	}
	out := &struct {
		Body struct {
			Enabled bool `json:"enabled"`
		}
	}{}
	out.Body.Enabled = in.Body.Enabled
	return out, nil
}

// completeFirstRun writes the first-run-complete marker so the wizard never
// reappears (FIRST_RUN.md # Phase 3). This is the bootstrap marker the
// cloud-image end-to-end test (C5 #209) asserts. Admin-only and idempotent —
// re-marking an already-complete box is a no-op 200.
func (s *Server) completeFirstRun(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		FirstRunComplete bool `json:"first_run_complete"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := s.store.SetBoxMeta(store.BoxMetaFirstRunComplete, "true"); err != nil {
		return nil, huma.Error500InternalServerError("persist first-run marker", err)
	}
	out := &struct {
		Body struct {
			FirstRunComplete bool `json:"first_run_complete"`
		}
	}{}
	out.Body.FirstRunComplete = true
	return out, nil
}

// profileName is the resolved environment profile as a string, defaulting to
// "appliance" when unset (a zero-value Server, e.g. the OpenAPI emitter). Mirrors
// profile.Read's "absent ⇒ appliance" rule (ENVIRONMENT.md # How the profile is
// realized).
func (s *Server) profileName() string {
	if s.profile == "" {
		return string(profile.Appliance)
	}
	return string(s.profile)
}

// boxMetaBool reads a "true"/"false" box_meta flag, treating an unset key as
// false (the flag was never written).
func (s *Server) boxMetaBool(key string) (bool, error) {
	v, err := s.store.GetBoxMeta(key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
}
