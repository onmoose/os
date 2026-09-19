package store

import (
	"path/filepath"
	"testing"
	"time"
)

// open returns a fresh Store backed by a tmp-dir SQLite file. Each test gets
// its own DB; modernc.org/sqlite is fast enough that this beats sharing.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sample(id, slug string) Instance {
	return Instance{
		ID: id, ManifestID: "whoami", Name: "Whoami", Slug: slug,
		Version: "1.10", State: "installing",
		OwnerUserID: "u_owner", Scope: ScopeHousehold,
		CreatedAt: time.Unix(1_700_000_000, 0),
	}
}

func TestInstanceSecretsRoundtripAndCascade(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []InstanceSecret{{Name: "auth", Value: "abc123"}, {Name: "session", Value: "def456"}}
	if err := s.SetInstanceSecrets("a", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetInstanceSecrets("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("roundtrip: got %v, want %v", got, want)
	}
	// Deleting the instance cascades the secrets away.
	if err := s.Delete("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.GetInstanceSecrets("a"); len(got) != 0 {
		t.Fatalf("secrets survived instance delete: %v", got)
	}
}

func TestInstanceConfigRoundtripAndCascade(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-123", Secret: true},
		{AppEnv: "OPENAI_MODEL", Value: "gpt-4o", Secret: false},
	}
	if err := s.SetInstanceConfig("a", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetInstanceConfig("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("roundtrip: got %v, want %v", got, want)
	}
	// SetInstanceConfig fully replaces the prior set.
	if err := s.SetInstanceConfig("a", []InstanceConfig{{AppEnv: "ENDPOINT", Value: "https://x"}}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = s.GetInstanceConfig("a")
	if len(got) != 1 || got[0].AppEnv != "ENDPOINT" {
		t.Fatalf("replace did not clear prior values: %v", got)
	}
	// Deleting the instance cascades the config away.
	if err := s.Delete("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.GetInstanceConfig("a"); len(got) != 0 {
		t.Fatalf("config survived instance delete: %v", got)
	}
}

func TestServiceInstanceCRUD(t *testing.T) {
	s := open(t)
	if _, err := s.GetServiceInstance("postgres", "15"); err != ErrNotFound {
		t.Fatalf("empty get: err = %v, want ErrNotFound", err)
	}
	si := ServiceInstance{
		Kind: "postgres", Version: "15", SuperuserPassword: "su-pw",
		State: "running", CreatedAt: time.Unix(1_700_000_000, 0),
	}
	if err := s.CreateServiceInstance(si); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateServiceInstance(si); err != ErrConflict {
		t.Fatalf("duplicate create: err = %v, want ErrConflict", err)
	}
	got, err := s.GetServiceInstance("postgres", "15")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SuperuserPassword != "su-pw" || got.State != "running" {
		t.Fatalf("get = %+v", got)
	}
	list, err := s.ListServiceInstances()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, err %v", list, err)
	}
}

func TestServiceGrantsRoundtripAndCascade(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []ServiceGrant{{
		LogicalName: "database", Kind: "postgres", Version: "15",
		DBName: "kan_a1b2", RoleName: "kan_a1b2", Password: "pw",
	}}
	if err := s.SetServiceGrants("a", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetServiceGrants("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("roundtrip: got %v, want %v", got, want)
	}
	// Deleting the instance cascades the grants away.
	if err := s.Delete("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.GetServiceGrants("a"); len(got) != 0 {
		t.Fatalf("grants survived instance delete: %v", got)
	}
}

func TestCreateRejectsInvalidScope(t *testing.T) {
	s := open(t)
	i := sample("a", "alpha")
	i.Scope = "bogus"
	if err := s.Create(i); err == nil {
		t.Fatal("Create accepted an out-of-range scope, want error")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "moose.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Second open runs migrate again on a populated DB; must not error or
	// truncate data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	if _, err := s2.Get("a"); err != nil {
		t.Fatalf("row lost across reopen: %v", err)
	}
}

func TestCreateGetListDelete(t *testing.T) {
	s := open(t)
	if _, err := s.Get("missing"); err != ErrNotFound {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := s.Create(sample("b", "beta")); err != nil {
		t.Fatalf("create b: %v", err)
	}
	got, err := s.Get("a")
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	if got.Slug != "alpha" || got.State != "installing" {
		t.Fatalf("get a = %+v", got)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if err := s.Delete("a"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	if _, err := s.Get("a"); err != ErrNotFound {
		t.Fatalf("Get(a) after delete = %v, want ErrNotFound", err)
	}
}

func TestSetServiceIdentity(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Fresh rows carry no identity (0 = none).
	row, err := s.Get("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.ServiceUID != 0 || row.ServiceGID != 0 {
		t.Fatalf("fresh row identity = %d:%d, want 0:0", row.ServiceUID, row.ServiceGID)
	}
	if err := s.SetServiceIdentity("a", 2100, 2100); err != nil {
		t.Fatalf("set: %v", err)
	}
	row, err = s.Get("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.ServiceUID != 2100 || row.ServiceGID != 2100 {
		t.Fatalf("roundtrip: got %d:%d, want 2100:2100", row.ServiceUID, row.ServiceGID)
	}
	// Identity must also survive the List path (same scan).
	insts, err := s.List()
	if err != nil || len(insts) != 1 {
		t.Fatalf("list: %v (%d rows)", err, len(insts))
	}
	if insts[0].ServiceUID != 2100 || insts[0].ServiceGID != 2100 {
		t.Fatalf("list roundtrip: got %d:%d, want 2100:2100", insts[0].ServiceUID, insts[0].ServiceGID)
	}
	if err := s.SetServiceIdentity("missing", 2101, 2101); err != ErrNotFound {
		t.Fatalf("set on missing instance: err = %v, want ErrNotFound", err)
	}
}

func TestSetInstancePendingRecreate(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Fresh rows are not pending-recreate.
	row, err := s.Get("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.PendingRecreate {
		t.Fatalf("fresh row PendingRecreate = true, want false")
	}
	if err := s.SetInstancePendingRecreate("a", true); err != nil {
		t.Fatalf("set: %v", err)
	}
	row, err = s.Get("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !row.PendingRecreate {
		t.Fatalf("flag did not round-trip through Get")
	}
	// Must also survive the List path (same scan).
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v (%d rows)", err, len(list))
	}
	if !list[0].PendingRecreate {
		t.Fatalf("flag did not round-trip through List")
	}
	// Clearing takes.
	if err := s.SetInstancePendingRecreate("a", false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if row, _ := s.Get("a"); row.PendingRecreate {
		t.Fatalf("clear did not take")
	}
	if err := s.SetInstancePendingRecreate("missing", true); err != ErrNotFound {
		t.Fatalf("set on missing instance: err = %v, want ErrNotFound", err)
	}
}

func TestInstanceExposure(t *testing.T) {
	s := open(t)
	// A Create with no exposure normalizes to public (the appliance default and the
	// value migrated rows carry); it round-trips through both Get and List.
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if row, _ := s.Get("a"); row.Exposure != ExposurePublic {
		t.Fatalf("default exposure = %q, want public", row.Exposure)
	}
	if err := s.SetInstanceExposure("a", ExposureRestricted); err != nil {
		t.Fatalf("set restricted: %v", err)
	}
	if row, _ := s.Get("a"); row.Exposure != ExposureRestricted {
		t.Fatalf("exposure did not round-trip through Get")
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Exposure != ExposureRestricted {
		t.Fatalf("exposure did not round-trip through List: %v %+v", err, list)
	}
	// Invalid values are rejected on both the setter and Create.
	if err := s.SetInstanceExposure("a", "bogus"); err == nil {
		t.Error("SetInstanceExposure accepted an out-of-range value")
	}
	if err := s.SetInstanceExposure("missing", ExposurePublic); err != ErrNotFound {
		t.Errorf("set on missing instance: err = %v, want ErrNotFound", err)
	}
	bad := sample("b", "beta")
	bad.Exposure = "bogus"
	if err := s.Create(bad); err == nil {
		t.Error("Create accepted an out-of-range exposure")
	}
	// An explicit restricted at Create is honored (the hosted install path).
	r := sample("c", "gamma")
	r.Exposure = ExposureRestricted
	if err := s.Create(r); err != nil {
		t.Fatalf("create restricted: %v", err)
	}
	if row, _ := s.Get("c"); row.Exposure != ExposureRestricted {
		t.Fatalf("explicit restricted not persisted at Create")
	}
}

func TestSetStateOnMissingInstanceErrors(t *testing.T) {
	s := open(t)
	if err := s.SetState("nope", "running"); err != ErrNotFound {
		t.Fatalf("SetState(missing) = %v, want ErrNotFound", err)
	}
}

func TestSlugTaken(t *testing.T) {
	s := open(t)
	taken, err := s.SlugTaken("alpha")
	if err != nil || taken {
		t.Fatalf("SlugTaken(empty)=%v,%v", taken, err)
	}
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	taken, err = s.SlugTaken("alpha")
	if err != nil || !taken {
		t.Fatalf("SlugTaken(alpha)=%v,%v", taken, err)
	}
}

func TestSetInstanceImagesReplacesAtomically(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := []InstanceImage{
		{Service: "web", Image: "nginx:1", Digest: "sha256:aaa"},
		{Service: "db", Image: "postgres:16", Digest: "sha256:bbb"},
	}
	if err := s.SetInstanceImages("a", first); err != nil {
		t.Fatalf("set first: %v", err)
	}
	// Replace with a smaller set — old rows must disappear.
	second := []InstanceImage{
		{Service: "web", Image: "nginx:2", Digest: "sha256:ccc"},
	}
	if err := s.SetInstanceImages("a", second); err != nil {
		t.Fatalf("set second: %v", err)
	}
	got, err := s.GetInstanceImages("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || got[0].Service != "web" || got[0].Image != "nginx:2" {
		t.Fatalf("after replace got %+v", got)
	}
}

func TestGetInstanceImagesOrderedByService(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Insert out of order; expect sorted by service.
	if err := s.SetInstanceImages("a", []InstanceImage{
		{Service: "zeta", Image: "z", Digest: "sha256:z"},
		{Service: "alpha", Image: "a", Digest: "sha256:a"},
		{Service: "mike", Image: "m", Digest: "sha256:m"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetInstanceImages("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := []string{"alpha", "mike", "zeta"}
	for i, w := range want {
		if got[i].Service != w {
			t.Fatalf("svc[%d]=%q want %q (full: %+v)", i, got[i].Service, w, got)
		}
	}
}

func TestDeleteCascadesToInstanceImages(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetInstanceImages("a", []InstanceImage{
		{Service: "web", Image: "nginx", Digest: "sha256:x"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := s.GetInstanceImages("a")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("FK cascade failed: %+v", got)
	}
}

func sampleUser(id, username, role string) User {
	return User{
		ID: id, Username: username, Role: role,
		CreatedAt: time.Unix(1_700_000_000, 0),
	}
}

func TestUserCRUD(t *testing.T) {
	s := open(t)
	if has, err := s.HasAnyUser(); err != nil || has {
		t.Fatalf("HasAnyUser on fresh store = %v, %v; want false, nil", has, err)
	}
	if err := s.CreateUser(sampleUser("u1", "andrei", RoleAdmin)); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if has, _ := s.HasAnyUser(); !has {
		t.Fatal("HasAnyUser after insert = false")
	}

	got, err := s.GetUser("u1")
	if err != nil || got.Username != "andrei" || got.Role != RoleAdmin {
		t.Fatalf("GetUser = %+v, %v", got, err)
	}
	if got, err := s.GetUserByUsername("andrei"); err != nil || got.ID != "u1" {
		t.Fatalf("GetUserByUsername = %+v, %v", got, err)
	}

	if _, err := s.GetUser("missing"); err != ErrNotFound {
		t.Fatalf("GetUser(missing) = %v, want ErrNotFound", err)
	}

	// Duplicate username -> ErrConflict.
	if err := s.CreateUser(sampleUser("u2", "andrei", RoleMember)); err != ErrConflict {
		t.Fatalf("CreateUser dup = %v, want ErrConflict", err)
	}

	// Invalid role rejected by CHECK constraint.
	if err := s.CreateUser(sampleUser("u3", "weirdo", "superuser")); err == nil {
		t.Fatal("CreateUser with bogus role = nil; want CHECK error")
	}

	if err := s.DeleteUser("u1"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := s.DeleteUser("u1"); err != ErrNotFound {
		t.Fatalf("DeleteUser(missing) = %v, want ErrNotFound", err)
	}
}

func TestCreateFirstAdmin(t *testing.T) {
	s := open(t)
	if err := s.CreateFirstAdmin(sampleUser("u1", "andrei", RoleAdmin)); err != nil {
		t.Fatalf("first admin: %v", err)
	}
	// Second call must lose: someone already bootstrapped.
	if err := s.CreateFirstAdmin(sampleUser("u2", "cindy", RoleAdmin)); err != ErrConflict {
		t.Fatalf("second first-admin = %v, want ErrConflict", err)
	}
	// Role enforcement: member is not a "first admin".
	s2 := open(t)
	if err := s2.CreateFirstAdmin(sampleUser("u1", "andrei", RoleMember)); err == nil {
		t.Fatal("CreateFirstAdmin with member role = nil; want error")
	}
}

func TestSessionCRUD(t *testing.T) {
	s := open(t)
	if err := s.CreateUser(sampleUser("u1", "andrei", RoleAdmin)); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Unix(1_700_000_100, 0)
	sess := Session{Token: "tok-1", UserID: "u1", CreatedAt: now, LastSeenAt: now}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession("tok-1")
	if err != nil || got.UserID != "u1" || !got.CreatedAt.Equal(now) {
		t.Fatalf("GetSession = %+v, %v", got, err)
	}

	later := now.Add(5 * time.Minute)
	if err := s.TouchSession("tok-1", later); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	got, _ = s.GetSession("tok-1")
	if !got.LastSeenAt.Equal(later) {
		t.Fatalf("last_seen_at = %v, want %v", got.LastSeenAt, later)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("created_at moved on touch: %v", got.CreatedAt)
	}

	if _, err := s.GetSession("ghost"); err != ErrNotFound {
		t.Fatalf("GetSession(ghost) = %v, want ErrNotFound", err)
	}

	if err := s.DeleteSession("tok-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession("tok-1"); err != ErrNotFound {
		t.Fatalf("after DeleteSession: %v", err)
	}
}

// The hosted forward-auth token (issue #305) is stamped on the session row and
// resolved back through a reverse lookup that must never match the empty-string
// default every non-hosted session carries.
func TestSessionForwardAuthToken(t *testing.T) {
	s := open(t)
	if err := s.CreateUser(sampleUser("u1", "andrei", RoleAdmin)); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Unix(1_700_000_100, 0)
	if err := s.CreateSession(Session{Token: "tok-1", UserID: "u1", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// A fresh session carries no forward-auth token, and the empty lookup must not
	// resolve it (the '' default must never be matchable).
	if _, err := s.GetSessionByForwardAuthToken(""); err != ErrNotFound {
		t.Fatalf("empty fa_token lookup = %v, want ErrNotFound", err)
	}
	if _, err := s.GetSessionByForwardAuthToken("fa-1"); err != ErrNotFound {
		t.Fatalf("unstamped fa_token lookup = %v, want ErrNotFound", err)
	}

	if err := s.SetSessionForwardAuthToken("tok-1", "fa-1"); err != nil {
		t.Fatalf("SetSessionForwardAuthToken: %v", err)
	}
	got, err := s.GetSessionByForwardAuthToken("fa-1")
	if err != nil || got.Token != "tok-1" || got.UserID != "u1" {
		t.Fatalf("GetSessionByForwardAuthToken = %+v, %v", got, err)
	}

	// Deleting the session drops the token with it.
	if err := s.DeleteSession("tok-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSessionByForwardAuthToken("fa-1"); err != ErrNotFound {
		t.Fatalf("fa_token after DeleteSession = %v, want ErrNotFound", err)
	}
}

func TestDeleteUserCascadesSessions(t *testing.T) {
	s := open(t)
	if err := s.CreateUser(sampleUser("u1", "andrei", RoleAdmin)); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Unix(1_700_000_100, 0)
	for _, tok := range []string{"t1", "t2", "t3"} {
		if err := s.CreateSession(Session{Token: tok, UserID: "u1", CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateSession %s: %v", tok, err)
		}
	}
	if err := s.DeleteUser("u1"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	for _, tok := range []string{"t1", "t2", "t3"} {
		if _, err := s.GetSession(tok); err != ErrNotFound {
			t.Fatalf("session %s survived user delete (err=%v); FK cascade broken", tok, err)
		}
	}
}

func TestDeleteSessionsForUser(t *testing.T) {
	s := open(t)
	if err := s.CreateUser(sampleUser("u1", "andrei", RoleAdmin)); err != nil {
		t.Fatalf("CreateUser u1: %v", err)
	}
	if err := s.CreateUser(sampleUser("u2", "cindy", RoleMember)); err != nil {
		t.Fatalf("CreateUser u2: %v", err)
	}
	now := time.Unix(1_700_000_100, 0)
	_ = s.CreateSession(Session{Token: "a1", UserID: "u1", CreatedAt: now, LastSeenAt: now})
	_ = s.CreateSession(Session{Token: "a2", UserID: "u1", CreatedAt: now, LastSeenAt: now})
	_ = s.CreateSession(Session{Token: "b1", UserID: "u2", CreatedAt: now, LastSeenAt: now})

	if err := s.DeleteSessionsForUser("u1"); err != nil {
		t.Fatalf("DeleteSessionsForUser: %v", err)
	}
	if _, err := s.GetSession("a1"); err != ErrNotFound {
		t.Fatalf("u1 session a1 = %v; want ErrNotFound", err)
	}
	if _, err := s.GetSession("b1"); err != nil {
		t.Fatalf("u2 session b1 collateral damage: %v", err)
	}
}

func TestSetMDNSName(t *testing.T) {
	s := open(t)
	if err := s.Create(sample("a", "alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetMDNSName("a", "alpha.local"); err != nil {
		t.Fatalf("set mdns: %v", err)
	}
	got, _ := s.Get("a")
	if got.MDNSName != "alpha.local" {
		t.Fatalf("mdns = %q", got.MDNSName)
	}
}

// --- audit_events ---

func sampleAuditEvent(actorUserID, action string) AuditEvent {
	return AuditEvent{
		TS:          1_700_000_000_000,
		ActorUserID: actorUserID,
		ActorRole:   "admin",
		Action:      action,
		TargetKind:  "app",
		TargetID:    "inst_abc",
		SourceIP:    "192.168.1.1",
		Success:     true,
		Metadata:    `{"slug":"whoami"}`,
	}
}

func TestAuditEventsInsertAndList(t *testing.T) {
	s := open(t)
	u := User{ID: "u1", Username: "alice", Role: RoleAdmin, CreatedAt: time.Unix(1_700_000_000, 0)}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	for _, action := range []string{"login.success", "app.install", "app.uninstall"} {
		if err := s.InsertAuditEvent(sampleAuditEvent("u1", action)); err != nil {
			t.Fatalf("InsertAuditEvent %s: %v", action, err)
		}
	}

	rows, err := s.ListAuditEvents(AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// Newest first — last inserted has the highest id.
	if rows[0].Action != "app.uninstall" {
		t.Fatalf("first row action = %q, want app.uninstall", rows[0].Action)
	}
	if rows[0].ActorUserID != "u1" {
		t.Fatalf("actor_user_id = %q, want u1", rows[0].ActorUserID)
	}
	if !rows[0].Success {
		t.Fatal("success should be true")
	}
	if rows[0].Metadata != `{"slug":"whoami"}` {
		t.Fatalf("metadata = %q", rows[0].Metadata)
	}
}

func TestAuditEventsAppendOnlyTriggersBlockUpdate(t *testing.T) {
	s := open(t)
	u := User{ID: "u1", Username: "alice", Role: RoleAdmin, CreatedAt: time.Unix(1_700_000_000, 0)}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.InsertAuditEvent(sampleAuditEvent("u1", "login.success")); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}

	_, err := s.db.Exec(`UPDATE audit_events SET action = 'tampered' WHERE id = 1`)
	if err == nil {
		t.Fatal("UPDATE on audit_events should be blocked by trigger")
	}
}

func TestAuditEventsAppendOnlyTriggersBlockDelete(t *testing.T) {
	s := open(t)
	u := User{ID: "u1", Username: "alice", Role: RoleAdmin, CreatedAt: time.Unix(1_700_000_000, 0)}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.InsertAuditEvent(sampleAuditEvent("u1", "login.success")); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}

	_, err := s.db.Exec(`DELETE FROM audit_events WHERE id = 1`)
	if err == nil {
		t.Fatal("DELETE on audit_events should be blocked by trigger")
	}
}

func TestAuditEventsActorUserIDSetNullOnUserDelete(t *testing.T) {
	s := open(t)
	u := User{ID: "u1", Username: "alice", Role: RoleAdmin, CreatedAt: time.Unix(1_700_000_000, 0)}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.InsertAuditEvent(sampleAuditEvent("u1", "login.success")); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}

	// Deleting the user must not delete the audit row — ON DELETE SET NULL.
	if err := s.DeleteUser("u1"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	rows, err := s.ListAuditEvents(AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after user delete, want 1 (history preserved)", len(rows))
	}
	if rows[0].ActorUserID != "" {
		t.Fatalf("actor_user_id should be NULL after user delete, got %q", rows[0].ActorUserID)
	}
}

func TestAuditEventsSystemEventNullActor(t *testing.T) {
	s := open(t)
	evt := AuditEvent{
		TS:        1_700_000_000_000,
		ActorRole: "system",
		Action:    "setup.complete",
		Success:   true,
	}
	if err := s.InsertAuditEvent(evt); err != nil {
		t.Fatalf("InsertAuditEvent system: %v", err)
	}
	rows, err := s.ListAuditEvents(AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(rows) != 1 || rows[0].ActorUserID != "" {
		t.Fatalf("system event: actor_user_id should be empty, got %q", rows[0].ActorUserID)
	}
}

func TestAuditEventsMemberVisibilityFilter(t *testing.T) {
	s := open(t)
	u1 := User{ID: "u1", Username: "alice", Role: RoleAdmin, CreatedAt: time.Unix(1_700_000_000, 0)}
	u2 := User{ID: "u2", Username: "bob", Role: RoleMember, CreatedAt: time.Unix(1_700_000_001, 0)}
	_ = s.CreateUser(u1)
	_ = s.CreateUser(u2)

	// u1 installs an app.
	_ = s.InsertAuditEvent(AuditEvent{
		TS: 1, ActorUserID: "u1", ActorRole: "admin",
		Action: "app.install", TargetKind: "app", TargetID: "inst1", Success: true,
	})
	// u2 logs in.
	_ = s.InsertAuditEvent(AuditEvent{
		TS: 2, ActorUserID: "u2", ActorRole: "member",
		Action: "login.success", Success: true,
	})
	// Admin resets u2's password — target is u2.
	_ = s.InsertAuditEvent(AuditEvent{
		TS: 3, ActorUserID: "u1", ActorRole: "admin",
		Action: "login.failure", TargetKind: "user", TargetID: "u2", Success: false,
	})

	// Admin sees all 3 rows.
	all, err := s.ListAuditEvents(AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("admin sees %d rows, want 3", len(all))
	}

	// u2 sees only: their own login.success + the event targeting them.
	u2rows, err := s.ListAuditEvents(AuditFilter{ActorUserID: "u2", Limit: 10})
	if err != nil {
		t.Fatalf("member list: %v", err)
	}
	if len(u2rows) != 2 {
		t.Fatalf("member sees %d rows, want 2", len(u2rows))
	}

	// u1's install is not visible to u2.
	for _, r := range u2rows {
		if r.Action == "app.install" {
			t.Fatal("u2 should not see u1's app.install")
		}
	}
}

func TestAuditEventsAfterIDCursor(t *testing.T) {
	s := open(t)
	u := User{ID: "u1", Username: "alice", Role: RoleAdmin, CreatedAt: time.Unix(1_700_000_000, 0)}
	_ = s.CreateUser(u)

	for i := 0; i < 5; i++ {
		_ = s.InsertAuditEvent(sampleAuditEvent("u1", "login.success"))
	}

	// Get all — should be 5 newest first (id 5,4,3,2,1).
	all, _ := s.ListAuditEvents(AuditFilter{Limit: 10})
	if len(all) != 5 {
		t.Fatalf("want 5, got %d", len(all))
	}
	pivotID := all[2].ID // id = 3; cursor should return ids 2, 1

	page2, err := s.ListAuditEvents(AuditFilter{AfterID: pivotID, Limit: 10})
	if err != nil {
		t.Fatalf("cursor page: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("cursor page: want 2 rows, got %d", len(page2))
	}
	for _, r := range page2 {
		if r.ID >= pivotID {
			t.Fatalf("row id %d should be < cursor %d", r.ID, pivotID)
		}
	}
}

func TestUpdateRoleAndCountAdmins(t *testing.T) {
	s := open(t)
	if err := s.CreateUser(sampleUser("u1", "alice", RoleAdmin)); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if err := s.CreateUser(sampleUser("u2", "bob", RoleMember)); err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	n, err := s.CountAdmins()
	if err != nil || n != 1 {
		t.Fatalf("CountAdmins = %d, %v; want 1, nil", n, err)
	}

	// Promote bob to admin.
	if err := s.UpdateRole("u2", RoleAdmin); err != nil {
		t.Fatalf("UpdateRole promote: %v", err)
	}
	n, _ = s.CountAdmins()
	if n != 2 {
		t.Fatalf("CountAdmins after promote = %d; want 2", n)
	}

	// Demote alice to member.
	if err := s.UpdateRole("u1", RoleMember); err != nil {
		t.Fatalf("UpdateRole demote: %v", err)
	}
	n, _ = s.CountAdmins()
	if n != 1 {
		t.Fatalf("CountAdmins after demote = %d; want 1", n)
	}

	// ErrNotFound on unknown id.
	if err := s.UpdateRole("ghost", RoleAdmin); err != ErrNotFound {
		t.Fatalf("UpdateRole(ghost) = %v, want ErrNotFound", err)
	}

	// Invalid role rejected.
	if err := s.UpdateRole("u1", "superuser"); err == nil {
		t.Fatal("UpdateRole(bogus role) = nil; want error")
	}
}

// Users have string IDs (TEXT in the schema); actor_user_id must round-trip
// non-integer-shaped values byte-identically. Guards against the schema
// regressing to INTEGER, which silently works for short ids like "u1"
// because of SQLite affinity but mangles real UUIDs.
func TestAuditEventsActorUserIDIsText(t *testing.T) {
	s := open(t)
	const uuid = "01HFGZ8XK4-bob-2026"
	u := User{ID: uuid, Username: "bob", Role: RoleMember, CreatedAt: time.Unix(1_700_000_000, 0)}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.InsertAuditEvent(sampleAuditEvent(uuid, "login.success")); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	rows, err := s.ListAuditEvents(AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(rows) != 1 || rows[0].ActorUserID != uuid {
		t.Fatalf("actor_user_id round-trip: got %q, want %q", rows[0].ActorUserID, uuid)
	}
}
