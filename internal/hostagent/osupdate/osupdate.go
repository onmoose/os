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
	// TrialTimeout bounds the trial; zero means DefaultTrialTimeout.
	TrialTimeout time.Duration
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
	st, err := a.RAUC.Status(ctx)
	if err != nil {
		slog.Error("os update: cannot read the slots; this box will not update its OS until it can", "err", err)
		return
	}
	a.mu.Lock()
	a.slot = st.Booted
	a.mu.Unlock()
	booted, oth := st.Booted, other(st.Booted)

	if _, err := os.Stat(TrialMarker(a.dir(), booted)); err == nil {
		slog.Info("os update: this boot is on trial; waiting for the brain before marking the slot good",
			"os", a.Version, "slot", booted)
		go a.trial(booted)
		return
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
	// The other slot was on trial and the box is back here: it reverted.
	r, err := a.load()
	if err != nil {
		slog.Error("os update: cannot read the record", "err", err)
	}
	out := &protocol.OSOutcome{Outcome: protocol.OSOutcomeReverted, From: a.Version, At: a.now().UTC().Format(time.RFC3339)}
	if r.Switch != nil {
		out.Version = r.Switch.Version
		out.From = r.Switch.FromVersion
		out.ID = outcomeID(r.Switch)
	} else {
		out.ID = "os-reverted-" + a.now().UTC().Format("20060102T150405Z")
	}
	if err := a.RAUC.Mark(ctx, "bad", "other"); err != nil {
		slog.Error("os update: could not mark the failed slot bad", "err", err, "slot", oth)
	}
	r.Last = out
	r.Installed = nil
	if err := a.save(r); err != nil {
		slog.Error("os update: could not write the record", "err", err)
	}
	if err := os.Remove(TrialMarker(a.dir(), oth)); err != nil {
		slog.Error("os update: could not remove the trial marker", "err", err, "slot", oth)
	}
	slog.Warn("os update: the new slot did not come up healthy; the box went back to the old slot",
		"os", out.Version, "slot", booted)
	// Which path made the revert: the new slot's host-agent (its trial timed
	// out), or the image's timer, which leaves this note because the new
	// slot's host-agent never got that far.
	note := filepath.Join(a.dir(), "safety-net-"+oth)
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := a.Healthy(ctx)
	cancel()
	bg := context.Background()
	if err != nil {
		slog.Error("os update: the new slot is not healthy; marking it bad and rebooting to the old slot",
			"err", err, "os", a.Version, "slot", slot)
		if err := a.RAUC.Mark(bg, "bad", "booted"); err != nil {
			slog.Error("os update: could not mark the slot bad; rebooting anyway, GRUB skips a slot still on trial", "err", err)
		}
		a.reboot()
		return
	}
	if err := a.RAUC.Mark(bg, "good", "booted"); err != nil {
		// Without the mark GRUB skips this slot on the next boot. Reboot now,
		// inside the window, rather than leave a slot that will revert at
		// some random later reboot.
		slog.Error("os update: could not mark the new slot good; rebooting to the old slot", "err", err, "slot", slot)
		a.reboot()
		return
	}
	// The marker goes first: once the slot is good, nothing may reboot it
	// away, and the image's timer reboots any slot that still has one.
	if err := os.Remove(TrialMarker(a.dir(), slot)); err != nil {
		slog.Error("os update: could not remove the trial marker", "err", err, "slot", slot)
	}
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
	slog.Info("os update: the new slot is healthy and marked good", "os", a.Version, "slot", slot)
}

func (a *Applier) reboot() {
	var err error
	if a.Reboot != nil {
		err = a.Reboot()
	} else {
		_, err = run(context.Background(), "systemctl", "reboot")
	}
	if err != nil {
		slog.Error("os update: reboot failed", "err", err)
	}
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
			slog.Error("os update: cannot read the record", "err", lerr)
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
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
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
		if err := a.RAUC.Mark(context.Background(), "active", "booted"); err != nil {
			slog.Error("os update: could not put the booted slot first again", "err", err)
		}
		os.Remove(TrialMarker(a.dir(), target))
		return cause
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
	slog.Info("os update: switching slots and rebooting", "os", rel.Version, "slot", target)
	a.reboot()
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
