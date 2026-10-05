package release

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRegistry writes a stub curl that serves manifests from dir: a request for
// manifests/<ref> answers with the file dir/<ref with ':' as '_'>, and HTTP 404
// when there is none. dir/<file>.digest, when present, is sent as the
// Docker-Content-Digest header instead of the body's real digest.
func fakeRegistry(t *testing.T, dir string) string {
	t.Helper()
	bin := t.TempDir()
	stub := `#!/usr/bin/env bash
out=""; head=""; url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -D) head="$2"; shift 2 ;;
    -H|-w) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
case "$url" in
  *ghcr.io/token*) echo '{"token":"t"}'; exit 0 ;;
  *ghcr.io/v2/*/manifests/*)
    ref="${url##*/manifests/}"; f="` + "${FAKE_DIR}" + `/${ref//:/_}"
    if [ ! -f "$f" ]; then : > "$out"; : > "$head"; printf 404; exit 0; fi
    cp "$f" "$out"
    if [ -f "$f.digest" ]; then d="$(cat "$f.digest")"; else d="sha256:$(sha256sum "$f" | cut -d' ' -f1)"; fi
    printf 'HTTP/2 200\r\ndocker-content-digest: %s\r\n\r\n' "$d" > "$head"
    printf 200; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func hex64(c byte) string { return strings.Repeat(string(c), 64) }

func runResolve(t *testing.T, bin, dir, ref string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("ghcr-resolve.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, "onmoose/brain", ref)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_DIR="+dir)
	out, err := cmd.Output()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), rc
}

// ghcr-resolve.sh pins a tag to the digest of the bytes the registry served,
// takes the amd64 entry of an index, and fails on anything it cannot pin.
func TestGHCRResolve(t *testing.T) {
	dir := t.TempDir()
	bin := fakeRegistry(t, dir)
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := "sha256:" + hex64('c')
	single := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"digest":"` + cfg + `"},"layers":[]}`)
	write("v1.0.0", single)
	if out, rc := runResolve(t, bin, dir, "v1.0.0"); rc != 0 || out != digestOf(single)+" "+cfg {
		t.Errorf("single manifest: rc=%d out=%q, want %q", rc, out, digestOf(single)+" "+cfg)
	}

	// An index: the linux/amd64 entry, fetched by its digest.
	cfg2 := "sha256:" + hex64('d')
	plat := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + cfg2 + `"},"layers":[]}`)
	pd := digestOf(plat)
	write(strings.ReplaceAll(pd, ":", "_"), plat)
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
		`{"digest":"sha256:%s","platform":{"os":"linux","architecture":"arm64"}},`+
		`{"digest":"%s","platform":{"os":"linux","architecture":"amd64"}}]}`, hex64('a'), pd))
	write("v2.0.0", index)
	if out, rc := runResolve(t, bin, dir, "v2.0.0"); rc != 0 || out != pd+" "+cfg2 {
		t.Errorf("index: rc=%d out=%q, want %q", rc, out, pd+" "+cfg2)
	}

	// An index with no amd64 image.
	write("v3.0.0", []byte(`{"manifests":[{"digest":"sha256:`+hex64('a')+`","platform":{"os":"linux","architecture":"arm64"}}]}`))
	if out, rc := runResolve(t, bin, dir, "v3.0.0"); rc == 0 {
		t.Errorf("index without amd64: want a failure, got %q", out)
	}

	// The registry's digest header disagrees with the bytes.
	write("v4.0.0", single)
	write("v4.0.0.digest", []byte("sha256:"+hex64('e')))
	if out, rc := runResolve(t, bin, dir, "v4.0.0"); rc == 0 {
		t.Errorf("digest mismatch: want a failure, got %q", out)
	}

	// Not published.
	if out, rc := runResolve(t, bin, dir, "v9.9.9"); rc == 0 {
		t.Errorf("missing tag: want a failure, got %q", out)
	}

	// A manifest with no config digest.
	write("v5.0.0", []byte(`{"schemaVersion":2,"config":{}}`))
	if out, rc := runResolve(t, bin, dir, "v5.0.0"); rc == 0 {
		t.Errorf("no config: want a failure, got %q", out)
	}
}

// writeImageTar writes a docker-save style tarball whose manifest.json names
// config as its Config entry.
func writeImageTar(t *testing.T, path, config string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	body := []byte(`[{"Config":"` + config + `","RepoTags":["moose-brain:dev"],"Layers":[]}]`)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// bundle-record.sh reads each image ID from the tarball's own manifest, in the
// layout of Docker 25+ and of older Docker, and records where the pair came from.
func TestBundleRecord(t *testing.T) {
	script, err := filepath.Abs("../control-plane/bundle-record.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeImageTar(t, filepath.Join(dir, "moose-brain.tar"), "blobs/sha256/"+hex64('b'))
	writeImageTar(t, filepath.Join(dir, "moose-ui.tar"), hex64('f')+".json")

	out, err := exec.Command("bash", script, dir, "released", "0.15.0",
		"ghcr.io/onmoose/brain@sha256:"+hex64('1'), "ghcr.io/onmoose/ui@sha256:"+hex64('2')).CombinedOutput()
	if err != nil {
		t.Fatalf("bundle-record: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "control-plane.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"MOOSE_BAKED_CP_SOURCE=released\n",
		"MOOSE_BAKED_CP_VERSION=0.15.0\n",
		"MOOSE_BAKED_BRAIN_REF=ghcr.io/onmoose/brain@sha256:" + hex64('1') + "\n",
		"MOOSE_BAKED_UI_REF=ghcr.io/onmoose/ui@sha256:" + hex64('2') + "\n",
		"MOOSE_BAKED_BRAIN_ID=sha256:" + hex64('b') + "\n",
		"MOOSE_BAKED_UI_ID=sha256:" + hex64('f') + "\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("record lacks %q:\n%s", want, got)
		}
	}

	// local takes no refs, and records empty ones.
	out, err = exec.Command("bash", script, dir, "local", "0.16.0").CombinedOutput()
	if err != nil {
		t.Fatalf("bundle-record local: %v\n%s", err, out)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "control-plane.env"))
	if !strings.Contains(string(got), "MOOSE_BAKED_CP_SOURCE=local\n") || !strings.Contains(string(got), "MOOSE_BAKED_BRAIN_REF=\n") {
		t.Errorf("local record:\n%s", got)
	}

	// Refused: a bad source, refs on local, a non-version, a tarball it cannot read.
	for _, args := range [][]string{
		{dir, "other", "0.16.0"},
		{dir, "local", "0.16.0", "a", "b"},
		{dir, "released", "0.16.0"},
		{dir, "local", "latest"},
	} {
		if out, err := exec.Command("bash", append([]string{script}, args...)...).CombinedOutput(); err == nil {
			t.Errorf("bundle-record %v: want a refusal, got\n%s", args[1:], out)
		}
	}
	writeImageTar(t, filepath.Join(dir, "moose-ui.tar"), "not-a-digest")
	if out, err := exec.Command("bash", script, dir, "local", "0.16.0").CombinedOutput(); err == nil {
		t.Errorf("unreadable config: want a refusal, got\n%s", out)
	}
}
