package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestDisplayNameMigratesFromAnOlderDatabase opens a database written before
// users had a display name, the way a box that has been running since before
// this change does. Nobody should see a blank where their name goes, so the
// migration fills it from the account name they were already shown as.
func TestDisplayNameMigratesFromAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")

	// The users table exactly as it was before display_name existed.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := old.Exec(`
		CREATE TABLE users (
			id            TEXT PRIMARY KEY,
			username      TEXT NOT NULL UNIQUE,
			role          TEXT NOT NULL CHECK (role IN ('admin','member')),
			recovery_hash TEXT NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL
		);
		INSERT INTO users (id, username, role, recovery_hash, created_at)
		VALUES ('u_1','andrei','admin','',1700000000),
		       ('u_2','cindy','member','',1700000001);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store on the old db: %v", err)
	}
	defer s.Close()

	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users; want 2", len(users))
	}
	for _, u := range users {
		if u.DisplayName != u.Username {
			t.Errorf("user %s: display_name = %q; want the account name %q",
				u.ID, u.DisplayName, u.Username)
		}
	}

	// Opening again must not undo or re-run anything: a rename made after the
	// migration has to survive the next boot.
	if err := s.UpdateDisplayName("u_2", "Cynthia"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	u, err := again.GetUser("u_2")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.DisplayName != "Cynthia" {
		t.Errorf("after reopen display_name = %q; want Cynthia", u.DisplayName)
	}
	if u.Username != "cindy" {
		t.Errorf("after rename username = %q; want cindy unchanged", u.Username)
	}
}

func TestDisplayNameMustBeUniqueAndPresent(t *testing.T) {
	s := open(t)
	base := User{ID: "u_1", Username: "cindy", DisplayName: "Cindy", Role: RoleAdmin}
	if err := s.CreateUser(base); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A blank display name is a caller bug, reported on the first insert rather
	// than as a conflict on the second.
	if err := s.CreateUser(User{ID: "u_2", Username: "bob", Role: RoleMember}); err == nil {
		t.Error("CreateUser with no display name succeeded")
	}

	// The index is the backstop under the API's own check.
	dupe := User{ID: "u_3", Username: "cindy2", DisplayName: "cindy", Role: RoleMember}
	if err := s.CreateUser(dupe); err != ErrConflict {
		t.Errorf("duplicate display name = %v; want ErrConflict", err)
	}
	if err := s.UpdateDisplayName("u_1", "Cynthia"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := s.CreateUser(dupe); err != nil {
		t.Errorf("create after the name was freed: %v", err)
	}
}

// TestDisplayNameMigrationSurvivesCaseOnlyUsernameClash is the upgrade that
// would otherwise brick a box. Usernames are unique case-sensitively, and the
// old validateUsername rejected only "--" and an "xn--" prefix, so a box can
// hold both "Bob" and "bob". Backfilling display names from them gives the
// NOCASE index two rows it treats as one, and a migration that errors is a
// brain that does not start.
func TestDisplayNameMigrationSurvivesCaseOnlyUsernameClash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := old.Exec(`
		CREATE TABLE users (
			id            TEXT PRIMARY KEY,
			username      TEXT NOT NULL UNIQUE,
			role          TEXT NOT NULL CHECK (role IN ('admin','member')),
			recovery_hash TEXT NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL
		);
		INSERT INTO users (id, username, role, recovery_hash, created_at)
		VALUES ('u_1','Bob','admin','',1700000000),
		       ('u_2','bob','member','',1700000001),
		       ('u_3','BOB','member','',1700000002);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migration failed on a box with case-only username clashes: %v", err)
	}
	defer s.Close()

	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users; want 3", len(users))
	}
	seen := map[string]string{}
	for _, u := range users {
		key := strings.ToLower(u.DisplayName)
		if other, dupe := seen[key]; dupe {
			t.Errorf("display names %q and %q still clash", other, u.DisplayName)
		}
		seen[key] = u.DisplayName
		// Account names are untouched by any of this.
		if u.Username != "Bob" && u.Username != "bob" && u.Username != "BOB" {
			t.Errorf("username %q was modified", u.Username)
		}
	}
	// The oldest account keeps the name it had.
	first, err := s.GetUser("u_1")
	if err != nil {
		t.Fatalf("get u_1: %v", err)
	}
	if first.DisplayName != "Bob" {
		t.Errorf("oldest account renamed to %q; want Bob", first.DisplayName)
	}

	// Idempotent: a second startup has nothing to do and changes nothing.
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer again.Close()
	after, err := again.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for i, u := range after {
		if u.DisplayName != users[i].DisplayName {
			t.Errorf("second startup renamed %q to %q", users[i].DisplayName, u.DisplayName)
		}
	}
}
