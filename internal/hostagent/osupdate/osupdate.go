// Package osupdate is stream A's transaction (UPDATES.md # 1, #563): it puts
// a new OS release into the other A/B slot, switches to it inside the update
// window, and decides on the next boot whether the new slot stays.
//
// The update-target loop (internal/hostagent/updatetarget) decides WHICH
// release and WHEN the window is open. This package does the work, in three
// parts:
//
//  1. Install, ahead of the window (an os-install job). Download the bundle to
//     the state partition, check its sha256 against the one the answer names,
//     and hand it to RAUC, which checks its signature against the image's
//     keyring and writes it into the other slot. system.conf has
//     activate-installed=false, so the boot order does not change yet. A
//     failure here changes nothing the box runs.
//  2. Switch, inside the window (an os-switch job). `rauc status mark-active
//     other` puts the new slot first with OK=1 and TRY=0 (RAUC's GRUB backend
//     resets the slot's try flag there; this package checks it did), writes a
//     trial marker for the new slot, and reboots.
//  3. Decide, at the next start of host-agent (Boot).
//     - On the new slot, with its trial marker: the boot is on trial. Once the
//     brain answers its health check the slot is marked good and the marker
//     goes. If it does not answer in time, the slot is marked bad and the box
//     reboots, and GRUB boots the old slot. If host-agent itself never gets
//     this far, moose-os-trial.timer, a unit in the image, reboots the box.
//     - On the old slot with the new slot's marker still there: the new slot
//     failed. The box records the revert, marks the new slot bad and stays.
//     - Any other boot is not on trial and is marked good at once, so a problem
//     that has nothing to do with the OS never moves a box back to its older
//     OS (the maintainer's call for #563).
//
// What survives a reboot lives on the state partition, in Dir: state.json (the
// record below) and one trial-<slot> marker file, which the image's timer reads
// without host-agent.
package osupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onmoose/os/internal/hostagent/updatetarget"
	"github.com/onmoose/os/internal/protocol"
)

// DefaultDir is where the record, the trial marker and the downloaded bundle
// live: under /var/lib/moose, a bind mount from the state partition, so it is
// the same directory on both slots.
const DefaultDir = "/var/lib/moose/os-update"

// DefaultTrialTimeout is how long a trial boot waits for the brain before it
// gives the slot up. The image's moose-os-trial.timer fires later (15 min), so
// a live host-agent always decides first.
const DefaultTrialTimeout = 10 * time.Minute

// MaxTrialTimeout is the longest trial allowed.
const MaxTrialTimeout = 14 * time.Minute

// TrialEndSinceBoot is when a trial must have decided, counted from boot like
// moose-os-trial.timer (OnBootSec=15min). The 2 min between them are for the
// final RAUC mark and its retries, so a live host-agent always decides before
// the timer and the timer never reverts a slot that is slow but healthy.
const TrialEndSinceBoot = 13 * time.Minute

// minTrialWait is the shortest health wait a trial gets, even when the box
// took long to reach it.
const minTrialWait = 10 * time.Second

// bootUptime reads the time since boot from /proc/uptime.
func bootUptime() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	var secs float64
	if _, err := fmt.Sscanf(string(b), "%f", &secs); err != nil {
		return 0, fmt.Errorf("read /proc/uptime: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// FailedNote is the file a trial writes before it gives its slot up, so the
// old slot can tell a tried slot from one whose switch never took effect.
func FailedNote(dir, slot string) string { return filepath.Join(dir, "trial-failed-"+slot) }

// SafetyNetNote is the file moose-os-trial.timer leaves when it reboots a slot.
func SafetyNetNote(dir, slot string) string { return filepath.Join(dir, "safety-net-"+slot) }

// markerRetries and markerRetry bound removeMarker. Vars for tests.
var (
	markerRetries = 80
	markerRetry   = 10 * time.Second
)

// markAttempts and markRetry bound the retries of one rauc mark. Vars for tests.
var (
	markAttempts = 3
	markRetry    = 2 * time.Second
)

// mark runs a rauc mark, retried a few times.
func (a *Applier) mark(state, which string) error {
	var err error
	for i := 0; i < markAttempts; i++ {
		if err = a.RAUC.Mark(context.Background(), state, which); err == nil {
			return nil
		}
		time.Sleep(markRetry)
	}
	return err
}

// removeLeftovers deletes a bundle a kill or a power cut left behind mid-download
// or mid-install: about 438 MB on the state partition.
func (a *Applier) removeLeftovers() {
	for _, f := range []string{"bundle.raucb", "bundle.raucb.part"} {
		if err := os.Remove(filepath.Join(a.dir(), f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Error("os update: could not remove a leftover download", "err", err, "src", f)
		}
	}
}

// statusAttempts and statusRetry bound how long Boot waits for RAUC to
// answer: about 30 s. Vars, so a test does not wait.
var (
	statusAttempts = 10
	statusRetry    = 3 * time.Second
)

// maxBundleBytes caps a download. A bundle holds one slot image, and a slot is
// 1 GiB, so anything past 2 GiB is not a bundle and must not fill the state
// partition.
const maxBundleBytes = 2 << 30

// Job bounds. A 438 MB download and install takes well under a minute on a
// hosted box; the bound is there for a stalled transfer.
const (
	installMaxDuration = 30 * time.Minute
	switchMaxDuration  = 5 * time.Minute
)

// RAUC is the slice of RAUC this package drives. Consumer-side interface; the
// provider is CLIRAUC.
type RAUC interface {
	Status(ctx context.Context) (Status, error)
	Install(ctx context.Context, path string) error
	Mark(ctx context.Context, state, which string) error
	GrubEnvVars(ctx context.Context) (map[string]string, error)
}

// Jobs runs work under host-agent's one job lock. Consumer-side; the provider
// is an adapter over hostagent.Agent.StartJob in cmd/host-agent-real. A
// refusal because a job is running must wrap ErrBusy.
type Jobs interface {
	StartJob(kind string, maxDuration time.Duration, fn func(ctx context.Context) error) (jobID string, err error)
}

// ErrBusy is the job lock's refusal, as Jobs reports it.
var ErrBusy = errors.New("another job is running")

// Doer is the HTTP surface the download needs.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Applier is stream A's transaction. Build it with every field set except the
// optional ones, call Boot once at start, then hand it to the update loop.
type Applier struct {
	RAUC RAUC
	Jobs Jobs
	// Version is the OS release this slot carries: host-agent's own version,
	// stamped from VERSION, the same number the slot's bundle was made with.
	Version string
	// Dir holds the record, the trial markers and the download. Empty means
	// DefaultDir.
	Dir string
	// FloorFile is where the running brain writes its minimum_host_agent. A
	// missing file means no floor check.
	FloorFile string
	// Healthy waits until the brain answers its health check or ctx ends.
	Healthy func(ctx context.Context) error
	// Reboot reboots the box. Nil means `systemctl reboot`.
	Reboot func() error
	// HTTP downloads the bundle. Nil means a plain client.
	HTTP Doer
	// TrialTimeout bounds the trial; zero means DefaultTrialTimeout. The
	// trial also ends TrialEndSinceBoot after boot, whichever comes first.
	TrialTimeout time.Duration
	// Uptime is the time since boot; nil means /proc/uptime.
	Uptime func() (time.Duration, error)
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	slot    string // the booted slot, read at Boot
	running string // the kind of OS job in flight, "" for none
}

// record is state.json. It is written by the slot that switches and read by
// the slot that boots next, which can be either release, so its fields only
// ever grow.
type record struct {
	// Installed is what this package last wrote into a slot that is not the
	// booted one. Nil when the other slot holds nothing of ours.
	Installed *installed `json:"installed,omitempty"`
	// Attempt is the last install attempt, for one attempt per target per
	// night.
	Attempt *attempt `json:"attempt,omitempty"`
	// Switch is the last switch, for one switch per target per night and for
	// the outcome the next boot records.
	Switch *switched `json:"switch,omitempty"`
	// Last is the last outcome, for the report and the admin notification.
	Last *protocol.OSOutcome `json:"last,omitempty"`
}

type installed struct {
	Version string    `json:"version"`
	Digest  string    `json:"digest"`
	Slot    string    `json:"slot"`
	At      time.Time `json:"at"`
}

type attempt struct {
	Version string    `json:"version"`
	Digest  string    `json:"digest"`
	Night   time.Time `json:"night"`
	Error   string    `json:"error,omitempty"`
}

type switched struct {
	Version     string    `json:"version"`
	FromVersion string    `json:"from_version"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	Night       time.Time `json:"night"`
	At          time.Time `json:"at"`
	// Activated is set once mark-active succeeded and the grubenv was
	// checked. A marker for the other slot without it is left by a power cut
	// before the switch took effect, not by a failed trial.
	Activated bool `json:"activated,omitempty"`
	// TrialReboots counts the reboots a trial made without managing to mark
	// the slot bad. Without the mark only GRUB's try flag keeps the slot from
	// booting again, and a firmware can lose that flag (#575), so after one
	// the trial stays up and leaves the decision to the image's timer.
	TrialReboots int `json:"trial_reboots,omitempty"`
}

func (a *Applier) dir() string {
	if a.Dir != "" {
		return a.Dir
	}
	return DefaultDir
}

func (a *Applier) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Applier) recordPath() string { return filepath.Join(a.dir(), "state.json") }

// TrialMarker is the file that says a slot is on trial. moose-os-trial.service
// reads the same path, so its name is part of the image.
func TrialMarker(dir, slot string) string { return filepath.Join(dir, "trial-"+slot) }

func (a *Applier) load() (record, error) {
	var r record
	b, err := os.ReadFile(a.recordPath())
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return record{}, fmt.Errorf("read %s: %w", a.recordPath(), err)
	}
	return r, nil
}

// save writes the record atomically and syncs it, because the next reader may
// be the other slot after a reboot.
func (a *Applier) save(r record) error {
	if err := os.MkdirAll(a.dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeSynced(a.recordPath(), b)
}

func writeSynced(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func other(slot string) string {
	switch slot {
	case "A":
		return "B"
	case "B":
		return "A"
	}
	return ""
}

// Running reports the OS release and booted slot.
func (a *Applier) Running() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Version, a.slot
}

// Floor reads the running brain's minimum_host_agent.
func (a *Applier) Floor() string {
	if a.FloorFile == "" {
		return ""
	}
	b, err := os.ReadFile(a.FloorFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Last is the last recorded outcome, or nil.
func (a *Applier) Last() *protocol.OSOutcome {
	r, err := a.load()
	if err != nil {
		return nil
	}
	return r.Last
}

// Boot reads the booted slot and decides what this boot is (see the package
// comment). It returns at once; a trial runs in the background.
func (a *Applier) Boot(ctx context.Context) {
	// Retried: rauc-service is D-Bus activated and can be slow on a busy
	// boot. Giving up on a trial boot would leave the decision to the image's
	// timer, which would revert a healthy slot.
	var st Status
	var err error
	for i := 0; i < statusAttempts; i++ {
		if st, err = a.RAUC.Status(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(statusRetry):
		}
	}
	if err != nil {
		slog.Error("os update: cannot read the slots; this box will not update its OS until host-agent restarts", "err", err)
		return
	}
	a.mu.Lock()
	a.slot = st.Booted
	a.mu.Unlock()
	booted, oth := st.Booted, other(st.Booted)
	a.removeLeftovers()
	r, rerr := a.load()
	if rerr != nil {
		slog.Error("os update: cannot read the record", "err", rerr)
	}

	if _, err := os.Stat(TrialMarker(a.dir(), booted)); err == nil {
		if s := r.Switch; s != nil && r.Last != nil && r.Last.Outcome == protocol.OSOutcomeGood && r.Last.ID == outcomeID(s) && s.To == booted {
			// The trial already passed; only the marker removal failed.
			slog.Warn("os update: removing a trial marker left on a slot already marked good", "slot", booted)
			go removeMarker(TrialMarker(a.dir(), booted), booted)
		} else {
			slog.Info("os update: this boot is on trial; waiting for the brain before marking the slot good",
				"os", a.Version, "slot", booted)
			go a.trial(booted)
			return
		}
	}

	// Not on trial: the slot is good. Marked at once, so an unrelated problem
	// later in this boot never sends the box back to the other slot.
	if err := a.RAUC.Mark(ctx, "good", "booted"); err != nil {
		slog.Error("os update: could not mark the booted slot good", "err", err, "slot", booted)
	}

	if oth == "" {
		return
	}
	if _, err := os.Stat(TrialMarker(a.dir(), oth)); err != nil {
		return
	}
	// Tried means the switch took effect: the record says so, or the new
	// slot left a note when it gave up (its host-agent, or the image's
	// timer). The notes cover a power cut between mark-active and the
	// record's activated flag.
	_, ferr := os.Stat(FailedNote(a.dir(), oth))
	_, serr := os.Stat(SafetyNetNote(a.dir(), oth))
	tried := ferr == nil || serr == nil
	if s := r.Switch; rerr == nil && !tried && (s == nil || !s.Activated || s.To != oth) {
		// The marker was written but the switch never took effect (a power
		// cut before mark-active): nothing was tried, nothing reverted. The
		// slot keeps what was installed into it.
		slog.Warn("os update: removing a trial marker from a switch that never took effect", "slot", oth)
		go removeMarker(TrialMarker(a.dir(), oth), oth)
		return
	}
	// The other slot was on trial and the box is back here: it reverted.
	out := &protocol.OSOutcome{Outcome: protocol.OSOutcomeReverted, From: a.Version, At: a.now().UTC().Format(time.RFC3339)}
	if r.Switch != nil {
		out.Version = r.Switch.Version
		out.From = r.Switch.FromVersion
		out.ID = outcomeID(r.Switch)
	} else {
		out.ID = "os-reverted-" + a.now().UTC().Format("20060102T150405Z")
	}
	if err := a.mark("bad", "other"); err != nil {
		slog.Error("os update: could not mark the failed slot bad", "err", err, "slot", oth)
	}
	r.Last = out
	r.Installed = nil
	if err := a.save(r); err != nil {
		slog.Error("os update: could not write the record", "err", err)
	}
	go removeMarker(TrialMarker(a.dir(), oth), oth)
	slog.Warn("os update: the new slot did not come up healthy; the box went back to the old slot",
		"os", out.Version, "slot", booted)
	// Which path made the revert: the new slot's host-agent (its trial timed
	// out), or the image's timer, which leaves this note because the new
	// slot's host-agent never got that far.
	if err := os.Remove(FailedNote(a.dir(), oth)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("os update: could not remove the failed-trial note", "err", err, "slot", oth)
	}
	note := SafetyNetNote(a.dir(), oth)
	if _, err := os.Stat(note); err == nil {
		slog.Warn("os update: the image's safety net rebooted the new slot; its host-agent never marked it", "os", out.Version, "slot", oth)
		if err := os.Remove(note); err != nil {
			slog.Error("os update: could not remove the safety-net note", "err", err, "slot", oth)
		}
	}
}

func outcomeID(s *switched) string {
	return fmt.Sprintf("os-%s-%d", s.Version, s.At.Unix())
}

// trial waits for the brain, then keeps or gives up the slot.
func (a *Applier) trial(slot string) {
	timeout := a.TrialTimeout
	if timeout <= 0 {
		timeout = DefaultTrialTimeout
	}
	if timeout > MaxTrialTimeout {
		timeout = MaxTrialTimeout
	}
	// Counted from boot, as the image's timer is.
	up := a.Uptime
	if up == nil {
		up = bootUptime
	}
	if since, err := up(); err == nil {
		if left := TrialEndSinceBoot - since; left < timeout {
			timeout = left
		}
	} else {
		slog.Error("os update: cannot read the time since boot; the trial uses its own bound", "err", err)
	}
	if timeout < minTrialWait {
		timeout = minTrialWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := a.Healthy(ctx)
	cancel()
	if err != nil {
		slog.Error("os update: the new slot is not healthy; marking it bad and rebooting to the old slot",
			"err", err, "os", a.Version, "slot", slot)
		// Left for the old slot: this slot was tried and failed.
		if werr := writeSynced(FailedNote(a.dir(), slot), []byte(a.Version+"\n")); werr != nil {
			slog.Error("os update: could not leave the failed-trial note", "err", werr, "slot", slot)
		}
		if merr := a.mark("bad", "booted"); merr != nil {
			// Without the mark only GRUB's try flag keeps this slot from
			// booting again, and a firmware can lose it (#575), so reboot
			// once at most; after that stay up and let the image's timer
			// decide, rather than reboot into the same slot for ever.
			r, lerr := a.load()
			if lerr != nil || r.Switch == nil || r.Switch.TrialReboots >= 1 {
				slog.Error("os update: could not mark the slot bad, and already rebooted once for it; staying up, the image's safety net decides", "err", merr, "slot", slot)
				return
			}
			r.Switch.TrialReboots++
			if serr := a.save(r); serr != nil {
				slog.Error("os update: could not write the record; staying up, the image's safety net decides", "err", serr, "slot", slot)
				return
			}
			slog.Error("os update: could not mark the slot bad; rebooting once, GRUB skips a slot still on trial", "err", merr, "slot", slot)
		}
		if err := a.reboot(); err != nil {
			slog.Error("os update: the trial cannot reboot; the image's safety net reboots the box", "err", err, "slot", slot)
		}
		return
	}
	if err := a.mark("good", "booted"); err != nil {
		// Do not reboot: with RAUC failing the next boot could be this slot
		// again, for ever. Stay up with the marker, and the image's timer
		// gives the slot up.
		slog.Error("os update: could not mark the new slot good; staying up, the image's safety net decides", "err", err, "slot", slot)
		return
	}
	// The outcome is recorded before the marker goes: if the removal fails,
	// the next boot finds the good outcome and only removes the marker.
	r, err := a.load()
	if err != nil {
		slog.Error("os update: cannot read the record", "err", err)
	}
	out := &protocol.OSOutcome{Outcome: protocol.OSOutcomeGood, Version: a.Version, At: a.now().UTC().Format(time.RFC3339)}
	if r.Switch != nil {
		out.From = r.Switch.FromVersion
		out.ID = outcomeID(r.Switch)
	} else {
		out.ID = "os-" + a.Version + "-" + a.now().UTC().Format("20060102T150405Z")
	}
	r.Last = out
	r.Installed = nil
	if err := a.save(r); err != nil {
		slog.Error("os update: could not write the record", "err", err)
	}
	removeMarker(TrialMarker(a.dir(), slot), slot)
	slog.Info("os update: the new slot is healthy and marked good", "os", a.Version, "slot", slot)
}

func (a *Applier) reboot() error {
	var err error
	if a.Reboot != nil {
		err = a.Reboot()
	} else {
		_, err = run(context.Background(), "systemctl", "reboot")
	}
	if err != nil {
		slog.Error("os update: reboot failed", "err", err)
	}
	return err
}

// Apply is the loop's call (updatetarget.OSApplier).
func (a *Applier) Apply(rel updatetarget.OSRelease, open bool, night time.Time) updatetarget.OSDecision {
	a.mu.Lock()
	slot, running := a.slot, a.running
	a.mu.Unlock()
	switch running {
	case protocol.JobKindOSInstall:
		return updatetarget.OSDecision{State: protocol.OSUpdateInstalling}
	case protocol.JobKindOSSwitch:
		return updatetarget.OSDecision{State: protocol.OSUpdateRebooting}
	}
	if slot == "" {
		return updatetarget.OSDecision{State: protocol.OSUpdateUnsupported, Detail: "the booted slot is not known"}
	}
	// While this boot is on trial the other slot is the way back. Nothing
	// may write it, and nothing may switch, until the trial is decided.
	if _, err := os.Stat(TrialMarker(a.dir(), slot)); err == nil {
		return updatetarget.OSDecision{State: protocol.OSUpdateWaiting, Detail: "this boot is on trial; nothing moves until the new slot is marked good"}
	}
	r, err := a.load()
	if err != nil {
		return updatetarget.OSDecision{State: protocol.OSUpdateFailed, Detail: err.Error()}
	}
	if s := r.Switch; s != nil && s.Version == rel.Version && s.Night.Equal(night) {
		return updatetarget.OSDecision{State: protocol.OSUpdateHeld,
			Detail: "this release was already tried tonight; the next window tries again"}
	}
	if in := r.Installed; in != nil && in.Version == rel.Version && in.Digest == rel.BundleSHA256 && in.Slot == other(slot) {
		if !open {
			return updatetarget.OSDecision{State: protocol.OSUpdateInstalled}
		}
		// One OS switch per night: a box several minors behind takes the
		// next step in a later window, not right after this one (UPDATES.md # 1).
		if s := r.Switch; s != nil && s.Night.Equal(night) {
			return updatetarget.OSDecision{State: protocol.OSUpdateHeld,
				Detail: "the box already switched its OS tonight; the next window switches again"}
		}
		return a.start(protocol.JobKindOSSwitch, switchMaxDuration, protocol.OSUpdateRebooting, func(ctx context.Context) error {
			return a.doSwitch(ctx, rel, night)
		})
	}
	if at := r.Attempt; at != nil && at.Version == rel.Version && at.Digest == rel.BundleSHA256 && at.Night.Equal(night) && at.Error != "" {
		return updatetarget.OSDecision{State: protocol.OSUpdateFailed, Detail: at.Error}
	}
	return a.start(protocol.JobKindOSInstall, installMaxDuration, protocol.OSUpdateInstalling, func(ctx context.Context) error {
		return a.doInstall(ctx, rel, night)
	})
}

func (a *Applier) start(kind string, d time.Duration, state string, fn func(ctx context.Context) error) updatetarget.OSDecision {
	a.mu.Lock()
	a.running = kind
	a.mu.Unlock()
	id, err := a.Jobs.StartJob(kind, d, func(ctx context.Context) error {
		defer func() {
			a.mu.Lock()
			a.running = ""
			a.mu.Unlock()
		}()
		return fn(ctx)
	})
	if err != nil {
		a.mu.Lock()
		a.running = ""
		a.mu.Unlock()
		if errors.Is(err, ErrBusy) {
			return updatetarget.OSDecision{State: protocol.OSUpdateWaiting, Detail: "another job is running; the next check tries again"}
		}
		return updatetarget.OSDecision{State: protocol.OSUpdateFailed, Detail: err.Error()}
	}
	return updatetarget.OSDecision{State: state, JobID: id}
}

// doInstall downloads, checks and installs rel into the other slot.
func (a *Applier) doInstall(ctx context.Context, rel updatetarget.OSRelease, night time.Time) (err error) {
	_, slot := a.Running()
	target := other(slot)
	defer func() {
		r, lerr := a.load()
		if lerr != nil {
			// Never write over a record we could not read: it holds the
			// switch guard and the last outcome.
			slog.Error("os update: cannot read the record; not recording this attempt", "err", lerr)
			if err == nil {
				err = lerr
			}
			return
		}
		r.Attempt = &attempt{Version: rel.Version, Digest: rel.BundleSHA256, Night: night}
		if err != nil {
			r.Attempt.Error = err.Error()
			slog.Error("os update: install failed; the running slot is untouched", "err", err, "os", rel.Version, "slot", target)
		} else {
			r.Installed = &installed{Version: rel.Version, Digest: rel.BundleSHA256, Slot: target, At: a.now().UTC()}
		}
		if serr := a.save(r); serr != nil {
			slog.Error("os update: could not write the record", "err", serr)
			if err == nil {
				err = serr
			}
		}
	}()

	if err := os.MkdirAll(a.dir(), 0o700); err != nil {
		return err
	}
	a.removeLeftovers()
	path := filepath.Join(a.dir(), "bundle.raucb")
	defer os.Remove(path)
	slog.Info("os update: downloading the bundle", "os", rel.Version, "slot", target, "url", updatetarget.RedactURL(rel.BundleURL))
	if err := a.download(ctx, rel, path); err != nil {
		return err
	}
	slog.Info("os update: bundle downloaded and its digest matches", "os", rel.Version, "digest", rel.BundleSHA256,
		"url", updatetarget.RedactURL(rel.BundleURL))
	// The other slot is marked bad while RAUC writes it, so a reboot in the
	// middle never boots half a slot.
	if err := a.RAUC.Install(ctx, path); err != nil {
		return fmt.Errorf("rauc install: %w", err)
	}
	st, err := a.RAUC.Status(ctx)
	if err != nil {
		return err
	}
	if got := st.Slots[target].Version; got != rel.Version {
		return fmt.Errorf("after the install slot %s holds version %q, not %s", target, got, rel.Version)
	}
	slog.Info("os update: installed into the other slot; it waits for the update window",
		"os", rel.Version, "slot", target)
	return nil
}

// download fetches the bundle to path and checks its sha256. The file is
// written as .part and renamed only once its digest matches, so RAUC never sees
// a bundle the answer did not name.
func (a *Applier) download(ctx context.Context, rel updatetarget.OSRelease, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.BundleURL, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", updatetarget.RedactURL(rel.BundleURL), updatetarget.CauseOf(err))
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", updatetarget.RedactURL(rel.BundleURL), updatetarget.CauseOf(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", updatetarget.RedactURL(rel.BundleURL), resp.StatusCode)
	}
	part := path + ".part"
	defer os.Remove(part)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBundleBytes+1))
	if err == nil && n > maxBundleBytes {
		err = fmt.Errorf("the bundle is larger than %d bytes", int64(maxBundleBytes))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", updatetarget.RedactURL(rel.BundleURL), err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != rel.BundleSHA256 {
		return fmt.Errorf("the bundle's sha256 is %s, not the %s the update target names; refusing it", got, rel.BundleSHA256)
	}
	return os.Rename(part, path)
}

// doSwitch makes the installed slot the next boot and reboots.
func (a *Applier) doSwitch(ctx context.Context, rel updatetarget.OSRelease, night time.Time) error {
	_, slot := a.Running()
	target := other(slot)
	st, err := a.RAUC.Status(ctx)
	if err != nil {
		return err
	}
	if got := st.Slots[target].Version; got != rel.Version {
		return fmt.Errorf("slot %s holds %q, not %s; not switching", target, got, rel.Version)
	}
	r, err := a.load()
	if err != nil {
		return err
	}
	// Recorded before anything moves, so a failure below still counts as
	// tonight's one attempt.
	r.Switch = &switched{Version: rel.Version, FromVersion: a.Version, From: slot, To: target, Night: night, At: a.now().UTC()}
	if err := a.save(r); err != nil {
		return err
	}
	undo := func(cause error) error {
		// The trial marker goes only once the booted slot is first again. If
		// that fails, the new slot may still boot next, and it must boot on
		// trial. A marker left while the old slot boots is removed by Boot as a
		// switch that never took effect.
		if err := a.RAUC.Mark(context.Background(), "active", "booted"); err != nil {
			slog.Error("os update: could not put the booted slot first again; the trial marker stays", "slot", target, "err", err)
			return cause
		}
		os.Remove(TrialMarker(a.dir(), target))
		return cause
	}
	// A new trial of this slot starts clean: the notes of an earlier trial
	// would make the image's timer skip it (its once-per-trial guard) or
	// make the old slot read it as tried.
	for _, n := range []string{SafetyNetNote(a.dir(), target), FailedNote(a.dir(), target)} {
		if err := os.Remove(n); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", n, err)
		}
	}
	if err := writeSynced(TrialMarker(a.dir(), target), []byte(rel.Version+"\n")); err != nil {
		return err
	}
	if err := a.RAUC.Mark(ctx, "active", "other"); err != nil {
		return undo(fmt.Errorf("rauc mark-active: %w", err))
	}
	// RAUC's GRUB backend puts the slot first with OK=1 and TRY=0. A stale
	// TRY=1 would make GRUB skip the new slot and the update would look like
	// a failed boot (#570). Check it rather than trust it.
	env, err := a.RAUC.GrubEnvVars(ctx)
	if err != nil {
		return undo(err)
	}
	if order := strings.Fields(env["ORDER"]); len(order) == 0 || order[0] != target || env[target+"_OK"] != "1" || env[target+"_TRY"] != "0" {
		return undo(fmt.Errorf("after mark-active the grubenv is ORDER=%q %s_OK=%q %s_TRY=%q; not rebooting",
			env["ORDER"], target, env[target+"_OK"], target, env[target+"_TRY"]))
	}
	r.Switch.Activated = true
	if err := a.save(r); err != nil {
		return undo(fmt.Errorf("record the switch: %w", err))
	}
	slog.Info("os update: switching slots and rebooting", "os", rel.Version, "slot", target)
	// A reboot that was not accepted must not leave the new slot first: an
	// unrelated reboot later would then switch the OS outside the window.
	if err := a.reboot(); err != nil {
		return undo(fmt.Errorf("reboot: %w", err))
	}
	return nil
}

// Peek reports what the box is doing about rel right now, from the job in
// flight and the record, without starting anything. The update loop decides
// once per tick (every 15 minutes); a reader in between would otherwise see
// "installing" long after the install ended. ok is false when Peek has nothing
// newer than the loop's last decision.
func (a *Applier) Peek(rel updatetarget.OSRelease) (state, detail string, ok bool) {
	a.mu.Lock()
	slot, running := a.slot, a.running
	a.mu.Unlock()
	switch running {
	case protocol.JobKindOSInstall:
		return protocol.OSUpdateInstalling, "", true
	case protocol.JobKindOSSwitch:
		return protocol.OSUpdateRebooting, "", true
	}
	if _, err := os.Stat(TrialMarker(a.dir(), slot)); err == nil {
		return protocol.OSUpdateWaiting, "this boot is on trial; nothing moves until the new slot is marked good", true
	}
	r, err := a.load()
	if err != nil {
		return "", "", false
	}
	if in := r.Installed; in != nil && in.Version == rel.Version && in.Digest == rel.BundleSHA256 && in.Slot == other(slot) {
		return protocol.OSUpdateInstalled, "", true
	}
	if at := r.Attempt; at != nil && at.Version == rel.Version && at.Digest == rel.BundleSHA256 && at.Error != "" {
		return protocol.OSUpdateFailed, at.Error, true
	}
	return "", "", false
}

// removeMarker removes a trial marker, retrying until it is gone or the image's
// timer would act (markerRetries x markerRetry, about 13 min).
func removeMarker(path, slot string) {
	var err error
	for i := 0; i < markerRetries; i++ {
		if err = os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(markerRetry)
	}
	slog.Error("os update: could not remove the trial marker; the image's safety net will revert this good slot", "err", err, "slot", slot)
}
