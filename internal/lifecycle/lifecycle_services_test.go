package lifecycle

// Managed-service provisioning (SERVICE_PROVISIONING.md # Tier 1): an app that
// declares `services.database: {type: postgres}` gets a per-app database+role in
// the shared Postgres instance, with credentials injected as
// MOOSE_SERVICE_DATABASE_*. Driven against the fake docker driver (whose
// RunOneOff default succeeds and ContainerHealth defaults to "healthy", so the
// readiness poll passes and the psql CREATE one-shot returns ok).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

const dbManifest = `
id: dbapp
manifest_version: 1
name: DB App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [dbapp]
services:
  database:
    type: postgres
    version: "15"
permissions:
  internet: false
  lan: false
`

const dbCompose = `
services:
  app:
    image: traefik/whoami:v1.10.3
    environment:
      POSTGRES_URL: ${MOOSE_SERVICE_DATABASE_DSN}
`

// installDBApp installs dbapp (manifest id overridable so two can coexist in one
// env) and returns the instance.
func installDBApp(t *testing.T, e *testEnv, id string) store.Instance {
	t.Helper()
	return installDBAppKind(t, e, id, "postgres", "15")
}

// installDBAppKind is installDBApp with the declared service type/version
// overridable, so the MySQL-family tests share the same fixture.
func installDBAppKind(t *testing.T, e *testEnv, id, kind, version string) store.Instance {
	t.Helper()
	man := strings.Replace(dbManifest, "id: dbapp", "id: "+id, 1)
	man = strings.Replace(man, "preferred_slugs: [dbapp]", "preferred_slugs: ["+id+"]", 1)
	man = strings.Replace(man, "type: postgres", "type: "+kind, 1)
	man = strings.Replace(man, `version: "15"`, `version: "`+version+`"`, 1)
	e.writeCatalogApp(t, id, dbCompose, man)
	e.docker.digests[testImage] = testDigest
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, id),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install %s: %v", id, err)
	}
	return inst
}

// hasCall reports whether any recorded docker call's flattened form contains sub.
func (f *fakeDocker) hasCall(sub string) bool {
	for _, c := range f.Calls() {
		if strings.Contains(c.String(), sub) {
			return true
		}
	}
	return false
}

func (f *fakeDocker) countMethod(name string) int {
	n := 0
	for _, m := range f.methods() {
		if m == name {
			n++
		}
	}
	return n
}

func TestInstallProvisionsPostgres(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBApp(t, e, "dbapp")

	// A shared Postgres-15 service instance was recorded and started.
	if _, err := e.store.GetServiceInstance("postgres", "15"); err != nil {
		t.Fatalf("service instance not recorded: %v", err)
	}
	if !e.docker.hasCall("ServiceUp(") {
		t.Fatalf("ServiceUp never called; calls: %v", e.docker.Calls())
	}

	// A per-app grant was persisted.
	grants, err := e.store.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	g := grants[0]
	if g.LogicalName != "database" || g.Kind != "postgres" || g.Version != "15" {
		t.Fatalf("grant = %+v", g)
	}

	// The brain issued CREATE ROLE / CREATE DATABASE via a one-shot psql container.
	if !e.docker.hasCall("CREATE DATABASE") {
		t.Fatalf("no provisioning psql call; calls: %v", e.docker.Calls())
	}

	// The .env carries the injected credential family.
	env := readInstanceEnv(t, e, inst.ID)
	dsn := envValue(env, "MOOSE_SERVICE_DATABASE_DSN")
	wantHost := "postgres-15.moose.internal"
	if !strings.HasPrefix(dsn, "postgres://") || !strings.Contains(dsn, wantHost) {
		t.Fatalf("DSN = %q, want postgres:// … %s", dsn, wantHost)
	}
	if got := envValue(env, "MOOSE_SERVICE_DATABASE_HOST"); got != wantHost {
		t.Fatalf("HOST = %q, want %q", got, wantHost)
	}
	if got := envValue(env, "MOOSE_SERVICE_DATABASE_NAME"); got != g.DBName {
		t.Fatalf("NAME = %q, want %q", got, g.DBName)
	}

	// The app service is attached to the service network in the override.
	override := readInstanceFile(t, e, inst.ID, "compose.override.yml")
	if !strings.Contains(override, "moose-svc-postgres-15") {
		t.Fatalf("override missing service network:\n%s", override)
	}
}

func TestSecondAppReusesServiceInstance(t *testing.T) {
	e := newTestEnv(t)
	installDBApp(t, e, "dbappa")
	upsAfterFirst := e.docker.countMethod("ServiceUp")
	installDBApp(t, e, "dbappb")

	// The shared Postgres instance is spun up once, not per app: the second
	// install finds the existing service_instances row and skips ServiceUp.
	if got := e.docker.countMethod("ServiceUp"); got != upsAfterFirst {
		t.Fatalf("ServiceUp called %d times total, want %d (reuse)", got, upsAfterFirst)
	}
}

func TestUninstallDropsServiceDB(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBApp(t, e, "dbapp")
	grants, _ := e.store.GetServiceGrants(inst.ID)
	dbName := grants[0].DBName

	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !e.docker.hasCall("DROP DATABASE") || !e.docker.hasCall(dbName) {
		t.Fatalf("uninstall did not drop %s; calls: %v", dbName, e.docker.Calls())
	}
}

func TestInstallProvisionsMySQLFamily(t *testing.T) {
	for _, tc := range []struct {
		kind, version, stem, client string
	}{
		// The version dot folds to a dash in every derived name (compose project
		// names reject dots); mariadb provisions via its own-named client binary.
		{"mysql", "8.0", "mysql-8-0", "mysql -uroot"},
		{"mariadb", "11.4", "mariadb-11-4", "mariadb -uroot"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e := newTestEnv(t)
			inst := installDBAppKind(t, e, "dbapp", tc.kind, tc.version)

			if _, err := e.store.GetServiceInstance(tc.kind, tc.version); err != nil {
				t.Fatalf("service instance not recorded: %v", err)
			}
			if !e.docker.hasCall("ServiceUp(") {
				t.Fatalf("ServiceUp never called; calls: %v", e.docker.Calls())
			}

			grants, err := e.store.GetServiceGrants(inst.ID)
			if err != nil || len(grants) != 1 {
				t.Fatalf("grants = %v, err %v", grants, err)
			}
			g := grants[0]
			if g.Kind != tc.kind || g.Version != tc.version {
				t.Fatalf("grant = %+v", g)
			}

			// Provisioning ran the engine's own client with CREATE DATABASE +
			// CREATE USER + GRANT.
			if !e.docker.hasCall(tc.client) || !e.docker.hasCall("CREATE DATABASE") ||
				!e.docker.hasCall("CREATE USER") || !e.docker.hasCall("GRANT ALL PRIVILEGES") {
				t.Fatalf("no MySQL provisioning call; calls: %v", e.docker.Calls())
			}

			// The injected family carries the dot-folded host, port 3306, and a
			// mysql:// DSN for both engines (one wire protocol).
			env := readInstanceEnv(t, e, inst.ID)
			wantHost := tc.stem + ".moose.internal"
			if got := envValue(env, "MOOSE_SERVICE_DATABASE_HOST"); got != wantHost {
				t.Fatalf("HOST = %q, want %q", got, wantHost)
			}
			if got := envValue(env, "MOOSE_SERVICE_DATABASE_PORT"); got != "3306" {
				t.Fatalf("PORT = %q, want 3306", got)
			}
			dsn := envValue(env, "MOOSE_SERVICE_DATABASE_DSN")
			if !strings.HasPrefix(dsn, "mysql://") || !strings.Contains(dsn, wantHost+":3306/"+g.DBName) {
				t.Fatalf("DSN = %q, want mysql:// … %s:3306/%s", dsn, wantHost, g.DBName)
			}

			override := readInstanceFile(t, e, inst.ID, "compose.override.yml")
			if !strings.Contains(override, "moose-svc-"+tc.stem) {
				t.Fatalf("override missing service network:\n%s", override)
			}
		})
	}
}

func TestUninstallDropsMySQLDB(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBAppKind(t, e, "dbapp", "mysql", "8.0")
	grants, _ := e.store.GetServiceGrants(inst.ID)
	dbName := grants[0].DBName

	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !e.docker.hasCall("DROP DATABASE") || !e.docker.hasCall("DROP USER") || !e.docker.hasCall(dbName) {
		t.Fatalf("uninstall did not drop %s; calls: %v", dbName, e.docker.Calls())
	}
}

// TestInstallProvisionsValkeyViaRedisAlias proves the compatibility alias: an
// app that declares `type: redis, version: "7"` is provisioned on the Valkey
// engine (valkey/8) — the grant stores the engine identity, the names key off
// valkey, and moose never spins up an upstream redis instance. The native
// `type: valkey` path is covered by TestInstallProvisionsValkeyNative.
func TestInstallProvisionsValkeyViaRedisAlias(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBAppKind(t, e, "cacheapp", "redis", "7")

	// redis 7 normalized to the Valkey engine: a shared valkey-8 instance was
	// recorded and started; no upstream redis-7 instance exists.
	if _, err := e.store.GetServiceInstance("valkey", "8"); err != nil {
		t.Fatalf("valkey service instance not recorded: %v", err)
	}
	if _, err := e.store.GetServiceInstance("redis", "7"); err == nil {
		t.Fatalf("an upstream redis-7 instance was recorded; redis must normalize to valkey")
	}
	if !e.docker.hasCall("ServiceUp(") {
		t.Fatalf("ServiceUp never called; calls: %v", e.docker.Calls())
	}

	// A per-app grant was persisted — an ACL user, no database (Valkey has none).
	// The grant stores the ENGINE identity, not the declared alias.
	grants, err := e.store.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	g := grants[0]
	if g.LogicalName != "database" || g.Kind != "valkey" || g.Version != "8" {
		t.Fatalf("grant = %+v, want kind=valkey version=8", g)
	}
	if g.RoleName == "" || g.DBName != "" {
		t.Fatalf("grant role=%q dbname=%q, want non-empty role and empty dbname", g.RoleName, g.DBName)
	}

	assertValkeyProvisioned(t, e, inst, g)
}

// TestInstallProvisionsValkeyNative is the native `type: valkey, version: "8"`
// path — identical engine behavior to the redis alias, declared directly.
func TestInstallProvisionsValkeyNative(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBAppKind(t, e, "cacheapp", "valkey", "8")

	if _, err := e.store.GetServiceInstance("valkey", "8"); err != nil {
		t.Fatalf("valkey service instance not recorded: %v", err)
	}
	grants, err := e.store.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	g := grants[0]
	if g.Kind != "valkey" || g.Version != "8" {
		t.Fatalf("grant = %+v, want kind=valkey version=8", g)
	}
	assertValkeyProvisioned(t, e, inst, g)
}

// assertValkeyProvisioned checks the Valkey ACL provisioning + injected env that
// is identical whether the engine was reached via the redis alias or natively.
func assertValkeyProvisioned(t *testing.T, e *testEnv, inst store.Instance, g store.ServiceGrant) {
	t.Helper()
	// The brain created the per-app ACL user and persisted it via a one-shot
	// valkey-cli (full keyspace, no @admin), then ACL SAVE'd it.
	if !e.docker.hasCall("ACL SETUSER "+g.RoleName) || !e.docker.hasCall("+@all -@admin") {
		t.Fatalf("no valkey ACL SETUSER call; calls: %v", e.docker.Calls())
	}
	// The keyspace-destruction commands are subtracted so one app can't wipe the
	// shared keyspace every other app reads from (Problem A — cross-app destroy).
	if !e.docker.hasCall("-flushall -flushdb -swapdb") {
		t.Fatalf("valkey ACL user can still flush the shared keyspace; calls: %v", e.docker.Calls())
	}
	if !e.docker.hasCall("ACL SAVE") {
		t.Fatalf("ACL not persisted (no ACL SAVE); calls: %v", e.docker.Calls())
	}

	// The injected family carries the valkey-8 host, port 6379, and a redis://
	// DSN with no database path (clients default to logical DB 0; redis:// is the
	// universal RESP scheme).
	env := readInstanceEnv(t, e, inst.ID)
	wantHost := "valkey-8.moose.internal"
	if got := envValue(env, "MOOSE_SERVICE_DATABASE_HOST"); got != wantHost {
		t.Fatalf("HOST = %q, want %q", got, wantHost)
	}
	if got := envValue(env, "MOOSE_SERVICE_DATABASE_PORT"); got != "6379" {
		t.Fatalf("PORT = %q, want 6379", got)
	}
	if got := envValue(env, "MOOSE_SERVICE_DATABASE_NAME"); got != "" {
		t.Fatalf("NAME = %q, want empty (valkey has no database)", got)
	}
	dsn := envValue(env, "MOOSE_SERVICE_DATABASE_DSN")
	if !strings.HasPrefix(dsn, "redis://") || !strings.Contains(dsn, wantHost+":6379") {
		t.Fatalf("DSN = %q, want redis:// … %s:6379", dsn, wantHost)
	}
	if strings.Contains(dsn, ":6379/") {
		t.Fatalf("DSN = %q has a database path; valkey grants carry none", dsn)
	}

	override := readInstanceFile(t, e, inst.ID, "compose.override.yml")
	if !strings.Contains(override, "moose-svc-valkey-8") {
		t.Fatalf("override missing service network:\n%s", override)
	}
}

// TestRedisAndValkeyCoalesce is the heart of the alias decision: a redis-7 app
// and a valkey-8 app must share ONE engine instance (moose-svc-valkey-8), not
// two. Proves redis-7 normalizes to valkey-8 *before* the lazy-spinup gate, so
// the second install reuses the existing service_instances row.
func TestRedisAndValkeyCoalesce(t *testing.T) {
	e := newTestEnv(t)
	installDBAppKind(t, e, "cacheredis", "redis", "7")
	upsAfterFirst := e.docker.countMethod("ServiceUp")
	installDBAppKind(t, e, "cachevalkey", "valkey", "8")

	// The second install (declared as valkey:8) finds the instance the first
	// (declared as redis:7) already spun up — no second ServiceUp.
	if got := e.docker.countMethod("ServiceUp"); got != upsAfterFirst {
		t.Fatalf("ServiceUp called %d times total, want %d (redis-7 and valkey-8 must coalesce)", got, upsAfterFirst)
	}
	// Exactly one service instance exists, and it is the Valkey engine.
	instances, err := e.store.ListServiceInstances()
	if err != nil {
		t.Fatalf("list service instances: %v", err)
	}
	if len(instances) != 1 || instances[0].Kind != "valkey" || instances[0].Version != "8" {
		t.Fatalf("service instances = %+v, want exactly one valkey-8", instances)
	}
}

func TestUninstallDropsValkeyACL(t *testing.T) {
	e := newTestEnv(t)
	inst := installDBAppKind(t, e, "cacheapp", "redis", "7")
	grants, _ := e.store.GetServiceGrants(inst.ID)
	user := grants[0].RoleName

	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !e.docker.hasCall("ACL DELUSER " + user) {
		t.Fatalf("uninstall did not drop ACL user %s; calls: %v", user, e.docker.Calls())
	}
}

// TestRedisProvisioningExecFailures forces each valkey-cli provisioning command
// to fail and asserts install surfaces the error (and rolls back). Targets the
// ACL substrings; readiness is a separate ContainerHealth poll (default
// "healthy"), so only the SETUSER / SAVE one-shot runs fail.
func TestRedisProvisioningExecFailures(t *testing.T) {
	for _, fail := range []string{"ACL SETUSER", "ACL SAVE"} {
		t.Run(fail, func(t *testing.T) {
			e := newTestEnv(t)
			e.docker.runOneOff = func(_, _, _ string, args []string) (string, error) {
				if strings.Contains(strings.Join(args, " "), fail) {
					return "boom", fmt.Errorf("forced %s failure", fail)
				}
				return "", nil
			}
			man := strings.Replace(dbManifest, "id: dbapp", "id: cacheapp", 1)
			man = strings.Replace(man, "preferred_slugs: [dbapp]", "preferred_slugs: [cacheapp]", 1)
			man = strings.Replace(man, "type: postgres", "type: redis", 1)
			man = strings.Replace(man, `version: "15"`, `version: "7"`, 1)
			e.writeCatalogApp(t, "cacheapp", dbCompose, man)
			e.docker.digests[testImage] = testDigest
			if _, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "cacheapp"),
				Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil); err == nil {
				t.Fatalf("install succeeded despite %s failure", fail)
			}
			// Rollback is clean: no instance row survives the failed provisioning.
			if list, _ := e.store.List(); len(list) != 0 {
				t.Fatalf("instance row must be rolled back after %s failure, got %v", fail, list)
			}
		})
	}
}

// TestWaitServiceReadyTimesOut covers the readiness-poll failure path: when the
// service container never reports "healthy" (the per-engine compose healthcheck,
// read via ContainerHealth — faked as a stuck "starting"), waitServiceReady
// polls until its deadline and returns a "not ready" error rather than hanging.
func TestWaitServiceReadyTimesOut(t *testing.T) {
	e := newTestEnv(t)
	e.docker.containerHealth = "starting" // never becomes healthy
	e.m.serviceReadyWait = 20 * time.Millisecond
	e.m.healthPoll = time.Millisecond
	err := e.m.waitServiceReady(context.Background(), "postgres", "15")
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("want a 'not ready' timeout error, got %v", err)
	}
}

// TestWaitServiceReadyAbortsOnContext covers two cancellation paths: (a) a
// failing health inspect keeps the poll going (transient, not fatal), and when
// the context is cancelled the wait returns context.Canceled rather than
// spinning to the deadline; (b) same with a healthy-but-not-yet-ready status
// ("starting"), confirming the select branch fires independently of inspect
// errors.
func TestWaitServiceReadyAbortsOnContext(t *testing.T) {
	t.Run("inspect error + cancelled ctx", func(t *testing.T) {
		e := newTestEnv(t)
		e.docker.containerHealthErr = fmt.Errorf("inspect boom")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := e.m.waitServiceReady(ctx, "postgres", "15"); err != context.Canceled {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	})
	t.Run("starting + cancelled ctx", func(t *testing.T) {
		e := newTestEnv(t)
		e.docker.containerHealth = "starting"
		e.m.healthPoll = time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := e.m.waitServiceReady(ctx, "postgres", "15"); err != context.Canceled {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	})
}

// TestPostgresComposePinsPGDATA guards the data bind against the image default
// moving underneath it. postgres 18 defaults PGDATA to
// /var/lib/postgresql/18/docker, so an unpinned 18 instance would bind ./data at
// a path the server never uses. The live proof that the cluster actually
// survives a container rebuild is TestLivePostgres18Persistence (dockerlive);
// this is the cheap always-on regression guard on the rendered template.
func TestPostgresComposePinsPGDATA(t *testing.T) {
	for _, version := range []string{"15", "16", "17", "18"} {
		compose := postgresServiceCompose(version)
		if !strings.Contains(compose, "PGDATA: /var/lib/postgresql/data") {
			t.Errorf("postgres %s: compose does not pin PGDATA:\n%s", version, compose)
		}
		if !strings.Contains(compose, "- ./data:/var/lib/postgresql/data") {
			t.Errorf("postgres %s: compose does not bind ./data at the pinned PGDATA:\n%s", version, compose)
		}
	}
}

func readInstanceEnv(t *testing.T, e *testEnv, id string) string {
	t.Helper()
	return readInstanceFile(t, e, id, ".env")
}

func readInstanceFile(t *testing.T, e *testEnv, id, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.stateDir, "instances", id, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
