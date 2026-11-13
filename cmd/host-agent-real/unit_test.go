package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// unitPath is the systemd unit every box runs this binary under. The cloud
// image copies it as is (dev/cloud/stage-control-plane.sh), and so does the
// medium lane (dev/test-qemu/bootstrap.sh).
const unitPath = "../../dist/systemd/host-agent.service"

// pinsUnitPath restores the folder pins at boot with this same binary
// (--restore-folder-pins). Its mounts must reach Docker too.
const pinsUnitPath = "../../dist/systemd/moose-folder-pins.service"

// serviceKeys reads the [Service] section of a unit file as key -> values.
func serviceKeys(t *testing.T, path string) map[string][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	keys := map[string][]string{}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "[Service]" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		keys[strings.TrimSpace(k)] = append(keys[strings.TrimSpace(k)], strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return keys
}

// TestUnitKeepsHostMountNamespace guards the folder pins (#604, #667), in
// both units that make them.
// host-agent bind-mounts each folder source at /run/moose/folder-pins/, and
// Docker binds that pin into the app. Any of these settings runs the service in
// its own mount namespace and stops its mounts reaching the host. Docker then
// binds an empty pin path and the app starts on an empty folder, with no error
// anywhere (CONTROL_PLANE.md # Locked: host-agent hardening directives).
func TestUnitKeepsHostMountNamespace(t *testing.T) {
	namespacing := []string{
		"ProtectSystem", "ProtectHome", "ReadWritePaths", "ReadOnlyPaths",
		"InaccessiblePaths", "ExecPaths", "NoExecPaths", "PrivateTmp",
		"PrivateDevices", "PrivateMounts", "PrivateUsers", "ProtectKernelTunables",
		"ProtectKernelModules", "ProtectKernelLogs", "ProtectControlGroups",
		"ProtectProc", "ProcSubset", "BindPaths", "BindReadOnlyPaths",
		"TemporaryFileSystem", "MountFlags", "MountAPIVFS", "RootDirectory",
		"RootImage", "RootEphemeral",
		// DynamicUser= implies ProtectSystem=strict, ProtectHome= and PrivateTmp=.
		"DynamicUser",
	}
	for _, path := range []string{unitPath, pinsUnitPath} {
		keys := serviceKeys(t, path)
		for _, k := range namespacing {
			if v, ok := keys[k]; ok {
				t.Errorf("%s sets %s=%s. That can run host-agent in its own mount namespace, so folder pins would not reach Docker (#667)", path, k, strings.Join(v, ","))
			}
		}
	}
}

// TestUnitHardening pins the lines that do ship, and the one that must not:
// RestrictSUIDSGID= would refuse the setgid bit host-agent puts on the shared
// tree (02770, brainlaunch.EnsureSharedTree).
func TestUnitHardening(t *testing.T) {
	keys := serviceKeys(t, unitPath)
	for _, k := range []string{"NoNewPrivileges", "LockPersonality"} {
		v := keys[k]
		if len(v) != 1 || v[0] != "yes" {
			t.Errorf("%s: want %s=yes, got %v", unitPath, k, v)
		}
	}
	if v, ok := keys["RestrictSUIDSGID"]; ok {
		t.Errorf("%s sets RestrictSUIDSGID=%s, which breaks the setgid shared tree", unitPath, strings.Join(v, ","))
	}
}
