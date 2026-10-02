package osupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onmoose/os/internal/hostagent/updatetarget"
	"github.com/onmoose/os/internal/protocol"
)

// fakeRAUC is a two-slot box with a grubenv, in memory.
type fakeRAUC struct {
	mu         sync.Mutex
	booted     string
	versions   map[string]string
	env        map[string]string
	marks      []string
	installErr error
	installed  []byte
	// staleTry makes mark-active leave the new slot's TRY at 1, the #570 case.
	staleTry bool
}

func newFakeRAUC(booted string) *fakeRAUC {
	return &fakeRAUC{booted: booted, versions: map[string]string{},
		env: map[string]string{"ORDER": "A B", "A_OK": "1", "A_TRY": "0", "B_OK": "0", "B_TRY": "0"}}
}

func (f *fakeRAUC) Status(context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := Status{Booted: f.booted, Slots: map[string]SlotInfo{}}
	for _, s := range []string{"A", "B"} {
		st.Slots[s] = SlotInfo{Version: f.versions[s], Good: f.env[s+"_OK"] == "1" && f.env[s+"_TRY"] == "0"}
	}
	return st, nil
}

func (f *fakeRAUC) Install(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installErr != nil {
		return f.installErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f.installed = b
	o := other(f.booted)
	f.versions[o] = strings.TrimSpace(string(b))
	f.env[o+"_OK"], f.env[o+"_TRY"] = "0", "0"
	return nil
}

func (f *fakeRAUC) Mark(_ context.Context, state, which string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	slot := f.booted
	if which == "other" {
		slot = other(f.booted)
	}
	f.marks = append(f.marks, state+":"+slot)
	switch state {
	case "good":
		f.env[slot+"_OK"], f.env[slot+"_TRY"] = "1", "0"
	case "bad":
		f.env[slot+"_OK"], f.env[slot+"_TRY"] = "0", "0"
	case "active":
		f.env[slot+"_OK"], f.env[slot+"_TRY"] = "1", "0"
		if f.staleTry {
			f.env[slot+"_TRY"] = "1"
		}
		f.env["ORDER"] = slot + " " + other(slot)
	}
	return nil
}

func (f *fakeRAUC) GrubEnvVars(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]string{}
	for k, v := range f.env {
		m[k] = v
	}
	return m, nil
}

// syncJobs runs a job to the end before it returns, so a test reads the
// outcome right after Apply.
type syncJobs struct {
	busy bool
	errs []error
}

func (j *syncJobs) StartJob(_ string, d time.Duration, fn func(ctx context.Context) error) (string, error) {
	if j.busy {
		return "", ErrBusy
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	j.errs = append(j.errs, fn(ctx))
	return "j_1", nil
}

type harness struct {
	a       *Applier
	rauc    *fakeRAUC
	jobs    *syncJobs
	reboots atomic.Int32
	healthy error
	rel     updatetarget.OSRelease
	night   time.Time
}

func newHarness(t *testing.T, booted string) *harness {
	t.Helper()
	body := "0.15.1\n"
	sum := sha256.Sum256([]byte(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	h := &harness{rauc: newFakeRAUC(booted), jobs: &syncJobs{}, night: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}
	h.rel = updatetarget.OSRelease{Version: "0.15.1", BundleURL: srv.URL + "/b.raucb", BundleSHA256: hex.EncodeToString(sum[:])}
	h.a = &Applier{
		RAUC: h.rauc, Jobs: h.jobs, Version: "0.15.0", Dir: t.TempDir(),
		Healthy: func(context.Context) error { return h.healthy },
		Reboot:  func() error { h.reboots.Add(1); return nil },
	}
	h.a.Boot(context.Background())
	return h
}

func TestNormalBootIsMarkedGoodAtOnce(t *testing.T) {
	h := newHarness(t, "A")
	if len(h.rauc.marks) != 1 || h.rauc.marks[0] != "good:A" {
		t.Fatalf("a boot not on trial must be marked good at once, marks %v", h.rauc.marks)
	}
	if v, s := h.a.Running(); v != "0.15.0" || s != "A" {
		t.Fatalf("Running() = %s %s", v, s)
	}
}

func TestInstallThenSwitch(t *testing.T) {
	h := newHarness(t, "A")
	d := h.a.Apply(h.rel, false, h.night)
	if d.State != protocol.OSUpdateInstalling || h.jobs.errs[0] != nil {
		t.Fatalf("install: %+v %v", d, h.jobs.errs)
	}
	if h.rauc.versions["B"] != "0.15.1" {
		t.Fatalf("slot B holds %q", h.rauc.versions["B"])
	}
	if _, err := os.Stat(h.a.dir() + "/bundle.raucb"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the downloaded bundle was not removed: %v", err)
	}
	if d := h.a.Apply(h.rel, false, h.night); d.State != protocol.OSUpdateInstalled {
		t.Fatalf("outside the window an installed slot must wait, got %+v", d)
	}
	if h.reboots.Load() != 0 {
		t.Fatal("rebooted outside the window")
	}
	d = h.a.Apply(h.rel, true, h.night)
	if d.State != protocol.OSUpdateRebooting || h.jobs.errs[1] != nil || h.reboots.Load() != 1 {
		t.Fatalf("switch: %+v %v reboots=%d", d, h.jobs.errs, h.reboots.Load())
	}
	if h.rauc.env["ORDER"] != "B A" || h.rauc.env["B_TRY"] != "0" {
		t.Fatalf("grubenv after the switch: %v", h.rauc.env)
	}
	if _, err := os.Stat(TrialMarker(h.a.dir(), "B")); err != nil {
		t.Fatalf("no trial marker for B: %v", err)
	}
	if d := h.a.Apply(h.rel, true, h.night); d.State != protocol.OSUpdateHeld {
		t.Fatalf("a second switch the same night must be held, got %+v", d)
	}
}

func TestWrongDigestIsRefusedAndNothingInstalled(t *testing.T) {
	h := newHarness(t, "A")
	h.rel.BundleSHA256 = strings.Repeat("0", 64)
	h.a.Apply(h.rel, true, h.night)
	if h.jobs.errs[0] == nil || !strings.Contains(h.jobs.errs[0].Error(), "sha256") {
		t.Fatalf("want a digest refusal, got %v", h.jobs.errs[0])
	}
	if h.rauc.installed != nil {
		t.Fatal("RAUC saw a bundle the answer did not name")
	}
	if d := h.a.Apply(h.rel, true, h.night); d.State != protocol.OSUpdateFailed || len(h.jobs.errs) != 1 {
		t.Fatalf("a failed install must not be retried the same night: %+v, %d jobs", d, len(h.jobs.errs))
	}
	if d := h.a.Apply(h.rel, true, h.night.Add(24*time.Hour)); d.State != protocol.OSUpdateInstalling {
		t.Fatalf("the next night tries again, got %+v", d)
	}
}

func TestStaleTryStopsTheSwitch(t *testing.T) {
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.rauc.staleTry = true
	h.a.Apply(h.rel, true, h.night)
	if h.jobs.errs[1] == nil || h.reboots.Load() != 0 {
		t.Fatalf("a TRY=1 after mark-active must stop the reboot: %v, reboots %d", h.jobs.errs[1], h.reboots.Load())
	}
	if h.rauc.env["ORDER"] != "A B" {
		t.Fatalf("the booted slot must be put first again, ORDER=%q", h.rauc.env["ORDER"])
	}
	if _, err := os.Stat(TrialMarker(h.a.dir(), "B")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the trial marker was left behind")
	}
}

func TestBusyLockWaits(t *testing.T) {
	h := newHarness(t, "A")
	h.jobs.busy = true
	if d := h.a.Apply(h.rel, true, h.night); d.State != protocol.OSUpdateWaiting {
		t.Fatalf("got %+v", d)
	}
}

// reboot simulates the box coming back on slot `booted`, with the record and
// markers on the shared state partition.
func (h *harness) reboot(t *testing.T, booted, version string) *Applier {
	t.Helper()
	h.rauc.mu.Lock()
	h.rauc.booted = booted
	h.rauc.marks = nil
	h.rauc.mu.Unlock()
	a := &Applier{RAUC: h.rauc, Jobs: h.jobs, Version: version, Dir: h.a.dir(),
		Healthy: func(context.Context) error { return h.healthy },
		Reboot:  func() error { h.reboots.Add(1); return nil }}
	return a
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestTrialGood(t *testing.T) {
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	b := h.reboot(t, "B", "0.15.1")
	b.Boot(context.Background())
	waitFor(t, func() bool { return b.Last() != nil })
	last := b.Last()
	if last.Outcome != protocol.OSOutcomeGood || last.Version != "0.15.1" || last.From != "0.15.0" || last.ID == "" {
		t.Fatalf("outcome %+v", last)
	}
	if _, err := os.Stat(TrialMarker(b.dir(), "B")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the trial marker is still there")
	}
	if h.rauc.env["B_OK"] != "1" || h.rauc.env["B_TRY"] != "0" {
		t.Fatalf("B not marked good: %v", h.rauc.env)
	}
}

func TestTrialUnhealthyRebootsAndOldSlotRecordsRevert(t *testing.T) {
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	h.healthy = errors.New("brain never answered")
	b := h.reboot(t, "B", "0.15.1")
	before := h.reboots.Load()
	b.Boot(context.Background())
	waitFor(t, func() bool { return h.reboots.Load() > before })
	h.rauc.mu.Lock()
	bOK := h.rauc.env["B_OK"]
	h.rauc.mu.Unlock()
	if bOK != "0" {
		t.Fatalf("B must be marked bad: %v", h.rauc.env)
	}
	// GRUB boots A again.
	a := h.reboot(t, "A", "0.15.0")
	a.Boot(context.Background())
	last := a.Last()
	if last == nil || last.Outcome != protocol.OSOutcomeReverted || last.Version != "0.15.1" {
		t.Fatalf("outcome %+v", last)
	}
	waitFor(t, func() bool { _, err := os.Stat(TrialMarker(a.dir(), "B")); return errors.Is(err, os.ErrNotExist) })
	if d := a.Apply(h.rel, true, h.night); d.State != protocol.OSUpdateHeld {
		t.Fatalf("after a revert the same release waits for the next night, got %+v", d)
	}
}

func TestFloorFile(t *testing.T) {
	dir := t.TempDir()
	a := &Applier{FloorFile: dir + "/minimum-host-agent"}
	if a.Floor() != "" {
		t.Fatal("a missing floor file must read as no floor")
	}
	if err := os.WriteFile(a.FloorFile, []byte("0.4.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a.Floor() != "0.4.0" {
		t.Fatalf("got %q", a.Floor())
	}
}

func TestParseStatusAndGrubEnv(t *testing.T) {
	js := `{"compatible":"moose-hosted-x86_64","booted":"B","boot_primary":"rootfs.1","slots":[
	 {"rootfs.0":{"class":"rootfs","bootname":"A","state":"inactive","boot_status":"good"}},
	 {"rootfs.1":{"class":"rootfs","bootname":"B","state":"booted","boot_status":"bad",
	   "slot_status":{"bundle":{"compatible":"moose-hosted-x86_64","version":"0.15.1"},"status":"ok"}}}]}`
	st, err := parseStatus([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if st.Booted != "B" || st.Slots["B"].Version != "0.15.1" || !st.Slots["A"].Good || st.Slots["B"].Good {
		t.Fatalf("got %+v", st)
	}
	env := parseGrubEnv([]byte("# GRUB Environment Block\nORDER=B A\nB_TRY=0\n"))
	if env["ORDER"] != "B A" || env["B_TRY"] != "0" {
		t.Fatalf("got %v", env)
	}
}

func TestPeek(t *testing.T) {
	h := newHarness(t, "A")
	if _, _, ok := h.a.Peek(h.rel); ok {
		t.Fatal("nothing done yet, Peek must have nothing to say")
	}
	bad := h.rel
	bad.BundleSHA256 = strings.Repeat("0", 64)
	h.a.Apply(bad, false, h.night)
	if st, d, ok := h.a.Peek(bad); !ok || st != protocol.OSUpdateFailed || !strings.Contains(d, "sha256") {
		t.Fatalf("after a wrong digest: %s %q %v", st, d, ok)
	}
	h.a.Apply(h.rel, false, h.night)
	if st, _, ok := h.a.Peek(h.rel); !ok || st != protocol.OSUpdateInstalled {
		t.Fatalf("after the install: %s %v", st, ok)
	}
}

func TestRevertNotesTheSafetyNet(t *testing.T) {
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	// The image's timer on slot B left its note and rebooted.
	note := h.a.dir() + "/safety-net-B"
	if err := os.WriteFile(note, []byte("2026-10-03T03:20:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := h.reboot(t, "A", "0.15.0")
	a.Boot(context.Background())
	if a.Last() == nil || a.Last().Outcome != protocol.OSOutcomeReverted {
		t.Fatalf("outcome %+v", a.Last())
	}
	if _, err := os.Stat(note); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the safety-net note was not removed")
	}
}

// While the booted slot is on trial, the other slot is the way back: nothing
// installs into it and nothing switches (review of #563).
func TestNothingMovesDuringATrial(t *testing.T) {
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	h.healthy = errors.New("not yet")
	b := h.reboot(t, "B", "0.15.1")
	b.TrialTimeout = time.Hour // the trial stays open for the test
	b.Boot(context.Background())
	next := updatetarget.OSRelease{Version: "0.16.0", BundleURL: h.rel.BundleURL, BundleSHA256: h.rel.BundleSHA256}
	jobs := len(h.jobs.errs)
	if d := b.Apply(next, true, h.night); d.State != protocol.OSUpdateWaiting {
		t.Fatalf("during a trial: %+v", d)
	}
	if len(h.jobs.errs) != jobs {
		t.Fatal("a job started during the trial")
	}
}

// After a good switch tonight, the next step waits for the next window.
func TestOneSwitchPerNight(t *testing.T) {
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	b := h.reboot(t, "B", "0.15.1")
	b.Boot(context.Background())
	waitFor(t, func() bool { return b.Last() != nil })
	// The next step is installed into slot A the same night.
	body := "0.16.0\n"
	sum := sha256.Sum256([]byte(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	next := updatetarget.OSRelease{Version: "0.16.0", BundleURL: srv.URL + "/n.raucb", BundleSHA256: hex.EncodeToString(sum[:])}
	if d := b.Apply(next, true, h.night); d.State != protocol.OSUpdateInstalling {
		t.Fatalf("install of the next step: %+v", d)
	}
	if d := b.Apply(next, true, h.night); d.State != protocol.OSUpdateHeld {
		t.Fatalf("a second switch the same night must wait: %+v", d)
	}
	if d := b.Apply(next, true, h.night.Add(24*time.Hour)); d.State != protocol.OSUpdateRebooting {
		t.Fatalf("the next night switches: %+v", d)
	}
}

// flakyRAUC fails Status a few times, the way a slow rauc-service does.
type flakyRAUC struct {
	*fakeRAUC
	fails int
}

func (f *flakyRAUC) Status(ctx context.Context) (Status, error) {
	if f.fails > 0 {
		f.fails--
		return Status{}, errors.New("rauc-service not ready")
	}
	return f.fakeRAUC.Status(ctx)
}

func TestBootRetriesStatus(t *testing.T) {
	r := &flakyRAUC{fakeRAUC: newFakeRAUC("A"), fails: 3}
	a := &Applier{RAUC: r, Jobs: &syncJobs{}, Version: "0.15.0", Dir: t.TempDir()}
	a.Boot(context.Background())
	if _, slot := a.Running(); slot != "A" {
		t.Fatalf("Boot gave up on a slow RAUC: slot %q", slot)
	}
}

// A record that cannot be read is never written over.
func TestInstallKeepsAnUnreadableRecord(t *testing.T) {
	h := newHarness(t, "A")
	path := h.a.dir() + "/state.json"
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.a.Apply(h.rel, false, h.night)
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "{not json" {
		t.Fatalf("the record was overwritten: %q %v", b, err)
	}
}

// A reboot that was not accepted puts the booted slot first again, so a later
// unrelated reboot does not switch the OS outside the window.
func TestFailedRebootUndoesTheSwitch(t *testing.T) {
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Reboot = func() error { return errors.New("systemctl: no") }
	h.a.Apply(h.rel, true, h.night)
	if h.jobs.errs[1] == nil {
		t.Fatal("the switch reported success with no reboot")
	}
	if h.rauc.env["ORDER"] != "A B" {
		t.Fatalf("ORDER=%q after a failed reboot, want the booted slot first", h.rauc.env["ORDER"])
	}
	if _, err := os.Stat(TrialMarker(h.a.dir(), "B")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the trial marker was left behind")
	}
}

// fastRetries is a no-op kept for readability: TestMain makes every retry
// fast for the whole package, once, so no goroutine a test leaves behind ever
// races a restore.
func fastRetries(*testing.T) {}

func TestMain(m *testing.M) {
	markAttempts, markRetry, markerRetries, markerRetry, statusRetry = 2, time.Millisecond, 3, time.Millisecond, time.Millisecond
	os.Exit(m.Run())
}

// failingMark makes one rauc mark state fail.
type failingMark struct {
	*fakeRAUC
	state string
}

func (f failingMark) Mark(ctx context.Context, state, which string) error {
	if state == f.state {
		return errors.New("rauc: d-bus timeout")
	}
	return f.fakeRAUC.Mark(ctx, state, which)
}

// A power cut between the marker and mark-active leaves a marker for a switch
// that never took effect: no revert, no bad mark, no warning (review of #563).
func TestMarkerWithoutActivationIsNoRevert(t *testing.T) {
	fastRetries(t)
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	r, _ := h.a.load()
	r.Switch = &switched{Version: h.rel.Version, From: "A", To: "B", Night: h.night, At: time.Now()}
	if err := h.a.save(r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TrialMarker(h.a.dir(), "B"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := h.reboot(t, "A", "0.15.0")
	a.Boot(context.Background())
	if a.Last() != nil {
		t.Fatalf("a switch that never took effect was recorded as %+v", a.Last())
	}
	for _, m := range h.rauc.marks {
		if m == "bad:B" {
			t.Fatal("the installed slot was marked bad")
		}
	}
	waitFor(t, func() bool { _, err := os.Stat(TrialMarker(a.dir(), "B")); return errors.Is(err, os.ErrNotExist) })
}

// A marker left on a slot whose trial already passed is only removed: no new
// trial, and the image's timer then finds nothing.
func TestStaleMarkerOnAGoodSlot(t *testing.T) {
	fastRetries(t)
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	b := h.reboot(t, "B", "0.15.1")
	b.Boot(context.Background())
	waitFor(t, func() bool { return b.Last() != nil })
	waitFor(t, func() bool { _, err := os.Stat(TrialMarker(b.dir(), "B")); return errors.Is(err, os.ErrNotExist) })
	if err := os.WriteFile(TrialMarker(b.dir(), "B"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.healthy = errors.New("would fail a new trial")
	before := h.reboots.Load()
	b2 := h.reboot(t, "B", "0.15.1")
	b2.Boot(context.Background())
	waitFor(t, func() bool { _, err := os.Stat(TrialMarker(b2.dir(), "B")); return errors.Is(err, os.ErrNotExist) })
	if h.reboots.Load() != before {
		t.Fatal("a stale marker started a new trial that rebooted")
	}
}

// When RAUC cannot mark the slot bad, the trial reboots once, then stays up.
func TestTrialRebootsOnceWithoutAMark(t *testing.T) {
	fastRetries(t)
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	h.healthy = errors.New("brain down")
	fm := failingMark{fakeRAUC: h.rauc, state: "bad"}
	boot := func() int32 {
		before := h.reboots.Load()
		b := h.reboot(t, "B", "0.15.1")
		b.RAUC = fm
		b.Boot(context.Background())
		time.Sleep(50 * time.Millisecond)
		return h.reboots.Load() - before
	}
	if n := boot(); n != 1 {
		t.Fatalf("first trial: %d reboots, want 1", n)
	}
	if n := boot(); n != 0 {
		t.Fatalf("second trial on the same slot: %d reboots, want 0 (stay up for the safety net)", n)
	}
}

// When RAUC cannot mark the slot good, the trial does not reboot.
func TestTrialStaysUpWhenMarkGoodFails(t *testing.T) {
	fastRetries(t)
	h := newHarness(t, "A")
	h.a.Apply(h.rel, false, h.night)
	h.a.Apply(h.rel, true, h.night)
	before := h.reboots.Load()
	b := h.reboot(t, "B", "0.15.1")
	b.RAUC = failingMark{fakeRAUC: h.rauc, state: "good"}
	b.Boot(context.Background())
	time.Sleep(50 * time.Millisecond)
	if h.reboots.Load() != before || b.Last() != nil {
		t.Fatalf("reboots %d, outcome %+v", h.reboots.Load()-before, b.Last())
	}
	if _, err := os.Stat(TrialMarker(b.dir(), "B")); err != nil {
		t.Fatal("the marker must stay for the image's timer")
	}
}

func TestBootRemovesLeftovers(t *testing.T) {
	h := newHarness(t, "A")
	for _, f := range []string{"bundle.raucb", "bundle.raucb.part"} {
		if err := os.WriteFile(h.a.dir()+"/"+f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := h.reboot(t, "A", "0.15.0")
	a.Boot(context.Background())
	for _, f := range []string{"bundle.raucb", "bundle.raucb.part"} {
		if _, err := os.Stat(h.a.dir() + "/" + f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was left", f)
		}
	}
}
