package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

const gib = 1 << 30

// testPart is one partition of a synthetic image.
type testPart struct {
	name       string
	sizeMiB    uint64
	squashfs   bool
	bytesUsed  uint64
	compressID uint16
}

// writeImage writes a sparse raw disk image with a GPT holding parts, in
// order, and a squashfs superblock at the start of each squashfs partition.
// It is the shape mkosi writes, not a full GPT: no protective MBR, no backup
// header and no CRCs, which slotbudget does not read.
func writeImage(t *testing.T, parts []testPart) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const entries, entrySize = 128, 128
	hdr := make([]byte, 92)
	copy(hdr, "EFI PART")
	binary.LittleEndian.PutUint64(hdr[72:], 2) // entries start at LBA 2
	binary.LittleEndian.PutUint32(hdr[80:], entries)
	binary.LittleEndian.PutUint32(hdr[84:], entrySize)
	if _, err := f.WriteAt(hdr, sectorSize); err != nil {
		t.Fatal(err)
	}

	lba := uint64(2048)
	for i, p := range parts {
		e := make([]byte, entrySize)
		e[0] = 0xAA // any non-zero type GUID
		first := lba
		last := first + p.sizeMiB*2048 - 1
		binary.LittleEndian.PutUint64(e[32:], first)
		binary.LittleEndian.PutUint64(e[40:], last)
		for j, c := range utf16.Encode([]rune(p.name)) {
			binary.LittleEndian.PutUint16(e[56+2*j:], c)
		}
		if _, err := f.WriteAt(e, int64(2*sectorSize+i*entrySize)); err != nil {
			t.Fatal(err)
		}
		if p.squashfs {
			sb := make([]byte, 96)
			binary.LittleEndian.PutUint32(sb[0:], squashfsMagic)
			binary.LittleEndian.PutUint16(sb[20:], p.compressID)
			binary.LittleEndian.PutUint16(sb[28:], 4)
			binary.LittleEndian.PutUint64(sb[40:], p.bytesUsed)
			if _, err := f.WriteAt(sb, int64(first*sectorSize)); err != nil {
				t.Fatal(err)
			}
		}
		lba = last + 1
	}
	if err := f.Truncate(int64(lba * sectorSize)); err != nil {
		t.Fatal(err)
	}
	return path
}

// hostedImage is the hosted layout: ESP 128 MiB, BIOS boot 1 MiB, slot A 1 GiB.
func hostedImage(t *testing.T, used uint64, compression uint16) string {
	return writeImage(t, []testPart{
		{name: "esp", sizeMiB: 128},
		{name: "", sizeMiB: 1},
		{name: slotLabel, sizeMiB: 1024, squashfs: true, bytesUsed: used, compressID: compression},
	})
}

// The measured image from CI run 36909222361: a 435 MB squashfs-xz in a 1 GiB
// slot is 40.5%, inside the 60% budget.
func TestWithinBudget(t *testing.T) {
	r, err := check(hostedImage(t, 435_000_000, squashfsXZ), 60)
	if err != nil {
		t.Fatal(err)
	}
	if r.overBudget() {
		t.Fatalf("435 MB in 1 GiB reported over a 60%% budget (%.1f%%)", r.percent())
	}
	if r.Slot.Number != 3 || r.Slot.Size != gib {
		t.Fatalf("slot = %+v, want partition 3 of 1 GiB", r.Slot)
	}
	if p := r.percent(); p < 40 || p > 41 {
		t.Fatalf("percent = %.2f, want about 40.5", p)
	}
	tbl := r.table()
	for _, want := range []string{"| 3 | moose-slot-a | 1.00 GiB", "2.68%", "40.5% of the slot", "to the 60% budget", "(no label)", "| | in the image |"} {
		if !strings.Contains(tbl, want) {
			t.Errorf("table lacks %q:\n%s", want, tbl)
		}
	}
}

// One byte over 60% fails. Exactly 60% does not.
func TestOverBudgetBoundary(t *testing.T) {
	limit := uint64(gib) * 60 / 100
	r, err := check(hostedImage(t, limit, squashfsXZ), 60)
	if err != nil {
		t.Fatal(err)
	}
	if r.overBudget() {
		t.Fatal("exactly 60% must pass")
	}
	r, err = check(hostedImage(t, limit+1, squashfsXZ), 60)
	if err != nil {
		t.Fatal(err)
	}
	if !r.overBudget() {
		t.Fatal("one byte over 60% must fail")
	}
	if !strings.Contains(r.table(), "OVER the 60% budget") {
		t.Fatalf("table does not say OVER:\n%s", r.table())
	}
}

// GRUB 2.12 reads only xz among compressed squashfs; anything else is a slot
// that does not boot, so it fails the check, not only the budget.
func TestNotXZFails(t *testing.T) {
	const zstd = 6
	_, err := check(hostedImage(t, 100_000_000, zstd), 60)
	var bad badSlotError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "not xz") {
		t.Fatalf("err = %v, want a bad slot error about xz", err)
	}
}

func TestNotSquashfsFails(t *testing.T) {
	img := writeImage(t, []testPart{
		{name: "esp", sizeMiB: 128},
		{name: slotLabel, sizeMiB: 1024}, // an ext4 slot has no hsqs magic
	})
	_, err := check(img, 60)
	var bad badSlotError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "squashfs superblock") {
		t.Fatalf("err = %v, want a bad slot error about the superblock", err)
	}
}

func TestNoSlotFails(t *testing.T) {
	img := writeImage(t, []testPart{{name: "esp", sizeMiB: 128}})
	_, err := check(img, 60)
	var bad badSlotError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a bad slot error", err)
	}
}

func TestNoGPTIsAReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.raw")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := check(path, 60)
	var bad badSlotError
	if err == nil || errors.As(err, &bad) {
		t.Fatalf("err = %v, want a read error (exit 2), not a bad slot", err)
	}
}

// repartSizes reads Label=, Format=, Compression= and the two size bounds
// from a systemd-repart definition.
func repartSizes(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			kv[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return kv
}

// The committed layout is the one this tool and BUILD.md # 1b # Disk budget
// describe: a 1 GiB squashfs-xz slot A and a 128 MiB ESP in the image, and the
// runtime definitions repart reads at every boot agree with them, so repart
// never tries to move or resize what the image already has. Slot B is the
// same size as slot A.
func TestCommittedLayout(t *testing.T) {
	build := filepath.Join("..", "mkosi.repart")
	runtime := filepath.Join("..", "mkosi.extra", "usr", "lib", "repart.d")

	slotA := repartSizes(t, filepath.Join(build, "10-slot-a.conf"))
	for k, want := range map[string]string{"Label": slotLabel, "Format": "squashfs", "Compression": "xz", "SizeMinBytes": "1G", "SizeMaxBytes": "1G"} {
		if slotA[k] != want {
			t.Errorf("mkosi.repart/10-slot-a.conf %s=%q, want %q", k, slotA[k], want)
		}
	}
	esp := repartSizes(t, filepath.Join(build, "00-esp.conf"))
	if esp["SizeMinBytes"] != "128M" || esp["SizeMaxBytes"] != "128M" {
		t.Errorf("mkosi.repart/00-esp.conf sizes = %q/%q, want 128M", esp["SizeMinBytes"], esp["SizeMaxBytes"])
	}
	for file, want := range map[string]string{"00-esp.conf": "128M", "10-slot-a.conf": "1G", "20-slot-b.conf": "1G"} {
		kv := repartSizes(t, filepath.Join(runtime, file))
		if kv["SizeMinBytes"] != want || kv["SizeMaxBytes"] != want {
			t.Errorf("repart.d/%s sizes = %q/%q, want %s", file, kv["SizeMinBytes"], kv["SizeMaxBytes"], want)
		}
	}
	state := repartSizes(t, filepath.Join(runtime, "30-state.conf"))
	if state["Label"] != "moose-state" || state["SizeMaxBytes"] != "" {
		t.Errorf("repart.d/30-state.conf = %v, want label moose-state and no size cap", state)
	}
}
