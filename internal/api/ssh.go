package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/crypto/ssh"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// Device access — the per-account SSH opt-in (AUTH.md # Device access).
//
// These routes live under /api/v1/me because Device access is a My-account
// panel: every signed-in user manages their own SSH, and no admin manages it for
// them (SETTINGS.md # panel inventory). Nothing here takes a user id from the
// caller; the account is always the session's own, so there is no cross-account
// surface to get wrong.
//
// Every write is elevation-class and audits success and failure, because turning
// SSH on changes who can get a shell on the box (CLAUDE.md # Elevation-class
// mutations).

// maxSSHKeysPerUser caps how many keys one account may hold. Several is normal
// (a laptop and a desktop); dozens means something is wrong, and every key is a
// credential that authenticates as that user until someone notices it.
const maxSSHKeysPerUser = 10

// maxSSHKeyBytes bounds a submitted key. A real key line is a few hundred bytes;
// this leaves room for a long comment and refuses a pasted file.
const maxSSHKeyBytes = 4096

func (s *Server) registerSSHRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-my-ssh", Method: "GET", Path: "/api/v1/me/ssh",
		Summary: "Read my SSH access settings and keys (auth required)",
	}, s.getMySSH)

	huma.Register(api, huma.Operation{
		OperationID: "set-my-ssh", Method: "PUT", Path: "/api/v1/me/ssh",
		Summary: "Save my whole SSH state: on or off, the password choice, and the complete key set (auth required, elevation-class)",
	}, s.setMySSH)
}

// SSHKeyDTO is one stored key as the dashboard sees it. The key material is sent
// back so the user can recognise what they added; the fingerprint is what the UI
// shows, because it is short and is what other tools display.
type SSHKeyDTO struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	AddedAt     int64  `json:"added_at"`
}

// SSHAccessDTO is the whole Device access panel state for one account.
//
// KeyRequired tells the UI which factor this profile makes mandatory, so the
// panel does not have to know the rule and the two cannot drift. The server
// enforces it regardless of what the UI does with it.
type SSHAccessDTO struct {
	Enabled         bool        `json:"enabled"`
	RequirePassword bool        `json:"require_password"`
	KeyRequired     bool        `json:"key_required"`
	Keys            []SSHKeyDTO `json:"keys"`
}

// keyRequired reports whether this profile makes a public key the mandatory
// factor. Hosted does: the box answers on the public internet, so a household
// password is not a credential there (DECISIONS.md 2026-09-09). The appliance
// does not: :22 is scoped to the LAN and the mesh by nftables, which is the
// perimeter the one-password model was designed around.
func (s *Server) keyRequired() bool {
	return s.profile == profile.Hosted
}

// effectiveRequirePassword resolves what the account asked for against what the
// profile makes mandatory. On the appliance the moose password is the mandatory
// factor and the key is the optional second lock, so the answer is always true
// no matter what the caller sent; on hosted the key is mandatory and the
// password is the account's own choice (AUTH.md # Device access, the profile
// table).
//
// It is applied once, at the only entry point a caller-supplied value comes in
// through. Every other push (the deleteUser restore, the rollbacks) reads the
// stored row, so normalising before the write makes all of them right and stops
// the row describing a posture sshd is not running.
//
// The brain is the right place for this and host-agent is not: host-agent does
// not know the environment profile and must not second-guess which factor is
// mandatory. Given a key and false, its renderer correctly writes
// "AuthenticationMethods publickey" — the bug was the brain handing it a false
// the appliance is not allowed to ask for.
func (s *Server) effectiveRequirePassword(asked bool) bool {
	if !s.keyRequired() {
		return true
	}
	return asked
}

func (s *Server) getMySSH(ctx context.Context, _ *struct{}) (*struct {
	Body SSHAccessDTO
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	dto, err := s.sshAccessDTO(id.User.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body SSHAccessDTO }{Body: dto}, nil
}

// SSHKeyInput is one key in the account's desired key set. A key the account
// already holds is named by its id. A new key carries its text instead. An entry
// has one or the other, never both.
type SSHKeyInput struct {
	ID string `json:"id,omitempty" required:"false" doc:"A key the account already holds. Omit for a new key."`
	// The key text for a new key. It goes through the same validation a single
	// paste always did (parseSSHPublicKey).
	PublicKey string `json:"public_key,omitempty" required:"false"`
	// Optional: the key's own comment names it when the user gives no label.
	// Read only for a new key; a held key keeps the label it was added with.
	Label string `json:"label,omitempty" required:"false"`
}

// setMySSH applies one Save of the SSH screen. The body is the whole desired
// state, not a change: the on/off flag, the password choice, and the complete
// key set. The brain diffs that against what it holds, writes the difference in
// one transaction, and pushes the result to the host once.
//
// Taking the whole state is what lets the screen be a draft. With one request
// per control, a Save over several of them is a batch that can half-fail. It is
// also what makes key rotation work: "remove the old key, add the new one" is
// one change here, and only the state it ends in is checked, so there is no
// moment in between where the account has no key.
func (s *Server) setMySSH(ctx context.Context, in *struct {
	Body struct {
		Enabled bool `json:"enabled"`
		// Optional, and only meaningful on hosted: it asks for the moose password
		// as a second required method alongside the key. On the appliance the
		// password is the mandatory factor already, so omitting this changes
		// nothing and the server resolves it to true either way
		// (effectiveRequirePassword).
		RequirePassword bool `json:"require_password,omitempty" required:"false"`
		// Required, and the complete set: a held key missing from it is removed.
		// Required rather than optional so an old client that sends only the flag
		// is refused instead of wiping every key the account holds.
		Keys []SSHKeyInput `json:"keys"`
	}
}) (*struct {
	Body SSHAccessDTO
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	tgt := audit.Target{Kind: "user", ID: id.User.ID}

	requirePassword := s.effectiveRequirePassword(in.Body.RequirePassword)

	// The audit meta carries what was *asked for*, because that is what every
	// record here is about, including the ones written when the request was
	// refused and nothing was applied. Writing the resolved value on those would
	// describe a posture the box never took and hide what the caller actually
	// sent, which is the thing an Activity reader is trying to see. The success
	// record adds the effective value alongside it, below, where there is a real
	// applied state to report.
	meta := map[string]any{"enabled": in.Body.Enabled, "require_password": in.Body.RequirePassword}

	// Every new key is parsed before anything is read or written. A rejected
	// paste is a validation 422, not an elevation-class failure, so it does not
	// audit (CLAUDE.md: validation 422s don't audit).
	newKeys, err := parseNewSSHKeys(id.User.ID, in.Body.Keys)
	if err != nil {
		return nil, err
	}

	// Everything below reads the account's state, decides, writes it, then pushes
	// the whole set to the host. Held across the host call on purpose: without it
	// two overlapping writes can commit in one order and land on the host in the
	// other (api.go # sshWrites).
	s.sshWrites.Lock()
	defer s.sshWrites.Unlock()

	prev, err := s.store.SSHAccessFor(id.User.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read ssh access failed", err)
	}
	held, err := s.store.ListSSHKeys(id.User.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read ssh keys failed", err)
	}

	// The plan is worked out before the elevation check so that a refused request
	// still names each key it tried to add or remove in Activity, the same trail
	// the per-key routes this replaced used to leave.
	plan, planErr := planSSHKeys(held, in.Body.Keys, newKeys)
	accessChanged := prev.Enabled != in.Body.Enabled ||
		s.effectiveRequirePassword(prev.RequirePassword) != requirePassword
	record := func(success bool) {
		s.auditSSHChange(ctx, tgt, meta, accessChanged, plan, success)
	}

	if err := requireElevated(ctx); err != nil {
		record(false)
		return nil, err
	}
	// Guard rejections from here on, not malformed requests, so they audit
	// (CLAUDE.md # Elevation-class mutations).
	if planErr != nil {
		record(false)
		return nil, planErr
	}
	if len(plan.final) > maxSSHKeysPerUser {
		record(false)
		return nil, huma.Error422UnprocessableEntity(
			fmt.Sprintf("you can have at most %d SSH keys; remove one first", maxSSHKeysPerUser))
	}
	// The profile's mandatory factor, checked against the state the request ends
	// in and not only in the UI. On hosted an account with no key could only ever
	// authenticate by password, on a port the open internet can reach, which is
	// the thing this design exists to prevent. Checking the end state is what
	// lets a rotation through: removing the only key is fine when the same Save
	// adds its replacement.
	if in.Body.Enabled && s.keyRequired() && len(plan.final) == 0 {
		record(false)
		return nil, huma.Error422UnprocessableEntity(
			"add an SSH key before turning SSH on; a password alone is not enough on this box")
	}

	// Brain commits first; the host is reconstructible. On host failure the change
	// is undone, so the two sides cannot disagree about who has a shell
	// (CLAUDE.md # Brain commits first).
	if err := s.store.ApplySSHChange(id.User.ID, in.Body.Enabled, requirePassword,
		plan.removeIDs(), plan.add); err != nil {
		record(false)
		switch {
		case errors.Is(err, store.ErrDuplicateSSHKey):
			return nil, huma.Error409Conflict("that key is already on your account")
		case errors.Is(err, store.ErrNotFound):
			return nil, huma.Error409Conflict(sshKeysMovedMessage)
		}
		return nil, huma.Error500InternalServerError("save ssh access failed", err)
	}

	// Push while SSH is on, and once more on the way off so the host revokes. An
	// account that was off and stays off changes nothing the host needs to know:
	// pushing its keys would write an authorized_keys file for an account sshd is
	// not admitting, and would fail outright on a box with no sshd installed.
	if in.Body.Enabled || prev.Enabled {
		if err := s.applySSH(ctx, id.User.Username, in.Body.Enabled, requirePassword, plan.final); err != nil {
			// The inverse of the change above: the old access row back, the added
			// keys out, the removed ones in with their original AddedAt so the list
			// keeps its order.
			if rbErr := s.store.ApplySSHChange(id.User.ID, prev.Enabled, prev.RequirePassword,
				plan.addIDs(), plan.remove); rbErr != nil {
				slog.Error("ssh access rollback failed", "user_id", id.User.ID,
					"username", id.User.Username, "service", "ssh", "err", rbErr)
			}
			record(false)
			return nil, huma.Error502BadGateway("host-agent ssh set-access failed", err)
		}
	}

	// Only now is there an applied state to name. On the appliance this differs
	// from what was asked whenever the caller omitted require_password, and the
	// pair is what makes the record readable: what they wanted, what they got.
	meta["require_password_applied"] = requirePassword
	record(true)
	dto, err := s.sshAccessDTO(id.User.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body SSHAccessDTO }{Body: dto}, nil
}

// sshKeysMovedMessage is the answer when a request names a key the account no
// longer holds. The usual cause is a second tab or device that removed it first.
const sshKeysMovedMessage = "your SSH keys changed somewhere else; reload the page and try again"

// sshPlan is the difference between the keys an account holds and the set a
// request asks for.
type sshPlan struct {
	remove []store.SSHKey // held, and missing from the request
	add    []store.SSHKey // new in the request, parsed and fingerprinted
	final  []store.SSHKey // the set the account ends with: kept keys, then added
}

func (p sshPlan) removeIDs() []string {
	ids := make([]string, 0, len(p.remove))
	for _, k := range p.remove {
		ids = append(ids, k.ID)
	}
	return ids
}

func (p sshPlan) addIDs() []string {
	ids := make([]string, 0, len(p.add))
	for _, k := range p.add {
		ids = append(ids, k.ID)
	}
	return ids
}

// parseNewSSHKeys validates every new key in a request and returns them keyed by
// their index in it. A refusal names the entry's location, so the screen can show
// the message under the key at fault rather than only at the Save button.
func parseNewSSHKeys(userID string, entries []SSHKeyInput) (map[int]store.SSHKey, error) {
	out := map[int]store.SSHKey{}
	for i, e := range entries {
		loc := fmt.Sprintf("body.keys[%d].public_key", i)
		if e.ID != "" {
			if e.PublicKey != "" {
				msg := "a key is either one you already have or a new one, not both"
				return nil, huma.Error422UnprocessableEntity(msg, &huma.ErrorDetail{Message: msg, Location: loc})
			}
			continue
		}
		line, fingerprint, err := parseSSHPublicKey(e.PublicKey)
		if err != nil {
			msg := err.Error()
			return nil, huma.Error422UnprocessableEntity(msg, &huma.ErrorDetail{Message: msg, Location: loc})
		}
		out[i] = store.SSHKey{
			ID:          newID(),
			UserID:      userID,
			Label:       strings.TrimSpace(e.Label),
			PublicKey:   line,
			Fingerprint: fingerprint,
		}
	}
	return out, nil
}

// planSSHKeys diffs the requested key set against the held one. It returns what
// it could work out even when it also returns an error, so a refused request can
// still audit the keys it named.
//
// Two refusals, both 409. An id the account does not hold means the request was
// built from a key list that has since changed. A new key the account would then
// hold twice is the duplicate the store's unique index would refuse anyway, but
// caught here it can name the entry. A new key that matches one being removed in
// the same request is fine: the store removes before it adds.
func planSSHKeys(held []store.SSHKey, entries []SSHKeyInput, newKeys map[int]store.SSHKey) (sshPlan, error) {
	var (
		plan    sshPlan
		planErr error
	)
	keep := map[string]bool{}
	heldIDs := map[string]bool{}
	for _, k := range held {
		heldIDs[k.ID] = true
	}
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		if !heldIDs[e.ID] && planErr == nil {
			planErr = huma.Error409Conflict(sshKeysMovedMessage)
		}
		keep[e.ID] = true
	}

	fingerprints := map[string]bool{}
	for _, k := range held {
		if keep[k.ID] {
			plan.final = append(plan.final, k)
			fingerprints[k.Fingerprint] = true
		} else {
			plan.remove = append(plan.remove, k)
		}
	}
	for i := range entries {
		k, ok := newKeys[i]
		if !ok {
			continue
		}
		if fingerprints[k.Fingerprint] {
			if planErr == nil {
				msg := "that key is already on your account"
				planErr = huma.Error409Conflict(msg, &huma.ErrorDetail{
					Message: msg, Location: fmt.Sprintf("body.keys[%d].public_key", i),
				})
			}
			continue
		}
		fingerprints[k.Fingerprint] = true
		plan.add = append(plan.add, k)
		plan.final = append(plan.final, k)
	}
	return plan, planErr
}

// auditSSHChange writes the Activity trail for one Save: one record per key
// added, one per key removed, and an access record when the on/off flag or the
// password choice moved. A Save that changes nothing still writes the access
// record, so every elevation-class request leaves at least one line.
//
// The same records are written on failure with success false, so a refused Save
// still shows which keys somebody tried to add or remove (CLAUDE.md #
// Elevation-class mutations).
func (s *Server) auditSSHChange(ctx context.Context, tgt audit.Target, meta map[string]any, accessChanged bool, plan sshPlan, success bool) {
	if accessChanged || (len(plan.add) == 0 && len(plan.remove) == 0) {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, success)
	}
	for _, k := range plan.add {
		s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, map[string]any{"fingerprint": k.Fingerprint}, success)
	}
	for _, k := range plan.remove {
		s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, map[string]any{"fingerprint": k.Fingerprint}, success)
	}
}

// applySSH sends one account's full desired state to host-agent.
func (s *Server) applySSH(ctx context.Context, username string, enabled, requirePassword bool, keys []store.SSHKey) error {
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k.PublicKey)
	}
	return s.host.SetSSHAccess(ctx, protocol.SetSSHAccessRequest{
		User:            username,
		Enabled:         enabled,
		AuthorizedKeys:  lines,
		RequirePassword: requirePassword,
	})
}

func (s *Server) sshAccessDTO(userID string) (SSHAccessDTO, error) {
	access, err := s.store.SSHAccessFor(userID)
	if err != nil {
		return SSHAccessDTO{}, huma.Error500InternalServerError("read ssh access failed", err)
	}
	keys, err := s.store.ListSSHKeys(userID)
	if err != nil {
		return SSHAccessDTO{}, huma.Error500InternalServerError("read ssh keys failed", err)
	}
	out := SSHAccessDTO{
		Enabled:         access.Enabled,
		RequirePassword: access.RequirePassword,
		KeyRequired:     s.keyRequired(),
		Keys:            []SSHKeyDTO{},
	}
	for _, k := range keys {
		out.Keys = append(out.Keys, SSHKeyDTO{
			ID:          k.ID,
			Label:       k.Label,
			PublicKey:   k.PublicKey,
			Fingerprint: k.Fingerprint,
			AddedAt:     k.AddedAt.Unix(),
		})
	}
	return out, nil
}

// parseSSHPublicKey validates a submitted key and returns the canonical
// authorized_keys line plus its SHA-256 fingerprint.
//
// The line is re-serialised from the parsed key rather than stored as typed,
// which drops anything the user pasted around it. That matters more than tidiness:
// authorized_keys accepts leading *options* (command=, from=, and others) that
// change what the key can do, and accepting them verbatim would let a paste
// carry behaviour nobody reviewed. Only the comment is kept, since a user names
// their keys by it.
func parseSSHPublicKey(in string) (line, fingerprint string, err error) {
	raw := strings.TrimSpace(in)
	if raw == "" {
		return "", "", huma.Error422UnprocessableEntity("paste or upload a public key")
	}
	if len(raw) > maxSSHKeyBytes {
		return "", "", huma.Error422UnprocessableEntity("that does not look like a public key; it is too long")
	}
	// The single most likely user error, and the one with the worst consequence,
	// so it gets its own message rather than a generic parse failure.
	if strings.Contains(raw, "PRIVATE KEY") {
		return "", "", huma.Error422UnprocessableEntity(
			"that is a private key; keep it secret and paste the matching .pub file instead")
	}
	// A pasted file can carry several lines. Take the first non-empty, non-comment
	// one rather than silently using the last, and say so if there are more.
	var candidate string
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		candidate = l
		break
	}
	if candidate == "" {
		return "", "", huma.Error422UnprocessableEntity("paste or upload a public key")
	}

	pub, comment, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(candidate))
	if parseErr != nil {
		return "", "", huma.Error422UnprocessableEntity(
			"that does not look like an SSH public key; it should start with something like 'ssh-ed25519'")
	}

	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		line += " " + sanitizeKeyComment(comment)
	}
	sum := sha256.Sum256(pub.Marshal())
	fingerprint = "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
	return line, fingerprint, nil
}

// sanitizeKeyComment strips anything that would break the one-key-per-line shape
// of authorized_keys. A newline in a comment would otherwise let a single
// submitted "key" write a second, unreviewed line into the file.
func sanitizeKeyComment(comment string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, comment)
}
