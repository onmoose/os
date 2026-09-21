package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/text/unicode/norm"

	"github.com/onmoose/os/internal/store"
)

// Display names and account names (FIRST_RUN.md # Identity & display names).
//
// A person types a display name ("José Smith"). The box derives a stable Linux
// account name from it ("josesmith"). The two have opposite lifetimes on
// purpose: the display name is mutable and is what every surface shows, while
// the account name is frozen at creation because it is the home directory, the
// file owner, and the SSH login. Renaming a Linux user is destructive, so we
// never expose it.
//
// This file is the single source of the derivation. Every account-creating path
// (/setup, POST /users, the hosted SSO handshake) goes through deriveAccountName
// so the three of them cannot drift apart again.

const (
	// maxAccountNameLen caps the derived account name at the conservative
	// classic Linux username length, so a long display name cannot produce a
	// name useradd or a hardened PAM stack rejects. The result is ASCII, so a
	// byte length is a rune length here.
	maxAccountNameLen = 32

	// maxDisplayNameLen caps what a person may type. Generous for a first name;
	// it exists to bound storage and rendering, not to police naming.
	maxDisplayNameLen = 64

	// maxAccountNameSuffix bounds the collision walk (josesmith, josesmith1,
	// josesmith2, ...). It is not defending against anything a person can do:
	// display names are unique, so the walk is short in every real case. It
	// stops a host-agent that wrongly answers "exists" to everything from
	// spinning forever.
	maxAccountNameSuffix = 100

	// accountNameFallback is the base used when a display name has no Latin
	// letters or digits to work with at all, which is every non-Latin script
	// (FIRST_RUN.md # Identity & display names). Deliberately not in
	// reservedAccountNames: reserving it would push the very first fallback to
	// "user1" for no reason.
	accountNameFallback = "user"
)

// latinFolds are the Latin letters that Unicode canonical decomposition does
// not take apart, because they are distinct letters rather than a base letter
// plus a mark. NFD turns "é" into "e" + an accent we can drop, but it leaves
// "ø" and "ß" untouched, so those need naming here or they would vanish and
// turn "Søren" into "srn".
//
// Keys are lowercase: transliterate lowercases before it folds.
var latinFolds = map[rune]string{
	'ø': "o", 'œ': "oe", 'æ': "ae", 'ß': "ss", 'ẞ': "ss",
	'ł': "l", 'ŀ': "l", 'đ': "d", 'ð': "d", 'þ': "th",
	'ħ': "h", 'ı': "i", 'ſ': "s", 'ŧ': "t", 'ƶ': "z",
}

// reservedAccountNames are names a derived account may never take. The list is
// FIRST_RUN.md # Identity & display names plus the standard Debian system
// accounts, and it is only the cheap first filter: the authoritative check is
// the host probe in deriveAccountName, because an app or an admin can add a
// daemon account long after this list was written.
//
// Names containing a hyphen ("www-data") are unreachable by construction, since
// accountNameBase keeps only [a-z0-9]. They are listed anyway, with their
// hyphen-free spellings next to them, because the spec asks for the list to be
// explicit rather than for the reader to re-derive which entries can fire.
//
// The spec writes the systemd entry as a glob, "systemd*". Every account Debian
// actually creates under it is hyphenated (systemd-network, systemd-resolve,
// systemd-timesync) and so is already unreachable, which leaves the bare name,
// listed here. It is deliberately not a prefix match: a prefix match would also
// reject systemd1, systemd2 and so on, which is every name the collision walk
// could fall back to, so a person called "Systemd" would have no name left.
var reservedAccountNames = map[string]bool{
	"root": true, "daemon": true, "bin": true, "sys": true, "sync": true,
	"games": true, "man": true, "lp": true, "mail": true, "news": true,
	"uucp": true, "proxy": true, "backup": true, "list": true, "irc": true,
	"gnats": true, "nobody": true, "adm": true, "admin": true, "sshd": true,
	"messagebus": true, "avahi": true, "caddy": true, "docker": true,
	"postgres": true, "redis": true, "mysql": true, "operator": true,
	"www-data": true, "wwwdata": true, "www": true, "systemd": true,
	"moose": true, "mooseapp": true, "mooseshared": true,
}

// normalizeDisplayName trims a typed display name and collapses internal
// whitespace runs to a single space, so " José   Smith " and "José Smith" are
// the same name rather than two that merely look alike.
func normalizeDisplayName(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// validateDisplayName checks a normalized display name. Returns a huma 422 with
// plain-English text, matching how the rest of this package reports bad input.
func validateDisplayName(s string) error {
	if s == "" {
		return huma.Error422UnprocessableEntity("name is required")
	}
	if len([]rune(s)) > maxDisplayNameLen {
		return huma.Error422UnprocessableEntity(
			fmt.Sprintf("name must be %d characters or fewer", maxDisplayNameLen))
	}
	for _, r := range s {
		// Fields() already removed every whitespace control character, so
		// anything still in this class is a real control code.
		if unicode.IsControl(r) {
			return huma.Error422UnprocessableEntity("name may not contain control characters")
		}
	}
	return nil
}

// foldDisplayName returns the key two display names are compared on for
// uniqueness. It folds case and whitespace but deliberately NOT accents, so
// "José" and "Jose" are two different people who may both live on the box. They
// then collide on the account name instead, which is what the numeric suffix in
// deriveAccountName is for.
func foldDisplayName(s string) string {
	return strings.ToLower(normalizeDisplayName(s))
}

// transliterate reduces a display name to ASCII letters and digits.
//
// Order matters: lowercase first (so the fold table only needs lowercase keys),
// then decompose so an accented letter becomes its base letter plus a combining
// mark, then drop the marks, then fold the letters decomposition does not
// split. Everything still non-ASCII after that is a script we have no mapping
// for, and it is dropped rather than guessed at.
func transliterate(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case r < unicode.MaxASCII:
			b.WriteRune(r)
		case unicode.Is(unicode.Mn, r):
			// A combining mark left over from NFD: the accent itself.
		default:
			b.WriteString(latinFolds[r]) // "" for anything unmapped
		}
	}
	return b.String()
}

// accountNameBase turns a display name into the candidate account name, before
// any collision handling. FIRST_RUN.md # Identity & display names, steps 1-3.
//
// Characters outside [a-z0-9] are removed rather than replaced with a
// separator, so "José Smith" becomes "josesmith". That is what makes the two
// reservations the <slug>--<user> personal-instance scheme depends on
// (DASHBOARD.md # instance naming) true by construction: with no hyphen in the
// alphabet at all, the result can neither contain "--" nor start with "xn--".
func accountNameBase(display string) string {
	var b strings.Builder
	for _, r := range transliterate(display) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	name := b.String()
	switch {
	case name == "":
		// No Latin letters or digits survived: a name written entirely in a
		// script we do not transliterate.
		name = accountNameFallback
	case name[0] >= '0' && name[0] <= '9':
		// useradd refuses an all-numeric name, and a leading digit is legal but
		// discouraged, so give it a letter to start on rather than reject a
		// name a person is entitled to use.
		name = "u" + name
	}
	// Cap last, after the prefix above has been added. Capping first would let a
	// 32-digit name come back out at 33 characters wearing its new "u".
	if len(name) > maxAccountNameLen {
		name = name[:maxAccountNameLen]
	}
	return name
}

// deriveAccountName produces the Linux account name for a display name, walking
// past anything already taken. taken reports whether a candidate is unavailable;
// it is the composition of the brain's own user table and the host's
// /etc/passwd, because either one holding the name is a reason not to use it.
//
// The host half is not optional. host-agent's set-password is an upsert: handed
// a name that already exists it sets a password on that account instead of
// creating one. Without this walk, a person called "Plex" on a box that runs
// Plex would be handed the plex daemon's account.
func deriveAccountName(display string, taken func(string) (bool, error)) (string, error) {
	base := accountNameBase(display)
	for i := 0; i <= maxAccountNameSuffix; i++ {
		candidate := base
		if i > 0 {
			suffix := fmt.Sprintf("%d", i)
			// Truncate the base so the suffix cannot push the name past the
			// length cap; without this "verylongname...9" would be cut back to
			// the un-suffixed name and collide all over again.
			trimmed := base
			if len(trimmed)+len(suffix) > maxAccountNameLen {
				trimmed = trimmed[:maxAccountNameLen-len(suffix)]
			}
			candidate = trimmed + suffix
		}
		if reservedAccountNames[candidate] {
			continue
		}
		// Assert the instance-slug reservations the derivation is supposed to
		// make unreachable. Cheap, and it fails loudly if the alphabet above
		// ever widens.
		if err := validateUsername(candidate); err != nil {
			continue
		}
		busy, err := taken(candidate)
		if err != nil {
			return "", err
		}
		if !busy {
			return candidate, nil
		}
	}
	return "", huma.Error409Conflict("could not find a free account name for this name; try a different one")
}

// --- server-side plumbing ------------------------------------------------

// accountNameTaken builds the availability check deriveAccountName walks with.
// Both halves matter: the brain's own table, because a name it already handed
// out is gone even if the host call that would have created it failed, and the
// host's passwd, because that is where a daemon account an app installed lives.
func (s *Server) accountNameTaken(ctx context.Context) func(string) (bool, error) {
	return func(name string) (bool, error) {
		_, err := s.store.GetUserByUsername(name)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, store.ErrNotFound):
			return false, err
		}
		return s.host.UserExists(ctx, name)
	}
}

// newAccount validates a typed display name and derives the account name to go
// with it. It is the one entry point the three account-creating paths share
// (/setup, POST /users, the hosted SSO handshake), so the three cannot drift.
//
// excludeUserID is the account exempt from the display-name uniqueness check,
// for a rename of an existing person; pass "" when creating.
func (s *Server) newAccount(ctx context.Context, displayName, excludeUserID string) (name, account string, err error) {
	name = normalizeDisplayName(displayName)
	if err := validateDisplayName(name); err != nil {
		return "", "", err
	}
	clash, err := s.displayNameTaken(name, excludeUserID)
	if err != nil {
		return "", "", huma.Error500InternalServerError("list users failed", err)
	}
	if clash != "" {
		return "", "", huma.Error409Conflict(displayNameClashMessage(clash))
	}
	account, err = deriveAccountName(name, s.accountNameTaken(ctx))
	if err != nil {
		return "", "", err
	}
	return name, account, nil
}

// displayNameTaken returns the existing display name that clashes with this one,
// or "" when the name is free. It returns the name rather than a bool so the
// error can quote the person already on the box, which is more use than echoing
// back what the admin just typed.
//
// The comparison folds case and whitespace but not accents (foldDisplayName),
// and it is done here rather than left to the database because SQLite's NOCASE
// collation only folds ASCII: it would let "José" and "JOSÉ" both through. The
// unique index is the backstop for the race between this check and the insert.
func (s *Server) displayNameTaken(name, excludeUserID string) (string, error) {
	want := foldDisplayName(name)
	users, err := s.store.ListUsers()
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if u.ID == excludeUserID {
			continue
		}
		if foldDisplayName(u.DisplayName) == want {
			return u.DisplayName, nil
		}
	}
	return "", nil
}

// displayNameClashMessage is the refusal an admin reads when two people on one
// box would have the same name. It suggests the fix the spec suggests
// (FIRST_RUN.md # Identity & display names: "use Cindy K. or Cindy 2") rather
// than only saying no.
func displayNameClashMessage(existing string) string {
	suggestion := existing + " 2"
	if first, _, ok := strings.Cut(existing, " "); ok {
		suggestion = first + " K."
	}
	return "someone on this box is already called \"" + existing +
		"\"; give this person a different name, like \"" + suggestion + "\""
}
