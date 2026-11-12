package api

// Changing an app's folders after install (DASHBOARD.md # Changing folders
// after install, BRAIN_UI_PROTOCOL.md `GET`/`PUT /api/v1/apps/{id}/folders`,
// #659). GET has the app-control gate. PUT has the gate of an update's yes
// (approverOK): pointing a folder somewhere new gives the app someone's files,
// which only the owner of a personal app may allow. PUT is a full replace in
// the install's shape, checked by the same resolveElections, and runs as a
// job that recreates the app. Every PUT is audited as app.folders_change.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

func (s *Server) registerAppFolders(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-app-folders", Method: "GET", Path: "/api/v1/apps/{id}/folders",
		Summary: "Read the folders an app uses and where they point (owner or admin)",
	}, s.getAppFolders)
	huma.Register(api, huma.Operation{
		OperationID: "update-app-folders", Method: "PUT", Path: "/api/v1/apps/{id}/folders",
		Summary: "Change the folders an app uses (job; the owner of a personal app, an admin for a household app)",
		// 202 with the job, or 204 with no body when nothing changes.
		DefaultStatus: http.StatusAccepted,
		Responses: map[string]*huma.Response{
			"204": {Description: "Nothing changed, so no job was started"},
			// Setting Responses stops huma adding its own error response.
			"default": {
				Description: "Error",
				Content: map[string]*huma.MediaType{
					"application/problem+json": {Schema: &huma.Schema{Ref: "#/components/schemas/ErrorModel"}},
				},
			},
		},
	}, s.updateAppFolders)
}

// AppFolderDTO is one declared folder of an installed app: the manifest's
// part, in the install plan's shape so the dashboard draws it with the same
// component, and the current choice. Skipped is true for an optional folder
// the app runs without; Source and Path are empty then.
type AppFolderDTO struct {
	Folder           string        `json:"folder"`
	Mode             string        `json:"mode"`
	Scope            string        `json:"scope"`
	SubfolderDefault string        `json:"subfolder_default,omitempty"`
	Optional         bool          `json:"optional"`
	Sources          FolderSources `json:"sources"`
	Source           string        `json:"source,omitempty"`
	Path             string        `json:"path,omitempty"`
	Skipped          bool          `json:"skipped"`
}

// AppExtraFolderDTO is one extra folder of an installed app. Name is the
// <Name> of /moose/extra/<Name>.
type AppExtraFolderDTO struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Path   string `json:"path"`
}

// AppFoldersDTO is the app page's Folders section. Folders and ExtraFolders
// are empty for an app that uses no folders, and the section is hidden then.
// ExtraFoldersMode is the manifest's extra_folders (read or write), empty when
// the user cannot add folders. CanChange is false for a caller who may see the
// folders but not change them: an admin on another person's personal app.
type AppFoldersDTO struct {
	Scope            string              `json:"scope"`
	CanChange        bool                `json:"can_change"`
	Folders          []AppFolderDTO      `json:"folders"`
	ExtraFoldersMode string              `json:"extra_folders_mode,omitempty"`
	ExtraFolders     []AppExtraFolderDTO `json:"extra_folders"`
}

// AppFoldersUpdateBody is the PUT request: the full set, in the shape of the
// install's config.folders and config.extra_folders. A declared folder with
// no item takes its suggestion, as at install.
type AppFoldersUpdateBody struct {
	Folders      []FolderElection      `json:"folders"`
	ExtraFolders []ExtraFolderElection `json:"extra_folders,omitempty"`
}

// foldersChangeable says whether an app's folders may change at all: it must
// declare folders or extra_folders (an app with none has its runtime tier
// fixed for life, APP_ISOLATION.md # User-namespace tiers), and it must be a
// store app. A Door-2 app's folders are grants with a target the admin typed
// into the compose form, not choices of this kind.
func foldersChangeable(man *manifest.Manifest, inst store.Instance) bool {
	return man.Permissions.FolderApp() && inst.Version != manifest.CustomVersion
}

func (s *Server) getAppFolders(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body AppFoldersDTO }, error) {
	inst, err := s.authorizeAppMutation(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	actor, _ := auth.FromContext(ctx) // authorizeAppMutation already required it
	man, err := s.life.InstanceManifest(in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("load manifest failed", err)
	}
	out := &struct{ Body AppFoldersDTO }{Body: AppFoldersDTO{
		Scope: inst.Scope, Folders: []AppFolderDTO{}, ExtraFolders: []AppExtraFolderDTO{},
	}}
	if !foldersChangeable(man, inst) {
		return out, nil
	}
	current, err := s.life.InstanceFolders(ctx, in.ID)
	if err != nil {
		slog.Error("read app folders", "instance_id", in.ID, "err", err)
		return nil, huma.Error500InternalServerError("Could not read this app's folders. Try again.")
	}
	out.Body.CanChange = approverOK(actor, inst)
	out.Body.ExtraFoldersMode = man.Permissions.ExtraFolders
	plan := buildInstallPlan(man, actor.IsAdmin()).Permissions.Folders
	for _, f := range plan {
		dto := AppFolderDTO{
			Folder: f.Folder, Mode: f.Mode, Scope: f.Scope, SubfolderDefault: f.SubfolderDefault,
			Optional: f.Optional, Sources: f.Sources, Skipped: true,
		}
		for _, mt := range current {
			if mt.Extra == "" && mt.Folder == f.Folder {
				dto.Source, dto.Path, dto.Skipped = mt.Source, mt.Path, false
			}
		}
		out.Body.Folders = append(out.Body.Folders, dto)
	}
	for _, mt := range current {
		if mt.Extra != "" {
			out.Body.ExtraFolders = append(out.Body.ExtraFolders, AppExtraFolderDTO{Name: mt.Extra, Source: mt.Source, Path: mt.Path})
		}
	}
	return out, nil
}

func (s *Server) updateAppFolders(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body AppFoldersUpdateBody
}) (*struct {
	Status int
	Body   *Job
}, error) {
	id := in.ID
	tgt := audit.Target{Kind: "app", ID: id}
	fail := func(meta map[string]any) { s.auditor.Record(ctx, audit.ActionAppFoldersChange, tgt, meta, false) }

	actor, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	inst, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get failed", err)
	}
	// A member asking about another person's personal app gets 404, as for
	// every app control.
	if !canSee(actor, inst) {
		return nil, huma.Error404NotFound("no such app")
	}
	if !approverOK(actor, inst) {
		msg := "Only the app's owner can change its folders."
		if inst.Scope == store.ScopeHousehold {
			msg = "Only an admin can change the folders of a household app."
		}
		fail(map[string]any{"reason": msg})
		return nil, huma.Error403Forbidden(msg)
	}
	man, err := s.life.InstanceManifest(id)
	if err != nil {
		fail(nil)
		return nil, huma.Error500InternalServerError("load manifest failed", err)
	}
	if !foldersChangeable(man, inst) {
		fail(map[string]any{"reason": "no folders"})
		return nil, huma.Error409Conflict("This app does not use folders, so it cannot be given one.")
	}
	switch inst.State {
	case "running", "stopped", "failed":
	default:
		fail(map[string]any{"reason": "busy"})
		return nil, huma.Error409Conflict("This app is busy right now. Try again when it has finished.")
	}
	current, err := s.life.InstanceFolders(ctx, id)
	if err != nil {
		fail(nil)
		slog.Error("change folders: read the current folders", "instance_id", id, "err", err)
		return nil, huma.Error500InternalServerError("Could not read this app's folders. Try again.")
	}

	// The same checks as install. A personal app's owner is the caller here
	// (approverOK). The places the app binds now pass as they are, so a
	// folder chosen under an older rule can stay where it is.
	kept := make(map[string]bool, len(current))
	for _, mt := range current {
		kept[mt.Source+":"+mt.Path] = true
	}
	var made, onDisk map[string]bool
	if inst.Scope == store.ScopePersonal {
		if made, err = s.madeFolderSet(inst.OwnerUserID); err != nil {
			fail(nil)
			slog.Error("change folders: read the folders made through moose", "instance_id", id, "user_id", inst.OwnerUserID, "err", err)
			return nil, huma.Error500InternalServerError("could not read your folders")
		}
		if wantsNewFolder(in.Body.Folders, in.Body.ExtraFolders) {
			res, err := s.host.ListUserFolders(ctx, actor.User.Username, "")
			if err != nil {
				fail(nil)
				slog.Error("change folders: list the top of the home", "instance_id", id, "username", actor.User.Username, "err", err)
				return nil, huma.Error502BadGateway("Could not check your folders. Try again.")
			}
			onDisk = make(map[string]bool, len(res.Folders))
			for _, n := range res.Folders {
				onDisk[n] = true
			}
		}
	}
	// A 422 is a validation answer, not an attempt on the app, so it is not
	// audited (CLAUDE.md # Go code discipline, LOGGING.md app.folders_change).
	mounts, err := resolveElectionsKeeping(man, inst.Scope, in.Body.Folders, in.Body.ExtraFolders, made, onDisk, kept)
	if err != nil {
		return nil, err
	}
	keepExtraNames(current, mounts)
	if lifecycle.SameFolders(current, mounts) {
		return &struct {
			Status int
			Body   *Job
		}{Status: http.StatusNoContent}, nil
	}

	jobCtx := ctx
	job := s.jobs.run("app-folders", func(job *Job) (map[string]any, error) {
		before, changed, err := s.life.SetFolders(context.Background(), id, inst.Version, mounts, job.setStep)
		meta := map[string]any{}
		if before != nil {
			meta["changes"] = folderChanges(before, mounts)
		}
		// A save that turned out to change nothing (another save got there
		// first) writes no record: nothing was asked of the app.
		if err == nil && !changed {
			return map[string]any{"instance_id": id}, nil
		}
		s.auditor.Record(jobCtx, audit.ActionAppFoldersChange, tgt, meta, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	snap := job.snapshot()
	return &struct {
		Status int
		Body   *Job
	}{Status: http.StatusAccepted, Body: &snap}, nil
}

// keepExtraNames gives an extra folder the app already has the
// /moose/extra/<Name> it has now, so removing one folder does not rename
// another under the app. A folder added now takes the last part of its path,
// with " 2", " 3" on a clash, as at install.
func keepExtraNames(current, mounts []lifecycle.FolderMount) {
	used := map[string]bool{}
	kept := make([]bool, len(mounts))
	for i := range mounts {
		mt := &mounts[i]
		if mt.Extra == "" {
			continue
		}
		for _, c := range current {
			if c.Extra != "" && c.Source == mt.Source && c.Path == mt.Path && !used[c.Extra] {
				mt.Extra, kept[i] = c.Extra, true
				used[c.Extra] = true
				break
			}
		}
	}
	for i := range mounts {
		mt := &mounts[i]
		if mt.Extra == "" || kept[i] {
			continue
		}
		base := mt.Path[strings.LastIndex(mt.Path, "/")+1:]
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s %d", base, n)
		}
		mt.Extra = name
		used[name] = true
	}
}

// folderChanges lists, for the audit record, each folder whose place
// changed: {folder, old, new}. A place reads "personal:Notes/Obsidian" or
// "shared:Documents", and "" when the folder is not used. An extra folder is
// named "extra/<Name>".
func folderChanges(before, after []lifecycle.FolderMount) []map[string]string {
	place := func(mts []lifecycle.FolderMount) map[string]string {
		out := map[string]string{}
		for _, mt := range mts {
			key := mt.Folder
			if mt.Extra != "" {
				key = "extra/" + mt.Extra
			}
			out[key] = mt.Source + ":" + mt.Path
		}
		return out
	}
	b, a := place(before), place(after)
	keys := make([]string, 0, len(a)+len(b))
	for k := range b {
		keys = append(keys, k)
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := []map[string]string{}
	for _, k := range keys {
		if b[k] != a[k] {
			out = append(out, map[string]string{"folder": k, "old": b[k], "new": a[k]})
		}
	}
	return out
}
