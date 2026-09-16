package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/catalog"
	"github.com/onmoose/moose/internal/lifecycle"
	"github.com/onmoose/moose/internal/manifest"
	"github.com/onmoose/moose/internal/store"
)

// source value constants for folder source menus.
const (
	sourceShared   = "shared"
	sourcePersonal = "personal"
)

// InstallPlanDTO is the response body for GET /api/v1/catalog/{id}/install-plan.
// It carries everything the consent screen needs: declared permissions, the
// role-derived scope options, and per-folder source menus. It is advisory —
// the authoritative validation and override stamping happen in slice 4 when
// POST /api/v1/apps receives the user's elections in its config.
type InstallPlanDTO struct {
	ManifestID   string                 `json:"manifest_id"`
	Name         string                 `json:"name"`
	Version      string                 `json:"version"`
	ScopeOptions []string               `json:"scope_options"`
	ScopeDefault string                 `json:"scope_default"`
	Footprint    InstallPlanFootprint   `json:"footprint"`
	Permissions  InstallPlanPermissions `json:"permissions"`
	Mail         *InstallPlanMail       `json:"mail,omitempty"`
	// Config is the user-supplied configuration form schema (APP_MANIFEST.md # D4),
	// present only when the manifest declares a config: block. Schema only — never
	// a value (a secret field has none; defaults/options are part of the schema).
	Config []InstallPlanConfigField `json:"config,omitempty"`
}

// InstallPlanConfigField is one user-supplied config field's form schema
// (APP_MANIFEST.md # D4). It carries everything the install form needs to render
// the input — never a value.
type InstallPlanConfigField struct {
	AppEnv      string   `json:"app_env"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Secret      bool     `json:"secret"`
	Required    bool     `json:"required"`
	Type        string   `json:"type"`
	Options     []string `json:"options,omitempty"`
	Default     string   `json:"default,omitempty"`
}

// InstallPlanMail is the outgoing-mail picker block, present only when the
// manifest declares mail support (SERVICE_PROVISIONING.md # BYO outgoing
// mail). Providers carry id, label and preset only. The full provider settings
// (host, username) stay on the admin-only CRUD surface; any installer just
// picks a name. Empty Providers ⇒ the UI renders the picker with only "None".
type InstallPlanMail struct {
	Optional  bool                 `json:"optional"`
	Providers []MailProviderOption `json:"providers"`
}

// MailProviderOption is one picker entry: the binding handle, its label, and
// the preset it was created from. ProviderType is the brand behind the account
// ("postmark", "custom"), which is what lets a picker draw the provider logo
// next to the account name. It carries no credential and no host, so it is
// safe on the non-admin surfaces this type serves.
type MailProviderOption struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	ProviderType string `json:"provider_type"`
}

// InstallPlanFootprint is the box-specific on-disk estimate the install dialog
// shows (BRAIN_UI_PROTOCOL.md # install-plan). Sharper than the catalog grid's
// coarse footprint: image bytes already pulled on THIS box are subtracted, and
// FreeBytes is the live data-drive reading. The brain returns raw bytes; the UI
// owns all wording and unit rounding. EstimatedStateBytes is a pointer so it is
// omitted entirely when the manifest declares no estimated_size, rather than
// rendering a misleading 0.
type InstallPlanFootprint struct {
	DownloadBytes       int64  `json:"download_bytes"`
	ImageDiskBytes      int64  `json:"image_disk_bytes"`
	EstimatedStateBytes *int64 `json:"estimated_state_bytes,omitempty"`
	FreeBytes           int64  `json:"free_bytes"`
}

// toInstallPlanFootprint maps the lifecycle estimate to the wire shape, setting
// EstimatedStateBytes only when the manifest actually declared one (HasEstimate)
// so the field is omitted otherwise.
func toInstallPlanFootprint(fp lifecycle.InstallFootprint) InstallPlanFootprint {
	out := InstallPlanFootprint{
		DownloadBytes:  fp.DownloadBytes,
		ImageDiskBytes: fp.ImageDiskBytes,
		FreeBytes:      fp.FreeBytes,
	}
	if fp.HasEstimate {
		n := fp.EstimatedStateBytes
		out.EstimatedStateBytes = &n
	}
	return out
}

// InstallPlanPermissions mirrors manifest.Permissions for the install-plan
// wire shape, with Folders mapped to the richer InstallPlanFolder type.
type InstallPlanPermissions struct {
	Internet bool                `json:"internet"`
	LAN      bool                `json:"lan"`
	GPU      bool                `json:"gpu"`
	Devices  []string            `json:"devices"`
	Folders  []InstallPlanFolder `json:"folders"`
}

// InstallPlanFolder is one declared folder with its per-scope source menus.
type InstallPlanFolder struct {
	Folder           string        `json:"folder"`
	Mode             string        `json:"mode"`
	Scope            string        `json:"scope"`
	SubfolderDefault string        `json:"subfolder_default,omitempty"`
	Sources          FolderSources `json:"sources"`
}

// FolderSources holds one SourceMenu per install scope. Both are always
// populated regardless of the caller's role — the household menu is present
// even for members (it's unreachable since household scope isn't offered, but
// keeping the shape uniform simplifies the UI).
type FolderSources struct {
	Household SourceMenu `json:"household"`
	Personal  SourceMenu `json:"personal"`
}

// SourceMenu is the set of source choices for a folder under one scope.
// A single-option menu renders as fixed/disabled in the UI.
type SourceMenu struct {
	Options []string `json:"options"`
	Default string   `json:"default"`
}

// scopeMenu returns the scope options and default for the caller's role.
// admin → (["household","personal"], "household")
// member → (["personal"], "personal")
// This is the single source of truth for the scope authorization table
// (DASHBOARD.md # install authorization). resolveOwnerScope enforces the same
// table on the write path — keep the two in sync.
func scopeMenu(isAdmin bool) (options []string, def string) {
	if isAdmin {
		return []string{store.ScopeHousehold, store.ScopePersonal}, store.ScopeHousehold
	}
	return []string{store.ScopePersonal}, store.ScopePersonal
}

// folderSourceMenu returns the allowed source options and default for a folder
// under the given install scope. Household forces shared (the household share is
// the single /srv/moose/shared/<Folder>/ path); personal offers personal
// (~/<Folder>/, default) or shared. This is the single source of truth for both
// the advisory install-plan menus (buildInstallPlan) and the authoritative
// write-path validation (resolveElections) — keep them sharing it.
func folderSourceMenu(scope string) (options []string, def string) {
	if scope == store.ScopeHousehold {
		return []string{sourceShared}, sourceShared
	}
	return []string{sourcePersonal, sourceShared}, sourcePersonal
}

// buildInstallPlan maps a parsed manifest and the caller's role into an
// InstallPlanDTO. Pure function — no I/O, trivially unit-testable.
func buildInstallPlan(man *manifest.Manifest, isAdmin bool) InstallPlanDTO {
	opts, def := scopeMenu(isAdmin)

	folders := make([]InstallPlanFolder, 0, len(man.Permissions.Folders))
	for _, f := range man.Permissions.Folders {
		// Per-scope source menus, from the same helper resolveElections uses on
		// the write path so the advisory plan and the authoritative validation
		// never drift. Household forces shared; personal offers personal (default)
		// or shared.
		ho, hd := folderSourceMenu(store.ScopeHousehold)
		po, pd := folderSourceMenu(store.ScopePersonal)
		householdMenu := SourceMenu{Options: ho, Default: hd}
		personalMenu := SourceMenu{Options: po, Default: pd}
		folders = append(folders, InstallPlanFolder{
			Folder:           f.Folder,
			Mode:             f.Mode,
			Scope:            f.Scope,
			SubfolderDefault: f.Default,
			Sources: FolderSources{
				Household: householdMenu,
				Personal:  personalMenu,
			},
		})
	}

	devices := man.Permissions.Devices
	if devices == nil {
		devices = []string{}
	}

	config := make([]InstallPlanConfigField, 0, len(man.Config))
	for _, c := range man.Config {
		config = append(config, InstallPlanConfigField{
			AppEnv:      c.AppEnv,
			Title:       c.Title,
			Description: c.Description,
			Secret:      c.Secret,
			Required:    c.Required,
			Type:        c.Type,
			Options:     c.Options,
			Default:     c.Default,
		})
	}

	return InstallPlanDTO{
		ManifestID:   man.ID,
		Name:         man.Name,
		Version:      man.Version,
		ScopeOptions: opts,
		ScopeDefault: def,
		Permissions: InstallPlanPermissions{
			Internet: man.Permissions.Internet,
			LAN:      man.Permissions.LAN,
			GPU:      man.Permissions.GPU,
			Devices:  devices,
			Folders:  folders,
		},
		Config: config,
	}
}

// FolderElection is one per-folder install-time choice from the consent screen
// (BRAIN_UI_PROTOCOL.md Pattern B config). Source is personal|shared; Subfolder
// narrows a pick-subfolder folder. Both optional — an omitted election defaults
// to the folder's menu default.
type FolderElection struct {
	Folder    string `json:"folder"`
	Source    string `json:"source,omitempty"`
	Subfolder string `json:"subfolder,omitempty"`
}

// resolveElections is the authoritative validation of the user's per-folder
// elections against the manifest's declared folders and the resolved install
// scope. The install-plan endpoint is advisory; this is the gate. It returns one
// fully-resolved lifecycle.FolderMount per declared folder (every declared
// folder is bound — an omitted election just takes the menu default), or a 422
// huma error the caller surfaces and audits. Rejects: an election for a folder
// the app never declared, a duplicate election, a source not allowed for the
// scope, and a subfolder on a non-pick-subfolder folder or one that escapes it.
func resolveElections(man *manifest.Manifest, scope string, elections []FolderElection) ([]lifecycle.FolderMount, error) {
	declared := make(map[string]bool, len(man.Permissions.Folders))
	for _, f := range man.Permissions.Folders {
		declared[f.Folder] = true
	}
	byFolder := make(map[string]FolderElection, len(elections))
	for _, e := range elections {
		if !declared[e.Folder] {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.folders: %q is not a folder this app requested", e.Folder))
		}
		if _, dup := byFolder[e.Folder]; dup {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.folders: duplicate election for %q", e.Folder))
		}
		byFolder[e.Folder] = e
	}

	mounts := make([]lifecycle.FolderMount, 0, len(man.Permissions.Folders))
	for _, f := range man.Permissions.Folders {
		options, src := folderSourceMenu(scope) // src starts at the menu default
		sub := f.Default                        // "" unless the manifest declared a pick-subfolder default
		if e, ok := byFolder[f.Folder]; ok {
			if e.Source != "" {
				if !slices.Contains(options, e.Source) {
					return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.folders[%s]: source %q is not allowed for a %s install (allowed: %s)", f.Folder, e.Source, scope, strings.Join(options, ", ")))
				}
				src = e.Source
			}
			if e.Subfolder != "" {
				if f.Scope != "pick-subfolder" {
					return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.folders[%s]: a subfolder may only be chosen when the app declares scope: pick-subfolder", f.Folder))
				}
				if strings.HasPrefix(e.Subfolder, "/") || strings.Contains(e.Subfolder, "..") {
					return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("config.folders[%s]: subfolder must be a relative path under the folder", f.Folder))
				}
				sub = e.Subfolder
			}
		}
		mounts = append(mounts, lifecycle.FolderMount{Folder: f.Folder, Source: src, Subfolder: sub})
	}
	return mounts, nil
}

func (s *Server) installPlan(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body InstallPlanDTO }, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	man, _, err := s.catalog.Load(ctx, in.ID)
	if errors.Is(err, catalog.ErrNotFound) {
		// Normal client outcome (stale id, typo) — 404 without logging, like getApp.
		return nil, huma.Error404NotFound("no such catalog app")
	}
	if err != nil {
		// The entry exists but won't parse or is missing its compose file — a
		// catalog integrity problem, not a client error. Surface as 500 and log
		// loudly so the operator sees the broken entry.
		slog.Error("install-plan: catalog entry failed to load", "manifest_id", in.ID, "err", err)
		return nil, huma.Error500InternalServerError("catalog entry is malformed")
	}
	// Unlisted app (`listed: false`) is pulled from the store — no install plan,
	// same 404 as a missing manifest. Mirrors the gate in installApp.
	if !man.IsListed() {
		return nil, huma.Error404NotFound("no such catalog app")
	}
	// buildInstallPlan stays pure (permissions + scope menus); the box-specific
	// footprint is attached here because it queries Docker + the host (read-only,
	// advisory — see InstallFootprint). A footprint sub-failure degrades to zeros
	// inside InstallFootprint rather than failing the plan.
	plan := buildInstallPlan(man, id.IsAdmin())
	plan.Footprint = toInstallPlanFootprint(s.life.InstallFootprint(ctx, man))
	// The mail picker menu is attached here (not in pure buildInstallPlan) for
	// the same reason as the footprint: it reads box state. Advisory like the
	// folder menus — installApp re-validates the election authoritatively.
	if man.Mail != nil {
		providers, err := s.store.ListMailProviders()
		if err != nil {
			return nil, huma.Error500InternalServerError("list mail providers failed", err)
		}
		mail := &InstallPlanMail{Optional: man.Mail.Optional, Providers: []MailProviderOption{}}
		for _, p := range providers {
			mail.Providers = append(mail.Providers, MailProviderOption{ID: p.ID, Label: p.Label, ProviderType: p.ProviderType})
		}
		plan.Mail = mail
	}
	return &struct{ Body InstallPlanDTO }{Body: plan}, nil
}
