package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/store"
)

func (s *Server) registerMeRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "change-my-password", Method: "POST", Path: "/api/v1/me/password",
		Summary: "Self-service password change (any authenticated user)", DefaultStatus: 204,
	}, s.changeMyPassword)
}

func (s *Server) registerUsers(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-users", Method: "GET", Path: "/api/v1/users",
		Summary: "List all dashboard users (admin only)",
	}, s.listUsers)

	huma.Register(api, huma.Operation{
		OperationID: "create-user", Method: "POST", Path: "/api/v1/users",
		Summary: "Create a new user (admin only)",
	}, s.createUser)

	huma.Register(api, huma.Operation{
		OperationID: "update-user-role", Method: "PATCH", Path: "/api/v1/users/{id}",
		Summary: "Change a user's role (admin only)",
	}, s.updateUserRole)

	huma.Register(api, huma.Operation{
		OperationID: "delete-user", Method: "DELETE", Path: "/api/v1/users/{id}",
		Summary: "Delete a user (admin only)", DefaultStatus: 204,
	}, s.deleteUser)

	huma.Register(api, huma.Operation{
		OperationID: "reset-user-password", Method: "POST", Path: "/api/v1/users/{id}/password",
		Summary: "Admin-set password reset (admin only)", DefaultStatus: 204,
	}, s.resetUserPassword)

	huma.Register(api, huma.Operation{
		OperationID: "rename-user", Method: "POST", Path: "/api/v1/users/{id}/name",
		Summary: "Change a user's display name (self, or admin for anyone)",
	}, s.renameUser)
}

// validateUsername enforces the constraints owner-scoped instance slugs depend
// on (DASHBOARD.md # instance naming): a username may not contain the `--`
// instance separator, nor start with `xn--` (reserved IDN/punycode prefix), so
// a `<slug>--<user>` slug always parses back into slug + user unambiguously.
func validateUsername(name string) error {
	if strings.Contains(name, "--") {
		return huma.Error422UnprocessableEntity("username may not contain '--'")
	}
	if strings.HasPrefix(name, "xn--") {
		return huma.Error422UnprocessableEntity("username may not start with 'xn--'")
	}
	return nil
}

func (s *Server) listUsers(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Users []UserDTO `json:"users"`
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	users, err := s.store.ListUsers()
	if err != nil {
		return nil, huma.Error500InternalServerError("list users failed", err)
	}
	out := &struct {
		Body struct {
			Users []UserDTO `json:"users"`
		}
	}{}
	out.Body.Users = []UserDTO{}
	for _, u := range users {
		out.Body.Users = append(out.Body.Users, userDTO(u))
	}
	return out, nil
}

func (s *Server) createUser(ctx context.Context, in *struct {
	Body struct {
		// DisplayName is the person's name, as the admin types it. The account
		// name is derived from it here, never sent by the caller
		// (FIRST_RUN.md # Identity & display names).
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		Role        string `json:"role,omitempty"`
	}
}) (*struct{ Body UserDTO }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}

	password := in.Body.Password
	if password == "" {
		return nil, huma.Error422UnprocessableEntity("name and password are required")
	}

	role := in.Body.Role
	if role == "" {
		role = store.RoleMember
	}
	if role != store.RoleAdmin && role != store.RoleMember {
		return nil, huma.Error422UnprocessableEntity("role must be admin or member")
	}

	displayName, username, err := s.newAccount(ctx, in.Body.DisplayName, "")
	if err != nil {
		s.auditor.Record(ctx, audit.ActionUserCreate, audit.Target{Kind: "user"},
			map[string]any{"role": role}, false)
		return nil, err
	}

	u := store.User{
		ID: newID(), Username: username, DisplayName: displayName,
		Role: role, CreatedAt: time.Now(),
	}
	meta := map[string]any{"username": username, "name": displayName, "role": role}
	if err := s.store.CreateUser(u); err != nil {
		s.auditor.Record(ctx, audit.ActionUserCreate, audit.Target{Kind: "user"}, meta, false)
		if errors.Is(err, store.ErrConflict) {
			// The derivation already walked past every name it could see, so a
			// conflict here is the race it cannot close: a concurrent create
			// that took the name between the check and this insert.
			return nil, huma.Error409Conflict("that name was just taken; try again")
		}
		return nil, huma.Error500InternalServerError("create user failed", err)
	}

	if err := s.host.SetPassword(ctx, username, password); err != nil {
		// Best-effort host cleanup before rolling back the store row:
		// covers the sliver where UpsertPassword created the Linux account
		// (useradd) but failed at chpasswd. Idempotent on the host side
		// (`docs/progress/0017-host-agent-delete-user.md`).
		if delErr := s.host.DeleteUser(ctx, username); delErr != nil {
			slog.Error("rollback host delete-user failed", "username", username, "err", delErr)
		}
		if delErr := s.store.DeleteUser(u.ID); delErr != nil {
			slog.Error("rollback create user failed", "user_id", u.ID, "err", delErr)
		}
		s.auditor.Record(ctx, audit.ActionUserCreate, audit.Target{Kind: "user"}, meta, false)
		return nil, huma.Error502BadGateway("host-agent set-password failed", err)
	}

	// Sync the new user's role to the host so admin creation also flips Linux
	// group membership in one round-trip. Called for both roles (admin and
	// member) so the brain-host contract stays uniform: after every user
	// mutation the host knows the canonical role. The provider's member path
	// is a no-op when the user isn't already in the admin group.
	if err := s.host.SetRole(ctx, username, role); err != nil {
		// Best-effort host cleanup: the Linux account already exists from the
		// successful SetPassword above, so a bare store rollback would leave
		// it orphaned with a working PAM password
		// (`docs/progress/0017-host-agent-delete-user.md`).
		if delErr := s.host.DeleteUser(ctx, username); delErr != nil {
			slog.Error("rollback host delete-user failed", "username", username, "err", delErr)
		}
		if delErr := s.store.DeleteUser(u.ID); delErr != nil {
			slog.Error("rollback create user failed", "user_id", u.ID, "err", delErr)
		}
		s.auditor.Record(ctx, audit.ActionUserCreate, audit.Target{Kind: "user"}, meta, false)
		return nil, huma.Error502BadGateway("host-agent set-role failed", err)
	}

	s.auditor.Record(ctx, audit.ActionUserCreate, audit.Target{Kind: "user", ID: u.ID}, meta, true)
	return &struct{ Body UserDTO }{Body: userDTO(u)}, nil
}

func (s *Server) updateUserRole(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Role string `json:"role"`
	}
}) (*struct{ Body UserDTO }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}

	role := in.Body.Role
	if role != store.RoleAdmin && role != store.RoleMember {
		return nil, huma.Error422UnprocessableEntity("role must be admin or member")
	}

	actor, _ := auth.FromContext(ctx)
	targetID := in.ID
	tgt := audit.Target{Kind: "user", ID: targetID}

	// No self-demote.
	if actor.User.ID == targetID && role != store.RoleAdmin {
		s.auditor.Record(ctx, audit.ActionUserRoleChange, tgt, map[string]any{"new_role": role}, false)
		return nil, huma.Error409Conflict("cannot demote yourself")
	}

	target, err := s.store.GetUser(targetID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such user")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get user failed", err)
	}
	meta := map[string]any{"old_role": target.Role, "new_role": role}

	// Last-admin guard: if demoting the only admin, reject.
	if target.Role == store.RoleAdmin && role == store.RoleMember {
		n, err := s.store.CountAdmins()
		if err != nil {
			return nil, huma.Error500InternalServerError("count admins failed", err)
		}
		if n <= 1 {
			s.auditor.Record(ctx, audit.ActionUserRoleChange, tgt, meta, false)
			return nil, huma.Error409Conflict("cannot demote the last admin")
		}
	}

	// Brain commits first; on host failure we restore the previous role so the
	// two sides stay aligned (USERS_AND_GROUPS.md: "if either side fails, both
	// roll back"). Mirror of createUser's brain-commit-then-rollback pattern.
	if err := s.store.UpdateRole(targetID, role); err != nil {
		s.auditor.Record(ctx, audit.ActionUserRoleChange, tgt, meta, false)
		return nil, huma.Error500InternalServerError("update role failed", err)
	}
	if err := s.host.SetRole(ctx, target.Username, role); err != nil {
		if rbErr := s.store.UpdateRole(targetID, target.Role); rbErr != nil {
			slog.Error("rollback update role failed", "user_id", targetID, "err", rbErr)
		}
		s.auditor.Record(ctx, audit.ActionUserRoleChange, tgt, meta, false)
		return nil, huma.Error502BadGateway("host-agent set-role failed", err)
	}

	s.auditor.Record(ctx, audit.ActionUserRoleChange, tgt, meta, true)

	target.Role = role
	return &struct{ Body UserDTO }{Body: userDTO(target)}, nil
}

func (s *Server) deleteUser(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}

	actor, _ := auth.FromContext(ctx)
	targetID := in.ID
	tgt := audit.Target{Kind: "user", ID: targetID}

	// Self-delete check fires before the last-admin guard on purpose: an admin
	// who wants to remove their own account always has to go through another
	// admin, even if there are several. Forces a second pair of eyes on the
	// "lose the only way in" move.
	if actor.User.ID == targetID {
		s.auditor.Record(ctx, audit.ActionUserDelete, tgt, nil, false)
		return nil, huma.Error409Conflict("cannot delete yourself")
	}

	target, err := s.store.GetUser(targetID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such user")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get user failed", err)
	}
	meta := map[string]any{"username": target.Username}

	// Last-admin guard.
	if target.Role == store.RoleAdmin {
		n, err := s.store.CountAdmins()
		if err != nil {
			return nil, huma.Error500InternalServerError("count admins failed", err)
		}
		if n <= 1 {
			s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
			return nil, huma.Error409Conflict("cannot delete the last admin")
		}
	}

	// Revoke SSH before the account goes, if it had any. The delete cascades the
	// brain's ssh_access and ssh_keys rows away, but the host keeps its own copy:
	// the account stays in sshd's AllowUsers and its key file stays in
	// /etc/ssh/moose-authorized-keys. Creating a user with the same name later
	// would then hand them a deleted account's key. Only accounts that were
	// actually enabled are pushed — a disabled one was never sent to the host, and
	// calling here on every delete would fail on a box with no sshd installed.
	//
	// The lock is taken before the read and held until the account is gone, on
	// every delete and not only on the enabled ones. The user's own session stays
	// valid until DeleteUser cascades it, so a request that is already elevated
	// could otherwise turn SSH on right after a disabled account reads as disabled.
	// The cascade would then drop the brain's rows while the host kept the key
	// file, which is the re-grant this revoke exists to prevent. A write that was
	// waiting on the lock finds the user row gone and fails its foreign key before
	// it can reach the host.
	s.sshWrites.Lock()
	defer s.sshWrites.Unlock()

	access, err := s.store.SSHAccessFor(targetID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read ssh access failed", err)
	}
	// Read before the delete cascades them away: they are what puts the account's
	// SSH back if a later step fails. Nothing else can reconstruct them — the host
	// holds only the rendered set, and after the revoke below not even that.
	sshKeys, err := s.store.ListSSHKeys(targetID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read ssh keys failed", err)
	}

	// The user's email accounts, and the app bindings to them, go with the user
	// row (ON DELETE CASCADE): an app bound to one falls back to unbound, as it
	// does when the account itself is deleted. Read them first, for the same
	// reason as the SSH keys: a host failure below puts the user row back, and
	// it must come back with its accounts and bindings, not without them.
	mailAccounts, err := s.store.ListMailProviders(targetID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read mail accounts failed", err)
	}
	mailBindings, err := s.store.ListMailBindingsForOwner(targetID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read mail bindings failed", err)
	}

	// This is the one place the brain-commits-first rule cannot hold: the revoke
	// has to read state the delete is about to cascade away, so the host is
	// changed first. That makes every later failure path owe a compensating
	// re-push — without one, a delete that fails after this point leaves the
	// account enabled in the brain and revoked on the host, with nothing to
	// notice or repair the difference (CLAUDE.md # Brain commits first).
	restoreSSH := func() {
		if !access.Enabled {
			return
		}
		if err := s.applySSH(ctx, target.Username, access.Enabled, access.RequirePassword, sshKeys); err != nil {
			slog.Error("ssh revoke rollback failed; the account is enabled in the brain but revoked on the host",
				"user_id", targetID, "username", target.Username, "service", "ssh", "err", err)
		}
	}

	if access.Enabled {
		if err := s.applySSH(ctx, target.Username, false, false, nil); err != nil {
			s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
			return nil, huma.Error502BadGateway("host-agent ssh set-access failed", err)
		}
	}

	// Brain commits first (FK cascades sessions); on host failure we restore
	// the row so the two sides stay aligned. Cascaded sessions don't come back —
	// the user has to log in again, which is acceptable for a rare error path.
	if err := s.store.DeleteUser(targetID); err != nil {
		restoreSSH()
		s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
		return nil, huma.Error500InternalServerError("delete user failed", err)
	}
	if err := s.host.DeleteUser(ctx, target.Username); err != nil {
		if rbErr := s.store.CreateUser(target); rbErr != nil {
			slog.Error("rollback delete user failed", "user_id", targetID, "err", rbErr)
		} else {
			// The user row is back, so put their SSH back with it. The cascade took
			// the ssh_access and ssh_keys rows, so these are re-inserted from what
			// was read above; restoring the row alone would hand the user back an
			// account whose keys had silently been destroyed by a delete that
			// reported failure.
			if rbErr := s.store.SetSSHAccess(targetID, access.Enabled, access.RequirePassword); rbErr != nil {
				slog.Error("rollback ssh access failed", "user_id", targetID,
					"username", target.Username, "service", "ssh", "err", rbErr)
			}
			for _, k := range sshKeys {
				if rbErr := s.store.AddSSHKey(k); rbErr != nil {
					slog.Error("rollback ssh key failed", "user_id", targetID,
						"username", target.Username, "service", "ssh", "err", rbErr)
				}
			}
			restoreSSH()
			// Accounts before bindings: a binding needs its account row.
			for _, p := range mailAccounts {
				if rbErr := s.store.CreateMailProvider(p); rbErr != nil {
					slog.Error("rollback mail account failed", "user_id", targetID, "username", target.Username, "err", rbErr)
				}
			}
			for _, b := range mailBindings {
				if rbErr := s.store.SetInstanceMailBinding(b.InstanceID, b.ProviderID); rbErr != nil {
					slog.Error("rollback mail binding failed", "user_id", targetID, "instance_id", b.InstanceID, "err", rbErr)
				}
			}
		}
		s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, false)
		return nil, huma.Error502BadGateway("host-agent delete-user failed", err)
	}

	s.auditor.Record(ctx, audit.ActionUserDelete, tgt, meta, true)
	return nil, nil
}

// renameUser changes what a person is called. It touches the display name and
// nothing else: the account name, the home directory, and file ownership are
// frozen at creation, because renaming a Linux user is destructive and we do
// not expose it (FIRST_RUN.md # Identity & display names).
//
// Anyone may rename themselves. Renaming somebody else is an admin action in
// the Users settings section, so it also needs the elevation window, matching
// every other mutation an admin makes to another account there.
func (s *Server) renameUser(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		DisplayName string `json:"display_name"`
	}
}) (*struct{ Body UserDTO }, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	tgt := audit.Target{Kind: "user", ID: in.ID}
	if in.ID != id.User.ID {
		if err := requireAdmin(ctx); err != nil {
			s.auditor.Record(ctx, audit.ActionUserRename, tgt, nil, false)
			return nil, err
		}
		if err := requireElevated(ctx); err != nil {
			s.auditor.Record(ctx, audit.ActionUserRename, tgt, nil, false)
			return nil, err
		}
	}

	target, err := s.store.GetUser(in.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("user not found")
		}
		s.auditor.Record(ctx, audit.ActionUserRename, tgt, nil, false)
		return nil, huma.Error500InternalServerError("get user failed", err)
	}

	name := normalizeDisplayName(in.Body.DisplayName)
	if err := validateDisplayName(name); err != nil {
		return nil, err
	}
	clash, err := s.displayNameTaken(name, target.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionUserRename, tgt, nil, false)
		return nil, huma.Error500InternalServerError("list users failed", err)
	}
	if clash != "" {
		s.auditor.Record(ctx, audit.ActionUserRename, tgt, nil, false)
		return nil, huma.Error409Conflict(displayNameClashMessage(clash))
	}

	meta := map[string]any{"username": target.Username, "name": name, "from": target.DisplayName}
	if err := s.store.UpdateDisplayName(target.ID, name); err != nil {
		s.auditor.Record(ctx, audit.ActionUserRename, tgt, meta, false)
		if errors.Is(err, store.ErrConflict) {
			return nil, huma.Error409Conflict("that name was just taken; try again")
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("user not found")
		}
		return nil, huma.Error500InternalServerError("rename user failed", err)
	}

	target.DisplayName = name
	s.auditor.Record(ctx, audit.ActionUserRename, tgt, meta, true)
	slog.Info("user renamed", "user_id", target.ID, "username", target.Username, "name", name)
	return &struct{ Body UserDTO }{Body: userDTO(target)}, nil
}

func (s *Server) changeMyPassword(ctx context.Context, in *struct {
	Body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
}) (*struct{}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}

	if in.Body.CurrentPassword == "" || in.Body.NewPassword == "" {
		return nil, huma.Error422UnprocessableEntity("current_password and new_password are required")
	}

	tgt := audit.Target{Kind: "user", ID: id.User.ID}
	valid, err := s.host.VerifyPassword(ctx, id.User.Username, in.Body.CurrentPassword)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionUserPasswordChange, tgt, nil, false)
		return nil, huma.Error502BadGateway("host-agent verify failed", err)
	}
	if !valid {
		s.auditor.Record(ctx, audit.ActionUserPasswordChange, tgt, nil, false)
		return nil, huma.Error401Unauthorized("current password is incorrect")
	}

	if err := s.host.SetPassword(ctx, id.User.Username, in.Body.NewPassword); err != nil {
		s.auditor.Record(ctx, audit.ActionUserPasswordChange, tgt, nil, false)
		return nil, huma.Error502BadGateway("host-agent set-password failed", err)
	}

	// Revoke all sessions for this user — password has changed (AUTH.md # Invalidation).
	_ = s.store.DeleteSessionsForUser(id.User.ID)

	s.auditor.Record(ctx, audit.ActionUserPasswordChange, tgt, nil, true)
	return nil, nil
}

func (s *Server) resetUserPassword(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Password string `json:"password"`
	}
}) (*struct{}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if err := requireElevated(ctx); err != nil {
		return nil, err
	}

	password := in.Body.Password
	if password == "" {
		return nil, huma.Error422UnprocessableEntity("password is required")
	}

	target, err := s.store.GetUser(in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such user")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get user failed", err)
	}
	tgt := audit.Target{Kind: "user", ID: target.ID}
	meta := map[string]any{"username": target.Username}

	if err := s.host.SetPassword(ctx, target.Username, password); err != nil {
		s.auditor.Record(ctx, audit.ActionUserPasswordReset, tgt, meta, false)
		return nil, huma.Error502BadGateway("host-agent set-password failed", err)
	}

	s.auditor.Record(ctx, audit.ActionUserPasswordReset, tgt, meta, true)
	return nil, nil
}
