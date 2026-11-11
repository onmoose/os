package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/fit"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
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
	// Requires lists the "at least one of" groups over Config (INSTALL_SETUP.md
	// # 3). POST /api/v1/apps answers 422 while a group has no filled member.
	// Omitted when the manifest declares none the box can use.
	Requires []RequiresGroupDTO `json:"requires,omitempty"`
	// Existing lists the copies of this app the caller can already see: the
	// same ones that make POST /api/v1/apps answer 409 duplicate-install. The
	// setup flow warns on its first page, before any question
	// (INSTALL_STEPS.md # Build rules). Empty when there are none.
	Existing []InstallPlanExisting `json:"existing"`
	// Unavailable is set when this box cannot install the app at all. It is a
	// reason code, not copy: the UI owns the sentence, like every other
	// wording on this plan. The detail page shows that sentence in place of a
	// working Install button. The codes are unavailableNeedsRemap and
	// unavailableTooBig. Advisory like the rest of the plan: POST /api/v1/apps
	// refuses such an install on its own.
	Unavailable string `json:"unavailable,omitempty" enum:"needs-remap,too-big"`
	// Fit says whether the app fits this box's memory and CPU (#599), from the
	// manifest's resources.recommended. The same figures as the app's store
	// page. A tight app needs an explicit "Install anyway" in the dialog; a
	// too-big one also sets Unavailable. Absent when the box cannot measure
	// itself.
	Fit *fit.AppFit `json:"fit,omitempty"`
}

// unavailableNeedsRemap: the app needs the userns-remap (root_setup or
// image_user) and this box runs none (APP_STORE.md # Apps this box cannot
// run).
const unavailableNeedsRemap = "needs-remap"

// unavailableTooBig: the app recommends more memory than the box has, or more
// CPU cores (APP_ISOLATION.md # Resource limits).
const unavailableTooBig = "too-big"

// InstallPlanExisting is one copy of the app that is already installed.
// Mine is true when the caller owns it; an admin also sees other users'
// personal copies.
type InstallPlanExisting struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	Mine       bool   `json:"mine"`
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
	// Role says what the field means, e.g. ai.anthropic.api_key
	// (INSTALL_SETUP.md # 1). Present only when the box can fill the field;
	// otherwise the field is a plain field.
	Role string `json:"role,omitempty"`
	// Separator joins a models.<type> list: the declared one or ",". Present
	// only on a fillable models field.
	Separator string `json:"separator,omitempty"`
}

// InstallPlanMail is the outgoing-mail picker block, present only when the
// manifest declares mail support (SERVICE_PROVISIONING.md # BYO outgoing
// mail). Providers are the caller's own accounts, with id, label and preset
// only; the full settings (host, username) stay on the CRUD surface. Empty
// Providers means the UI renders the picker with only "None" and the add
// card.
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
	// CreatedAt is when the account was added, in Unix seconds. The setup
	// flow picks the newest account in advance.
	CreatedAt int64 `json:"created_at"`
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
	// ExtraFolders is "read" or "write" when the user may add more folders
	// (APP_MANIFEST.md # folders, #655); omitted when the app allows none.
	ExtraFolders string `json:"extra_folders,omitempty"`
}

// InstallPlanFolder is one declared folder with its per-scope source menus.
// Scope is kept for a dashboard from before #655 and limits nothing: the user
// may point any folder at any path inside the folder limit. The suggestion is
// the folder plus SubfolderDefault.
type InstallPlanFolder struct {
	Folder           string        `json:"folder"`
	Mode             string        `json:"mode"`
	Scope            string        `json:"scope"`
	SubfolderDefault string        `json:"subfolder_default,omitempty"`
	Optional         bool          `json:"optional"` // the user may skip the folder (#655)
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
			Optional:         f.Optional,
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
	roles := man.FillableRoles()
	for _, c := range man.Config {
		role, sep := fieldRole(roles, &c)
		config = append(config, InstallPlanConfigField{
			AppEnv:      c.AppEnv,
			Title:       c.Title,
			Description: c.Description,
			Secret:      c.Secret,
			Required:    c.Required,
			Type:        c.Type,
			Options:     c.Options,
			Default:     c.Default,
			Role:        role,
			Separator:   sep,
		})
	}

	return InstallPlanDTO{
		ManifestID:   man.ID,
		Name:         man.Name,
		Version:      man.Version,
		ScopeOptions: opts,
		ScopeDefault: def,
		Permissions: InstallPlanPermissions{
			Internet:     man.Permissions.Internet,
			LAN:          man.Permissions.LAN,
			GPU:          man.Permissions.GPU,
			Devices:      devices,
			Folders:      folders,
			ExtraFolders: man.Permissions.ExtraFolders,
		},
		Config:   config,
		Requires: requiresDTO(man),
	}
}

// FolderElection is one per-folder install-time choice from the consent screen
// (BRAIN_UI_PROTOCOL.md # Folder elections after #655). Source is
// personal|shared. Path is where the folder points, relative to the top of the
// home (personal) or of the shared tree (shared), such as "Notes/Obsidian".
// New asks the brain to make a top-level folder that is not there yet. Skip
// leaves out an optional folder. Subfolder is the form from before #655 and
// means <Folder>/<Subfolder>; it may not come with Path. Every field but Folder
// is optional: an omitted field takes the menu default and the suggestion.
type FolderElection struct {
	Folder    string `json:"folder"`
	Source    string `json:"source,omitempty"`
	Path      string `json:"path,omitempty"`
	New       bool   `json:"new,omitempty"`
	Skip      bool   `json:"skip,omitempty"`
	Subfolder string `json:"subfolder,omitempty"`
}

// ExtraFolderElection is one extra folder the user added, for an app whose
// manifest declares extra_folders (APP_MANIFEST.md # folders). The brain picks
// its /moose/extra/<Name> name.
type ExtraFolderElection struct {
	Source string `json:"source,omitempty"`
	Path   string `json:"path"`
	New    bool   `json:"new,omitempty"`
}

// newFolderTaken are the top-level names a new folder may not have besides a
// use-case folder's: ~/Shared is moose's link to the shared tree (STORAGE.md
// # What apps and users actually see).
var newFolderTaken = map[string]bool{"Shared": true}

// checkFolderPath checks one chosen path against the folder limit
// (APP_ISOLATION.md # User content). It returns whether the first level is a
// new folder the brain must record, or the plain sentence that says why the
// path is refused.
//
// Every level must be a plain name: not empty, not starting with a dot, and
// with no colon (manifest.ValidFolderLevel). The first level must be a
// use-case folder or, for a personal source, a folder made through moose
// (made) or one the user asks moose to make now (isNew). A new folder may not
// be named bin, start with a dot, take a name moose uses, or take a name that
// is already in the home (onDisk, the top of the owner's home as host-agent
// listed it, #658): a folder made by hand is not one moose made. The
// first-level rule is the brain's alone: host-agent cannot see the rows.
func checkFolderPath(source, path string, isNew bool, made, onDisk map[string]bool) (newTop bool, msg string) {
	if !manifest.ValidSubfolder(path) {
		return false, "choose a folder such as Documents/Taxes. A folder name cannot be empty, start with a dot, or contain a colon or a dollar sign"
	}
	top, _, _ := strings.Cut(path, "/")
	switch {
	case slices.Contains(protocol.UseCaseFolderDirs, top):
		return false, ""
	case source == sourceShared:
		return false, "choose a folder inside one of the household's shared folders, such as Documents"
	case made[top]:
		return false, ""
	case !isNew:
		return false, "choose a folder inside one of your folders, such as Documents, or make a new folder"
	case top == "bin" || newFolderTaken[top]:
		return false, fmt.Sprintf("a new folder cannot be named %q. Choose another name", top)
	case onDisk[top]:
		return false, fmt.Sprintf("a folder named %q is already in your home, and moose can only use a new folder it made itself. Choose another name", top)
	}
	return true, ""
}

// resolveElections is the authoritative validation of the user's folder
// elections against the manifest's declared folders and extra_folders and the
// resolved install scope (#655). The install-plan endpoint is advisory; this
// is the gate. made is the names of the folders the brain made for the owner
// through moose (store.MadeFolders); it matters only for a personal source.
// onDisk is the top level of the owner's home, listed only when an election
// asks for a new folder (#658); nil otherwise.
//
// It returns one lifecycle.FolderMount per declared folder the user did not
// skip, in manifest order, then one per extra folder, or a 422 huma error the
// caller surfaces and audits. An omitted election takes the menu default
// source and the suggestion (lifecycle.SuggestedFolderPath). Rejects: an
// election for a folder the app never declared, a duplicate election, a
// source not allowed for the scope, a skip of a required folder, a path and a
// subfolder together, a path outside the folder limit, an extra folder for an
// app that declares no extra_folders, and the same extra folder twice.
func resolveElections(man *manifest.Manifest, scope string, elections []FolderElection, extras []ExtraFolderElection, made, onDisk map[string]bool) ([]lifecycle.FolderMount, error) {
	declared := make(map[string]bool, len(man.Permissions.Folders))
	for _, f := range man.Permissions.Folders {
		declared[f.Folder] = true
	}
	byFolder := make(map[string]FolderElection, len(elections))
	for _, e := range elections {
		if !declared[e.Folder] {
			return nil, configError("config.folders."+e.Folder, fmt.Sprintf("config.folders: %q is not a folder this app requested", e.Folder))
		}
		if _, dup := byFolder[e.Folder]; dup {
			return nil, configError("config.folders."+e.Folder, fmt.Sprintf("config.folders: duplicate election for %q", e.Folder))
		}
		byFolder[e.Folder] = e
	}
	options, defSource := folderSourceMenu(scope)
	source := func(location, label, elected string) (string, error) {
		if elected == "" {
			return defSource, nil
		}
		if !slices.Contains(options, elected) {
			return "", configError(location, fmt.Sprintf("%s: source %q is not allowed for a %s install (allowed: %s)", label, elected, scope, strings.Join(options, ", ")))
		}
		return elected, nil
	}

	mounts := make([]lifecycle.FolderMount, 0, len(man.Permissions.Folders)+len(extras))
	for _, f := range man.Permissions.Folders {
		loc, label := "config.folders."+f.Folder, "config.folders["+f.Folder+"]"
		e := byFolder[f.Folder]
		if e.Skip {
			if !f.Optional {
				return nil, configError(loc, fmt.Sprintf("This app needs the %s folder to run. Choose a folder for it", lifecycle.FolderDir(f.Folder)))
			}
			continue
		}
		src, err := source(loc, label, e.Source)
		if err != nil {
			return nil, err
		}
		path := lifecycle.SuggestedFolderPath(f)
		switch {
		case e.Path != "" && e.Subfolder != "":
			return nil, configError(loc, label+": send path or subfolder, not both")
		case e.Path != "":
			path = e.Path
		case e.Subfolder != "":
			// The form from before #655: a subfolder under the folder.
			if !manifest.ValidSubfolder(e.Subfolder) {
				return nil, configError(loc, label+": subfolder must be a relative path under the folder")
			}
			path = lifecycle.FolderDir(f.Folder) + "/" + e.Subfolder
		}
		newTop, msg := checkFolderPath(src, path, e.New, made, onDisk)
		if msg != "" {
			return nil, configError(loc, fmt.Sprintf("For the %s folder, %s.", lifecycle.FolderDir(f.Folder), msg))
		}
		mounts = append(mounts, lifecycle.FolderMount{Folder: f.Folder, Source: src, Path: path, NewFolder: newTop})
	}

	if len(extras) > 0 && man.Permissions.ExtraFolders == "" {
		return nil, configError("config.extra_folders[0]", "This app does not let you add more folders")
	}
	used := map[string]bool{}
	seen := map[string]bool{}
	for i, x := range extras {
		loc := fmt.Sprintf("config.extra_folders[%d]", i)
		src, err := source(loc, loc, x.Source)
		if err != nil {
			return nil, err
		}
		if x.Path == "" {
			return nil, configError(loc, "Choose a folder to add")
		}
		newTop, msg := checkFolderPath(src, x.Path, x.New, made, onDisk)
		if msg != "" {
			return nil, configError(loc, fmt.Sprintf("For the added folder %q, %s.", x.Path, msg))
		}
		if seen[src+":"+x.Path] {
			return nil, configError(loc, "This folder is already added")
		}
		seen[src+":"+x.Path] = true
		// /moose/extra/<Name>, where <Name> is the last part of the path. A
		// clash takes " 2", then " 3" (APP_MANIFEST.md # folders).
		base := x.Path[strings.LastIndex(x.Path, "/")+1:]
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s %d", base, n)
		}
		used[name] = true
		mounts = append(mounts, lifecycle.FolderMount{Source: src, Path: x.Path, Extra: name, NewFolder: newTop})
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
	// the same reason as the footprint: it reads box state. It lists only the
	// caller's own accounts, the only ones installApp will accept. Advisory
	// like the folder menus: installApp re-validates the election
	// authoritatively.
	if man.Mail != nil {
		providers, err := s.store.ListMailProviders(id.User.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("list mail providers failed", err)
		}
		mail := &InstallPlanMail{Optional: man.Mail.Optional, Providers: []MailProviderOption{}}
		for _, p := range providers {
			mail.Providers = append(mail.Providers, MailProviderOption{ID: p.ID, Label: p.Label, ProviderType: p.ProviderType, CreatedAt: p.CreatedAt.Unix()})
		}
		plan.Mail = mail
	}
	existing, err := s.visibleCopies(id, man.ID)
	if err != nil {
		return nil, err
	}
	plan.Existing = existing
	if box, ok := s.life.FitBox(ctx); ok {
		res := s.fitForCaller(ctx)(lifecycle.CheckFit(box, man.ID, man.Resources.Recommended))
		plan.Fit = &res
		if res.State == fit.TooBig {
			plan.Unavailable = unavailableTooBig
		}
	}
	// The same rule as the store lists (storeCatalog), read from the manifest
	// itself so it holds even when the browse record does not say it. Only a
	// known "no remap" sets it; an unknown state leaves Install to the
	// install path, which refuses on its own.
	// It wins over too-big: the app cannot run here at any size.
	if (man.RootSetup || man.ImageUser) && s.life.RemapState(ctx) == lifecycle.RemapOff {
		plan.Unavailable = unavailableNeedsRemap
	}
	return &struct{ Body InstallPlanDTO }{Body: plan}, nil
}

// visibleCopies lists the installed copies of an app that the caller can see
// (canSee). It is the one rule for both the install plan's warning and the
// 409 of checkDuplicate, so the plan warns about exactly the copies an install
// would stop on.
func (s *Server) visibleCopies(id auth.Identity, manifestID string) ([]InstallPlanExisting, error) {
	instances, err := s.store.InstancesByManifest(manifestID)
	if err != nil {
		return nil, huma.Error500InternalServerError("duplicate check failed", err)
	}
	out := []InstallPlanExisting{}
	for _, i := range instances {
		if !canSee(id, i) {
			continue
		}
		out = append(out, InstallPlanExisting{
			InstanceID: i.ID,
			Name:       i.Name,
			Scope:      i.Scope,
			Mine:       i.OwnerUserID == id.User.ID,
		})
	}
	return out, nil
}
