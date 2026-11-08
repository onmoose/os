package lifecycle

// Moving a managed database to a new major version during an update (#635),
// against the fakes: a good move, a failed update after the move, a failed
// restore on the new major, a name that is taken there, the rollback by hand,
// the crash recovery, the 7-day cleanup of the old database and uninstall.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

// upDatabase17 is upDatabase on the next Postgres major.
var upDatabase17 = strings.Replace(upDatabase, `"16"`, `"17"`, 1)

// installUpDB installs 1.0 of the test app with a Postgres 16 database.
func installUpDB(t *testing.T, e *testEnv) (store.Instance, store.ServiceGrant) {
	t.Helper()
	inst := installUp(t, e, upDatabase)
	grants, err := e.store.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 || grants[0].Version != "16" {
		t.Fatalf("grants after install = %+v %v", grants, err)
	}
	return inst, grants[0]
}

// oneOffs is the RunOneOff calls on one service image, as text.
func (e *testEnv) oneOffs(image string) []string {
	var out []string
	for _, c := range e.docker.Calls() {
		if (c.method == "RunOneOff" || c.method == "RunOneOffStream") && len(c.args) > 0 && c.args[0] == image {
			out = append(out, c.String())
		}
	}
	return out
}

func anyContains(calls []string, s string) bool {
	for _, c := range calls {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

func (e *testEnv) envFile(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.m.instanceDir(id), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateMovesTheDatabase(t *testing.T) {
	e := newTestEnv(t)
	inst, g := installUpDB(t, e)
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)

	e.m.CheckUpdates(context.Background())
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable {
		t.Fatalf("check = %+v, want available", r.Update)
	}
	var steps []string
	if err := e.m.Update(context.Background(), inst.ID, func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(strings.Join(steps, " "), "moving_databases") {
		t.Errorf("steps %v, want moving_databases", steps)
	}
	grants, _ := e.store.GetServiceGrants(inst.ID)
	if len(grants) != 1 || grants[0].Version != "17" || grants[0].DBName != g.DBName || grants[0].RoleName != g.RoleName || grants[0].Password != g.Password {
		t.Fatalf("grant after the move = %+v, want %s on 17 with the same names", grants, g.DBName)
	}
	retired, _ := e.store.GetRetiredServiceGrants(inst.ID)
	if len(retired) != 1 || retired[0].Version != "16" || retired[0].DBName != g.DBName {
		t.Fatalf("retired = %+v, want the 16 database kept", retired)
	}
	env := e.envFile(t, inst.ID)
	if !strings.Contains(env, "MOOSE_SERVICE_DATABASE_HOST=postgres-17.moose.internal") || strings.Contains(env, "postgres-16") {
		t.Fatalf(".env does not point at 17:\n%s", env)
	}
	on17 := e.oneOffs("postgres:17")
	if !anyContains(on17, `CREATE ROLE "`+g.RoleName+`"`) || !anyContains(on17, "pg_restore") {
		t.Fatalf("on 17: %v, want the role made and the dump restored", on17)
	}
	// The restore on 17 reads the dump the snapshot took on 16.
	if !anyContains(e.docker.streamed, "dump:"+g.DBName) {
		t.Fatalf("streamed %v, want the 16 dump fed to pg_restore", e.docker.streamed)
	}
	if anyContains(e.oneOffs("postgres:16"), "DROP DATABASE") {
		t.Fatal("the old database was dropped at once; it must stay 7 days")
	}
	if _, err := e.store.GetServiceInstance("postgres", "17"); err != nil {
		t.Fatalf("no postgres 17 instance: %v", err)
	}
}

func TestUpdateAfterTheMoveRollsBack(t *testing.T) {
	e := newTestEnv(t)
	inst, g := installUpDB(t, e)
	e.versionsMigrate(t, "2.0")
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)

	if err := e.m.Update(context.Background(), inst.ID, nil); err == nil {
		t.Fatal("want a failed update")
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" || r.UpdateFailure.Version != "2.0" || r.UpdateFailure.Kind != store.UpdateFailedHealth {
		t.Fatalf("row: state %q version %q failure %+v", r.State, r.Version, r.UpdateFailure)
	}
	grants, _ := e.store.GetServiceGrants(inst.ID)
	if len(grants) != 1 || grants[0].Version != "16" {
		t.Fatalf("grant after the rollback = %+v, want 16", grants)
	}
	if retired, _ := e.store.GetRetiredServiceGrants(inst.ID); len(retired) != 0 {
		t.Fatalf("retired = %+v, want none", retired)
	}
	if !anyContains(e.oneOffs("postgres:17"), `DROP DATABASE IF EXISTS "`+g.DBName+`"`) {
		t.Fatal("the database on 17 was not dropped")
	}
	if env := e.envFile(t, inst.ID); !strings.Contains(env, "postgres-16.moose.internal") {
		t.Fatalf(".env is not back on 16:\n%s", env)
	}
	if got := readSnapFile(t, e.dataFile(inst.ID, "app.db")); got != "v1 data" {
		t.Fatalf("data = %q, want the old data", got)
	}
}

func TestUpdateMoveRestoreFails(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installUpDB(t, e)
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	e.docker.runStream = func(image, _, _ string, args []string, stdin io.Reader, stdout io.Writer) (string, error) {
		if image == "postgres:17" {
			return "pg_restore: error", errors.New("exit status 1")
		}
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		if stdout != nil {
			_, _ = io.WriteString(stdout, "dump:"+args[len(args)-1])
		}
		return "", nil
	}
	err := e.m.Update(context.Background(), inst.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "could not move the app's data to Postgres 17") {
		t.Fatalf("update error = %v, want the move message", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" || !strings.Contains(r.UpdateFailure.Message, "Postgres 17") {
		t.Fatalf("row: state %q version %q failure %+v", r.State, r.Version, r.UpdateFailure)
	}
	if grants, _ := e.store.GetServiceGrants(inst.ID); grants[0].Version != "16" {
		t.Fatalf("grant = %+v, want 16", grants)
	}
	ups := 0
	for _, c := range e.docker.Calls() {
		if c.method == "ComposeUp" {
			ups++
		}
	}
	if ups < 2 {
		t.Fatalf("compose up %d times, want the old version started again", ups)
	}
}

// TestUpdateMoveRefusesATakenName: another app's grant holds the same name on
// the new major. The move must not drop it.
func TestUpdateMoveRefusesATakenName(t *testing.T) {
	e := newTestEnv(t)
	inst, g := installUpDB(t, e)
	other := store.Instance{ID: "other-id", ManifestID: "other", Name: "Other", Slug: "other", Version: "1", State: "running",
		OwnerUserID: "u_admin", Scope: store.ScopeHousehold, CreatedAt: time.Now()}
	if err := e.store.Create(other); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetServiceGrants(other.ID, []store.ServiceGrant{{LogicalName: "database", Kind: "postgres", Version: "17",
		DBName: g.DBName, RoleName: g.RoleName, Password: "x"}}); err != nil {
		t.Fatal(err)
	}
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	err := e.m.Update(context.Background(), inst.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "another app's database there has the same name") {
		t.Fatalf("update error = %v", err)
	}
	if anyContains(e.oneOffs("postgres:17"), "DROP") {
		t.Fatal("a DROP ran on 17, where another app's database has the name")
	}
	if grants, _ := e.store.GetServiceGrants(inst.ID); grants[0].Version != "16" {
		t.Fatalf("grant = %+v, want 16", grants)
	}
}

func TestRollbackByHandAfterAMove(t *testing.T) {
	e := newTestEnv(t)
	inst, g := installUpDB(t, e)
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	streamed := len(e.docker.streamed)
	if err := e.m.Rollback(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" {
		t.Fatalf("row: state %q version %q", r.State, r.Version)
	}
	grants, _ := e.store.GetServiceGrants(inst.ID)
	if len(grants) != 1 || grants[0].Version != "16" || grants[0].DBName != g.DBName {
		t.Fatalf("grant = %+v, want back on 16", grants)
	}
	if retired, _ := e.store.GetRetiredServiceGrants(inst.ID); len(retired) != 0 {
		t.Fatalf("retired = %+v, want none", retired)
	}
	if !anyContains(e.oneOffs("postgres:17"), `DROP DATABASE IF EXISTS "`+g.DBName+`"`) {
		t.Fatal("the database on 17 was not dropped")
	}
	// The old database is filled from the snapshot, on 16.
	if len(e.docker.streamed) <= streamed || !anyContains(e.oneOffs("postgres:16"), "pg_restore") {
		t.Fatal("the snapshot was not restored into the 16 database")
	}
	if env := e.envFile(t, inst.ID); !strings.Contains(env, "postgres-16.moose.internal") {
		t.Fatalf(".env is not back on 16:\n%s", env)
	}
}

// TestRollbackByHandNeedsTheOldDatabase: once the old database is gone, there
// is nothing to go back to.
func TestRollbackByHandNeedsTheOldDatabase(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installUpDB(t, e)
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := e.store.DeleteRetiredServiceGrant(inst.ID, "database", "16"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Rollback(context.Background(), inst.ID, nil); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("rollback = %v, want ErrNoRollback", err)
	}
	if grants, _ := e.store.GetServiceGrants(inst.ID); grants[0].Version != "17" {
		t.Fatalf("grant = %+v, want still 17", grants)
	}
}

// TestReconcileMovesTheDatabaseBack: the brain stopped after the move; the
// startup pass points the app back at the old major.
func TestReconcileMovesTheDatabaseBack(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst, g := installUpDB(t, e)
	id := inst.ID

	oldPins, _ := e.store.GetInstanceImages(id)
	if err := e.m.keepPrevious(id, oldPins); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.TakeSnapshot(ctx, id); err != nil {
		t.Fatal(err)
	}
	rec := updateRecord{From: "1.0", To: "2.0", PrevKept: true, SnapshotTaken: true, Writing: true,
		MovedServices: []movedService{{Service: "database", Kind: "postgres", From: "16", To: "17", Started: true}}}
	if err := e.m.writeUpdateRecord(id, rec); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MoveServiceGrant(id, "database", "17", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetState(id, "updating"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	r := e.row(t, id)
	if r.State != "running" || r.UpdateFailure.Kind != store.UpdateFailedInterrupted {
		t.Fatalf("row: state %q failure %+v", r.State, r.UpdateFailure)
	}
	if grants, _ := e.store.GetServiceGrants(id); grants[0].Version != "16" {
		t.Fatalf("grant = %+v, want back on 16", grants)
	}
	if retired, _ := e.store.GetRetiredServiceGrants(id); len(retired) != 0 {
		t.Fatalf("retired = %+v, want none", retired)
	}
	if !anyContains(e.oneOffs("postgres:17"), `DROP DATABASE IF EXISTS "`+g.DBName+`"`) {
		t.Fatal("the database on 17 was not dropped")
	}
}

func TestPruneDropsTheOldDatabaseAfter7Days(t *testing.T) {
	e := newTestEnv(t)
	inst, g := installUpDB(t, e)
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	e.m.PruneSnapshots(time.Now().Add(time.Hour))
	if retired, _ := e.store.GetRetiredServiceGrants(inst.ID); len(retired) != 1 {
		t.Fatalf("retired after an hour = %+v, want it kept", retired)
	}
	if anyContains(e.oneOffs("postgres:16"), "DROP DATABASE") {
		t.Fatal("the old database was dropped before its 7 days")
	}
	e.m.PruneSnapshots(time.Now().Add(SnapshotKeep + time.Minute))
	if retired, _ := e.store.GetRetiredServiceGrants(inst.ID); len(retired) != 0 {
		t.Fatalf("retired after 7 days = %+v, want none", retired)
	}
	if !anyContains(e.oneOffs("postgres:16"), `DROP DATABASE IF EXISTS "`+g.DBName+`"`) {
		t.Fatal("the old database was not dropped after 7 days")
	}
	if grants, _ := e.store.GetServiceGrants(inst.ID); grants[0].Version != "17" {
		t.Fatalf("grant = %+v, want 17", grants)
	}
}

func TestUninstallDropsTheOldDatabase(t *testing.T) {
	e := newTestEnv(t)
	inst, g := installUpDB(t, e)
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	for _, image := range []string{"postgres:16", "postgres:17"} {
		if !anyContains(e.oneOffs(image), `DROP DATABASE IF EXISTS "`+g.DBName+`"`) {
			t.Errorf("uninstall did not drop the database on %s", image)
		}
	}
}

// TestCheckClassifiesALegacyServiceMajor: a row a brain from before #635
// classified as service_major is classified again.
func TestCheckClassifiesALegacyServiceMajor(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installUpDB(t, e)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	if err := e.store.SetInstanceUpdateCheck(inst.ID, store.UpdateCheck{Version: "2.0", Status: store.UpdateServiceMajor, Reason: "old"}); err != nil {
		t.Fatal(err)
	}
	e.m.CheckUpdates(context.Background())
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable {
		t.Fatalf("check = %+v, want available", r.Update)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"15", "18", -1}, {"18", "15", 1}, {"16", "16", 0},
		{"8.0", "8.4", -1}, {"10.11", "11.4", -1}, {"11.4", "10.11", 1},
		{"9", "10", -1}, {"8", "8.0", -1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestMoveNamesAreHeldUntilTheUpdateEnds: two updates that run at once cannot
// both take the same names on a new major (#635). The second one is refused
// until the first lets go.
func TestMoveNamesAreHeldUntilTheUpdateEnds(t *testing.T) {
	e := newTestEnv(t)
	if taken, err := e.m.reserveMoveNames("a", "postgres", "18", "app_ab12", "app_ab12"); err != nil || taken {
		t.Fatalf("first hold = %v %v, want free", taken, err)
	}
	if taken, _ := e.m.reserveMoveNames("a", "postgres", "18", "app_ab12", "app_ab12"); taken {
		t.Fatal("the same update was refused its own names")
	}
	if taken, _ := e.m.reserveMoveNames("b", "postgres", "18", "app_ab12", "app_ab12"); !taken {
		t.Fatal("a second update got names the first one holds")
	}
	if taken, _ := e.m.reserveMoveNames("b", "postgres", "17", "app_ab12", "app_ab12"); taken {
		t.Fatal("the names were refused on another major")
	}
	e.m.releaseMoveNames("a")
	if taken, _ := e.m.reserveMoveNames("b", "postgres", "18", "app_ab12", "app_ab12"); taken {
		t.Fatal("the names are still held after the first update ended")
	}
}

// TestUpdateAlwaysLetsTheNamesGo: an update that moved a database, good or
// rolled back, holds no names once it has ended.
func TestUpdateAlwaysLetsTheNamesGo(t *testing.T) {
	for _, bad := range []bool{false, true} {
		e := newTestEnv(t)
		inst, _ := installUpDB(t, e)
		if bad {
			e.versionsMigrate(t, "2.0")
		} else {
			e.versionsMigrate(t)
		}
		e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
		_ = e.m.Update(context.Background(), inst.ID, nil)
		if n := len(e.m.moveNames); n != 0 {
			t.Fatalf("bad=%v: %d names still held after the update", bad, n)
		}
	}
}

// TestUpdateMoveRefusedWhenTheDriveIsFull: the snapshot fits, but the copy
// of the database on the new major would leave the drive too full. Nothing is
// made on the new major, and the old version runs again on its database.
func TestUpdateMoveRefusedWhenTheDriveIsFull(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installUpDB(t, e)
	e.versionsMigrate(t)
	e.publish(t, "2.0", upImage2, upDigest2, upDatabase17)
	// The database is 10 KB. The snapshots dir has room; the state dir, which
	// holds the services, is 94% full, so 10 KB more crosses 95%.
	e.docker.runOneOff = func(_, _, _ string, args []string) (string, error) {
		if q := strings.Join(args, " "); strings.Contains(q, "pg_database_size") {
			return "10240", nil
		}
		return "", nil
	}
	e.m.diskSpace = func(dir string) (uint64, uint64, error) {
		if dir == e.m.stateDir {
			return 6 << 10, 100 << 10, nil
		}
		return 90 << 10, 100 << 10, nil
	}
	err := e.m.Update(context.Background(), inst.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "not enough free space on the drive to move the app's data to Postgres") {
		t.Fatalf("update error = %v, want the space message", err)
	}
	if len(e.oneOffs("postgres:17")) != 0 {
		t.Fatalf("something ran on 17: %v", e.oneOffs("postgres:17"))
	}
	r := e.row(t, inst.ID)
	if r.State != "running" || r.Version != "1.0" {
		t.Fatalf("row: state %q version %q", r.State, r.Version)
	}
	if grants, _ := e.store.GetServiceGrants(inst.ID); grants[0].Version != "16" {
		t.Fatalf("grant = %+v, want 16", grants)
	}
}
