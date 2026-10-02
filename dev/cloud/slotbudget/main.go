// Command slotbudget checks the hosted image's OS slot against its budget
// (BUILD.md # 1b # Disk budget, #561).
//
// An OS slot is a fixed 1 GiB partition that holds a read-only squashfs. Its
// size is fixed for the life of a box, because slot B is made right after
// slot A at first boot. So the build fails long before the slot is full: when
// the squashfs in slot A fills more than 60% of the partition, the build stops
// and someone has to make room or change the budget on purpose.
//
//	slotbudget [-max-percent 60] [-summary FILE] [-title TEXT] [-extract FILE] IMAGE.raw
//
// It reads the raw disk image directly: the GPT, then the squashfs superblock
// at the start of the partition labelled moose-slot-a. It needs no loop
// device and no root. It prints the partition table with each partition's
// share of the smallest hosted disk (40 GB, a Hetzner CX23) and the slot's
// measured content, and appends the same table to FILE when -summary is given
// (CI passes $GITHUB_STEP_SUMMARY).
//
// With -extract FILE it also writes the slot's squashfs to FILE, cut to the
// squashfs's own size rounded up to 4 KiB (the block size mksquashfs pads
// to), never past the partition. That is the slot image the OS update bundle
// carries (dev/cloud/rauc-bundle.sh, BUILD.md # 1b # The bundle, #562): the
// same bytes as slot A in the disk image. It is written only when the slot
// is within budget.
//
// Exit status: 0 within budget, 1 over budget or not the slot GRUB can read,
// 2 when the image cannot be read.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf16"
)

const (
	sectorSize = 512

	// slotLabel is slot A's GPT partition name (dev/cloud/mkosi.repart/10-slot-a.conf).
	slotLabel = "moose-slot-a"

	// smallestDisk is the disk of the smallest hosted box, a Hetzner CX23:
	// 40 GB. Every cost is shown as a share of it, so no cost looks smaller
	// than it is on the box where it hurts most.
	smallestDisk = 40_000_000_000

	squashfsMagic = 0x73717368 // "hsqs", little-endian
	squashfsXZ    = 4          // the superblock's compression id for xz
)

// partition is one used GPT entry.
type partition struct {
	Number int
	Name   string
	Start  uint64 // byte offset
	Size   uint64 // bytes
}

// squashfs is what the slot check needs from a squashfs superblock.
type squashfs struct {
	BytesUsed   uint64
	Compression uint16
}

// report is the result of one check.
type report struct {
	Partitions []partition
	Slot       partition
	Content    squashfs
	MaxPercent int
}

func (r report) percent() float64 {
	return float64(r.Content.BytesUsed) * 100 / float64(r.Slot.Size)
}

func (r report) overBudget() bool {
	return r.Content.BytesUsed*100 > r.Slot.Size*uint64(r.MaxPercent)
}

func main() {
	maxPercent := flag.Int("max-percent", 60, "fail when the squashfs fills more than this share of the slot")
	summary := flag.String("summary", "", "append the Markdown table to this file (for $GITHUB_STEP_SUMMARY)")
	title := flag.String("title", "OS slot budget", "the heading of the table in -summary")
	extract := flag.String("extract", "", "write the slot's squashfs to this file (the OS update bundle's image)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: slotbudget [-max-percent N] [-summary FILE] [-title TEXT] [-extract FILE] IMAGE.raw")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	r, err := check(flag.Arg(0), *maxPercent)
	if err != nil {
		fmt.Fprintln(os.Stderr, "slotbudget:", err)
		var bad badSlotError
		if errors.As(err, &bad) {
			os.Exit(1)
		}
		os.Exit(2)
	}

	table := r.table()
	fmt.Print(table)
	if *summary != "" {
		if err := appendFile(*summary, "### "+*title+"\n\n"+table+"\n"); err != nil {
			fmt.Fprintln(os.Stderr, "slotbudget: write summary:", err)
			os.Exit(2)
		}
	}
	if r.overBudget() {
		fmt.Fprintf(os.Stderr, "slotbudget: OVER BUDGET. The squashfs in %s is %s, %.1f%% of the %s slot; the budget is %d%%.\n"+
			"A slot is fixed for the life of a box, so make the image smaller, or raise the budget on purpose (BUILD.md # 1b # Disk budget).\n",
			r.Slot.Name, human(r.Content.BytesUsed), r.percent(), human(r.Slot.Size), r.MaxPercent)
		os.Exit(1)
	}
	fmt.Printf("slot budget ok: the squashfs is %.1f%% of the slot (budget %d%%)\n", r.percent(), r.MaxPercent)
	if *extract != "" {
		n, err := extractSlot(flag.Arg(0), r, *extract)
		if err != nil {
			fmt.Fprintln(os.Stderr, "slotbudget: extract:", err)
			os.Exit(2)
		}
		fmt.Printf("slot image written: %s, %s\n", *extract, human(n))
	}
}

// slotImageAlign is the block size mksquashfs pads a squashfs to by default.
const slotImageAlign = 4096

// slotImageSize is how many bytes of the slot make its image: the squashfs's
// bytes_used rounded up to 4 KiB, never past the end of the partition.
func slotImageSize(r report) uint64 {
	n := (r.Content.BytesUsed + slotImageAlign - 1) / slotImageAlign * slotImageAlign
	if n > r.Slot.Size {
		n = r.Slot.Size
	}
	return n
}

// extractSlot copies slot A's image out of the disk image at path into dst,
// and returns how many bytes it wrote.
func extractSlot(path string, r report, dst string) (uint64, error) {
	src, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n := slotImageSize(r)
	if _, err := io.Copy(out, io.NewSectionReader(src, int64(r.Slot.Start), int64(n))); err != nil {
		out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return n, nil
}

// badSlotError marks an image that was read fine but whose slot is wrong:
// missing, not a squashfs, or not compressed the way GRUB can read.
type badSlotError struct{ msg string }

func (e badSlotError) Error() string { return e.msg }

// check reads IMAGE and measures slot A.
func check(path string, maxPercent int) (report, error) {
	if maxPercent <= 0 || maxPercent > 100 {
		return report{}, fmt.Errorf("-max-percent must be 1 to 100, got %d", maxPercent)
	}
	f, err := os.Open(path)
	if err != nil {
		return report{}, err
	}
	defer f.Close()

	parts, err := readGPT(f)
	if err != nil {
		return report{}, err
	}
	r := report{Partitions: parts, MaxPercent: maxPercent}
	found := false
	for _, p := range parts {
		if p.Name == slotLabel {
			r.Slot, found = p, true
			break
		}
	}
	if !found {
		return report{}, badSlotError{fmt.Sprintf("no partition labelled %s in %s", slotLabel, path)}
	}
	sq, err := readSquashfs(f, r.Slot.Start)
	if err != nil {
		return report{}, err
	}
	if sq.Compression != squashfsXZ {
		return report{}, badSlotError{fmt.Sprintf("%s is a squashfs with compression id %d, not xz (%d); GRUB 2.12 reads no other compressed squashfs", slotLabel, sq.Compression, squashfsXZ)}
	}
	if sq.BytesUsed > r.Slot.Size {
		return report{}, badSlotError{fmt.Sprintf("%s: the squashfs says it is %d bytes, more than its %d byte partition", slotLabel, sq.BytesUsed, r.Slot.Size)}
	}
	r.Content = sq
	return r, nil
}

// readGPT returns the used entries of the GPT in r, assuming 512-byte sectors
// (a raw image mkosi wrote).
func readGPT(r io.ReaderAt) ([]partition, error) {
	hdr := make([]byte, 92)
	if _, err := r.ReadAt(hdr, sectorSize); err != nil {
		return nil, fmt.Errorf("read GPT header: %w", err)
	}
	if string(hdr[0:8]) != "EFI PART" {
		return nil, errors.New("no GPT header at sector 1")
	}
	entriesLBA := binary.LittleEndian.Uint64(hdr[72:80])
	count := binary.LittleEndian.Uint32(hdr[80:84])
	entrySize := binary.LittleEndian.Uint32(hdr[84:88])
	if entrySize < 128 || count == 0 || count > 1024 {
		return nil, fmt.Errorf("GPT header has %d entries of %d bytes", count, entrySize)
	}
	buf := make([]byte, int(count)*int(entrySize))
	if _, err := r.ReadAt(buf, int64(entriesLBA)*sectorSize); err != nil {
		return nil, fmt.Errorf("read GPT entries: %w", err)
	}
	var parts []partition
	zero := make([]byte, 16)
	for i := 0; i < int(count); i++ {
		e := buf[i*int(entrySize) : (i+1)*int(entrySize)]
		if bytes.Equal(e[0:16], zero) {
			continue
		}
		first := binary.LittleEndian.Uint64(e[32:40])
		last := binary.LittleEndian.Uint64(e[40:48])
		if last < first {
			return nil, fmt.Errorf("GPT entry %d ends before it starts", i+1)
		}
		parts = append(parts, partition{
			Number: i + 1,
			Name:   gptName(e[56:128]),
			Start:  first * sectorSize,
			Size:   (last - first + 1) * sectorSize,
		})
	}
	if len(parts) == 0 {
		return nil, errors.New("the GPT has no partitions")
	}
	return parts, nil
}

// gptName decodes a GPT partition name: UTF-16LE, NUL-padded.
func gptName(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i : i+2])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// readSquashfs reads the squashfs 4.0 superblock at off.
func readSquashfs(r io.ReaderAt, off uint64) (squashfs, error) {
	sb := make([]byte, 96)
	if _, err := r.ReadAt(sb, int64(off)); err != nil {
		return squashfs{}, fmt.Errorf("read squashfs superblock: %w", err)
	}
	if binary.LittleEndian.Uint32(sb[0:4]) != squashfsMagic {
		return squashfs{}, badSlotError{fmt.Sprintf("%s does not start with a squashfs superblock", slotLabel)}
	}
	if major := binary.LittleEndian.Uint16(sb[28:30]); major != 4 {
		return squashfs{}, badSlotError{fmt.Sprintf("%s is squashfs version %d, want 4", slotLabel, major)}
	}
	return squashfs{
		Compression: binary.LittleEndian.Uint16(sb[20:22]),
		BytesUsed:   binary.LittleEndian.Uint64(sb[40:48]),
	}, nil
}

// table renders the partitions, each as a share of the smallest disk, and the
// slot's content against its budget.
func (r report) table() string {
	var b strings.Builder
	b.WriteString("| # | Partition | Size | Share of 40 GB | Content | Headroom |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	var reserved uint64
	for _, p := range r.Partitions {
		reserved += p.Size
		name := p.Name
		if name == "" {
			name = "(no label)"
		}
		content, headroom := "", ""
		if p.Number == r.Slot.Number {
			content = fmt.Sprintf("squashfs %s (%.1f%% of the slot)", human(r.Content.BytesUsed), r.percent())
			budget := p.Size * uint64(r.MaxPercent) / 100
			if r.Content.BytesUsed <= budget {
				headroom = fmt.Sprintf("%s to the %d%% budget", human(budget-r.Content.BytesUsed), r.MaxPercent)
			} else {
				headroom = fmt.Sprintf("%s OVER the %d%% budget", human(r.Content.BytesUsed-budget), r.MaxPercent)
			}
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s |\n", p.Number, name, human(p.Size), share(p.Size), content, headroom)
	}
	fmt.Fprintf(&b, "| | in the image | %s | %s | | |\n", human(reserved), share(reserved))
	return b.String()
}

func share(n uint64) string {
	return fmt.Sprintf("%.2f%%", float64(n)*100/smallestDisk)
}

// human prints n in MiB below 1 GiB and in GiB from there, with the byte count.
func human(n uint64) string {
	const mib, gib = 1 << 20, 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.2f GiB (%d bytes)", float64(n)/gib, n)
	}
	return fmt.Sprintf("%.1f MiB (%d bytes)", float64(n)/mib, n)
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
