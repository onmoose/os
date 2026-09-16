package admission

import (
	"context"
	"strings"
	"testing"

	"github.com/onmoose/moose/internal/manifest"
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
