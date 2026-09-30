package usermgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSubIDRange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    subIDRange
		found   bool
		wantErr bool
	}{
		{name: "the line", content: "moose-remap:1000000:65536\n", want: subIDRange{1000000, 65536}, found: true},
		{name: "other accounts around it", content: "alice:100000:65536\nmoose-remap:1000000:65536\nbob:165536:65536\n", want: subIDRange{1000000, 65536}, found: true},
		{name: "no line", content: "alice:100000:65536\n"},
		{name: "empty file", content: ""},
		{name: "a longer name is another account", content: "moose-remap2:5:5\n"},
		{name: "blank and comment lines", content: "\n# moose-remap:1:1\n  \nmoose-remap:1000000:65536", want: subIDRange{1000000, 65536}, found: true},
		{name: "two fields", content: "moose-remap:1000000\n", wantErr: true},
		{name: "four fields", content: "moose-remap:1000000:65536:1\n", wantErr: true},
		{name: "start not a number", content: "moose-remap:x:65536\n", wantErr: true},
		{name: "count not a number", content: "moose-remap:1000000:y\n", wantErr: true},
		{name: "start zero would be real root", content: "moose-remap:0:65536\n", wantErr: true},
		{name: "count zero", content: "moose-remap:1000000:0\n", wantErr: true},
		{name: "two lines", content: "moose-remap:1000000:65536\nmoose-remap:2000000:65536\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, found, err := parseSubIDRange([]byte(tc.content), "moose-remap")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %+v found %v, want an error", r, found)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r != tc.want || found != tc.found {
				t.Errorf("got %+v found %v, want %+v %v", r, found, tc.want, tc.found)
			}
		})
	}
}

func TestRemapBase(t *testing.T) {
	const line = "moose-remap:1000000:65536\n"
	for _, tc := range []struct {
		name           string
		subuid, subgid *string // nil means the file does not exist
		base           int
		ok             bool
		wantErr        string
	}{
		{name: "both agree", subuid: strPtr(line), subgid: strPtr(line), base: 1000000, ok: true},
		{name: "a larger range is fine", subuid: strPtr("moose-remap:1000000:131072\n"), subgid: strPtr("moose-remap:1000000:131072\n"), base: 1000000, ok: true},
		{name: "neither has the line", subuid: strPtr("alice:100000:65536\n"), subgid: strPtr("alice:100000:65536\n")},
		{name: "neither file exists"},
		{name: "starts differ", subuid: strPtr(line), subgid: strPtr("moose-remap:2000000:65536\n"), wantErr: "differ"},
		{name: "counts differ", subuid: strPtr(line), subgid: strPtr("moose-remap:1000000:1024\n"), wantErr: "differ"},
		{name: "range too small", subuid: strPtr("moose-remap:1000000:1024\n"), subgid: strPtr("moose-remap:1000000:1024\n"), wantErr: "at least 65536"},
		{name: "only subuid has it", subuid: strPtr(line), subgid: strPtr(""), wantErr: "only one"},
		{name: "only subgid has it", subuid: nil, subgid: strPtr(line), wantErr: "only one"},
		{name: "malformed subgid", subuid: strPtr(line), subgid: strPtr("moose-remap:oops\n"), wantErr: "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			m := &LinuxUserManager{SubUIDPath: filepath.Join(dir, "subuid"), SubGIDPath: filepath.Join(dir, "subgid")}
			writeIfSet(t, m.SubUIDPath, tc.subuid)
			writeIfSet(t, m.SubGIDPath, tc.subgid)
			base, ok, err := m.RemapBase()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if base != tc.base || ok != tc.ok {
				t.Errorf("got base %d ok %v, want %d %v", base, ok, tc.base, tc.ok)
			}
		})
	}
}

// A file that exists but cannot be read is an error, not "no remap".
func TestRemapBaseUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	// A directory at the file's path makes ReadFile fail with an error that is
	// not "does not exist", as root and as any user.
	m := &LinuxUserManager{SubUIDPath: dir, SubGIDPath: filepath.Join(dir, "missing")}
	if _, _, err := m.RemapBase(); err == nil {
		t.Fatal("want an error for an unreadable subuid file")
	}
}

// ReadRemapBase is the function the fake host-agent calls (#548). It is the
// same code as the method, so the cases above hold for it; this checks that it
// reads the paths it is given.
func TestReadRemapBase(t *testing.T) {
	dir := t.TempDir()
	subuid, subgid := filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")
	if _, ok, err := ReadRemapBase(subuid, subgid); ok || err != nil {
		t.Fatalf("no files: ok %v err %v, want no remap", ok, err)
	}
	writeIfSet(t, subuid, strPtr("alice:100000:65536\nmoose-remap:1000000:65536\n"))
	writeIfSet(t, subgid, strPtr("moose-remap:1000000:65536\n"))
	base, ok, err := ReadRemapBase(subuid, subgid)
	if err != nil || !ok || base != 1000000 {
		t.Fatalf("got base %d ok %v err %v, want 1000000 true nil", base, ok, err)
	}
	writeIfSet(t, subgid, strPtr(""))
	if _, _, err := ReadRemapBase(subuid, subgid); err == nil {
		t.Fatal("a range in only one file: want an error")
	}
}

func strPtr(s string) *string { return &s }

func writeIfSet(t *testing.T, path string, content *string) {
	t.Helper()
	if content == nil {
		return
	}
	if err := os.WriteFile(path, []byte(*content), 0o644); err != nil {
		t.Fatal(err)
	}
}
