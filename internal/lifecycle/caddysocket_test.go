package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// The pre-#608 fixtures are the staged files as the image baked them before
// #608 (git show 48266cd^:dev/control-plane/...), which is what a box made
// before it still has on disk.
const pre608Dir = "testdata/controlplane-pre608"

// stageControlPlane copies caddy.json and compose.yml from src into a temp
// dir, as the image build stages them on a box, and returns the dir.
func stageControlPlane(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{controlPlaneCaddyConfig, controlPlaneComposeFile} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type chownCall struct {
	path     string
	uid, gid int
}

// migrateRig is a manager with a remapped Docker (base 100000), a recorded
// chown and a recorded admin wait that answers waitErr.
type migrateRig struct {
	m      *Manager
	docker *fakeDocker
	mu     sync.Mutex
	chowns []chownCall
	waits  []string
}

func newMigrateRig(waitErr error) *migrateRig {
	r := &migrateRig{docker: newFakeDocker()}
	r.docker.usernsRemap = true
	host := newFakeHost()
	base := 100000
	host.remapBase = &base
	r.m = &Manager{docker: r.docker, host: host}
	r.m.chown = func(p string, uid, gid int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.chowns = append(r.chowns, chownCall{p, uid, gid})
		return nil
	}
	r.m.caddyAdminWait = func(_ context.Context, addr string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.waits = append(r.waits, addr)
		return waitErr
	}
	return r
}

func (r *migrateRig) ups() int {
	n := 0
	for _, c := range r.docker.Calls() {
		if c.method == "ControlPlaneUp" {
			n++
		}
	}
	return n
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parsedJSON(t *testing.T, p string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(readFile(t, p), &v); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	return v
}

func parsedYAML(t *testing.T, p string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(readFile(t, p), &v); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	return v
}

func countComments(b []byte) int {
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			n++
		}
	}
	return n
}

// TestMigrateCaddyAdminSocket_MovesPre608Box: the pre-#608 files become, as
// parsed documents, exactly the files a box made since #608 has, with every
// comment kept. The socket dir is made for the remap base, copies of the
// originals are kept, Caddy is brought up once, and the brain gets the socket
// address to dial.
func TestMigrateCaddyAdminSocket_MovesPre608Box(t *testing.T) {
	dir := stageControlPlane(t, pre608Dir)
	r := newMigrateRig(nil)

	addr, err := r.m.MigrateCaddyAdminSocket(context.Background(), dir)
	if err != nil {
		t.Fatalf("MigrateCaddyAdminSocket: %v", err)
	}
	wantAddr := "unix://" + filepath.Join(dir, "caddy", "admin.sock")
	if addr != wantAddr {
		t.Fatalf("addr = %q, want %q", addr, wantAddr)
	}
	if got, err := ControlPlaneCaddyAdmin(dir); err != nil || got != wantAddr {
		t.Fatalf("the staged caddy.json now reads %q, %v; want %q", got, err, wantAddr)
	}

	staged := filepath.Join("..", "..", "dev", "control-plane")
	if got, want := parsedJSON(t, filepath.Join(dir, "caddy.json")), parsedJSON(t, filepath.Join(staged, "caddy.json")); !reflect.DeepEqual(got, want) {
		t.Errorf("migrated caddy.json differs from the one a new box gets:\n got %v\nwant %v", got, want)
	}
	if got, want := parsedYAML(t, filepath.Join(dir, "compose.yml")), parsedYAML(t, filepath.Join(staged, "compose.yml")); !reflect.DeepEqual(got, want) {
		t.Errorf("migrated compose.yml differs from the one a new box gets:\n got %v\nwant %v", got, want)
	}
	if got, want := countComments(readFile(t, filepath.Join(dir, "compose.yml"))), countComments(readFile(t, filepath.Join(pre608Dir, "compose.yml"))); got != want {
		t.Errorf("migrated compose.yml has %d comment lines, the original %d: the edit dropped comments", got, want)
	}

	for _, f := range []string{"caddy.json", "compose.yml"} {
		if got, want := readFile(t, filepath.Join(dir, f+".tcp-admin")), readFile(t, filepath.Join(pre608Dir, f)); string(got) != string(want) {
			t.Errorf("%s.tcp-admin is not a copy of the original", f)
		}
	}

	fi, err := os.Stat(filepath.Join(dir, "caddy"))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir: %v, %v; want a dir with mode 0700", fi, err)
	}
	if want := []chownCall{{filepath.Join(dir, "caddy"), 100000, 100000}}; !reflect.DeepEqual(r.chowns, want) {
		t.Errorf("chowns = %v, want %v", r.chowns, want)
	}
	if r.ups() != 1 {
		t.Errorf("compose up ran %d times, want 1", r.ups())
	}
	if !reflect.DeepEqual(r.waits, []string{wantAddr}) {
		t.Errorf("waited on %v, want the socket %q", r.waits, wantAddr)
	}

	// A second start finds the box on the socket and does nothing.
	r2 := newMigrateRig(nil)
	if addr, err := r2.m.MigrateCaddyAdminSocket(context.Background(), dir); addr != "" || err != nil {
		t.Fatalf("second run = %q, %v; want nothing to do", addr, err)
	}
	if len(r2.docker.Calls()) != 0 || len(r2.chowns) != 0 {
		t.Errorf("second run touched Docker %v or the dir %v", r2.docker.methods(), r2.chowns)
	}
}

// TestMigrateCaddyAdminSocket_NewBoxNoop: a box made since #608 reads one
// file and is left alone.
func TestMigrateCaddyAdminSocket_NewBoxNoop(t *testing.T) {
	dir := stageControlPlane(t, filepath.Join("..", "..", "dev", "control-plane"))
	r := newMigrateRig(nil)
	addr, err := r.m.MigrateCaddyAdminSocket(context.Background(), dir)
	if addr != "" || err != nil {
		t.Fatalf("got %q, %v; want nothing to do", addr, err)
	}
	if len(r.docker.Calls()) != 0 || len(r.waits) != 0 {
		t.Errorf("a new box touched Docker %v or waited %v", r.docker.methods(), r.waits)
	}
	if _, err := os.Stat(filepath.Join(dir, "caddy.json.tcp-admin")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a new box got a backup: %v", err)
	}
}

// TestMigrateCaddyAdminSocket_SocketFailsRestores: when the socket does not
// answer, the original files go back and compose up runs again, so Caddy is
// recreated on TCP. The brain gets an error and keeps its TCP address.
func TestMigrateCaddyAdminSocket_SocketFailsRestores(t *testing.T) {
	dir := stageControlPlane(t, pre608Dir)
	r := newMigrateRig(errors.New("admin API not ready"))

	addr, err := r.m.MigrateCaddyAdminSocket(context.Background(), dir)
	if err == nil || addr != "" {
		t.Fatalf("got %q, %v; want an error and no address", addr, err)
	}
	for _, f := range []string{"caddy.json", "compose.yml"} {
		if got, want := readFile(t, filepath.Join(dir, f)), readFile(t, filepath.Join(pre608Dir, f)); string(got) != string(want) {
			t.Errorf("%s was not put back byte for byte", f)
		}
	}
	if r.ups() != 2 {
		t.Errorf("compose up ran %d times, want 2: once on the socket, once back on TCP", r.ups())
	}
	if got, err := ControlPlaneCaddyAdmin(dir); err != nil || got != "" {
		t.Errorf("after the restore the staged caddy.json reads %q, %v; want TCP", got, err)
	}
}

// TestMigrateCaddyAdminSocket_ComposeFailsRestores: a failed compose up is
// treated like a socket that does not answer.
func TestMigrateCaddyAdminSocket_ComposeFailsRestores(t *testing.T) {
	dir := stageControlPlane(t, pre608Dir)
	r := newMigrateRig(nil)
	r.docker.controlPlaneUpErr = errors.New("compose boom")

	if addr, err := r.m.MigrateCaddyAdminSocket(context.Background(), dir); err == nil || addr != "" {
		t.Fatalf("got %q, %v; want an error", addr, err)
	}
	if len(r.waits) != 0 {
		t.Errorf("waited on the socket after compose up failed: %v", r.waits)
	}
	if got := readFile(t, filepath.Join(dir, "caddy.json")); string(got) != string(readFile(t, filepath.Join(pre608Dir, "caddy.json"))) {
		t.Error("caddy.json was not put back")
	}
}

// TestMigrateCaddyAdminSocket_RemapMismatchChangesNothing: without an owner
// the brain can trust for the socket dir, nothing is written and Docker is
// not asked to bring anything up.
func TestMigrateCaddyAdminSocket_RemapMismatchChangesNothing(t *testing.T) {
	dir := stageControlPlane(t, pre608Dir)
	r := newMigrateRig(nil)
	r.docker.usernsRemap = false // host-agent says 100000, Docker says no remap

	if _, err := r.m.MigrateCaddyAdminSocket(context.Background(), dir); !errors.Is(err, ErrRemapMismatch) {
		t.Fatalf("err = %v, want ErrRemapMismatch", err)
	}
	assertUntouched(t, dir, r)
}

// TestMigrateCaddyAdminSocket_FileInTheWayChangesNothing: a file where the
// socket dir goes stops the move before any write.
func TestMigrateCaddyAdminSocket_FileInTheWayChangesNothing(t *testing.T) {
	dir := stageControlPlane(t, pre608Dir)
	if err := os.WriteFile(filepath.Join(dir, "caddy"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := newMigrateRig(nil)
	if _, err := r.m.MigrateCaddyAdminSocket(context.Background(), dir); err == nil {
		t.Fatal("want an error with a file where the socket dir goes")
	}
	assertUntouched(t, dir, r)
}

func assertUntouched(t *testing.T, dir string, r *migrateRig) {
	t.Helper()
	for _, f := range []string{"caddy.json", "compose.yml"} {
		if got := readFile(t, filepath.Join(dir, f)); string(got) != string(readFile(t, filepath.Join(pre608Dir, f))) {
			t.Errorf("%s changed", f)
		}
		if _, err := os.Stat(filepath.Join(dir, f+".tcp-admin")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s.tcp-admin was written: %v", f, err)
		}
	}
	if r.ups() != 0 {
		t.Errorf("compose up ran %d times, want 0", r.ups())
	}
}

// TestComposeWithCaddySocket covers the shapes the line edit must handle and
// the ones it must refuse.
func TestComposeWithCaddySocket(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // "" with wantErr false: the input unchanged
		wantErr bool
	}{
		{
			name: "items indented under the key, comment after the list",
			in: "services:\n  caddy:\n    image: caddy\n    volumes:\n      - ./caddy.json:/etc/caddy/caddy.json:ro\n      # a note\n      - caddy_data:/data\n    # about tmpfs\n    tmpfs:\n      - /config\n  moose-ui:\n    volumes:\n      - x:/y\n",
			want: "services:\n  caddy:\n    image: caddy\n    volumes:\n      - ./caddy.json:/etc/caddy/caddy.json:ro\n      # a note\n      - caddy_data:/data\n      - ./caddy:/run/moose-caddy\n    # about tmpfs\n    tmpfs:\n      - /config\n  moose-ui:\n    volumes:\n      - x:/y\n",
		},
		{
			name: "items at the key's indent, list last in the file",
			in:   "services:\n  moose-ui:\n    image: ui\n  caddy:\n    image: caddy\n    volumes:\n    - ./caddy.json:/etc/caddy/caddy.json:ro\n",
			want: "services:\n  moose-ui:\n    image: ui\n  caddy:\n    image: caddy\n    volumes:\n    - ./caddy.json:/etc/caddy/caddy.json:ro\n    - ./caddy:/run/moose-caddy\n",
		},
		{
			name: "bind already there",
			in:   "services:\n  caddy:\n    volumes:\n      - ./caddy:/run/moose-caddy\n",
		},
		{
			name:    "flow list",
			in:      "services:\n  caddy:\n    volumes: [\"./caddy.json:/etc/caddy/caddy.json:ro\"]\n",
			wantErr: true,
		},
		{
			name:    "no volumes",
			in:      "services:\n  caddy:\n    image: caddy\n",
			wantErr: true,
		},
		{
			name:    "no caddy service",
			in:      "services:\n  moose-ui:\n    volumes:\n      - a:/b\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := composeWithCaddySocket([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("composeWithCaddySocket: %v", err)
			}
			want := tc.want
			if want == "" {
				want = tc.in
			}
			if string(got) != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// TestCaddyJSONWithSocket: the admin address is replaced in place, and an
// address that is also used elsewhere in the file is refused rather than
// guessed at.
func TestCaddyJSONWithSocket(t *testing.T) {
	in := []byte(`{"admin": {"listen": "0.0.0.0:2019"}, "apps": {}}`)
	got, err := caddyJSONWithSocket(in, "0.0.0.0:2019")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"admin": {"listen": "unix//run/moose-caddy/admin.sock"}, "apps": {}}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}

	twice := []byte(`{"admin": {"listen": "0.0.0.0:2019"}, "apps": {"x": "0.0.0.0:2019"}}`)
	if _, err := caddyJSONWithSocket(twice, "0.0.0.0:2019"); err == nil {
		t.Error("want an error when the address appears twice")
	}
}
