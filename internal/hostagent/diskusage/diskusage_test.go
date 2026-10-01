//go:build linux

package diskusage

import (
	"errors"
	"testing"
)

// TestDataDiskRealStatfs points the reporter at a real existing directory (the
// test's temp dir) and asserts a coherent statfs reading: a total > 0 and a free
// that fits within it. This exercises the real syscall path, not a fake.
func TestDataDiskRealStatfs(t *testing.T) {
	r := &Reporter{dataPath: t.TempDir()}
	free, total := r.DataDisk()
	if total <= 0 {
		t.Fatalf("want total > 0, got %d", total)
	}
	if free < 0 || free > total {
		t.Fatalf("free %d out of range [0, %d]", free, total)
	}
}

// TestDataDiskMissingPathFailsOpen pins the fail-open contract: statfs on a
// nonexistent path returns (0, 0), which the brain reads as "not measured"
// rather than an error or a scary empty disk.
func TestDataDiskMissingPathFailsOpen(t *testing.T) {
	r := &Reporter{dataPath: "/nonexistent/moose/data-drive-xyz"}
	free, total := r.DataDisk()
	if free != 0 || total != 0 {
		t.Fatalf("want (0, 0) on statfs error, got (%d, %d)", free, total)
	}
}

// fakeDevices returns a deviceID func mapping the given paths to fixed st_dev
// ids; any path absent from the map yields an error (stand-in for a missing
// mount) so tests can drive the present/absent branch without two filesystems.
func fakeDevices(m map[string]uint64) func(string) (uint64, error) {
	return func(path string) (uint64, error) {
		if dev, ok := m[path]; ok {
			return dev, nil
		}
		return 0, errors.New("no device")
	}
}

// TestDisksBothPresent: when the data drive is a distinct filesystem from the OS
// drive, both System and Data appear, each with a coherent statfs reading.
func TestDisksBothPresent(t *testing.T) {
	osDir, dataDir := t.TempDir(), t.TempDir()
	r := &Reporter{
		osPath:   osDir,
		dataPath: dataDir,
		deviceID: fakeDevices(map[string]uint64{osDir: 1, dataDir: 2}),
	}
	disks := r.Disks()
	if len(disks) != 2 {
		t.Fatalf("want 2 disks (System, Data), got %d: %+v", len(disks), disks)
	}
	if disks[0].Label != "System" || disks[1].Label != "Data" {
		t.Fatalf("want labels [System Data], got [%s %s]", disks[0].Label, disks[1].Label)
	}
	for _, d := range disks {
		if d.TotalBytes <= 0 || d.FreeBytes < 0 || d.FreeBytes > d.TotalBytes {
			t.Fatalf("incoherent reading for %s: free=%d total=%d", d.Label, d.FreeBytes, d.TotalBytes)
		}
	}
}

// TestDisksLevel0OnlySystem: when /srv/moose shares the OS drive's device (a
// Level-0 box: a directory on the OS drive, not a real data drive), only the
// System bar appears — no duplicate Data bar.
func TestDisksLevel0OnlySystem(t *testing.T) {
	osDir, dataDir := t.TempDir(), t.TempDir()
	r := &Reporter{
		osPath:   osDir,
		dataPath: dataDir,
		deviceID: fakeDevices(map[string]uint64{osDir: 7, dataDir: 7}), // same device
	}
	disks := r.Disks()
	if len(disks) != 1 || disks[0].Label != "System" {
		t.Fatalf("want only [System], got %+v", disks)
	}
}

// TestDisksDataPathMissing: when the data path has no backing device (the mount
// is gone), the Data entry is dropped and only System remains.
func TestDisksDataPathMissing(t *testing.T) {
	osDir := t.TempDir()
	r := &Reporter{
		osPath:   osDir,
		dataPath: "/nonexistent/moose/data-drive-xyz",
		deviceID: fakeDevices(map[string]uint64{osDir: 1}), // data path errors
	}
	disks := r.Disks()
	if len(disks) != 1 || disks[0].Label != "System" {
		t.Fatalf("want only [System], got %+v", disks)
	}
}

// TestDisksOSDeviceMissing: when the OS device lookup itself errors,
// dataDrivePresent fails open to absent — only System (its statfs still works).
func TestDisksOSDeviceMissing(t *testing.T) {
	osDir, dataDir := t.TempDir(), t.TempDir()
	r := &Reporter{
		osPath:   osDir,
		dataPath: dataDir,
		deviceID: fakeDevices(map[string]uint64{dataDir: 2}), // os path errors
	}
	disks := r.Disks()
	if len(disks) != 1 || disks[0].Label != "System" {
		t.Fatalf("want only [System] when OS device lookup errors, got %+v", disks)
	}
}

// TestDisksDataStatfsFailsAfterPresent: the device check says the data drive is
// distinct, but its statfs then fails (a racing unmount). The entry is omitted
// rather than reported as a zero-byte disk.
func TestDisksDataStatfsFailsAfterPresent(t *testing.T) {
	osDir := t.TempDir()
	const ghost = "/nonexistent/moose/ghost-data"
	r := &Reporter{
		osPath:   osDir,
		dataPath: ghost,
		deviceID: fakeDevices(map[string]uint64{osDir: 1, ghost: 2}), // distinct, but statfs will fail
	}
	disks := r.Disks()
	if len(disks) != 1 || disks[0].Label != "System" {
		t.Fatalf("want only [System] when Data statfs fails, got %+v", disks)
	}
}

// TestDisksOSStatfsFailsOmitsSystem: a volume whose statfs fails is omitted, not
// zero-filled — even the OS drive.
func TestDisksOSStatfsFailsOmitsSystem(t *testing.T) {
	dataDir := t.TempDir()
	const ghostOS = "/nonexistent/moose/ghost-os"
	r := &Reporter{
		osPath:   ghostOS,
		dataPath: dataDir,
		deviceID: fakeDevices(map[string]uint64{ghostOS: 1, dataDir: 2}),
	}
	disks := r.Disks()
	if len(disks) != 1 || disks[0].Label != "Data" {
		t.Fatalf("want only [Data] when System statfs fails, got %+v", disks)
	}
}

// TestDisksRealDeviceID exercises the default statDeviceID seam via New(): on a
// typical test box /srv/moose is absent, so its device lookup errors and only
// the real System (/) bar comes back with a coherent reading. This covers the
// real stat(2) path (success for /, error for the missing data mount).
func TestDisksRealDeviceID(t *testing.T) {
	disks := New().Disks()
	if len(disks) == 0 {
		t.Fatal("want at least the System bar from a real /")
	}
	if disks[0].Label != "System" || disks[0].TotalBytes <= 0 {
		t.Fatalf("want a coherent System bar, got %+v", disks[0])
	}
}

// TestNewHostedMeasuresStatePartition pins the hosted paths: the "System"
// volume is the state partition seen through /var/lib/moose, never the 4 GiB
// OS slot at / (BUILD.md # 1b). A box that reported / would show about 4 GB
// on a 40 GB disk.
func TestNewHostedMeasuresStatePartition(t *testing.T) {
	r := NewHosted()
	if r.osPath != "/var/lib/moose" {
		t.Fatalf("hosted System path = %q, want /var/lib/moose", r.osPath)
	}
	if r.osPath == "/" {
		t.Fatal("hosted System must not measure the OS slot at /")
	}
	if r.dataPath != "/srv/moose" {
		t.Fatalf("hosted data path = %q, want /srv/moose", r.dataPath)
	}
}

// TestHostedOneVolumeWhenSrvIsTheSamePartition: on the A/B layout /srv/moose
// is a bind mount of the state partition, so it shares /var/lib/moose's
// device and Disks reports one System bar, measured on the state path.
func TestHostedOneVolumeWhenSrvIsTheSamePartition(t *testing.T) {
	state := t.TempDir()
	srv := t.TempDir()
	r := &Reporter{
		osPath:   state,
		dataPath: srv,
		deviceID: fakeDevices(map[string]uint64{state: 7, srv: 7}),
	}
	disks := r.Disks()
	if len(disks) != 1 || disks[0].Label != "System" || disks[0].TotalBytes <= 0 {
		t.Fatalf("want one coherent System bar, got %+v", disks)
	}
	_, want, ok := statfsBytes(state)
	if !ok || disks[0].TotalBytes != want {
		t.Fatalf("System total = %d, want the state path's %d", disks[0].TotalBytes, want)
	}
}
