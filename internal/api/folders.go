package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/folderlist"
	"github.com/onmoose/os/internal/hostclient"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/protocol"
)

// FolderEntryDTO is one folder in a folder chooser listing (#658,
// BRAIN_UI_PROTOCOL.md # Folder chooser). Missing marks a use-case folder that
// is not there yet: it is made when an app is pointed at it. MadeByMoose marks
// a top-level folder the brain made for the caller (STORAGE.md # Folders made
// through moose). Both appear only at the top level.
type FolderEntryDTO struct {
	Name        string `json:"name"`
	Missing     bool   `json:"missing,omitempty"`
	MadeByMoose bool   `json:"made_by_moose,omitempty"`
}

// FolderListDTO is GET /api/v1/me/folders.
type FolderListDTO struct {
	Folders   []FolderEntryDTO `json:"folders"`
	Truncated bool             `json:"truncated"`
}

func (s *Server) registerFolderRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-my-folders", Method: "GET", Path: "/api/v1/me/folders",
		Summary: "One level of the caller's own folders, for the folder chooser",
	}, s.listMyFolders)
}

// listMyFolders is the folder chooser's read (#658). source=personal is the
// caller's own home, through host-agent; there is no username parameter, so
// nobody lists another person's home. source=shared is the household shared
// tree, which the brain lists itself. The top level is filtered to the folders
// an app may be pointed at (APP_ISOLATION.md # User content). Not audited: it
// is a read of the caller's own folder names.
func (s *Server) listMyFolders(ctx context.Context, in *struct {
	Source string `query:"source" enum:"personal,shared" default:"personal" doc:"personal (your home) or shared (the household's shared folders)"`
	Path   string `query:"path" doc:"The folder to list, relative to the top, such as Documents/Taxes. Empty lists the top."`
}) (*struct{ Body FolderListDTO }, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	rel := strings.Trim(in.Path, "/")
	if rel != "" && !manifest.ValidSubfolder(rel) {
		return nil, configError("path", "A folder name cannot be empty, start with a dot, or contain a colon or a dollar sign")
	}
	top, _, _ := strings.Cut(rel, "/")

	if in.Source == sourceShared {
		if rel != "" && !slices.Contains(protocol.UseCaseFolderDirs, top) {
			return nil, configError("path", "Choose a folder inside one of the household's shared folders")
		}
		names, truncated, err := folderlist.List(s.life.SharedRoot(), rel)
		if rel == "" && errors.Is(err, folderlist.ErrNotFound) {
			// No shared tree yet (the inner loop, or a box with no shared
			// folder made): every shared use-case folder is missing.
			names, truncated, err = nil, false, nil
		}
		if err != nil {
			return nil, folderListError(err, "shared", rel)
		}
		out := FolderListDTO{Folders: []FolderEntryDTO{}, Truncated: truncated}
		if rel == "" {
			out.Folders = topLevel(names, nil)
			out.Truncated = false
		} else {
			for _, n := range names {
				out.Folders = append(out.Folders, FolderEntryDTO{Name: n})
			}
		}
		return &struct{ Body FolderListDTO }{out}, nil
	}

	made, err := s.madeFolderSet(id.User.ID)
	if err != nil {
		slog.Error("list folders: read the folders made through moose", "user_id", id.User.ID, "err", err)
		return nil, huma.Error500InternalServerError("Could not read your folders. Try again.")
	}
	if rel != "" && !slices.Contains(protocol.UseCaseFolderDirs, top) && !made[top] {
		return nil, configError("path", "Choose a folder inside one of your folders, such as Documents")
	}
	res, err := s.host.ListUserFolders(ctx, id.User.Username, rel)
	if err != nil {
		return nil, folderListError(err, "personal", rel)
	}
	out := FolderListDTO{Folders: []FolderEntryDTO{}, Truncated: res.Truncated}
	if rel == "" {
		out.Folders = topLevel(res.Folders, made)
		// The filtered top has at most the use-case folders and the rows,
		// so it is never cut, even when the home itself was.
		out.Truncated = false
	} else {
		for _, n := range res.Folders {
			out.Folders = append(out.Folders, FolderEntryDTO{Name: n})
		}
	}
	return &struct{ Body FolderListDTO }{out}, nil
}

// topLevel filters a top-level listing to the folders an app may start at: the
// use-case folders, with missing for one that is not there, and the folders
// made through moose that are still there. A row whose folder was deleted or
// renamed is not offered (STORAGE.md # Folders made through moose).
func topLevel(onDisk []string, made map[string]bool) []FolderEntryDTO {
	there := make(map[string]bool, len(onDisk))
	for _, n := range onDisk {
		there[n] = true
	}
	out := []FolderEntryDTO{}
	for _, n := range protocol.UseCaseFolderDirs {
		out = append(out, FolderEntryDTO{Name: n, Missing: !there[n]})
	}
	for n := range made {
		if there[n] && !slices.Contains(protocol.UseCaseFolderDirs, n) {
			out = append(out, FolderEntryDTO{Name: n, MadeByMoose: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// folderListError maps a listing failure to the answer the chooser shows.
func folderListError(err error, source, rel string) error {
	switch {
	case errors.Is(err, hostclient.ErrFolderNotFound), errors.Is(err, folderlist.ErrNotFound):
		return huma.Error404NotFound("This folder is not there any more.")
	case errors.Is(err, hostclient.ErrNotADirectory), errors.Is(err, folderlist.ErrNotADirectory):
		return huma.Error409Conflict("A file or a link is where this folder should be.")
	case errors.Is(err, hostclient.ErrBadPath):
		return configError("path", "Choose a folder such as Documents/Taxes")
	}
	slog.Error("list folders", "source", source, "dir", rel, "err", err)
	return huma.Error502BadGateway("Could not read your folders. Try again.")
}

// madeFolderSet returns the names of the top-level folders the brain made for
// the user through moose (store.MadeFolders), as a set.
func (s *Server) madeFolderSet(userID string) (map[string]bool, error) {
	names, err := s.store.MadeFolders(userID)
	if err != nil {
		return nil, err
	}
	made := make(map[string]bool, len(names))
	for _, n := range names {
		made[n] = true
	}
	return made, nil
}

// wantsNewFolder says whether any personal-source election asks moose to make
// a new top-level folder, the only case a personal install lists the home for.
func wantsNewFolder(elections []FolderElection, extras []ExtraFolderElection) bool {
	for _, e := range elections {
		if e.New && !e.Skip && e.Source != sourceShared {
			return true
		}
	}
	for _, x := range extras {
		if x.New && x.Source != sourceShared {
			return true
		}
	}
	return false
}
