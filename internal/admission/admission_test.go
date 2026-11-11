package admission

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/manifest"
)

// TestCheckStructure is the table-driven core of the admission policy
// (APP_LIFECYCLE.md). Each row carries a compose snippet + the substrings the
// rejection message must contain (service name, field). validateSyntax is
// skipped here so the test stays hermetic — Check (with daemon) is exercised
// via integration lanes.
func TestCheckStructure(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
		wantSub []string // substrings the error message must contain
	}{
		{
			name: "happy",
			yaml: `
services:
  web:
    image: nginx:1.27
`,
			wantErr: false,
		},
		{
			name: "ports rejected",
			yaml: `
services:
  web:
    image: nginx
    ports: ["8080:80"]
`,
			wantErr: true, wantSub: []string{`"web"`, "ports"},
		},
		{
			name: "privileged rejected",
			yaml: `
services:
  web:
    image: nginx
    privileged: true
`,
			wantErr: true, wantSub: []string{`"web"`, "privileged"},
		},
		{
			name: "cap_add rejected",
			yaml: `
services:
  web:
    image: nginx
    cap_add: [SYS_ADMIN]
`,
			wantErr: true, wantSub: []string{`"web"`, "cap_add"},
		},
		{
			name: "build rejected",
			yaml: `
services:
  web:
    image: nginx
    build: .
`,
			wantErr: true, wantSub: []string{`"web"`, "build"},
		},
		{
			name: "extends rejected",
			yaml: `
services:
  web:
    image: nginx
    extends:
      service: base
`,
			wantErr: true, wantSub: []string{`"web"`, "extends"},
		},
		{
			name: "deploy.replicas > 1 rejected",
			yaml: `
services:
  web:
    image: nginx
    deploy:
      replicas: 3
`,
			wantErr: true, wantSub: []string{`"web"`, "deploy.replicas"},
		},
		{
			name: "deploy.replicas 1 allowed",
			yaml: `
services:
  web:
    image: nginx
    deploy:
      replicas: 1
`,
			wantErr: false,
		},
		{
			name: "network_mode host rejected",
			yaml: `
services:
  web:
    image: nginx
    network_mode: host
`,
			wantErr: true, wantSub: []string{`"web"`, "network_mode"},
		},
		{
			name: "pid host rejected",
			yaml: `
services:
  web:
    image: nginx
    pid: host
`,
			wantErr: true, wantSub: []string{`"web"`, "pid"},
		},
		{
			name: "ipc host rejected",
			yaml: `
services:
  web:
    image: nginx
    ipc: host
`,
			wantErr: true, wantSub: []string{`"web"`, "ipc"},
		},
		{
			name: "userns_mode host rejected",
			yaml: `
services:
  web:
    image: nginx
    userns_mode: host
`,
			wantErr: true, wantSub: []string{`"web"`, "userns_mode"},
		},
		{
			name: "absolute bind path rejected",
			yaml: `
services:
  web:
    image: nginx
    volumes: ["/etc/passwd:/etc/passwd:ro"]
`,
			wantErr: true, wantSub: []string{`"web"`, "absolute"},
		},
		{
			name: "named volume rejected (short form)",
			yaml: `
services:
  web:
    image: nginx
    volumes: ["data:/var/data"]
`,
			wantErr: true, wantSub: []string{`"web"`, "named volume"},
		},
		{
			name: "named volume rejected (long form)",
			yaml: `
services:
  web:
    image: nginx
    volumes:
      - type: volume
        source: data
        target: /var/data
`,
			wantErr: true, wantSub: []string{`"web"`, "named volume"},
		},
		{
			name: "relative bind allowed",
			yaml: `
services:
  web:
    image: nginx
    volumes: ["./data:/var/data"]
`,
			wantErr: false,
		},
		{
			name:    "no services",
			yaml:    `services: {}`,
			wantErr: true, wantSub: []string{"no services"},
		},
		{
			name: "numeric user rejected (bare int)",
			yaml: `
services:
  web:
    image: nginx
    user: 1000
`,
			wantErr: true, wantSub: []string{`"web"`, "numeric user"},
		},
		{
			name: "numeric user rejected (quoted)",
			yaml: `
services:
  web:
    image: nginx
    user: "1000"
`,
			wantErr: true, wantSub: []string{`"web"`, "numeric user"},
		},
		{
			name: "numeric user rejected (uid:gid)",
			yaml: `
services:
  web:
    image: nginx
    user: "1000:1000"
`,
			wantErr: true, wantSub: []string{`"web"`, "numeric user"},
		},
		{
			name: "numeric user rejected (root)",
			yaml: `
services:
  web:
    image: nginx
    user: "0"
`,
			wantErr: true, wantSub: []string{`"web"`, "numeric user"},
		},
		{
			name: "numeric gid component rejected (name:gid)",
			yaml: `
services:
  web:
    image: nginx
    user: "www-data:33"
`,
			wantErr: true, wantSub: []string{`"web"`, "numeric user"},
		},
		{
			name: "named user allowed",
			yaml: `
services:
  web:
    image: nginx
    user: www-data
`,
			wantErr: false,
		},
		{
			name: "variable user allowed",
			yaml: `
services:
  web:
    image: nginx
    user: "${APP_UID}"
`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckStructure(context.Background(), []byte(tc.yaml))
			if tc.wantErr && err == nil {
				t.Fatalf("want rejection, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if err != nil {
				for _, sub := range tc.wantSub {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing %q", err.Error(), sub)
					}
				}
			}
		})
	}
}

// TestCheckManifest covers the manifest-side rule: service_user is only for
// folderless apps — a folder app already has a managed non-root identity
// (APP_MANIFEST.md # B).
func TestCheckManifest(t *testing.T) {
	folders := []manifest.Folder{{Folder: "documents", Mode: "read"}}
	cases := []struct {
		name    string
		man     manifest.Manifest
		wantErr bool
	}{
		{name: "plain folderless", man: manifest.Manifest{}, wantErr: false},
		{name: "service_user folderless", man: manifest.Manifest{ServiceUser: true}, wantErr: false},
		{name: "folders without service_user", man: manifest.Manifest{
			Permissions: manifest.Permissions{Folders: folders}}, wantErr: false},
		{name: "service_user with folders rejected", man: manifest.Manifest{
			ServiceUser: true, Permissions: manifest.Permissions{Folders: folders}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckManifest(&tc.man)
			if tc.wantErr && err == nil {
				t.Fatal("want rejection, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "service_user") {
				t.Errorf("error %q must name service_user", err.Error())
			}
		})
	}
}

// root_setup is folderless only: admission refuses it with every grant that
// puts the app in the host user namespace, and with service_user
// (APP_MANIFEST.md # B). Each refusal names root_setup and the grant.
func TestCheckManifestRootSetup(t *testing.T) {
	folders := []manifest.Folder{{Folder: "documents", Mode: "read"}}
	cases := []struct {
		name     string
		man      manifest.Manifest
		wantName string // "" means accepted
	}{
		{name: "folderless", man: manifest.Manifest{RootSetup: true}},
		{name: "with internet and lan", man: manifest.Manifest{RootSetup: true, Permissions: manifest.Permissions{Internet: true, LAN: true}}},
		{name: "with folders", man: manifest.Manifest{RootSetup: true, Permissions: manifest.Permissions{Folders: folders}}, wantName: "folders"},
		// extra_folders alone makes a folder app (#655).
		{name: "with extra_folders", man: manifest.Manifest{RootSetup: true, Permissions: manifest.Permissions{ExtraFolders: "read"}}, wantName: "folders"},
		{name: "with gpu", man: manifest.Manifest{RootSetup: true, Permissions: manifest.Permissions{GPU: true}}, wantName: "gpu: true"},
		{name: "with devices", man: manifest.Manifest{RootSetup: true, Permissions: manifest.Permissions{Devices: []string{"/dev/ttyUSB0"}}}, wantName: "devices"},
		{name: "with service_user", man: manifest.Manifest{RootSetup: true, ServiceUser: true}, wantName: "service_user"},
		// Breaks both rules: the refusal names root_setup, since removing
		// service_user alone would not fix it.
		{name: "with service_user and folders", man: manifest.Manifest{RootSetup: true, ServiceUser: true, Permissions: manifest.Permissions{Folders: folders}}, wantName: "folders"},
		// Without root_setup the same grants are fine.
		{name: "gpu alone", man: manifest.Manifest{Permissions: manifest.Permissions{GPU: true}}},
		{name: "devices alone", man: manifest.Manifest{Permissions: manifest.Permissions{Devices: []string{"/dev/ttyUSB0"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckManifest(&tc.man)
			if tc.wantName == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want rejection, got nil")
			}
			var ae *Error
			if !errors.As(err, &ae) {
				t.Errorf("want an *admission.Error, got %T", err)
			}
			if !strings.Contains(err.Error(), "root_setup") || !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error %q must name root_setup and %q", err.Error(), tc.wantName)
			}
		})
	}
}

// image_user is folderless only, like root_setup, and it cannot sit with
// service_user or root_setup: each picks the runtime user its own way
// (APP_MANIFEST.md # B). Each refusal names image_user and the other field.
func TestCheckManifestImageUser(t *testing.T) {
	folders := []manifest.Folder{{Folder: "documents", Mode: "read"}}
	cases := []struct {
		name     string
		man      manifest.Manifest
		wantName string // "" means accepted
	}{
		{name: "folderless", man: manifest.Manifest{ImageUser: true}},
		{name: "with internet and lan", man: manifest.Manifest{ImageUser: true, Permissions: manifest.Permissions{Internet: true, LAN: true}}},
		{name: "with folders", man: manifest.Manifest{ImageUser: true, Permissions: manifest.Permissions{Folders: folders}}, wantName: "folders"},
		{name: "with gpu", man: manifest.Manifest{ImageUser: true, Permissions: manifest.Permissions{GPU: true}}, wantName: "gpu: true"},
		{name: "with devices", man: manifest.Manifest{ImageUser: true, Permissions: manifest.Permissions{Devices: []string{"/dev/ttyUSB0"}}}, wantName: "devices"},
		{name: "with service_user", man: manifest.Manifest{ImageUser: true, ServiceUser: true}, wantName: "service_user"},
		{name: "with root_setup", man: manifest.Manifest{ImageUser: true, RootSetup: true}, wantName: "root_setup"},
		{name: "with service_user and folders", man: manifest.Manifest{ImageUser: true, ServiceUser: true, Permissions: manifest.Permissions{Folders: folders}}, wantName: "folders"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckManifest(&tc.man)
			if tc.wantName == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want rejection, got nil")
			}
			var ae *Error
			if !errors.As(err, &ae) {
				t.Errorf("want an *admission.Error, got %T", err)
			}
			if !strings.Contains(err.Error(), "image_user") || !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error %q must name image_user and %q", err.Error(), tc.wantName)
			}
		})
	}
}

// An image_user app's compose may not set user: on any service, by name or
// by number, since that replaces the image's own user. Without image_user the
// rule does not apply.
func TestCheckManifestCompose(t *testing.T) {
	const plain = "services:\n  app:\n    image: example/app:1\n  worker:\n    image: example/worker:1\n"
	const named = "services:\n  app:\n    image: example/app:1\n  worker:\n    image: example/worker:1\n    user: node\n"
	const numeric = "services:\n  app:\n    image: example/app:1\n    user: \"1001\"\n"
	if err := CheckManifestCompose(&manifest.Manifest{ImageUser: true}, []byte(plain)); err != nil {
		t.Fatalf("plain compose refused: %v", err)
	}
	for name, compose := range map[string]string{"named": named, "numeric": numeric} {
		err := CheckManifestCompose(&manifest.Manifest{ImageUser: true}, []byte(compose))
		var ae *Error
		if !errors.As(err, &ae) || !strings.Contains(err.Error(), "image_user") || !strings.Contains(err.Error(), "user:") {
			t.Errorf("%s: err = %v, want an admission error naming image_user and user:", name, err)
		}
	}
	if err := CheckManifestCompose(&manifest.Manifest{}, []byte(named)); err != nil {
		t.Errorf("named user without image_user refused: %v", err)
	}
}

// storage.data_volumes and storage.cache_volumes (APP_MANIFEST.md # C): a cache
// may sit inside a data folder; every other overlap, and every path that
// leaves the app's folder or names the brain's own files, is refused with a
// message that names the field and the entry.
func TestStoragePaths(t *testing.T) {
	cases := []struct {
		name      string
		data      []string
		cache     []string
		wantData  []string
		wantCache []string
		wantSub   []string // the refusal must contain these; nil means accepted
	}{
		{name: "none"},
		{name: "data with a nested cache", data: []string{"./data"}, cache: []string{"./data/cache", "./data/thumbs/"},
			wantData: []string{"data"}, wantCache: []string{"data/cache", "data/thumbs"}},
		{name: "folders next to data", data: []string{"./data", "./config"}, wantData: []string{"data", "config"}},
		{name: "cache with no data", cache: []string{"./data/nginx-cache"}, wantCache: []string{"data/nginx-cache"}},
		{name: "explicit empty lists", data: []string{}, cache: []string{}},
		{name: "absolute", data: []string{"/var/lib/x"}, wantSub: []string{"storage.data_volumes", "/var/lib/x", "absolute"}},
		{name: "variable", data: []string{"${MOOSE_DATA_DIR}/x"}, wantSub: []string{"variable"}},
		{name: "escapes", cache: []string{"./data/../../etc"}, wantSub: []string{"storage.cache_volumes", "leaves the app's folder"}},
		{name: "whole folder", data: []string{"./"}, wantSub: []string{"whole app folder"}},
		{name: "snapshots dir", data: []string{"./snapshots"}, wantSub: []string{"snapshots", "moose keeps"}},
		{name: "env file", data: []string{"./.env"}, wantSub: []string{".env"}},
		{name: "empty entry", data: []string{" "}, wantSub: []string{"is empty"}},
		{name: "same data twice", data: []string{"./data", "data/"}, wantSub: []string{"twice"}},
		{name: "data inside data", data: []string{"./data", "./data/db"}, wantSub: []string{"./data/db", "inside", "./data"}},
		{name: "cache inside cache", cache: []string{"./data/cache", "./data/cache/a"}, wantSub: []string{"storage.cache_volumes", "inside"}},
		{name: "same path in both", data: []string{"./data/x"}, cache: []string{"./data/x"}, wantSub: []string{"both"}},
		{name: "data inside cache", data: []string{"./data/cache/keep"}, cache: []string{"./data/cache"}, wantSub: []string{"inside cache_volumes"}},
		{name: "look-alike prefix is no overlap", data: []string{"./data"}, cache: []string{"./data2"},
			wantData: []string{"data"}, wantCache: []string{"data2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := manifest.Storage{DataVolumes: tc.data, CacheVolumes: tc.cache}
			data, cache, err := StoragePaths(st)
			if tc.wantSub != nil {
				var ae *Error
				if !errors.As(err, &ae) {
					t.Fatalf("want an admission.Error, got %v", err)
				}
				for _, sub := range tc.wantSub {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q must contain %q", err.Error(), sub)
					}
				}
				// CheckManifest reports the same refusal.
				if err := CheckManifest(&manifest.Manifest{Storage: st}); err == nil {
					t.Error("CheckManifest accepted what StoragePaths refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if strings.Join(data, ",") != strings.Join(tc.wantData, ",") || strings.Join(cache, ",") != strings.Join(tc.wantCache, ",") {
				t.Errorf("got data %v cache %v, want %v %v", data, cache, tc.wantData, tc.wantCache)
			}
		})
	}
}
