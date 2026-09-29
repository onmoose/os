package lifecycle

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"
)

const testPasswd = `root:x:0:0:root:/root:/bin/sh
# a comment
broken line
plunk:x:1001:1001::/home/plunk:/bin/sh
nextjs:x:1002:65533::/home/nextjs:/sbin/nologin
twice:x:1003:1003::/:/bin/sh
twice:x:1004:1004::/:/bin/sh
huge:x:70000:70000::/:/bin/sh
`

const testGroup = `root:x:0:
nogroup:x:65533:
app:x:2200:plunk
`

func TestResolveImageUser(t *testing.T) {
	for _, tc := range []struct {
		spec      string
		want      string // "uid:gid", or "" when refused
		wantErr   string
		readsFile bool
	}{
		{spec: "", want: "0:0"},
		{spec: "1001:1001", want: "1001:1001"},
		{spec: ":2200", want: "0:2200"},
		// A number with no group takes its passwd entry's gid, or 0.
		{spec: "1001", want: "1001:1001", readsFile: true},
		{spec: "1002", want: "1002:65533", readsFile: true},
		{spec: "4242", want: "4242:0", readsFile: true},
		{spec: "4242:", want: "4242:0", readsFile: true},
		{spec: "0", want: "0:0", readsFile: true},
		{spec: "plunk", want: "1001:1001", readsFile: true},
		{spec: "nextjs", want: "1002:65533", readsFile: true},
		{spec: "twice", want: "1003:1003", readsFile: true}, // the first line wins, like Docker
		{spec: "plunk:app", want: "1001:2200", readsFile: true},
		{spec: "plunk:7", want: "1001:7", readsFile: true},
		{spec: "1001:app", want: "1001:2200", readsFile: true},
		{spec: "ghost", wantErr: "not in the image's own user list", readsFile: true},
		{spec: "plunk:ghosts", wantErr: "not in the image's own group list", readsFile: true},
		{spec: "huge", wantErr: "below 65536", readsFile: true},
		{spec: "70000:0", wantErr: "below 65536"},
		{spec: "1:70000", wantErr: "below 65536"},
		{spec: "-1", wantErr: "not in the image's own user list", readsFile: true}, // not a number, so a name
	} {
		t.Run(tc.spec, func(t *testing.T) {
			read := false
			ids, err := resolveImageUser("app", tc.spec, func() ([]byte, []byte, error) {
				read = true
				return []byte(testPasswd), []byte(testGroup), nil
			})
			if read != tc.readsFile {
				t.Errorf("read the files = %v, want %v", read, tc.readsFile)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one with %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := fmt.Sprintf("%d:%d", ids.uid, ids.gid); got != tc.want {
				t.Fatalf("resolved %s, want %s", got, tc.want)
			}
		})
	}
}

// A name with no passwd file at all is refused, a number is not.
func TestResolveImageUserWithoutFiles(t *testing.T) {
	none := func() ([]byte, []byte, error) { return nil, nil, nil }
	if _, err := resolveImageUser("app", "plunk", none); err == nil {
		t.Error("a name resolved with no passwd file")
	}
	if ids, err := resolveImageUser("app", "1001", none); err != nil || ids != (imageIDs{1001, 0}) {
		t.Errorf("1001 with no passwd = %+v, %v; want 1001:0", ids, err)
	}
	boom := func() ([]byte, []byte, error) { return nil, nil, errors.New("boom") }
	if _, err := resolveImageUser("app", "plunk", boom); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a failed read = %v, want it passed on", err)
	}
}

func tarOf(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		body := h.Linkname
		if h.Typeflag == tar.TypeReg {
			body = strings.Repeat("x", int(h.Size))
			if h.Name == "passwd" && h.Size == 0 {
				body = "plunk:x:1001:1001::/:/bin/sh\n"
				h.Size = int64(len(body))
			}
		}
		if h.Typeflag == tar.TypeSymlink {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadUserFile(t *testing.T) {
	// docker cp of one path writes a tar with that one entry, named by its
	// base name.
	passwd := &tar.Header{Name: "passwd", Typeflag: tar.TypeReg, Mode: 0o644}
	p, kind, err := readUserFile(bytes.NewReader(tarOf(t, passwd)))
	if err != nil || kind != entryRegular || !strings.Contains(string(p), "plunk:x:1001") {
		t.Fatalf("read = %q, %v; want the passwd", p, err)
	}

	// A symlinked passwd counts as missing: its target is not in the stream.
	link := &tar.Header{Name: "passwd", Typeflag: tar.TypeSymlink, Linkname: "/usr/lib/passwd", Mode: 0o777}
	if p, kind, err := readUserFile(bytes.NewReader(tarOf(t, link))); err != nil || p != nil || kind != entryOther {
		t.Fatalf("symlink read = %q, %v; want nothing", p, err)
	}

	// A directory in place of the file counts as missing, and only its first
	// header is read.
	dir := &tar.Header{Name: "passwd/", Typeflag: tar.TypeDir, Mode: 0o755}
	if p, kind, err := readUserFile(bytes.NewReader(tarOf(t, dir))); err != nil || p != nil || kind != entryOther {
		t.Fatalf("directory read = %q, %d, %v; want entryOther", p, kind, err)
	}

	big := &tar.Header{Name: "group", Typeflag: tar.TypeReg, Mode: 0o644, Size: maxUserFile + 1}
	if _, _, err := readUserFile(bytes.NewReader(tarOf(t, big))); err == nil {
		t.Fatal("a group file over the limit was read")
	}

	if p, kind, err := readUserFile(bytes.NewReader(nil)); err != nil || p != nil || kind != entryNone {
		t.Fatalf("empty stream = %q, %v; want nothing", p, err)
	}
}

// The cap fails the read once it is passed, instead of ending the stream the
// way io.LimitReader would.
func TestCapReader(t *testing.T) {
	c := &capReader{r: strings.NewReader("0123456789"), left: 4}
	buf := make([]byte, 10)
	if n, err := c.Read(buf); n != 4 || err != nil {
		t.Fatalf("first read = %d, %v; want 4, nil", n, err)
	}
	if _, err := c.Read(buf); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("read past the cap = %v, want a real error", err)
	}
}

// imageUserManifest is the whoami manifest with image_user: true.
func imageUserManifest() string {
	return strings.Replace(whoamiManifest(testDigest), "main_port: 80\n", "main_port: 80\nimage_user: true\n", 1)
}

const whoamiRef = "traefik/whoami@" + testDigest

// On a daemon with no remap an image_user app is refused before any state:
// there the image's uid would be a real host uid.
func TestInstallImageUserRefusedWithoutRemap(t *testing.T) {
	e := newTestEnv(t)
	_, err := installWhoamiWith(t, e, imageUserManifest())
	if !errors.Is(err, ErrImageUserNeedsRemap) {
		t.Fatalf("install = %v, want ErrImageUserNeedsRemap", err)
	}
	assertNoInstallState(t, e)
}

// A user: in the compose of an image_user app is refused before any state,
// with or without the remap.
func TestInstallImageUserRefusesComposeUser(t *testing.T) {
	e := newTestEnv(t)
	e.remapped()
	e.writeCatalogApp(t, "whoami", whoamiCompose+"    user: nobody\n", imageUserManifest())
	e.docker.digests[testImage] = testDigest
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), goldenAdmin, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "image_user") {
		t.Fatalf("install = %v, want the image_user compose refusal", err)
	}
	assertNoInstallState(t, e)
}

const imageUserCompose = `
services:
  whoami:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data:/data
      - ./data/cache:/cache
`

func installImageUser(t *testing.T, e *testEnv, compose string) (store.Instance, error) {
	t.Helper()
	e.writeCatalogApp(t, "whoami", compose, imageUserManifest())
	e.docker.digests[testImage] = testDigest
	return e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), goldenAdmin, store.ScopeHousehold, nil, "", nil, nil, nil)
}

// An image_user app on a remapped daemon is in the image tier: remapped, no
// capability back, no user:, and its bind dirs owned by base plus the ids
// the image runs as.
func TestInstallRemappedImageUserTier(t *testing.T) {
	for _, tc := range []struct {
		spec, passwd string
		want         string
	}{
		{spec: "1001", passwd: testPasswd, want: "1001001:1001001"},
		{spec: "1001:1001", want: "1001001:1001001"},
		{spec: "plunk", passwd: testPasswd, want: "1001001:1001001"},
		{spec: "", want: "1000000:1000000"},
		{spec: "0", passwd: testPasswd, want: "1000000:1000000"},
	} {
		t.Run("user="+tc.spec, func(t *testing.T) {
			e := newTestEnv(t)
			e.remapped()
			chowns := recordChowns(e)
			e.docker.imageUsers = map[string]string{whoamiRef: tc.spec}
			e.docker.imagePasswd = map[string]string{whoamiRef: tc.passwd}
			inst, err := installImageUser(t, e, imageUserCompose)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if row, _ := e.store.Get(inst.ID); row.UsernsTier != store.UsernsTierImage {
				t.Fatalf("tier = %q, want image", row.UsernsTier)
			}
			svc := parseOverrideServices(t, readInstanceFile(t, e, inst.ID, "compose.override.yml"))["whoami"]
			if svc.UsernsMode != "" || len(svc.CapAdd) > 0 || svc.User != "" || len(svc.CapDrop) != 1 || svc.CapDrop[0] != "ALL" {
				t.Fatalf("image tier service = %+v, want remapped, cap_drop ALL, no cap_add, no user", svc)
			}
			for _, rel := range []string{"data", "data/cache"} {
				p := filepath.Join(e.m.instanceDir(inst.ID), filepath.FromSlash(rel))
				if got := chowns.owner(p); got != tc.want {
					t.Errorf("%s owner = %q, want %s", rel, got, tc.want)
				}
			}
		})
	}
}

// An image user the brain cannot resolve refuses the install after the pull,
// and the rollback leaves no row and never starts the app.
func TestInstallImageUserUnresolvedRollsBack(t *testing.T) {
	for name, set := range map[string]func(e *testEnv){
		"unknown name":  func(e *testEnv) { e.docker.imagePasswd = map[string]string{whoamiRef: testPasswd} },
		"no passwd":     func(e *testEnv) {},
		"probe failure": func(e *testEnv) { e.docker.imageFilesErr = errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.remapped()
			e.docker.imageUsers = map[string]string{whoamiRef: "ghost"}
			set(e)
			if _, err := installImageUser(t, e, imageUserCompose); err == nil {
				t.Fatal("install succeeded with an unresolved image user")
			}
			if insts, _ := e.store.List(); len(insts) != 0 {
				t.Errorf("rollback left %d rows", len(insts))
			}
			if e.docker.called("ComposeUp") {
				t.Error("the app was started")
			}
		})
	}
}

// Each service's dirs go to its own image's user, and a dir two services
// with different users both bind is refused.
func TestInstallImageUserPerService(t *testing.T) {
	const workerImage = "example/worker:1"
	const workerDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const workerRef = "example/worker@" + workerDigest
	compose := func(workerDir string) string {
		return `
services:
  whoami:
    image: traefik/whoami:v1.10.3
    volumes:
      - ./data:/data
  worker:
    image: example/worker:1
    volumes:
      - ` + workerDir + `:/work
`
	}
	setup := func(e *testEnv) {
		e.remapped()
		e.docker.digests[workerImage] = workerDigest
		e.docker.imageUsers = map[string]string{whoamiRef: "1001:1001", workerRef: "2002:2002"}
	}

	e := newTestEnv(t)
	setup(e)
	chowns := recordChowns(e)
	inst, err := installImageUser(t, e, compose("./work"))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	for rel, want := range map[string]string{"data": "1001001:1001001", "work": "1002002:1002002"} {
		if got := chowns.owner(filepath.Join(e.m.instanceDir(inst.ID), rel)); got != want {
			t.Errorf("%s owner = %q, want %s", rel, got, want)
		}
	}

	e = newTestEnv(t)
	setup(e)
	_, err = installImageUser(t, e, compose("./data"))
	if err == nil || !strings.Contains(err.Error(), "run as different users") {
		t.Fatalf("shared dir install = %v, want the different-users refusal", err)
	}
	if insts, _ := e.store.List(); len(insts) != 0 {
		t.Errorf("rollback left %d rows", len(insts))
	}
}

func TestBindDirOwners(t *testing.T) {
	dirs := map[string][]string{"app": {"data"}, "side": {"cache", "data"}}
	// Every tier but the image tier gives every dir the one owner.
	iso := isolation{uid: 2100, gid: 2100, tier: store.UsernsTierDefault, remapBase: testRemapBase}
	owners, err := iso.bindDirOwners(dirs)
	if err != nil || len(owners) != 2 || owners["data"] != (hostOwner{1002100, 1002100}) || owners["cache"] != (hostOwner{1002100, 1002100}) {
		t.Fatalf("default tier owners = %v, %v", owners, err)
	}
	// The image tier: the same user on both services may share a dir.
	iso = isolation{tier: store.UsernsTierImage, remapBase: testRemapBase, imageIDs: map[string]imageIDs{"app": {1001, 1001}, "side": {1001, 1001}}}
	owners, err = iso.bindDirOwners(dirs)
	if err != nil || owners["data"] != (hostOwner{1001001, 1001001}) {
		t.Fatalf("image tier owners = %v, %v", owners, err)
	}
	iso.imageIDs["side"] = imageIDs{1001, 0}
	if _, err := iso.bindDirOwners(dirs); err == nil {
		t.Fatal("a shared dir with two owners was accepted")
	}
	delete(iso.imageIDs, "side")
	if _, err := iso.bindDirOwners(dirs); err == nil {
		t.Fatal("a service with no resolved image user was accepted")
	}
	if _, _, err := (&isolation{tier: store.UsernsTierImage, remapBase: testRemapBase}).bindOwner(); err == nil {
		t.Fatal("bindOwner gave the image tier one owner")
	}
}
