package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/onmoose/os/internal/notify"
	"github.com/onmoose/os/internal/protocol"
)

// This file is the brain's part of stream A (UPDATES.md # 1, #563): it tells
// host-agent which OS floor this control plane needs, and it tells admins what
// an OS update did. host-agent does the update itself.

// floorFileName is where the brain writes minimumAgentVersion, in its state
// directory. host-agent reads it to refuse an OS target below the running
// control plane's floor (UPDATES.md # 1): booting a host-agent the brain
// refuses to work with would leave a box nobody can reach.
const floorFileName = "minimum-host-agent"

// writeFloorFile writes the floor. A failure is a warning: host-agent then has
// no floor to check, which is the state of a box before #563.
func writeFloorFile(stateDir string) {
	path := filepath.Join(stateDir, floorFileName)
	if err := os.WriteFile(path, []byte(minimumAgentVersion+"\n"), 0o644); err != nil {
		slog.Warn("could not write the control-plane floor for host-agent; it will not check OS targets against it", "err", err, "state_dir", stateDir)
	}
}

// osOutcomeReader is the slice of the host client the outcome check needs.
type osOutcomeReader interface {
	SystemUpdateTarget(ctx context.Context) (protocol.UpdateTarget, error)
}

// notificationLookup says whether a notification was ever raised for a key.
type notificationLookup interface {
	HasNotification(dedupKey string) (bool, error)
}

// osOutcomeNotifier is the slice of the notifier the check needs.
type osOutcomeNotifier interface {
	OSUpdateOutcome(outcomeID, outcome, version, from string) bool
}

// osOutcomeMaxAge bounds which outcomes still get a notification. host-agent
// keeps the last outcome for good, and notifications are pruned after 90 days,
// so without a bound an old outcome would be announced again after its row was
// pruned. A box whose brain was down for a week after an update loses that one
// notification; the outcome is still on the update-target read.
const osOutcomeMaxAge = 7 * 24 * time.Hour

// checkOSOutcome raises one admin notification per OS update outcome.
func checkOSOutcome(ctx context.Context, host osOutcomeReader, seen notificationLookup, n osOutcomeNotifier, now time.Time) {
	t, err := host.SystemUpdateTarget(ctx)
	if err != nil || t.OS == nil || t.OS.Last == nil || t.OS.Last.ID == "" {
		return
	}
	last := t.OS.Last
	at, err := time.Parse(time.RFC3339, last.At)
	if err != nil || now.Sub(at) > osOutcomeMaxAge {
		return
	}
	done, err := seen.HasNotification(notify.OSUpdateDedupKey(last.ID))
	if err != nil {
		slog.Warn("os update: could not check for an earlier notification; trying on the next poll", "err", err)
		return
	}
	if done {
		return
	}
	if n.OSUpdateOutcome(last.ID, last.Outcome, last.Version, last.From) {
		slog.Info("os update: notified admins of the outcome", "os", last.Version)
	}
}

// osOutcomeLoop runs checkOSOutcome on the health-poll cadence.
func osOutcomeLoop(ctx context.Context, host osOutcomeReader, seen notificationLookup, n osOutcomeNotifier, interval time.Duration) {
	for {
		checkOSOutcome(ctx, host, seen, n, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
