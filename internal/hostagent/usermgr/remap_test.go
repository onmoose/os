package usermgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSubIDStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		start   int
		found   bool
		wantErr bool
	}{
		{name: "the line", content: "moose-remap:1000000:65536\n", start: 1000000, found: true},
		{name: "other accounts around it", content: "alice:100000:65536\nmoose-remap:1000000:65536\nbob:165536:65536\n", start: 1000000, found: true},
		{name: "no line", content: "alice:100000:65536\n", found: false},
		{name: "empty file", content: "", found: false},
		{name: "a longer name is another account", content: "moose-remap2:5:5\n", found: false},
		{name: "blank and comment lines", content: "\n# moose-remap:1:1\n  \nmoose-remap:1000000:65536", start: 1000000, found: true},
		{name: "two fields", content: "moose-remap:1000000\n", wantErr: true},
		{name: "four fields", content: "moose-remap:1000000:65536:1\n", wantErr: true},
		{name: "start not a number", content: "moose-remap:x:65536\n", wantErr: true},
		{name: "count not a number", content: "moose-remap:1000000:y\n", wantErr: true},
		{name: "start zero would be real root", content: "moose-remap:0:65536\n", wantErr: true},
		{name: "count zero", content: "moose-remap:1000000:0\n", wantErr: true},
		{name: "two lines", content: "moose-remap:1000000:65536\nmoose-remap:2000000:65536\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, found, err := parseSubIDStart([]byte(tc.content), "moose-remap")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got start %d found %v, want an error", start, found)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if start != tc.start || found != tc.found {
				t.Errorf("got start %d found %v, want %d %v", start, found, tc.start, tc.found)
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
		{name: "both agree", subuid: ptr(line), subgid: ptr(line), base: 1000000, ok: true},
		{name: "neither has the line", subuid: ptr("alice:100000:65536\n"), subgid: ptr("alice:100000:65536\n")},
		{name: "neither file exists"},
		{name: "starts differ", subuid: ptr(line), subgid: ptr("moose-remap:2000000:65536\n"), wantErr: "differ"},
		{name: "only subuid has it", subuid: ptr(line), subgid: ptr(""), wantErr: "only one"},
		{name: "only subgid has it", subuid: nil, subgid: ptr(line), wantErr: "only one"},
		{name: "malformed subgid", subuid: ptr(line), subgid: ptr("moose-remap:oops\n"), wantErr: "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			m := &LinuxUserManager{SubUIDPath: filepath.Join(dir, "subuid"), SubGIDPath: filepath.Join(dir, "subgid")}
			write(t, m.SubUIDPath, tc.subuid)
			write(t, m.SubGIDPath, tc.subgid)
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

func ptr(s string) *string { return &s }

func write(t *testing.T, path string, content *string) {
	t.Helper()
	if content == nil {
		return
	}
	if err := os.WriteFile(path, []byte(*content), 0o644); err != nil {
		t.Fatal(err)
	}
}
