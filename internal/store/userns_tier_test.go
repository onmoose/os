package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestUsernsTierRoundTrips checks the tier column: an unset tier is the host
// tier, each valid tier is kept through Get and List, and anything else is
// refused.
func TestUsernsTierRoundTrips(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if row, _ := s.Get("a"); row.UsernsTier != UsernsTierHost {
		t.Fatalf("unset tier = %q, want host", row.UsernsTier)
	}
	for i, tier := range []string{UsernsTierDefault, UsernsTierCaps, UsernsTierHost} {
		in := sample("t"+tier, "slug-"+tier)
		in.UsernsTier = tier
		if err := s.Create(in); err != nil {
			t.Fatalf("create %s: %v", tier, err)
		}
		if row, _ := s.Get(in.ID); row.UsernsTier != tier {
			t.Fatalf("tier %s read back as %q", tier, row.UsernsTier)
		}
		list, err := s.List()
		if err != nil || len(list) != i+2 {
			t.Fatalf("list: %v (%d rows)", err, len(list))
		}
	}
	bad := sample("b", "beta")
	bad.UsernsTier = "root"
	if err := s.Create(bad); err == nil {
		t.Fatal("Create accepted an unknown tier")
	}
}

// TestUsernsTierMigratesFromAnOlderDatabase opens an instances table from
// before the tier column, the way a running box has it. Every such instance
// was installed on a daemon with no remap, so it gets the host tier.
func TestUsernsTierMigratesFromAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := old.Exec(`
		CREATE TABLE instances (
			id          TEXT PRIMARY KEY,
			manifest_id TEXT NOT NULL,
			name        TEXT NOT NULL,
			slug        TEXT NOT NULL UNIQUE,
			version     TEXT NOT NULL,
			state       TEXT NOT NULL,
			mdns_name   TEXT NOT NULL DEFAULT '',
			owner_user_id TEXT NOT NULL DEFAULT '',
			scope         TEXT NOT NULL DEFAULT 'household',
			service_uid INTEGER NOT NULL DEFAULT 0,
			service_gid INTEGER NOT NULL DEFAULT 0,
			pending_recreate INTEGER NOT NULL DEFAULT 0,
			exposure    TEXT NOT NULL DEFAULT 'public',
			created_at  INTEGER NOT NULL
		);
		INSERT INTO instances (id, manifest_id, name, slug, version, state, created_at)
		VALUES ('i_1','whoami','Whoami','whoami','1','running',1700000000);
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
	row, err := s.Get("i_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.UsernsTier != UsernsTierHost {
		t.Fatalf("migrated tier = %q, want host", row.UsernsTier)
	}
}
