package main

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/onmoose/os/internal/caddy"
)

// caddyEdge is the part of Caddy's config that main builds from the brain's
// own settings: the hosted TLS policy and the dashboard route. The brain
// writes it at startup and again when Caddy lost its config (#613), so both
// use these two methods.
type caddyEdge struct {
	cd *caddy.Client

	// tls is set on a hosted box with a complete acme-dns enrollment.
	tls          bool
	certSubjects []string
	acmeDNS      string
	enrollment   caddy.EnrollmentCredentials

	// uiUpstream is empty in dev, where Vite serves the dashboard and there
	// is no dashboard route.
	dashboardHost string
	brainUpstream string
	uiUpstream    string
}

// writeTLS configures the hosted wildcard HTTPS (ENVIRONMENT.md # Networking
// & discovery). A no-op unless tls is set.
func (e caddyEdge) writeTLS(ctx context.Context) error {
	if !e.tls {
		return nil
	}
	return e.cd.EnsureWildcardTLS(ctx, e.certSubjects, e.acmeDNS, e.enrollment)
}

// writeDashboard installs the dashboard host route (WEB_UI.md # deploy
// model). A no-op unless the UI upstream is set.
func (e caddyEdge) writeDashboard(ctx context.Context) error {
	if e.uiUpstream == "" {
		return nil
	}
	return e.cd.EnsureDashboard(ctx, e.dashboardHost, e.brainUpstream, e.uiUpstream)
}

// caddyMarker is the slice of caddy.Client the restorer uses.
type caddyMarker interface {
	Configured(ctx context.Context) (bool, error)
	BeginConfig(ctx context.Context) (string, error)
	RestoreServer(ctx context.Context) error
	MarkConfigured(ctx context.Context, token string) error
}

// routeRestorer is the slice of lifecycle.Manager the restorer uses.
type routeRestorer interface {
	RestoreRoutes(ctx context.Context, only map[string]bool) (map[string]bool, error)
}

// caddyRestorer writes Caddy's config again when Caddy lost it while the
// brain runs (#613). Caddy keeps no config across a recreate, so a new
// container starts from the staged caddy.json: no app route, no dashboard
// route, no probe server and no TLS policy. The brain writes those at its
// startup, and before this nothing wrote them again until the brain
// restarted, so every app answered 404 in between.
//
// It runs on the health tick, after EnsureCaddyNetworks and before the
// probes. Each tick it asks Caddy for the mark the brain writes once its
// config is in place (caddy.Client.Configured). When the mark is gone it
// writes the config again, in the order the startup does: the trusted
// proxies (caddy.Client.BeginConfig, which returns a token for this Caddy),
// the probe server and the catch-all, the TLS policy, the dashboard route,
// every app route, then the mark. The mark goes on only if Caddy is still the
// one that got the writes (the token), so a recreate in the middle is seen on
// the next tick. A failed server, TLS or dashboard write stops there, with no
// mark, so the next tick starts again.
// The apps RestoreRoutes could not write yet (lock held, install running, a
// failed write) are kept and tried again on each tick until they are done.
// The mark goes on even while some apps are owed, so that a second Caddy
// recreate is still seen.
//
// The probes run after it on the same tick, so they read the routes it just
// wrote. An app whose route is not back yet answers the probe with no sample
// (no route), which clears its app-unresponsive issue and so can only delay
// an automatic restart (#598), never cause one.
type caddyRestorer struct {
	caddy caddyMarker
	life  routeRestorer
	// edge writes the TLS policy and the dashboard route.
	edge func(ctx context.Context) error
	// armed is set at the end of startup, once the startup writes and the
	// mark are done. Before that the startup is still writing the config, and
	// a missing mark means nothing.
	armed atomic.Bool
	// owed are the apps whose route is still to be written, from the last
	// restore. Only the health loop goroutine touches it.
	owed map[string]bool
}

func (r *caddyRestorer) check(ctx context.Context) {
	if !r.armed.Load() {
		return
	}
	ok, err := r.caddy.Configured(ctx)
	if err != nil {
		slog.Warn("caddy restore: could not read whether caddy holds the brain's config; checking again next tick", "err", err)
		return
	}
	if ok {
		if len(r.owed) > 0 {
			owed, err := r.life.RestoreRoutes(ctx, r.owed)
			if err != nil {
				slog.Warn("caddy restore: app routes", "err", err)
				return
			}
			r.owed = owed
		}
		return
	}
	slog.Warn("caddy: lost the brain's config (recreated or restarted); writing it again")
	token, err := r.caddy.BeginConfig(ctx)
	if err != nil {
		slog.Warn("caddy restore: trusted proxies failed; trying again next tick", "err", err)
		return
	}
	if err := r.caddy.RestoreServer(ctx); err != nil {
		slog.Warn("caddy restore: server config failed; trying again next tick", "err", err)
		return
	}
	if err := r.edge(ctx); err != nil {
		slog.Warn("caddy restore: TLS or dashboard route failed; trying again next tick", "err", err)
		return
	}
	owed, err := r.life.RestoreRoutes(ctx, nil)
	if err != nil {
		slog.Warn("caddy restore: app routes failed; trying again next tick", "err", err)
		return
	}
	if err := r.caddy.MarkConfigured(ctx, token); err != nil {
		slog.Warn("caddy restore: trying again next tick", "err", err)
		return
	}
	r.owed = owed
	if len(owed) > 0 {
		// count is how many apps are still owed their route.
		slog.Info("caddy: config written again; some app routes follow on the next ticks", "count", len(owed))
		return
	}
	slog.Info("caddy: config written again")
}
