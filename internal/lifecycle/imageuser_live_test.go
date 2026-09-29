//go:build dockerlive

package lifecycle

// Real-system check for the image tier's user probe: the real docker CLI
// reads a real image's Config.User and its /etc/passwd without starting it,
// and leaves no container behind. Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveImageUserProbe -v
//
// Needs a Docker daemon and network access to pull busybox.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestLiveImageUserProbe(t *testing.T) {
	ctx := context.Background()
	const ref = "busybox:1.37.0"
	d := NewCLIDocker()
	if err := d.Pull(ctx, ref); err != nil {
		t.Fatalf("pull: %v", err)
	}
	spec, err := d.ImageUser(ctx, ref)
	if err != nil || spec != "" {
		t.Fatalf("ImageUser = %q, %v; want busybox's empty user", spec, err)
	}
	passwd, group, err := d.ImageUserFiles(ctx, "live-probe", ref)
	if err != nil {
		t.Fatalf("ImageUserFiles: %v", err)
	}
	if !strings.Contains(string(passwd), "root:x:0:0") || !strings.Contains(string(group), "www-data:x:33:") {
		t.Fatalf("passwd %q / group %q do not look like busybox's", passwd, group)
	}
	ids, err := resolveImageUser("app", "nobody:www-data", func() ([]byte, []byte, error) { return passwd, group, nil })
	if err != nil || ids != (imageIDs{65534, 33}) {
		t.Fatalf("nobody:www-data = %+v, %v; want 65534:33", ids, err)
	}
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=moose.instance_id=live-probe").Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("probe container left behind: %q, %v", out, err)
	}

	// A path the image does not have, and a directory in place of the file,
	// both count as missing.
	cidOut, err := exec.Command("docker", "create", "--pull", "never", "--entrypoint", "/x", ref).Output()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cid := strings.TrimSpace(string(cidOut))
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", "-v", cid).CombinedOutput(); err != nil {
			t.Logf("rm %s: %v %s", cid, err, out)
		}
	})
	for _, p := range []string{"/etc/nope", "/etc"} {
		if b, err := copyUserFile(ctx, cid, p); err != nil || b != nil {
			t.Errorf("copyUserFile(%s) = %d bytes, %v; want missing", p, len(b), err)
		}
	}
}
