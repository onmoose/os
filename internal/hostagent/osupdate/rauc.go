package osupdate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Status is what `rauc status` reports that this package uses.
type Status struct {
	// Booted is the bootname of the slot the box booted ("A" or "B").
	Booted string
	// Slots maps a bootname to what RAUC knows about that slot.
	Slots map[string]SlotInfo
}

// SlotInfo is one slot's RAUC status.
type SlotInfo struct {
	// Version is the bundle version RAUC installed into the slot, or "" for a
	// slot RAUC never wrote (slot A as the image shipped it).
	Version string
	// Good is RAUC's boot status for the slot: in the bootloader's order,
	// <slot>_OK=1 and <slot>_TRY=0.
	Good bool
}

// CLIRAUC drives RAUC through its command line, which talks to the
// rauc-service D-Bus daemon. GrubEnv is the grubenv RAUC's GRUB backend writes
// (/etc/rauc/system.conf); empty means DefaultGrubEnv.
type CLIRAUC struct {
	GrubEnv string
}

// DefaultGrubEnv is where the hosted image keeps the grubenv (BUILD.md # 1b).
const DefaultGrubEnv = "/efi/grub/grubenv"

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// raucStatusJSON is the part of `rauc status --detailed --output-format=json`
// read here. RAUC writes slots as a list of one-key objects, keyed by the slot
// name (rootfs.0).
type raucStatusJSON struct {
	Booted string                    `json:"booted"`
	Slots  []map[string]raucSlotJSON `json:"slots"`
}

type raucSlotJSON struct {
	Bootname   string `json:"bootname"`
	BootStatus string `json:"boot_status"`
	SlotStatus *struct {
		Bundle *struct {
			Version string `json:"version"`
		} `json:"bundle"`
	} `json:"slot_status"`
}

// parseStatus reads RAUC's JSON status.
func parseStatus(b []byte) (Status, error) {
	var j raucStatusJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Status{}, fmt.Errorf("read rauc status: %w", err)
	}
	st := Status{Booted: j.Booted, Slots: map[string]SlotInfo{}}
	for _, m := range j.Slots {
		for _, s := range m {
			if s.Bootname == "" {
				continue
			}
			info := SlotInfo{Good: s.BootStatus == "good"}
			if s.SlotStatus != nil && s.SlotStatus.Bundle != nil {
				info.Version = s.SlotStatus.Bundle.Version
			}
			st.Slots[s.Bootname] = info
		}
	}
	if st.Booted == "" {
		return Status{}, fmt.Errorf("rauc status names no booted slot")
	}
	return st, nil
}

// Status reads RAUC's view of the slots.
func (r CLIRAUC) Status(ctx context.Context) (Status, error) {
	out, err := run(ctx, "rauc", "status", "--detailed", "--output-format=json")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(out)
}

// Install writes the bundle into the other slot. With activate-installed=false
// in system.conf it does not change the boot order: the switch is a separate
// step, inside the window.
func (r CLIRAUC) Install(ctx context.Context, path string) error {
	_, err := run(ctx, "rauc", "install", path)
	return err
}

// Mark runs `rauc status mark-<state> <which>`: state is good, bad or active;
// which is booted or other.
func (r CLIRAUC) Mark(ctx context.Context, state, which string) error {
	_, err := run(ctx, "rauc", "status", "mark-"+state, which)
	return err
}

// GrubEnvVars reads the grubenv as key=value pairs.
func (r CLIRAUC) GrubEnvVars(ctx context.Context) (map[string]string, error) {
	path := r.GrubEnv
	if path == "" {
		path = DefaultGrubEnv
	}
	out, err := run(ctx, "grub-editenv", path, "list")
	if err != nil {
		return nil, err
	}
	return parseGrubEnv(out), nil
}

func parseGrubEnv(b []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			m[k] = v
		}
	}
	return m
}
