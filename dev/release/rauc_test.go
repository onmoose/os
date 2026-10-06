package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rauc-ca.sh makes a root CA and issues a signer with the codeSigning purpose
// from it, and refuses to overwrite either. The maintainer runs it offline for
// the release root; CI runs it for the throwaway root (#562).
func TestRaucCAMakesARootAndASigner(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	dir := filepath.Join(t.TempDir(), "ca")
	env := []string{"RAUC_CA_NO_PASSPHRASE=1", "RAUC_CA_NAME=moose test"}

	if rc, out := runScript(t, env, "rauc-ca.sh", "root", dir); rc != 0 {
		t.Fatalf("root: exit %d\n%s", rc, out)
	}
	if rc, out := runScript(t, env, "rauc-ca.sh", "signer", dir, "s1"); rc != 0 {
		t.Fatalf("signer: exit %d\n%s", rc, out)
	}
	for _, f := range []string{"root-ca.pem", "root-ca.key", "s1.pem", "s1.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	// The keys are private to their owner; the certs are public.
	for f, want := range map[string]os.FileMode{"root-ca.key": 0o600, "s1.key": 0o600, "root-ca.pem": 0o644, "s1.pem": 0o644} {
		st, _ := os.Stat(filepath.Join(dir, f))
		if st.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", f, st.Mode().Perm(), want)
		}
	}

	out, err := exec.Command("openssl", "x509", "-in", filepath.Join(dir, "s1.pem"), "-noout", "-ext", "extendedKeyUsage,basicConstraints").CombinedOutput()
	if err != nil {
		t.Fatalf("read signer: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Code Signing") || !strings.Contains(string(out), "CA:FALSE") {
		t.Errorf("signer lacks codeSigning or is a CA:\n%s", out)
	}
	out, err = exec.Command("openssl", "x509", "-in", filepath.Join(dir, "root-ca.pem"), "-noout", "-ext", "basicConstraints").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "CA:TRUE") {
		t.Errorf("root is not a CA: %v\n%s", err, out)
	}
	if out, err := exec.Command("openssl", "verify", "-CAfile", filepath.Join(dir, "root-ca.pem"), filepath.Join(dir, "s1.pem")).CombinedOutput(); err != nil {
		t.Errorf("signer does not chain to the root: %v\n%s", err, out)
	}

	if rc, out := runScript(t, env, "rauc-ca.sh", "root", dir); rc != 1 || !strings.Contains(out, "refusing to overwrite") {
		t.Errorf("second root: exit %d, want 1 and a refusal\n%s", rc, out)
	}
	if rc, out := runScript(t, env, "rauc-ca.sh", "signer", dir, "s1"); rc != 1 || !strings.Contains(out, "pick a new name") {
		t.Errorf("second s1: exit %d, want 1 and a refusal\n%s", rc, out)
	}
	if rc, _ := runScript(t, env, "rauc-ca.sh", "signer", dir, "../evil"); rc != 2 {
		t.Errorf("signer name with a slash: exit %d, want 2", rc)
	}
}

// sign-bundle.sh refuses before it starts any container when the release
// signer is not there, and when the image does not carry the committed release
// root (or there is none committed yet), naming the how-to.
func TestSignBundleRefusesWithoutTheReleaseSetup(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	tmp := t.TempDir()
	in := filepath.Join(tmp, "in.raucb")
	etc := filepath.Join(tmp, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{in, filepath.Join(etc, "system.conf"), filepath.Join(etc, "keyring.pem")} {
		if err := os.WriteFile(f, []byte("not real\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A docker that fails loudly: no refusal below may get that far.
	bin := stubBin(t, "docker", "#!/bin/sh\necho DOCKER-RAN; exit 9\n")
	path := "PATH=" + bin + ":" + os.Getenv("PATH")
	out := filepath.Join(tmp, "out.raucb")

	rc, o := runScript(t, []string{path, "RAUC_SIGNING_CERT=", "RAUC_SIGNING_KEY="}, "sign-bundle.sh", in, etc, out)
	if rc != 1 || !strings.Contains(o, "no release signer") || !strings.Contains(o, "docs/dev/rauc-signing.md") {
		t.Errorf("no secrets: exit %d, want 1 naming the how-to\n%s", rc, o)
	}

	rc, o = runScript(t, []string{path, "RAUC_SIGNING_CERT=cert", "RAUC_SIGNING_KEY=key"}, "sign-bundle.sh", in, etc, out)
	if _, err := os.Stat("rauc/release-ca.pem"); err != nil {
		// No release root committed yet: refused, naming the how-to.
		if rc != 1 || !strings.Contains(o, "no release root CA") || !strings.Contains(o, "docs/dev/rauc-signing.md") {
			t.Errorf("no release root: exit %d, want 1 naming the how-to\n%s", rc, o)
		}
	} else if rc != 1 || !strings.Contains(o, "not dev/release/rauc/release-ca.pem") {
		// A root is committed: an image keyring that is not it is refused.
		t.Errorf("wrong image keyring: exit %d, want 1\n%s", rc, o)
	}
	if strings.Contains(o, "DOCKER-RAN") {
		t.Errorf("a refusal started a container:\n%s", o)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a refusal wrote a bundle")
	}
}

// A root run that fails part way removes the key it wrote, so a second run is
// not refused by a half-made root.
func TestRaucCAFailedRunLeavesNothing(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	dir := filepath.Join(t.TempDir(), "ca")
	if rc, out := runScript(t, []string{"RAUC_CA_NO_PASSPHRASE=1", "RAUC_CA_ROOT_DAYS=not-a-number"}, "rauc-ca.sh", "root", dir); rc == 0 || !strings.Contains(out, "removed the partial files") {
		t.Fatalf("bad lifetime: exit %d, want a failure that cleans up\n%s", rc, out)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("a failed root left %v", left)
	}
	if rc, out := runScript(t, []string{"RAUC_CA_NO_PASSPHRASE=1"}, "rauc-ca.sh", "root", dir); rc != 0 {
		t.Fatalf("retry: exit %d\n%s", rc, out)
	}
}

// release.yml refuses to tag an OS release unless the committed release root
// is a real one: present, a CA, not a throwaway or check CA (#562).
func TestRequireReleaseCA(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	tmp := t.TempDir()
	mk := func(name, cn string) string {
		dir := filepath.Join(tmp, name)
		if rc, out := runScript(t, []string{"RAUC_CA_NO_PASSPHRASE=1", "RAUC_CA_NAME=" + cn}, "rauc-ca.sh", "root", dir); rc != 0 {
			t.Fatalf("make %s: exit %d\n%s", name, rc, out)
		}
		return filepath.Join(dir, "root-ca.pem")
	}
	good := mk("real", "moose OS release")
	throwaway := mk("throwaway", "moose THROWAWAY (not for release)")
	cases := []struct {
		name, file string
		wantRC     int
		want       string
	}{
		{"missing", filepath.Join(tmp, "none.pem"), 1, "docs/dev/rauc-signing.md"},
		{"throwaway", throwaway, 1, "throwaway or check CA"},
		{"real", good, 0, "release root CA: subject=CN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc, out := runScript(t, []string{"MOOSE_RAUC_RELEASE_CA_FILE=" + c.file}, "require-release-ca.sh")
			if rc != c.wantRC || !strings.Contains(out, c.want) {
				t.Errorf("exit %d, want %d with %q\n%s", rc, c.wantRC, c.want, out)
			}
		})
	}
	// A signer is not a CA.
	if rc, out := runScript(t, []string{"RAUC_CA_NO_PASSPHRASE=1"}, "rauc-ca.sh", "signer", filepath.Join(tmp, "real"), "s"); rc != 0 {
		t.Fatalf("signer: %d\n%s", rc, out)
	}
	if rc, out := runScript(t, []string{"MOOSE_RAUC_RELEASE_CA_FILE=" + filepath.Join(tmp, "real", "s.pem")}, "require-release-ca.sh"); rc != 1 || !strings.Contains(out, "not a CA certificate") {
		t.Errorf("signer as root: exit %d\n%s", rc, out)
	}
}
