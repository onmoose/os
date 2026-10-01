package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0", "1.1", -1},
		{"1.10", "1.9", 1},
		{"1.0~rc1", "1.0", -1},
		{"1.0~rc1", "1.0~rc2", -1},
		{"1:1.0", "2.0", 1},
		{"5:29.8.2-1~debian.13~trixie", "5:29.8.10-1~debian.13~trixie", -1},
		{"5:29.8.2-1~debian.13~trixie", "5:28.5.2-1~debian.13~trixie", 1},
		{"3.5.7-1~deb13u2", "3.5.7-1~deb13u3", -1},
		{"3.5.7-1", "3.5.7-1~deb13u3", 1},
		{"1.0a", "1.0", 1},
		{"1.0+b1", "1.0", 1},
		{"2.41.5-0+deb13u1", "2.41-5", 1},
		{"1.0-1", "1.0", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareVersions(c.b, c.a); got != -c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestSameMajor(t *testing.T) {
	if !sameMajor("5:29.8.2-1~debian.13~trixie", "5:29.0.0-1~debian.13~trixie") {
		t.Error("29.8.2 and 29.0.0 should share a major")
	}
	if sameMajor("5:29.8.2-1~debian.13~trixie", "5:30.0.0-1~debian.13~trixie") {
		t.Error("29 and 30 are different majors")
	}
	if sameMajor("2.3.6-1", "1:2.3.6-1") {
		t.Error("a different epoch is a different major")
	}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A synthetic mkosi manifest in the published shape (mkosi/manifest.py).
const manifest = `{
  "manifest_version": 1,
  "config": {"name": "moose-cloud", "distribution": "debian", "architecture": "x86-64"},
  "packages": [
    {"type": "deb", "name": "zlib1g", "version": "1:1.3", "architecture": "amd64"},
    {"type": "deb", "name": "openssl", "version": "3.5.7-1~deb13u2", "architecture": "amd64"},
    {"type": "deb", "name": "tzdata", "version": "2026b-0+deb13u1", "architecture": "all"}
  ],
  "extension": {}
}`

func TestNormalizeIsSortedAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "image.manifest", manifest)
	var out bytes.Buffer
	if err := run([]string{"normalize", m}, &out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.HasPrefix(body, "# ") {
		t.Errorf("the lock should start with its header comment, got %q", body)
	}
	want := "openssl 3.5.7-1~deb13u2 amd64\ntzdata 2026b-0+deb13u1 all\nzlib1g 1:1.3 amd64\n"
	if !strings.HasSuffix(body, want) {
		t.Errorf("normalize body:\n%s\nwant it to end with:\n%s", body, want)
	}
	lock := writeFile(t, dir, "cloud-packages.lock", body)
	out.Reset()
	if err := run([]string{"check", m, lock}, &out); err != nil {
		t.Fatalf("check of a manifest against its own lock failed: %v", err)
	}
}

func TestCheckReportsEveryKindOfChange(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "image.manifest", manifest)
	lock := writeFile(t, dir, "cloud-packages.lock", "# header\n"+
		"openssl 3.5.7-1~deb13u1 amd64\n"+
		"tzdata 2026b-0+deb13u1 all\n"+
		"bash 5.2 amd64\n")
	err := run([]string{"check", m, lock}, &bytes.Buffer{})
	var mm errMismatch
	if !errors.As(err, &mm) {
		t.Fatalf("want a mismatch, got %v", err)
	}
	for _, want := range []string{
		"changed openssl 3.5.7-1~deb13u1 to 3.5.7-1~deb13u2",
		"added   zlib1g 1:1.3",
		"removed bash 5.2",
		"3 changes",
	} {
		if !strings.Contains(string(mm), want) {
			t.Errorf("report does not say %q:\n%s", want, mm)
		}
	}
}

func TestCheckRefusesAnEmptyManifest(t *testing.T) {
	dir := t.TempDir()
	m := writeFile(t, dir, "image.manifest", `{"packages": []}`)
	lock := writeFile(t, dir, "cloud-packages.lock", "")
	err := run([]string{"check", m, lock}, &bytes.Buffer{})
	var mm errMismatch
	if err == nil || errors.As(err, &mm) {
		t.Fatalf("an empty manifest must be an error, not a pass or a package diff: %v", err)
	}
}

const securityIndex = `Package: openssl
Version: 3.5.7-1~deb13u3
Architecture: amd64

Package: libssl3t64
Version: 3.5.7-1~deb13u3
Architecture: amd64
`

func TestTitleAndDiffNameTheChanges(t *testing.T) {
	dir := t.TempDir()
	old := writeFile(t, dir, "old.lock", "openssl 3.5.7-1~deb13u2 amd64\ntzdata 2026b-0+deb13u1 all\ncurl 8.0 amd64\n")
	cur := writeFile(t, dir, "new.lock", "openssl 3.5.7-1~deb13u3 amd64\ntzdata 2026c-0+deb13u1 all\ncurl 8.0 amd64\n")
	sec := writeFile(t, dir, "Packages", securityIndex)

	var out bytes.Buffer
	if err := run([]string{"title", "-security", sec, old, cur}, &out); err != nil {
		t.Fatal(err)
	}
	want := "OS lock (security): openssl 3.5.7-1~deb13u2 to 3.5.7-1~deb13u3, tzdata 2026b-0+deb13u1 to 2026c-0+deb13u1\n"
	if out.String() != want {
		t.Errorf("title = %q, want %q", out.String(), want)
	}

	out.Reset()
	if err := run([]string{"diff", "-security", sec, old, cur}, &out); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, w := range []string{
		"1 of the 2 changed packages come from `trixie-security`",
		"| `openssl` | `3.5.7-1~deb13u2` | `3.5.7-1~deb13u3` | trixie-security |",
		"| `tzdata` | `2026b-0+deb13u1` | `2026c-0+deb13u1` |  |",
	} {
		if !strings.Contains(body, w) {
			t.Errorf("diff does not contain %q:\n%s", w, body)
		}
	}
	if strings.Contains(body, "curl") {
		t.Errorf("an unchanged package is in the diff:\n%s", body)
	}
}

func TestTitleWithoutSecurityAndWithManyChanges(t *testing.T) {
	var oldB, newB strings.Builder
	for _, n := range []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg"} {
		oldB.WriteString(n + " 1.0-1 amd64\n")
		newB.WriteString(n + " 1.0-2 amd64\n")
	}
	dir := t.TempDir()
	old := writeFile(t, dir, "old.lock", oldB.String())
	cur := writeFile(t, dir, "new.lock", newB.String())
	var out bytes.Buffer
	if err := run([]string{"title", old, cur}, &out); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "OS lock: aaaa 1.0-1 to 1.0-2, ") || !strings.Contains(got, " more") {
		t.Errorf("title = %q", got)
	}
	if len(got) > maxTitle+len(" and 99 more") {
		t.Errorf("title is %d characters, too long: %q", len(got), got)
	}
}

func TestDiffFlagsAddedPackagesForReview(t *testing.T) {
	dir := t.TempDir()
	old := writeFile(t, dir, "old.lock", "curl 8.0 amd64\n")
	cur := writeFile(t, dir, "new.lock", "curl 8.0 amd64\nlibnew1 1.0 amd64\n")
	var out bytes.Buffer
	if err := run([]string{"diff", old, cur}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "| `libnew1` | `(new)` | `1.0` |") || !strings.Contains(out.String(), "expected-packages.txt") {
		t.Errorf("diff:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "next normal OS release") {
		t.Errorf("a bump with no security change should say it waits for the next release:\n%s", out.String())
	}
}

const dockerIndex = `Package: docker-ce
Version: 5:29.8.2-1~debian.13~trixie

Package: docker-ce
Version: 5:29.10.0-1~debian.13~trixie

Package: docker-ce
Version: 5:30.0.0-1~debian.13~trixie

Package: containerd.io
Version: 2.3.6-1~debian.13~trixie

Package: containerd.io
Version: 1.7.27-1
`

func TestNewestPinsStaysInTheMajor(t *testing.T) {
	dir := t.TempDir()
	lock := writeFile(t, dir, "third-party.lock", "# comment kept\ncontainerd.io=2.3.6-1~debian.13~trixie\ndocker-ce=5:29.8.2-1~debian.13~trixie\n")
	idx := writeFile(t, dir, "Packages", dockerIndex)
	var out bytes.Buffer
	if err := run([]string{"newest-pins", lock, idx}, &out); err != nil {
		t.Fatal(err)
	}
	want := "# comment kept\ncontainerd.io=2.3.6-1~debian.13~trixie\ndocker-ce=5:29.10.0-1~debian.13~trixie\n"
	if out.String() != want {
		t.Errorf("newest-pins:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestNewestPinsRefusesAPackageMissingFromTheIndex(t *testing.T) {
	dir := t.TempDir()
	lock := writeFile(t, dir, "third-party.lock", "docker-buildx-plugin=0.1-1\n")
	idx := writeFile(t, dir, "Packages", dockerIndex)
	if err := run([]string{"newest-pins", lock, idx}, &bytes.Buffer{}); err == nil {
		t.Fatal("a pin for a package the repo does not have must be an error")
	}
}

// The committed lock must hang together: a valid timestamp, one pin per Docker
// package the image installs, each pinned version in the resolved list, and
// the resolved list in the exact form normalize writes. A bad hand edit fails
// here, in make check, instead of in a 10 minute image build.
func TestCommittedLockIsConsistent(t *testing.T) {
	snap, err := os.ReadFile("../debian-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{8}T\d{6}Z$`).MatchString(strings.TrimSpace(string(snap))) {
		t.Errorf("debian-snapshot %q is not a snapshot.debian.org timestamp like 20261001T082322Z", snap)
	}

	pins := map[string]string{}
	f, err := os.Open("../third-party.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, ver, ok := strings.Cut(line, "=")
		if !ok || name == "" || ver == "" {
			t.Fatalf("third-party.lock line %q is not package=version", line)
		}
		pins[name] = ver
	}

	conf, err := os.ReadFile("../../cloud/mkosi.conf")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docker-ce", "docker-ce-cli", "containerd.io", "docker-compose-plugin"} {
		if pins[name] == "" {
			t.Errorf("third-party.lock has no pin for %s, which dev/cloud/mkosi.conf installs from Docker's repo", name)
		}
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `\s*$`).Match(conf) {
			t.Errorf("third-party.lock pins %s, but dev/cloud/mkosi.conf does not install it", name)
		}
	}

	body, err := os.ReadFile("../cloud-packages.lock")
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := parseLock(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != formatLock(pkgs) {
		t.Error("cloud-packages.lock is not in the form oslock normalize writes (sorted, with the header); regenerate it, do not edit it by hand")
	}
	resolved := map[string]string{}
	for _, p := range pkgs {
		resolved[p.Name] = p.Version
	}
	for name, ver := range pins {
		if resolved[name] != ver {
			t.Errorf("third-party.lock pins %s=%s, but cloud-packages.lock has %q", name, ver, resolved[name])
		}
	}
}
