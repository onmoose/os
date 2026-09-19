//go:build linux

// Package diskusage is host-agent's disk free/total reporter behind GET
// /v1/system/status. It is a thin syscall.Statfs wrapper over the volumes of
// interest — pure Go, no cgo. Two consumers:
//
//   - DataDisk() backs the install-plan free_bytes figure (the data drive only,
//     BRAIN_UI_PROTOCOL.md # install-plan) so the install dialog can warn before
//     a download that won't fit.
//   - Disks() backs the system-resources panel's Storage bars (LOCAL_ANALYTICS.md
//     # Real-time system resources): one entry per mounted volume — the OS drive
//     always, the data drive when present.
//
// The figures are advisory: a statfs snapshot reserves nothing, so the brain
// treats them as a hint, never a gate. A statfs error (mount absent, path gone)
// fails open — DataDisk returns (0, 0), which the brain reads as "not measured";
// Disks omits the entry — the same fail-open posture as the locus-B reporters.
package diskusage

import (
	"log/slog"
	"syscall"

	"github.com/onmoose/os/internal/protocol"
)

// Mount points and labels of the volumes of interest (STORAGE.md mount layout).
// The OS drive is the root filesystem; the data drive is the mergerfs union (or,
// on a Level-0 box, just a directory on the OS drive — see Disks).
const (
	osDiskMount   = "/"
	osDiskLabel   = "System"
	dataDiskMount = "/srv/moose"
	dataDiskLabel = "Data"
)

// Reporter implements hostagent.DiskReporter and hostagent.DiskSpaceReporter.
// The mount paths and deviceID lookup are fields so tests can point them at real
// temp dirs and drive the present/absent branch without two real filesystems.
type Reporter struct {
	osPath   string
	dataPath string
	// deviceID returns a path's backing-filesystem id (st_dev). Disks compares
	// the data drive's against the OS drive's to tell a real data drive from a
	// Level-0 directory on the OS drive. A field so tests can inject it.
	deviceID func(path string) (uint64, error)
}

// New returns a Reporter measuring the OS drive (/) and the data drive
// (/srv/moose).
func New() *Reporter {
	return &Reporter{
		osPath:   osDiskMount,
		dataPath: dataDiskMount,
		deviceID: statDeviceID,
	}
}

// DataDisk returns the data drive's available and total bytes. free is the space
// an unprivileged writer can actually use (Bavail × Frsize — already net of the
// root reserve); total is the filesystem size (Blocks × Frsize). On a statfs
// error it fails open to (0, 0): the brain reads 0 as "not measured" and shows
// no free figure rather than a misleading empty disk.
func (r *Reporter) DataDisk() (free, total int64) {
	free, total, _ = statfsBytes(r.dataPath)
	return free, total
}

// Disks returns one entry per mounted volume of interest for the Storage bars:
// the OS drive ("System") always, the data drive ("Data") only when it is a
// distinct mount. On a Level-0 box /srv/moose is a plain directory on the OS
// drive (STORAGE.md), so a successful statfs is not enough to call it a data
// drive — we include it only when its backing filesystem differs from the OS
// drive's. A volume whose statfs fails is omitted rather than reported as zero.
func (r *Reporter) Disks() []protocol.DiskSpace {
	disks := []protocol.DiskSpace{}
	if free, total, ok := statfsBytes(r.osPath); ok {
		disks = append(disks, protocol.DiskSpace{Label: osDiskLabel, FreeBytes: free, TotalBytes: total})
	}
	if r.dataDrivePresent() {
		if free, total, ok := statfsBytes(r.dataPath); ok {
			disks = append(disks, protocol.DiskSpace{Label: dataDiskLabel, FreeBytes: free, TotalBytes: total})
		}
	}
	return disks
}

// dataDrivePresent reports whether the data-drive path is a distinct filesystem
// from the OS drive. False when /srv/moose is missing (no data drive ever) or
// shares the OS drive's device (Level-0: a directory on the OS drive). Any
// lookup error fails open to "absent" so a probe failure drops the Data bar
// rather than showing a duplicate of System.
func (r *Reporter) dataDrivePresent() bool {
	osDev, err := r.deviceID(r.osPath)
	if err != nil {
		return false
	}
	dataDev, err := r.deviceID(r.dataPath)
	if err != nil {
		return false
	}
	return dataDev != osDev
}

// statfsBytes returns a path's available and total bytes. ok is false on a
// statfs error (the caller fails open). free = Bavail × Frsize (net of the root
// reserve); total = Blocks × Frsize. Block counts are in units of f_frsize (the
// fundamental block size) per POSIX, not f_bsize (the preferred I/O size); they
// are equal on ext4/xfs/btrfs but can differ on e.g. NFS.
func statfsBytes(path string) (free, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		slog.Error("diskusage: statfs failed", "path", path, "err", err)
		return 0, 0, false
	}
	frsize := int64(st.Frsize)
	return int64(st.Bavail) * frsize, int64(st.Blocks) * frsize, true
}

// statDeviceID returns a path's backing-filesystem id (st_dev from stat(2)).
func statDeviceID(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}
