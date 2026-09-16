package api

// Door-2 (custom-container) permission authoring (DASHBOARD.md # Permissions, #
// Form is a projection of the synthetic manifest). The install form sends a
// structured permission election; the "Edit as YAML" toggle sends a raw overlay
// instead. The brain owns all YAML rendering/parsing so the frontend ships no
// YAML dependency: render projects the form to overlay text, parse validates
// overlay text back to form fields, and both feed the same Permissions through
// the same gate the form path uses.

import (
	"context"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/moose/internal/manifest"
)

// customFolderDTO is one Door-2 folder grant on the wire: a use-case folder
// (Source picker), an in-container destination the admin typed (Target), and the
// read/write mode. Mirrors manifest.Folder minus the store-only scope/default.
type customFolderDTO struct {
	Folder string `json:"folder"`
	Mode   string `json:"mode,omitempty"`   // read|write (default read)
	Target string `json:"target,omitempty"` // in-container destination path
}

// customPermsInput is the structured Door-2 permission election (form mode), and
// the body the Edit-as-YAML render endpoint marshals.
type customPermsInput struct {
	Internet *bool             `json:"internet,omitempty"` // nil ⇒ default on (DASHBOARD.md # Permissions)
	LAN      bool              `json:"lan,omitempty"`
	GPU      bool              `json:"gpu,omitempty"`
	Folders  []customFolderDTO `json:"folders,omitempty"`
	Devices  []string          `json:"devices,omitempty"`
}

func (in customPermsInput) toPermissions() manifest.Permissions {
	p := manifest.Permissions{
		Internet: in.Internet == nil || *in.Internet,
		LAN:      in.LAN,
		GPU:      in.GPU,
		Devices:  in.Devices,
	}
	for _, f := range in.Folders {
		p.Folders = append(p.Folders, manifest.Folder{Folder: f.Folder, Mode: f.Mode, Target: f.Target})
	}
	return p
}

// customPermsOutput projects a resolved Permissions back to the wire — the parse
// result the form repopulates from (including any `devices` an admin added only
// in YAML). Internet is concrete here, since a parsed overlay always resolves it.
type customPermsOutput struct {
	Internet bool              `json:"internet"`
	LAN      bool              `json:"lan"`
	GPU      bool              `json:"gpu"`
	Folders  []customFolderDTO `json:"folders"`
	Devices  []string          `json:"devices"`
}

func permsToOutput(p manifest.Permissions) customPermsOutput {
	out := customPermsOutput{Internet: p.Internet, LAN: p.LAN, GPU: p.GPU, Devices: p.Devices}
	for _, f := range p.Folders {
		out.Folders = append(out.Folders, customFolderDTO{Folder: f.Folder, Mode: f.Mode, Target: f.Target})
	}
	return out
}

// resolveCustomPerms produces the elected permission set from a Door-2 install
// request: the raw Edit-as-YAML overlay when present (parsed + validated through
// the same gate the form uses), else the structured form fields. An absent
// permissions object defaults to internet-on, matching the form's default.
func resolveCustomPerms(in *customPermsInput, overlay string) (manifest.Permissions, error) {
	if strings.TrimSpace(overlay) != "" {
		return manifest.ParsePermissionsOverlay([]byte(overlay))
	}
	if in == nil {
		return manifest.Permissions{Internet: true}, nil
	}
	return in.toPermissions(), nil
}

// renderCustomOverlay marshals an elected permission set to the YAML the
// Edit-as-YAML editor shows when the admin flips out of the form. Admin-only,
// like the rest of Door 2.
func (s *Server) renderCustomOverlay(ctx context.Context, in *struct {
	Body struct {
		Permissions customPermsInput `json:"permissions"`
	}
}) (*struct {
	Body struct {
		Overlay string `json:"overlay"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	y, err := manifest.RenderPermissionsOverlay(in.Body.Permissions.toPermissions())
	if err != nil {
		return nil, huma.Error500InternalServerError("render overlay", err)
	}
	out := &struct {
		Body struct {
			Overlay string `json:"overlay"`
		}
	}{}
	out.Body.Overlay = string(y)
	return out, nil
}

// parseCustomOverlay validates an admin-edited overlay and projects it back to
// form fields, so flipping out of YAML repopulates the form (and surfaces a bad
// folder target / unknown key as a 422 instead of swallowing it). Admin-only.
func (s *Server) parseCustomOverlay(ctx context.Context, in *struct {
	Body struct {
		Overlay string `json:"overlay"`
	}
}) (*struct {
	Body struct {
		Permissions customPermsOutput `json:"permissions"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	perms, err := manifest.ParsePermissionsOverlay([]byte(in.Body.Overlay))
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	out := &struct {
		Body struct {
			Permissions customPermsOutput `json:"permissions"`
		}
	}{}
	out.Body.Permissions = permsToOutput(perms)
	return out, nil
}
