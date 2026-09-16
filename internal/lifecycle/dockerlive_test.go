//go:build dockerlive

package lifecycle

// Real-system verification for managed-service provisioning: drives the actual
// Manager against the real `docker`/`docker compose` CLI and a real Postgres
// container (host/caddy are fakes — they don't touch the new path). Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLivePostgresProvisioning -v
//
// Requires a working Docker daemon and network access to pull postgres + the
// app image. Excluded from the default suite by the build tag.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/store"
)

func TestLivePostgresProvisioning(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cat := catalog.New(catDir)
	docker := NewCLIDocker()
	m := NewManager(s, cat, newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check) // real `docker compose config -q`

	// Real ingress network so the app override's external reference resolves.
	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}

	// A folderless app whose main service is a real, fast-starting image. It
	// declares a Postgres dependency so the brain provisions a DB+role and
	// injects the DSN; we verify the provisioning side-effects directly.
	man := `
id: liveapp
manifest_version: 1
name: Live App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [liveapp]
services:
  database:
    type: postgres
    version: "15"
permissions:
  internet: false
  lan: false
`
	compose := `
services:
  app:
    image: traefik/whoami:v1.10.3
    environment:
      POSTGRES_URL: ${MOOSE_SERVICE_DATABASE_DSN}
`
	writeLiveCatalogApp(t, catDir, "liveapp", compose, man)

	// Cleanup: uninstall the app (drops DB/role), then tear down the shared
	// service container + network the test created.
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", serviceContainerName("postgres", "15")).Run()
		_ = exec.Command("docker", "network", "rm", serviceNetworkName("postgres", "15")).Run()
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
		// Postgres writes its data dir as the container's uid (root); remove it via
		// a throwaway container so t.TempDir cleanup doesn't hit permission-denied.
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s",
			"alpine", "rm", "-rf", "/s/services").Run()
	})

	inst, err := m.Install(ctx, mustLoadApp(t, m, "liveapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	grants, err := s.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	dbName := grants[0].DBName

	// The shared Postgres container is up and actually has the provisioned DB.
	if !pgDatabaseExists(t, "15", dbName) {
		t.Fatalf("database %q not found in the live Postgres", dbName)
	}
	t.Logf("provisioned database %q present in live Postgres", dbName)

	// The role can connect to its own database with the injected password.
	if !pgRoleCanConnect(t, "15", grants[0].RoleName, grants[0].Password, dbName) {
		t.Fatalf("role %q could not connect to %q with injected password", grants[0].RoleName, dbName)
	}
	t.Logf("role %q connects to %q with the injected credentials", grants[0].RoleName, dbName)

	// Uninstall drops the database.
	if err := m.Uninstall(ctx, inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if pgDatabaseExists(t, "15", dbName) {
		t.Fatalf("database %q survived uninstall", dbName)
	}
	t.Logf("database %q dropped on uninstall", dbName)
}

// TestLivePostgres18Persistence is the data-durability proof for the shared
// Postgres bind, run against 18 because that is the major where the image's
// default PGDATA moved (/var/lib/postgresql/18/docker) away from the path the
// compose template binds. A wrong bind is invisible to every other assertion:
// the server boots, passes pg_isready, provisions databases and serves traffic —
// the cluster just lives on the container filesystem and vanishes with it.
//
// So the test writes real rows, then destroys the *container* (not a restart —
// `docker restart` keeps the container filesystem, and would pass happily
// against the broken bind) and brings the service back through the brain's own
// boot path, reconcileServices. Surviving that is what proves ./data holds the
// cluster.
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLivePostgres18Persistence -v -timeout 300s
func TestLivePostgres18Persistence(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cat := catalog.New(catDir)
	docker := NewCLIDocker()
	m := NewManager(s, cat, newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)

	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}

	man := `
id: livepg18
manifest_version: 1
name: Live PG18 App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [livepg18]
services:
  database:
    type: postgres
    version: "18"
permissions:
  internet: false
  lan: false
`
	compose := `
services:
  app:
    image: traefik/whoami:v1.10.3
    environment:
      POSTGRES_URL: ${MOOSE_SERVICE_DATABASE_DSN}
`
	writeLiveCatalogApp(t, catDir, "livepg18", compose, man)

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", serviceContainerName("postgres", "18")).Run()
		_ = exec.Command("docker", "network", "rm", serviceNetworkName("postgres", "18")).Run()
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s",
			"alpine", "rm", "-rf", "/s/services").Run()
	})

	inst, err := m.Install(ctx, mustLoadApp(t, m, "livepg18"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	grants, err := s.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	g := grants[0]

	// uuidv7() is the 18-only built-in that motivated the major (issue #354); an
	// app pinned to 18 for it must actually find it in the provisioned engine.
	// Writing through it also gives the durability check a real row to look for.
	if _, err := pgExec(t, "18", g.RoleName, g.Password, g.DBName,
		"CREATE TABLE durable (id uuid PRIMARY KEY DEFAULT uuidv7(), note text)"); err != nil {
		t.Fatalf("create table with uuidv7 default: %v", err)
	}
	if _, err := pgExec(t, "18", g.RoleName, g.Password, g.DBName,
		"INSERT INTO durable (note) VALUES ('survives a container rebuild')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Destroy the container, taking its filesystem with it, then bring the
	// service back the way the brain does on boot.
	if out, err := exec.Command("docker", "rm", "-f", serviceContainerName("postgres", "18")).CombinedOutput(); err != nil {
		t.Fatalf("rm service container: %v\n%s", err, out)
	}
	m.reconcileServices(ctx)
	if err := m.waitServiceReady(ctx, "postgres", "18"); err != nil {
		t.Fatalf("service not ready after rebuild: %v", err)
	}

	if !pgDatabaseExists(t, "18", g.DBName) {
		t.Fatalf("database %q did not survive the container rebuild — the data bind is not holding the cluster", g.DBName)
	}
	if !pgRoleCanConnect(t, "18", g.RoleName, g.Password, g.DBName) {
		t.Fatalf("role %q could not connect after the container rebuild", g.RoleName)
	}
	out, err := pgExec(t, "18", g.RoleName, g.Password, g.DBName, "SELECT note FROM durable")
	if err != nil {
		t.Fatalf("read back after rebuild: %v", err)
	}
	if out != "survives a container rebuild" {
		t.Fatalf("row after rebuild = %q, want the inserted note", out)
	}
	t.Logf("database %q and its rows survived the container rebuild", g.DBName)
}

// pgExec runs one SQL statement as the provisioned app role over TCP, returning
// the trimmed single-value output.
func pgExec(t *testing.T, version, role, password, dbName, sql string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", "exec", "-e", "PGPASSWORD="+password,
		serviceContainerName("postgres", version),
		"psql", "-h", "127.0.0.1", "-U", role, "-d", dbName, "-tAc", sql).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// TestLiveMySQLProvisioning mirrors TestLivePostgresProvisioning for the MySQL
// family: a real mysql:8.0 service container is lazily spun up, the per-app
// DB+user is provisioned via a one-shot run of the image's own client, the user
// connects over TCP with the injected password, and the DB is dropped on
// uninstall. MariaDB shares the code path (only the image, client binaries, and
// root-password env var differ), so one live engine suffices.
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveMySQLProvisioning -v -timeout 300s
func TestLiveMySQLProvisioning(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cat := catalog.New(catDir)
	docker := NewCLIDocker()
	m := NewManager(s, cat, newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)

	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}

	man := `
id: livemysql
manifest_version: 1
name: Live MySQL App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [livemysql]
services:
  database:
    type: mysql
    version: "8.0"
permissions:
  internet: false
  lan: false
`
	compose := `
services:
  app:
    image: traefik/whoami:v1.10.3
    environment:
      DATABASE_URL: ${MOOSE_SERVICE_DATABASE_DSN}
`
	writeLiveCatalogApp(t, catDir, "livemysql", compose, man)

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", serviceContainerName("mysql", "8.0")).Run()
		_ = exec.Command("docker", "network", "rm", serviceNetworkName("mysql", "8.0")).Run()
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
		// mysqld chowns its data dir to the mysql user; remove it via a throwaway
		// container so t.TempDir cleanup doesn't hit permission-denied.
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s",
			"alpine", "rm", "-rf", "/s/services").Run()
	})

	inst, err := m.Install(ctx, mustLoadApp(t, m, "livemysql"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	grants, err := s.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	dbName := grants[0].DBName

	if !mysqlDatabaseExists(t, dbName) {
		t.Fatalf("database %q not found in the live MySQL", dbName)
	}
	t.Logf("provisioned database %q present in live MySQL", dbName)

	if !mysqlUserCanConnect(t, grants[0].RoleName, grants[0].Password, dbName) {
		t.Fatalf("user %q could not connect to %q with injected password", grants[0].RoleName, dbName)
	}
	t.Logf("user %q connects to %q with the injected credentials", grants[0].RoleName, dbName)

	if err := m.Uninstall(ctx, inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if mysqlDatabaseExists(t, dbName) {
		t.Fatalf("database %q survived uninstall", dbName)
	}
	t.Logf("database %q dropped on uninstall", dbName)
}

// TestLiveRedisProvisioning mirrors the SQL live tests for the Valkey engine,
// and proves the redis alias end-to-end: the manifest declares `type: redis,
// version: "7"`, which normalizes to a real valkey/valkey:8 service container
// (never upstream redis). The container is lazily spun up with an external
// aclfile, the per-app ACL user is provisioned via a one-shot valkey-cli, that
// user authenticates and reads/writes the keyspace with the injected password,
// and ACL DELUSER removes it on uninstall.
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveRedisProvisioning -v -timeout 300s
func TestLiveRedisProvisioning(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cat := catalog.New(catDir)
	docker := NewCLIDocker()
	m := NewManager(s, cat, newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)

	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}

	man := `
id: livecache
manifest_version: 1
name: Live Cache App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [livecache]
services:
  cache:
    type: redis
    version: "7"
permissions:
  internet: false
  lan: false
`
	compose := `
services:
  app:
    image: traefik/whoami:v1.10.3
    environment:
      REDIS_URL: ${MOOSE_SERVICE_CACHE_DSN}
`
	writeLiveCatalogApp(t, catDir, "livecache", compose, man)

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", serviceContainerName("valkey", "8")).Run()
		_ = exec.Command("docker", "network", "rm", serviceNetworkName("valkey", "8")).Run()
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
		// valkey chowns its data dir to the valkey user; remove it via a throwaway
		// container so t.TempDir cleanup doesn't hit permission-denied.
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s",
			"alpine", "rm", "-rf", "/s/services").Run()
	})

	inst, err := m.Install(ctx, mustLoadApp(t, m, "livecache"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	grants, err := s.GetServiceGrants(inst.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, err %v", grants, err)
	}
	g := grants[0]
	if g.Kind != "valkey" || g.Version != "8" || g.DBName != "" {
		t.Fatalf("grant = %+v, want valkey/8 with empty db name (redis alias)", g)
	}

	if !redisACLUserExists(t, g.RoleName) {
		t.Fatalf("ACL user %q not found in the live Valkey", g.RoleName)
	}
	t.Logf("provisioned ACL user %q present in live Valkey", g.RoleName)

	if !redisUserCanConnect(t, g.RoleName, g.Password) {
		t.Fatalf("user %q could not auth + read/write keyspace with the injected password", g.RoleName)
	}
	t.Logf("user %q authenticates and reads/writes the keyspace with the injected credentials", g.RoleName)

	if err := m.Uninstall(ctx, inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if redisACLUserExists(t, g.RoleName) {
		t.Fatalf("ACL user %q survived uninstall", g.RoleName)
	}
	t.Logf("ACL user %q dropped on uninstall", g.RoleName)
}

// redisACLUserExists reports whether an ACL user is present, scanning ACL LIST
// (one `user <name> ...` line per account). valkey-cli runs as the default
// (superuser) account via REDISCLI_AUTH from the container env.
func redisACLUserExists(t *testing.T, user string) bool {
	t.Helper()
	out, _ := exec.Command("docker", "exec", serviceContainerName("valkey", "8"),
		"valkey-cli", "ACL", "LIST").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "user "+user+" ") {
			return true
		}
	}
	return false
}

// redisUserCanConnect verifies the provisioned user authenticates with the
// injected password and exercises its ~* keyspace grant (SET then GET), using
// the redis:// URL form the DSN is built from.
func redisUserCanConnect(t *testing.T, user, password string) bool {
	t.Helper()
	url := fmt.Sprintf("redis://%s:%s@127.0.0.1:6379", user, password)
	container := serviceContainerName("valkey", "8")
	deadline := time.Now().Add(10 * time.Second)
	for {
		set, err := exec.Command("docker", "exec", container,
			"valkey-cli", "--no-auth-warning", "-u", url, "set", "moose:probe", "ok").CombinedOutput()
		if err == nil && strings.Contains(string(set), "OK") {
			got, _ := exec.Command("docker", "exec", container,
				"valkey-cli", "--no-auth-warning", "-u", url, "get", "moose:probe").CombinedOutput()
			if strings.TrimSpace(string(got)) == "ok" {
				return true
			}
		}
		if time.Now().After(deadline) {
			t.Logf("connect attempt output: %s", set)
			return false
		}
		time.Sleep(time.Second)
	}
}

// TestLiveServiceUserBootAndWrite drives a real folderless `service_user: true`
// install end to end: the override pins the host-allocated identity (fake host:
// 2100), the container actually runs as that UID, and it can write its ./data
// bind once the bind is owned by the allocated identity.
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveServiceUserBootAndWrite -v
//
// The unprivileged test runner cannot chown ./data to 2100 (the brain logs the
// documented warning and proceeds); the production brain runs as euid 0 and
// chowns it during install. The test reproduces that step with a root helper
// container before asserting the write path.
func TestLiveServiceUserBootAndWrite(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Registered before any instance files exist so it runs LAST: scrub the
	// 2100-owned leftovers a failed run may leave, so t.TempDir cleanup works.
	t.Cleanup(func() {
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s",
			"alpine", "rm", "-rf", "/s/instances").Run()
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
	})

	cat := catalog.New(catDir)
	docker := NewCLIDocker()
	m := NewManager(s, cat, newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)

	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}

	man := `
id: liveuser
manifest_version: 1
name: Live Service User
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [liveuser]
service_user: true
permissions:
  internet: false
  lan: false
`
	compose := `
services:
  app:
    image: alpine:3.20
    command: ["sleep", "600"]
    volumes:
      - ./data:/data
`
	writeLiveCatalogApp(t, catDir, "liveuser", compose, man)

	inst, err := m.Install(ctx, mustLoadApp(t, m, "liveuser"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() {
		out, _ := exec.Command("docker", "ps", "-aq",
			"--filter", "label=moose.instance_id="+inst.ID).Output()
		for _, cid := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rm", "-f", cid).Run()
		}
	})
	if inst.ServiceUID != 2100 || inst.ServiceGID != 2100 {
		t.Fatalf("allocated identity = %d:%d, want 2100:2100 (fake band start)", inst.ServiceUID, inst.ServiceGID)
	}

	// Simulate the production brain's euid-0 chown of the data bind (the
	// unprivileged runner's own chown attempt was skipped with a warning).
	dataDir := filepath.Join(stateDir, "instances", inst.ID, "data")
	if out, err := exec.Command("docker", "run", "--rm", "-v", dataDir+":/d",
		"alpine:3.20", "chown", "2100:2100", "/d").CombinedOutput(); err != nil {
		t.Fatalf("chown data dir via helper: %v\n%s", err, out)
	}

	// The main container runs as the allocated identity…
	cid, err := exec.Command("docker", "ps", "-q",
		"--filter", "label=moose.instance_id="+inst.ID,
		"--filter", "label=com.docker.compose.service=app").Output()
	if err != nil || strings.TrimSpace(string(cid)) == "" {
		t.Fatalf("app container not found: %v", err)
	}
	container := strings.TrimSpace(string(cid))
	out, err := exec.Command("docker", "exec", container, "id", "-u").CombinedOutput()
	if err != nil {
		t.Fatalf("exec id -u: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "2100" {
		t.Fatalf("container uid = %s, want 2100", got)
	}

	// …and can write its data bind.
	if out, err := exec.Command("docker", "exec", container,
		"sh", "-c", "echo ok > /data/probe").CombinedOutput(); err != nil {
		t.Fatalf("write probe as 2100: %v\n%s", err, out)
	}
	fi, err := os.Stat(filepath.Join(dataDir, "probe"))
	if err != nil {
		t.Fatalf("stat probe on host: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("stat probe: unexpected Stat_t type")
	}
	if int(st.Uid) != 2100 || int(st.Gid) != 2100 {
		t.Fatalf("probe owned by %d:%d, want 2100:2100", st.Uid, st.Gid)
	}
	t.Logf("container runs as 2100 and wrote data/probe owned 2100:2100")

	// Hand ./data back to the test user so Uninstall's RemoveAll succeeds under
	// the unprivileged runner (the production brain is root and needs no help).
	if out, err := exec.Command("docker", "run", "--rm", "-v", dataDir+":/d", "alpine:3.20",
		"chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/d").CombinedOutput(); err != nil {
		t.Fatalf("chown data dir back: %v\n%s", err, out)
	}
	if err := m.Uninstall(ctx, inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
}

// mysqlDatabaseExists reports whether a database is present in the live
// container, querying as root via the in-container password env.
func mysqlDatabaseExists(t *testing.T, dbName string) bool {
	t.Helper()
	out, _ := exec.Command("docker", "exec", serviceContainerName("mysql", "8.0"),
		"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot -N -e "$1"`, "sh",
		"SELECT 1 FROM information_schema.schemata WHERE schema_name='"+dbName+"'").CombinedOutput()
	return strings.TrimSpace(string(out)) == "1"
}

// mysqlUserCanConnect verifies the provisioned user authenticates against its
// DB over TCP with the injected password, exercising the real DSN.
func mysqlUserCanConnect(t *testing.T, user, password, dbName string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("docker", "exec", "-e", "MYSQL_PWD="+password,
			serviceContainerName("mysql", "8.0"),
			"mysql", "-h127.0.0.1", "--protocol=TCP", "-u"+user, "-D", dbName, "-N", "-e", "SELECT 1").CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return true
		}
		if time.Now().After(deadline) {
			t.Logf("connect attempt output: %s", out)
			return false
		}
		time.Sleep(time.Second)
	}
}

// TestLiveKanBoot installs the real `kan` catalog app against a freshly
// provisioned Postgres and asserts it boots — the concrete goal the managed-
// service slice unblocks. Pulls kan's images, so it's the heaviest live test.
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveKanBoot -v -timeout 600s
//
// Un-skipped by #92: the override no longer force-restarts kan's one-shot
// `migrate` job, so the `web` service's service_completed_successfully gate
// fires and `compose up -d` returns. The managed-Postgres path this slice owns
// is covered by TestLivePostgresProvisioning above.
func TestLiveKanBoot(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	catDir := t.TempDir()
	docker := NewCLIDocker()
	m := NewManager(s, catalog.New(catDir), newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)
	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", serviceContainerName("postgres", "15")).Run()
		_ = exec.Command("docker", "network", "rm", serviceNetworkName("postgres", "15")).Run()
		_ = exec.Command("docker", "network", "rm", ingressNetwork).Run()
		_ = exec.Command("docker", "run", "--rm", "-v", stateDir+":/s",
			"alpine", "rm", "-rf", "/s/services").Run()
	})

	// The real kan app, catalog-authored into a temp dir here (the repo-root
	// catalog/ moved to the control plane — cloud #62 — so it is no longer on disk).
	// The compose is kan's own: a migrate service runs first
	// (service_completed_successfully) then web boots against the provisioned
	// Postgres with the injected DSN + auth secret. This keeps the one live check
	// that a real app's own migration job runs against a moose-provisioned DB.
	man := `id: kan
manifest_version: 1
name: Kan
version: "0.5.6"
compose_file: compose.yml
main_service: web
main_port: 3000
preferred_slugs: [kan, kanban]
services:
  database:
    type: postgres
    version: "15"
secrets:
  - name: auth
permissions:
  internet: true
  lan: false
`
	compose := `services:
  migrate:
    image: ghcr.io/kanbn/kan-migrate:0.5.6
    restart: "no"
    environment:
      POSTGRES_URL: ${MOOSE_SERVICE_DATABASE_DSN}

  web:
    image: ghcr.io/kanbn/kan:0.5.6
    environment:
      POSTGRES_URL: ${MOOSE_SERVICE_DATABASE_DSN}
      BETTER_AUTH_SECRET: ${MOOSE_SECRET_AUTH}
      NEXT_PUBLIC_BASE_URL: ${MOOSE_APP_URL}
      NEXT_PUBLIC_ALLOW_CREDENTIALS: "true"
    depends_on:
      migrate:
        condition: service_completed_successfully
    restart: unless-stopped
`
	writeLiveCatalogApp(t, catDir, "kan", compose, man)

	inst, err := m.Install(ctx, mustLoadApp(t, m, "kan"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("install kan: %v", err)
	}
	if inst.State != "running" {
		t.Fatalf("kan end state = %q, want running", inst.State)
	}
	t.Logf("kan booted against provisioned Postgres (instance %s)", inst.ID)

	// The DSN was injected, and kan's migrate job ran against the DB (a kan table
	// exists in the provisioned database).
	grants, _ := s.GetServiceGrants(inst.ID)
	if len(grants) != 1 {
		t.Fatalf("grants = %v", grants)
	}
	if !pgHasUserTables(t, grants[0].DBName) {
		t.Fatalf("no tables in %q — kan migrate did not run against the provisioned DB", grants[0].DBName)
	}
	t.Logf("kan migrate populated the provisioned database %q", grants[0].DBName)

	if err := m.Uninstall(ctx, inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
}

// pgHasUserTables reports whether the named DB has any non-system tables.
func pgHasUserTables(t *testing.T, dbName string) bool {
	t.Helper()
	out, _ := exec.Command("docker", "exec", serviceContainerName("postgres", "15"),
		"psql", "-U", "postgres", "-d", dbName, "-tAc",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema='public'").CombinedOutput()
	return strings.TrimSpace(string(out)) != "" && strings.TrimSpace(string(out)) != "0"
}

func writeLiveCatalogApp(t *testing.T, catDir, id, compose, man string) {
	t.Helper()
	dir := filepath.Join(catDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pgDatabaseExists reports whether a database is present in the live container
// of the given Postgres major.
func pgDatabaseExists(t *testing.T, version, dbName string) bool {
	t.Helper()
	out, _ := exec.Command("docker", "exec", serviceContainerName("postgres", version),
		"psql", "-U", "postgres", "-tAc",
		"SELECT 1 FROM pg_database WHERE datname='"+dbName+"'").CombinedOutput()
	return strings.TrimSpace(string(out)) == "1"
}

// pgRoleCanConnect verifies the provisioned role authenticates against its DB
// over TCP with the injected password (PGPASSWORD), exercising the real DSN.
func pgRoleCanConnect(t *testing.T, version, role, password, dbName string) bool {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-e", "PGPASSWORD="+password,
		serviceContainerName("postgres", version),
		"psql", "-h", "127.0.0.1", "-U", role, "-d", dbName, "-tAc", "SELECT 1")
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := cmd.CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return true
		}
		if time.Now().After(deadline) {
			t.Logf("connect attempt output: %s", out)
			return false
		}
		time.Sleep(time.Second)
		cmd = exec.Command("docker", "exec", "-e", "PGPASSWORD="+password,
			serviceContainerName("postgres", version),
			"psql", "-h", "127.0.0.1", "-U", role, "-d", dbName, "-tAc", "SELECT 1")
	}
}
