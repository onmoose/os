// Package controlplane owns the box's **declaration** of which control-plane
// images it should be running: the brain and the dashboard UI.
//
// It exists because that declaration lives in two files, not one. UPDATES.md
// # 8.3 makes the staged control-plane compose the handoff point between
// host-agent (which recreates containers) and the brain (which reconciles that
// same compose on startup), so the two actors never disagree about what should
// be running. That works for `moose-ui`, which is a service in that compose.
// It cannot work for the brain: the brain is **not** in the compose — a process
// cannot bring itself up, so host-agent launches it with `docker run`
// (internal/hostagent/brainlaunch, CONTROL_PLANE.md # host-agent launches the
// brain container) from a ref that until now came only from MOOSE_BRAIN_IMAGE.
//
// So the box declares its pair across two files, both written before anything
// is recreated:
//
//   - compose.yml   — the UI's image (RewriteUIImage, compose.go). The brain
//     reads this one and reconciles to it.
//   - images.json   — this ledger: the pair the box should be running, plus
//     the pair it was running before. host-agent reads it.
//
// Without the ledger, an applied update is one `docker rm` away from silently
// undoing itself: brainlaunch leaves an existing brain container alone, but a
// box whose brain container has gone (pruned, removed by hand, recreated) would
// relaunch the ref baked into the image at build time and quietly go backwards.
// The ledger is also where the previous pair is recorded, which is what the
// revert path and the 7-day retention window (UPDATES.md # 3) both read.
//
// Nothing here touches Docker or the network. This package writes and reads the
// declaration; the transaction that acts on it is a separate slice.
package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ledgerFile is the ledger's name inside MOOSE_CONTROL_PLANE_DIR, alongside the
// compose.yml it is the companion to.
const ledgerFile = "images.json"

// RetentionWindow is how long the previous image pair (and the brain SQLite
// snapshot taken beside it) is kept before it can be garbage-collected —
// UPDATES.md # 3: "keep the previous brain/UI image pair and SQLite snapshot
// for 7 days, then GC". The realistic complaint is "moose broke since last
// night", and a rollback button that has already deleted its target is not a
// rollback button.
const RetentionWindow = 7 * 24 * time.Hour

// Pair is one control-plane generation: the two image refs that were applied
// together, and when. Refs are whatever `docker run` / `docker compose` accept;
// in production they are digests, because a tag can move under a box
// (BUILD.md # 6 — boxes pull by digest).
type Pair struct {
	Brain     string    `json:"brain"`
	UI        string    `json:"ui"`
	AppliedAt time.Time `json:"applied_at"`
}

// Ledger is the file's whole content: what the box should be running, and the
// one generation it can go back to. Deliberately a single previous entry, not a
// history — UPDATES.md # 3 specifies single-generation rollback ("enough for
// the one-step-back UX, no n-deep history in v1"), and a list would invite a
// depth policy nobody has asked for.
type Ledger struct {
	Current  Pair  `json:"current"`
	Previous *Pair `json:"previous,omitempty"`
}

// LedgerPath is the ledger's path inside the staged control-plane directory.
func LedgerPath(dir string) string { return filepath.Join(dir, ledgerFile) }

// ReadLedger loads the ledger from dir. A missing file returns ok=false with a
// nil error: **that is the normal state of every box that has never updated**,
// including every box shipping today, so it must not read as a failure. A
// present-but-unparseable file IS an error — it means something wrote garbage
// where the box's identity lives, and silently falling back to the baked ref
// would hide it.
func ReadLedger(dir string) (l Ledger, ok bool, err error) {
	p := LedgerPath(dir)
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return Ledger{}, false, nil
	}
	if err != nil {
		return Ledger{}, false, fmt.Errorf("read control-plane ledger: %w", err)
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return Ledger{}, false, fmt.Errorf("parse control-plane ledger %s: %w", p, err)
	}
	return l, true, nil
}

// WriteLedger writes the ledger to dir atomically: a temp file in the same
// directory, fsynced, then renamed over the target, then the directory itself
// fsynced. The ceremony is the point — this file is read on every boot to
// decide which brain to launch, and a half-written one produced by a power cut
// mid-update would be read by the next boot as the box's identity. Rename is
// atomic within a directory, so a reader sees either the old ledger or the new
// one and never a partial.
func WriteLedger(dir string, l Ledger) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal control-plane ledger: %w", err)
	}
	return writeFileAtomic(LedgerPath(dir), append(b, '\n'), 0o644)
}

// ResolveBrainImage answers the question host-agent asks on every boot: which
// brain image should I launch? The ledger wins when it names one, because it
// records what this box last **applied**; envDefault (MOOSE_BRAIN_IMAGE, or the
// baked default) is what this box last **shipped with**, which is older by
// definition once an update has landed.
//
// Every failure mode falls back to envDefault rather than refusing to launch: a
// box that cannot read its ledger still needs a brain to come up, because the
// brain is how anyone finds out what is wrong. fromLedger lets the caller log
// which source won, so the fallback is visible rather than silent.
func ResolveBrainImage(dir, envDefault string) (ref string, fromLedger bool) {
	if dir == "" {
		return envDefault, false
	}
	l, ok, err := ReadLedger(dir)
	if err != nil || !ok || l.Current.Brain == "" {
		return envDefault, false
	}
	return l.Current.Brain, true
}

// PreviousExpired reports whether the retained previous pair is past the
// retention window and may be garbage-collected. False when there is no
// previous pair at all — nothing to collect.
//
// This package only answers the question. Deleting the images and the SQLite
// snapshot belongs to the code that created them.
func (l Ledger) PreviousExpired(now time.Time) bool {
	if l.Previous == nil {
		return false
	}
	return now.Sub(l.Previous.AppliedAt) > RetentionWindow
}

// writeFileAtomic writes data to path via a same-directory temp file, an fsync,
// and a rename, then fsyncs the directory so the rename itself is durable.
// Shared by the ledger and the compose rewrite: both are read at boot to decide
// what the box runs, so neither can afford a torn write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp beside %s: %w", path, err)
	}
	// No-op after a successful rename; cleans up after every failure below.
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename onto %s: %w", path, err)
	}
	// Without this the rename can still be lost to a power cut even though the
	// file contents were synced — the directory entry is its own write.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s to sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
