package release

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubBin writes an executable stub called name into a fresh dir and returns
// the dir, to put first on PATH.
func stubBin(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runScript(t *testing.T, env []string, script string, args ...string) (int, string) {
	t.Helper()
	abs, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{abs}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// The image and its checksum are one pair: both present is left, neither is
// uploaded together, and exactly one refuses, naming the stray to delete. A
// published asset is never uploaded over and never deleted.
func TestAttachImageTreatsImageAndChecksumAsOnePair(t *testing.T) {
	gh := `#!/usr/bin/env bash
case "$1 $2" in
  "release view") printf '%s' "$FAKE_ASSETS" | tr ',' '\n'; exit 0 ;;
  "release upload") shift 3; echo "$*" >> "$FAKE_LOG"; exit 0 ;;
esac
echo "unexpected gh call: $*" >&2; exit 3
`
	bin := stubBin(t, "gh", gh)
	const img, sum = "moose-v0.16.0-amd64.raw.xz", "moose-v0.16.0-amd64.raw.xz.sha256"
	cases := []struct {
		name       string
		assets     string
		wantRC     int
		wantUpload string // what upload was called with, or "" for none
		wantInOut  string
	}{
		{"neither: upload both together", "", 0, img + " " + sum, "attached"},
		{"both: leave them", img + "," + sum, 0, "", "already has"},
		{"only the image: refuse, name it", img, 1, "", "gh release delete-asset v0.16.0 " + img},
		{"only the checksum: refuse, name it", sum, 1, "", "gh release delete-asset v0.16.0 " + sum},
		{"other assets only: upload both", "notes.txt", 0, img + " " + sum, "attached"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "uploads")
			rc, out := runScript(t, []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FAKE_ASSETS=" + c.assets, "FAKE_LOG=" + log},
				"attach-image.sh", "v0.16.0", img, sum)
			if rc != c.wantRC {
				t.Fatalf("exit = %d, want %d\n%s", rc, c.wantRC, out)
			}
			if !strings.Contains(out, c.wantInOut) {
				t.Errorf("output should contain %q:\n%s", c.wantInOut, out)
			}
			got, _ := os.ReadFile(log)
			if strings.TrimSpace(string(got)) != c.wantUpload {
				t.Errorf("uploads = %q, want %q", strings.TrimSpace(string(got)), c.wantUpload)
			}
		})
	}
}

// ghcr-retag.sh copies the source tag's manifest to the destination tag byte
// for byte, with its content type, and checks the registry then reports the
// same digest. A registry that answers with another digest fails the run.
func TestRetagCopiesTheManifestByteForByte(t *testing.T) {
	// A stub registry: GET serves $FAKE_MANIFEST, PUT stores the body and its
	// content type, HEAD answers with the digest of what was stored (or
	// $FAKE_HEAD_DIGEST, to simulate a registry that disagrees).
	curl := `#!/usr/bin/env bash
method=GET headers="" out="" data="" ctype="" url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift ;;
    -I) method=HEAD ;;
    -D) headers="$2"; shift ;;
    -o) out="$2"; shift ;;
    -u) shift ;;
    --data-binary) data="${2#@}"; shift ;;
    -H) case "$2" in Content-Type:*) ctype="${2#Content-Type: }";; esac; shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
case "$url" in
  *ghcr.io/token*) echo '{"token":"t"}'; exit 0 ;;
esac
case "$method" in
  GET)
    printf 'HTTP/2 200\r\ncontent-type: application/vnd.oci.image.index.v1+json\r\n\r\n' > "$headers"
    printf '%s' "$FAKE_MANIFEST" > "$out" ;;
  PUT)
    echo "$url" > "$FAKE_DIR/put_url"; echo "$ctype" > "$FAKE_DIR/put_ctype"; cp "$data" "$FAKE_DIR/put_body" ;;
  HEAD)
    d="${FAKE_HEAD_DIGEST:-sha256:$(sha256sum "$FAKE_DIR/put_body" | cut -d' ' -f1)}"
    printf 'HTTP/2 200\r\ndocker-content-digest: %s\r\n\r\n' "$d" ;;
esac
`
	bin := stubBin(t, "curl", curl)
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	sum := sha256.Sum256([]byte(manifest))
	want := "sha256:" + hex.EncodeToString(sum[:])

	dir := t.TempDir()
	env := []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FAKE_DIR=" + dir, "FAKE_MANIFEST=" + manifest,
		"GHCR_USER=u", "GHCR_TOKEN=t"}
	rc, out := runScript(t, env, "ghcr-retag.sh", "onmoose/brain", "v0.16.0", "latest")
	if rc != 0 {
		t.Fatalf("exit = %d, want 0\n%s", rc, out)
	}
	if strings.TrimSpace(out) != want {
		t.Errorf("printed digest = %q, want %q", strings.TrimSpace(out), want)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "put_body"))
	if string(body) != manifest {
		t.Errorf("PUT body = %q, want the source manifest unchanged", body)
	}
	ctype, _ := os.ReadFile(filepath.Join(dir, "put_ctype"))
	if strings.TrimSpace(string(ctype)) != "application/vnd.oci.image.index.v1+json" {
		t.Errorf("PUT content type = %q, want the source's", ctype)
	}
	url, _ := os.ReadFile(filepath.Join(dir, "put_url"))
	if !strings.HasSuffix(strings.TrimSpace(string(url)), "/v2/onmoose/brain/manifests/latest") {
		t.Errorf("PUT went to %q, want .../onmoose/brain/manifests/latest", url)
	}

	// The registry reports a different digest after the write: fail.
	rc, out = runScript(t, append(env, "FAKE_HEAD_DIGEST=sha256:"+strings.Repeat("0", 64)),
		"ghcr-retag.sh", "onmoose/brain", "v0.16.0", "latest")
	if rc == 0 {
		t.Fatalf("want a failure when the registry disagrees on the digest, got 0\n%s", out)
	}
}

// is-newest.sh guards `latest`: it moves only for the newest control-plane
// version by semver, so an older release re-run or dispatched never moves it
// back. An unreadable tag list is "unknown" (exit 2), never "newest".
func TestIsNewestGuardsLatest(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"CONTROL_PLANE_VERSION": "0.9.0"})
	r.tag("control-plane-v0.9.0", base)
	r.tag("control-plane-v0.10.0", base)
	r.tag("control-plane-v99.0.0-rc1", base) // not X.Y.Z: ignored
	r.tag("v99.0.0", base)                   // the OS line: does not count

	newest := func(prefix, ver, remote string) int {
		t.Helper()
		script, err := filepath.Abs("is-newest.sh")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script, prefix, ver)
		cmd.Dir = r.clone
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "RELEASE_REMOTE="+remote)
		err = cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return 0
	}
	cases := []struct {
		name, prefix, ver, remote string
		want                      int
	}{
		{"an older version never moves latest", "control-plane-v", "0.9.0", "origin", 1},
		{"semver, not text: 0.10.0 is newer than 0.9.0", "control-plane-v", "0.10.0", "origin", 0},
		{"a version newer than every tag", "control-plane-v", "0.11.0", "origin", 0},
		{"no tags on the line yet", "nothing-v", "0.1.0", "origin", 0},
		{"not X.Y.Z", "control-plane-v", "0.10", "origin", 2},
		{"unreadable tag list", "control-plane-v", "0.10.0", filepath.Join(t.TempDir(), "missing.git"), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := newest(c.prefix, c.ver, c.remote); got != c.want {
				t.Errorf("exit = %d, want %d", got, c.want)
			}
		})
	}
}
