// Package release tests the release decision script, dev/release/decide.sh,
// which .github/workflows/release.yml runs on every push to main. The workflow
// itself only runs on main, so this is where its logic is exercised: each case
// builds a throwaway "origin" repo and a clone, sets up tags and version files,
// and runs the script the way the workflow does.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type repo struct {
	t     *testing.T
	clone string
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	run(t, root, "git", "init", "-q", "--bare", origin)
	run(t, root, "git", "clone", "-q", origin, clone)
	run(t, clone, "git", "checkout", "-q", "-b", "main")
	return &repo{t: t, clone: clone}
}

// commit writes the given files and commits them, returning the new sha.
func (r *repo) commit(files map[string]string) string {
	r.t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(r.clone, name), []byte(body+"\n"), 0o644); err != nil {
			r.t.Fatal(err)
		}
		run(r.t, r.clone, "git", "add", name)
	}
	run(r.t, r.clone, "git", "commit", "-q", "--allow-empty", "-m", "c")
	run(r.t, r.clone, "git", "push", "-q", "origin", "main")
	return run(r.t, r.clone, "git", "rev-parse", "HEAD")
}

func (r *repo) tag(name, sha string) {
	r.t.Helper()
	run(r.t, r.clone, "git", "tag", name, sha)
	run(r.t, r.clone, "git", "push", "-q", "origin", name)
}

type result struct {
	out  map[string]string
	ok   bool
	logs string
}

// decide runs the script as release.yml does, with an optional image check.
func (r *repo) decide(file, prefix, before, imageCheck string) result {
	r.t.Helper()
	script, err := filepath.Abs("decide.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("bash", script, file, prefix, before)
	cmd.Dir = r.clone
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "RELEASE_IMAGE_CHECK="+imageCheck)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	res := result{out: map[string]string{}, ok: err == nil, logs: stderr.String()}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if k, v, found := strings.Cut(line, "="); found {
			res.out[k] = v
		}
	}
	return res
}

func TestBumpOfOneLineReleasesOnlyThatLine(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"VERSION": "0.15.0", "CONTROL_PLANE_VERSION": "0.15.0"})
	r.tag("v0.15.0", base)
	r.tag("control-plane-v0.15.0", base)

	// Only CONTROL_PLANE_VERSION moves.
	r.commit(map[string]string{"CONTROL_PLANE_VERSION": "0.16.0"})
	osLine := r.decide("VERSION", "v", base, "")
	cp := r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "")
	if !osLine.ok || osLine.out["created"] != "false" {
		t.Fatalf("OS line: want a clean no-op, got ok=%v out=%v\n%s", osLine.ok, osLine.out, osLine.logs)
	}
	if !cp.ok || cp.out["created"] != "true" || cp.out["tag"] != "control-plane-v0.16.0" {
		t.Fatalf("control-plane line: want a release of control-plane-v0.16.0, got ok=%v out=%v\n%s", cp.ok, cp.out, cp.logs)
	}
	// The notes start from the last tag on the same line, never the OS tag.
	if cp.out["prev_tag"] != "control-plane-v0.15.0" {
		t.Errorf("control-plane prev_tag = %q, want control-plane-v0.15.0", cp.out["prev_tag"])
	}
	if osLine.out["prev_tag"] != "" {
		t.Errorf("OS prev_tag = %q, want empty: the only v* tag is the current one, and control-plane-v* must not count", osLine.out["prev_tag"])
	}
}

func TestBumpOfVersionReleasesOnlyTheOS(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"VERSION": "0.15.0", "CONTROL_PLANE_VERSION": "0.15.0"})
	r.tag("v0.15.0", base)
	r.tag("control-plane-v0.15.0", base)
	r.commit(map[string]string{"VERSION": "0.15.1"})

	osLine := r.decide("VERSION", "v", base, "")
	cp := r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "")
	if !osLine.ok || osLine.out["created"] != "true" || osLine.out["tag"] != "v0.15.1" || osLine.out["prev_tag"] != "v0.15.0" {
		t.Fatalf("OS line: want a release of v0.15.1 from v0.15.0, got ok=%v out=%v\n%s", osLine.ok, osLine.out, osLine.logs)
	}
	if !cp.ok || cp.out["created"] != "false" {
		t.Fatalf("control-plane line: want a clean no-op, got ok=%v out=%v\n%s", cp.ok, cp.out, cp.logs)
	}
}

func TestBumpToAnAlreadyTaggedVersionFailsLoudly(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"CONTROL_PLANE_VERSION": "0.15.0"})
	r.tag("control-plane-v0.15.0", base)
	r.tag("control-plane-v0.16.0", base) // left behind by a failed earlier release
	r.commit(map[string]string{"CONTROL_PLANE_VERSION": "0.16.0"})

	cp := r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "")
	if cp.ok {
		t.Fatalf("want a hard error for a bump to an already-tagged version, got out=%v", cp.out)
	}
	if !strings.Contains(cp.logs, "already exists") {
		t.Errorf("error should say the tag already exists; got:\n%s", cp.logs)
	}
}

func TestPublishedImageTagIsNeverOverwritten(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"CONTROL_PLANE_VERSION": "0.15.0"})

	// No control-plane-v0.15.0 git tag yet, but the image tag v0.15.0 is
	// published (the first release before the split). Exit 0 = exists.
	cp := r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "true")
	if cp.ok {
		t.Fatalf("want a refusal when the image tag is already published, got out=%v", cp.out)
	}
	if !strings.Contains(cp.logs, "already published") {
		t.Errorf("error should say the image tag is already published; got:\n%s", cp.logs)
	}

	// The registry cannot answer (exit 2): refuse, never read it as missing.
	cp = r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "sh -c 'exit 2'")
	if cp.ok {
		t.Fatalf("want a refusal when the image check errors, got out=%v", cp.out)
	}

	// The image tag is free (exit 1): release.
	cp = r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "false")
	if !cp.ok || cp.out["created"] != "true" {
		t.Fatalf("want a release when the image tag is free, got ok=%v out=%v\n%s", cp.ok, cp.out, cp.logs)
	}
}

func TestFileThatIsNotAVersionIsRefused(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"CONTROL_PLANE_VERSION": "v0.15.0"})
	cp := r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "")
	if cp.ok {
		t.Fatalf("want a refusal for a file that is not X.Y.Z, got out=%v", cp.out)
	}
}

func TestFirstPushWithNoPreviousFileWarnsAndSkips(t *testing.T) {
	r := newRepo(t)
	before := r.commit(map[string]string{"VERSION": "0.15.0"})
	r.commit(map[string]string{"CONTROL_PLANE_VERSION": "0.15.0"})
	// The one-time hand tag sits at an older commit, as control-plane-v0.15.0
	// does at v0.15.0.
	r.tag("control-plane-v0.15.0", before)

	cp := r.decide("CONTROL_PLANE_VERSION", "control-plane-v", before, "")
	if !cp.ok || cp.out["created"] != "false" {
		t.Fatalf("want a skip when the tag exists and the file is new, got ok=%v out=%v\n%s", cp.ok, cp.out, cp.logs)
	}
	if !strings.Contains(cp.logs, "::warning::") {
		t.Errorf("want a warning that the previous value could not be read; got:\n%s", cp.logs)
	}
}

// A run that tagged one line and then died (on the other line's tag, or on a
// GitHub Release) must be resumable: a re-run sees its own tag at this commit
// and carries on, instead of tripping the "bumped to an already-tagged
// version" error. A tag at any other commit is never resumed.
func TestTagAtThisCommitResumes(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"VERSION": "0.15.0", "CONTROL_PLANE_VERSION": "0.15.0"})
	r.tag("v0.15.0", base)
	r.tag("control-plane-v0.15.0", base)
	head := r.commit(map[string]string{"VERSION": "0.16.0", "CONTROL_PLANE_VERSION": "0.16.0"})
	r.tag("v0.16.0", head) // the first run got this far

	// The image check would refuse, but a resume must not run it: the images
	// may be half-pushed by the run being resumed, and that is what it finishes.
	osLine := r.decide("VERSION", "v", base, "true")
	if !osLine.ok || osLine.out["created"] != "true" || osLine.out["tag_exists"] != "true" {
		t.Fatalf("OS line: want a resume (created, tag_exists), got ok=%v out=%v\n%s", osLine.ok, osLine.out, osLine.logs)
	}
	cp := r.decide("CONTROL_PLANE_VERSION", "control-plane-v", base, "false")
	if !cp.ok || cp.out["created"] != "true" || cp.out["tag_exists"] != "false" {
		t.Fatalf("control-plane line: want a fresh release, got ok=%v out=%v\n%s", cp.ok, cp.out, cp.logs)
	}

	// The same tag, but this run releases a later commit: a hard error, as
	// for any bump to a version tagged elsewhere.
	r.commit(map[string]string{"VERSION": "0.16.0", "README": "x"})
	if again := r.decide("VERSION", "v", base, ""); again.ok {
		t.Fatalf("want a hard error for a tag at another commit, got out=%v", again.out)
	}
}

// An annotated tag at this commit resumes too: the tag object is not the
// commit, so the script must compare the peeled commit.
func TestAnnotatedTagAtThisCommitResumes(t *testing.T) {
	r := newRepo(t)
	base := r.commit(map[string]string{"VERSION": "0.15.0"})
	head := r.commit(map[string]string{"VERSION": "0.16.0"})
	run(t, r.clone, "git", "tag", "-a", "-m", "m", "v0.16.0", head)
	run(t, r.clone, "git", "push", "-q", "origin", "v0.16.0")
	res := r.decide("VERSION", "v", base, "")
	if !res.ok || res.out["created"] != "true" || res.out["tag_exists"] != "true" {
		t.Fatalf("want a resume for an annotated tag, got ok=%v out=%v\n%s", res.ok, res.out, res.logs)
	}
}

// ghcr-tag-exists.sh with a stub curl: one call checks several repositories,
// any one published means "exists", and an unclear answer is never "missing".
func TestImageCheckAcrossRepos(t *testing.T) {
	bin := t.TempDir()
	stub := `#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    *ghcr.io/token*) echo '{"token":"t"}'; exit 0 ;;
    *ghcr.io/v2/*/manifests/*)
      name="${a#https://ghcr.io/v2/}"; name="${name%%/manifests/*}"; name="${name//\//_}"
      var="FAKE_${name}"; printf '%s' "${!var:-404}"; exit 0 ;;
  esac
done
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("ghcr-tag-exists.sh")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		brain, ui  string
		wantExitRC int
	}{
		{"both missing", "404", "404", 1},
		{"only the ui is published", "404", "200", 0},
		{"only the brain is published", "200", "404", 0},
		{"ui unclear, brain missing", "404", "500", 2},
		{"ui unclear, brain published", "200", "500", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("bash", script, "onmoose/brain", "onmoose/ui", "v0.16.0")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
				"FAKE_onmoose_brain="+c.brain, "FAKE_onmoose_ui="+c.ui)
			err := cmd.Run()
			rc := 0
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if rc != c.wantExitRC {
				t.Errorf("exit = %d, want %d", rc, c.wantExitRC)
			}
		})
	}
}
