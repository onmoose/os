package lifecycle

// Managed data services — Tier 1 (SERVICE_PROVISIONING.md). The brain runs one
// shared container per service type+version (lazy spinup), and provisions a
// per-app credential inside it. Credentials are injected back into the app as
// MOOSE_SERVICE_<NAME>_*. v1 supports Postgres, the MySQL family (mysql,
// mariadb — one code path, per-engine deltas only), and Valkey. The SQL engines
// get a per-app database+role; Valkey has no database concept, so the per-app
// unit is an ACL user with full keyspace — the credential itself is the
// isolation boundary (revocable on uninstall), not a keyspace partition.
//
// Valkey is the BSD-3 Linux Foundation fork of Redis 7.2.4; moose runs it for
// both the `valkey` and the `redis` manifest types — `redis` is a pure
// compatibility alias, normalized to the Valkey engine (redis 7 → valkey 8) by
// normalizeEngine before anything in this file touches it, so the maps and code
// paths below only ever know "valkey". moose never runs upstream Redis at any
// version: Redis 7.4+ is RSALv2/SSPLv1 and Redis 8+ is AGPLv3, both on moose's
// avoid-list (DECISIONS.md 2026-06-13).
//
// Provisioning runs the service's own client (psql / mysql / mariadb /
// valkey-cli) in a throwaway one-shot container — `docker run --rm --network
// moose-svc-<k>-<v> --env-file <serviceDir>/.env <serviceImage> <client …>`
// (DockerDriver.RunOneOff) — rather than a Go SQL client or a `docker exec` into
// the long-running service container. Two constraints shape this: the brain
// never joins the service's Docker network (only the ephemeral container does —
// DECISIONS.md 2026-06-02, control plane off app-reachable networks), and the
// docker-socket-proxy that fronts the brain's only Docker path denies the EXEC
// family, so `docker exec` is unavailable in production (DECISIONS.md 2026-06-14;
// CONTROL_PLANE.md # Docker socket exposure). The client connects over TCP to
// the service's <kind>-<version>.moose.internal alias, so it authenticates with
// a password: the superuser password rides --env-file (the same .env the service
// compose uses) and a wrapper `sh -c` remaps it to the client's expected env var
// (PGPASSWORD / MYSQL_PWD / REDISCLI_AUTH) so it never reaches host argv; the
// per-app credential rides argv (base64url, no shell/SQL-quoting hazards), as it
// did under docker exec.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onmoose/moose/internal/manifest"
	"github.com/onmoose/moose/internal/store"
)

// serviceReadyTimeout bounds the lazy-spinup readiness wait (a cold database
// container initialising its data dir). Generous: first boot runs the engine's
// init (initdb / mysqld --initialize).
const serviceReadyTimeout = 90 * time.Second

// normalizeEngine maps a declared service (manifest) type+version to the engine
// identity the brain actually provisions. `redis` is a BSD-3 compatibility alias
// for the Valkey engine: a `redis: "7"` declaration normalizes to `valkey: "8"`
// (Valkey 8 is RESP/ACL-compatible with Redis 7), so a redis-7 app and a
// valkey-8 app coalesce onto the one shared moose-svc-valkey-8 instance. moose
// never runs upstream Redis (license: see file header). All other types pass
// through unchanged. Normalization happens once, early (provisionServices and
// serviceNetworkNames), so the maps and provisioning paths below only ever see
// "valkey".
func normalizeEngine(kind, version string) (string, string) {
	if kind == "redis" {
		return "valkey", "8"
	}
	return kind, version
}

// servicePort is the in-container port each managed service kind listens on.
var servicePort = map[string]int{"postgres": 5432, "mysql": 3306, "mariadb": 3306, "valkey": 6379}

// serviceImageRepo maps a managed kind to the upstream image repo; the version
// is the tag. v1 pins by tag (digest-pinning the service image is deferred,
// NEXT.md). valkey/valkey is the BSD-3 image (never upstream redis).
var serviceImageRepo = map[string]string{"postgres": "postgres", "mysql": "mysql", "mariadb": "mariadb", "valkey": "valkey/valkey"}

// serviceDSNScheme is the URL scheme writeEnv stamps into MOOSE_SERVICE_*_DSN.
// MariaDB speaks the MySQL wire protocol, so both family members use mysql://;
// Valkey speaks RESP, so the universal redis:// scheme every client understands.
var serviceDSNScheme = map[string]string{"postgres": "postgres", "mysql": "mysql", "mariadb": "mysql", "valkey": "redis"}

// provisionedKinds is the set of engine identities the brain can actually
// provision (after normalizeEngine — `redis` never reaches here). A schema-valid
// but unprovisioned type fails install with a clear error before anything is
// spun up.
var provisionedKinds = map[string]bool{"postgres": true, "mysql": true, "mariadb": true, "valkey": true}

// mysqlTools names the per-image client binaries and root-password env var for
// the MySQL family. The mariadb image ships mariadb-named binaries (the mysql
// names are deprecated there); presence in this map is the "is MySQL family"
// test.
var mysqlTools = map[string]struct{ client, admin, rootPWVar string }{
	"mysql":   {"mysql", "mysqladmin", "MYSQL_ROOT_PASSWORD"},
	"mariadb": {"mariadb", "mariadb-admin", "MARIADB_ROOT_PASSWORD"},
}

// serviceName is the "<kind>-<version>" stem used for the container name, the
// Docker network, the compose project, and the in-network DNS alias. Dots in a
// version (mysql "8.0") fold to dashes — compose project names reject dots —
// so mysql 8.0 names mysql-8-0 / moose-svc-mysql-8-0 / mysql-8-0.moose.internal.
func serviceName(kind, version string) string {
	return kind + "-" + strings.ReplaceAll(version, ".", "-")
}

// serviceContainerName is the compose container_name and the brain's handle for
// the readiness poll's `docker inspect` (the brain no longer execs into it).
func serviceContainerName(kind, version string) string {
	return "moose-svc-" + serviceName(kind, version)
}

// serviceNetworkName is the dedicated internal network apps attach to in order
// to reach this service; no declaration → no membership → no reachability.
func serviceNetworkName(kind, version string) string {
	return "moose-svc-" + serviceName(kind, version)
}

// serviceDNSAlias is the host apps put in their DSN. Matches the name
// SERVICE_PROVISIONING.md states verbatim (e.g. postgres-15.moose.internal).
func serviceDNSAlias(kind, version string) string {
	return serviceName(kind, version) + ".moose.internal"
}

func (m *Manager) serviceDir(kind, version string) string {
	return filepath.Join(m.stateDir, "services", serviceName(kind, version))
}

// serviceNetworkNames returns the set of service networks an app's declared
// services require — one per distinct kind+version. writeOverride attaches every
// app service to these so any container in the app's compose can reach the DB.
func serviceNetworkNames(services map[string]manifest.ServiceDep) []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range sortedServiceKeys(services) {
		dep := services[key]
		// Normalize first: a redis-7 app must attach to the valkey-8 network to
		// reach the coalesced instance — the app key derives from the engine, not
		// the declared alias.
		engine, engVersion := normalizeEngine(dep.Type, dep.Version)
		n := serviceNetworkName(engine, engVersion)
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func sortedServiceKeys(services map[string]manifest.ServiceDep) []string {
	keys := make([]string, 0, len(services))
	for k := range services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// provisionServices provisions every managed service the manifest declares and
// returns the resulting grants for persistence. For each declaration it ensures
// the shared service instance is running, then creates a dedicated database+role
// inside it. Returns nil for a manifest with no services. Postgres and the
// MySQL family in v1; any other (schema-valid) type is a terminal install error
// until its provisioning lands.
func (m *Manager) provisionServices(ctx context.Context, instanceID, manifestID string, services map[string]manifest.ServiceDep) ([]store.ServiceGrant, error) {
	if len(services) == 0 {
		return nil, nil
	}
	var grants []store.ServiceGrant
	for _, key := range sortedServiceKeys(services) {
		dep := services[key]
		// Normalize the declared type to the engine identity once, here, before
		// anything touches the maps or the provisioning paths: `redis` is a BSD-3
		// alias to the Valkey engine. The grant stores the engine identity, so
		// everything downstream (compose, names, ready probe, drop, DSN) keys off
		// valkey and never sees "redis".
		engine, engVersion := normalizeEngine(dep.Type, dep.Version)
		if !provisionedKinds[engine] {
			return nil, fmt.Errorf("managed service %q (%s) is not provisioned yet", key, engine)
		}
		if err := m.ensureServiceInstance(ctx, engine, engVersion); err != nil {
			return nil, fmt.Errorf("ensure %s-%s: %w", engine, engVersion, err)
		}
		stem := sanitizeIdent(manifestID)
		// MySQL caps user names at 32 chars (Postgres truncates past 63); the
		// role name is the db name, so bound the stem to fit "<stem>_xxxx".
		if _, isMySQL := mysqlTools[engine]; isMySQL && len(stem) > 26 {
			stem = stem[:26]
		}
		// name is the per-app db+role for the SQL engines and the ACL username for
		// Valkey (which has no database).
		name := stem + "_" + randSuffix()
		pw, err := randSecret(24)
		if err != nil {
			return nil, fmt.Errorf("service password: %w", err)
		}
		if err := m.provisionDB(ctx, engine, engVersion, name, pw); err != nil {
			return nil, fmt.Errorf("provision %s: %w", key, err)
		}
		grant := store.ServiceGrant{
			LogicalName: key, Kind: engine, Version: engVersion,
			RoleName: name, Password: pw,
		}
		if engine != "valkey" {
			grant.DBName = name // Valkey has no database; the ACL user is the boundary.
		}
		grants = append(grants, grant)
		slog.Info("provisioned managed service",
			"instance_id", instanceID, "service", key, "kind", engine, "version", engVersion)
	}
	return grants, nil
}

// existingServicePW reads the superuser password from the service dir's .env if
// it already exists on disk. Returns the password and true when found; empty
// string and false otherwise (file absent, unreadable, or malformed).
func (m *Manager) existingServicePW(kind, version string) (string, bool) {
	envPath := filepath.Join(m.serviceDir(kind, version), ".env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(data))
	_, val, ok := strings.Cut(line, "=")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(val), true
}

// ensureServiceInstance starts the shared service container of a kind+version if
// it isn't already recorded (lazy spinup). Idempotent: an existing row means the
// instance was spun up before and the reconcile pass keeps it running.
func (m *Manager) ensureServiceInstance(ctx context.Context, kind, version string) error {
	if _, err := m.store.GetServiceInstance(kind, version); err == nil {
		return nil
	} else if err != store.ErrNotFound {
		return err
	}

	// Reuse the existing superuser password if the service dir survived a store
	// wipe — overwriting .env would mismatch the already-initialised container.
	superuserPW, existed := m.existingServicePW(kind, version)
	if !existed {
		var err error
		superuserPW, err = randSecret(24)
		if err != nil {
			return err
		}
		if err := m.writeServiceDir(kind, version, superuserPW); err != nil {
			return err
		}
	}
	netName := serviceNetworkName(kind, version)
	if err := m.docker.NetworkCreate(ctx, netName, true); err != nil {
		return fmt.Errorf("create service network: %w", err)
	}
	if out, err := m.docker.ServiceUp(ctx, m.serviceDir(kind, version), netName); err != nil {
		return fmt.Errorf("service up: %w\n%s", err, out)
	}
	if err := m.waitServiceReady(ctx, kind, version); err != nil {
		return err
	}
	if err := m.store.CreateServiceInstance(store.ServiceInstance{
		Kind: kind, Version: version, SuperuserPassword: superuserPW,
		State: "running", CreatedAt: time.Now(),
	}); err != nil {
		return err
	}
	slog.Info("started managed service instance", "kind", kind, "version", version)
	return nil
}

// waitServiceReady polls the service container's healthcheck status until it
// reports "healthy" or the timeout elapses. The per-engine healthcheck is
// defined in the service compose (pg_isready / a TCP admin ping / valkey-cli
// ping) and runs inside the container via Docker's own healthcheck mechanism —
// not an API exec — so it works under the socket-proxy; the brain only reads the
// status (a CONTAINERS inspect). An inspect error (the container may not be
// registered the instant `up -d` returns) is transient: keep polling until the
// deadline rather than failing fast.
func (m *Manager) waitServiceReady(ctx context.Context, kind, version string) error {
	container := serviceContainerName(kind, version)
	deadline := time.Now().Add(m.serviceReadyWait)
	var last string
	for {
		health, err := m.docker.ContainerHealth(ctx, container)
		if err == nil && health == "healthy" {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = health
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s (last: %s)", container, m.serviceReadyWait, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.healthPoll):
		}
	}
}

// provisionDB creates the per-app credential inside the shared instance of the
// right engine: a database+role for the SQL engines, an ACL user for Valkey
// (name carries the db+role name for SQL, the ACL username for Valkey). Callers
// have already normalized the kind (redis → valkey) and gated on provisionedKinds.
func (m *Manager) provisionDB(ctx context.Context, kind, version, name, password string) error {
	switch kind {
	case "postgres":
		return m.provisionPostgresDB(ctx, version, name, password)
	case "mysql", "mariadb":
		return m.provisionMySQLDB(ctx, kind, version, name, password)
	case "valkey":
		return m.provisionValkeyACL(ctx, version, name, password)
	default:
		return fmt.Errorf("managed service kind %q is not provisioned yet", kind)
	}
}

// clientWrapper is the per-engine `sh -c` script the one-shot container runs: it
// remaps the service superuser password (delivered by --env-file under the .env
// var name) to the env var the client reads, then execs the client over TCP to
// the service's DNS alias. The per-command tokens (psql -c statements, the mysql
// -e SQL, the valkey-cli ACL args) ride positional "$@", so the wrapper shell
// never reparses them. Keeping the superuser password in an env var (not argv)
// mirrors the old in-container expansion; the client connects over TCP — there
// is no local socket in a separate one-shot container — hence the explicit -h.
func clientWrapper(kind, version string) string {
	alias := serviceDNSAlias(kind, version)
	if kind == "valkey" {
		return fmt.Sprintf(`REDISCLI_AUTH="$VALKEY_SUPERUSER_PASSWORD" exec valkey-cli -h %s "$@"`, alias)
	}
	if t, ok := mysqlTools[kind]; ok {
		return fmt.Sprintf(`MYSQL_PWD="$%s" exec %s -uroot -h %s "$@"`, t.rootPWVar, t.client, alias)
	}
	return fmt.Sprintf(`PGPASSWORD="$POSTGRES_SUPERUSER_PASSWORD" exec psql -h %s -U postgres -v ON_ERROR_STOP=1 "$@"`, alias)
}

// runServiceClient runs the service's own client (psql / mysql / mariadb /
// valkey-cli) in a one-shot container joined to the service's internal network,
// to provision or drop a per-app credential. The throwaway container — not the
// brain — joins the app-reachable network, and it replaces a `docker exec` the
// socket-proxy denies (see the file header). args are the client's positional
// arguments, appended after clientWrapper's `sh -c <script> sh`.
func (m *Manager) runServiceClient(ctx context.Context, kind, version string, args ...string) (string, error) {
	image := serviceImageRepo[kind] + ":" + version
	net := serviceNetworkName(kind, version)
	envFile := filepath.Join(m.serviceDir(kind, version), ".env")
	full := append([]string{"sh", "-c", clientWrapper(kind, version), "sh"}, args...)
	return m.docker.RunOneOff(ctx, image, net, envFile, full)
}

// provisionPostgresDB creates a login role and an owned database inside the
// shared Postgres instance via a one-shot psql container. ON_ERROR_STOP makes
// the run fail fast; the role owns only its own database, which is the per-app
// isolation boundary (SERVICE_PROVISIONING.md # Per-app isolation).
func (m *Manager) provisionPostgresDB(ctx context.Context, version, dbName, password string) error {
	// Each statement goes in its own -c: CREATE DATABASE cannot run inside a
	// transaction block, and psql wraps a single multi-statement -c in one
	// transaction. Repeated -c runs each in its own (ON_ERROR_STOP still halts
	// the run on the first failure). The role owns only its own database — the
	// per-app isolation boundary.
	if out, err := m.runServiceClient(ctx, "postgres", version,
		"-c", fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD '%s'`, dbName, password),
		"-c", fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, dbName, dbName),
	); err != nil {
		return fmt.Errorf("psql: %w\n%s", err, out)
	}
	return nil
}

// provisionMySQLDB creates a database and a scoped user inside the shared
// MySQL/MariaDB instance via a one-shot run of the image's own client. The SQL
// rides as a positional parameter (clientWrapper's "$@" → the client's -e arg)
// so the wrapper shell never parses it (backticks in a double-quoted script
// would be command substitution); the client's batch mode stops at the first
// error (psql's ON_ERROR_STOP equivalent). The user gets ALL on its own database
// only — the per-app isolation boundary.
func (m *Manager) provisionMySQLDB(ctx context.Context, kind, version, dbName, password string) error {
	t := mysqlTools[kind]
	sql := fmt.Sprintf("CREATE DATABASE `%s`; CREATE USER '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'",
		dbName, dbName, password, dbName, dbName)
	if out, err := m.runServiceClient(ctx, kind, version, "-e", sql); err != nil {
		return fmt.Errorf("%s: %w\n%s", t.client, err, out)
	}
	return nil
}

// provisionValkeyACL creates the per-app ACL user inside the shared Valkey
// instance via a one-shot valkey-cli. The user gets the full keyspace (~*) and
// all pub/sub channels (&*) — Valkey denies channels by default — and every
// command except @admin and the keyspace-destruction commands, so a compromised
// app can't touch the ACL system, CONFIG, SHUTDOWN, or replication and subvert
// the shared instance (-@admin), nor wipe the shared keyspace every other app
// reads from (-flushall -flushdb -swapdb). Subtracting those three by name
// rather than -@dangerous keeps INFO/KEYS/SORT — also @dangerous, and called by
// ordinary clients on connect — reachable. The keyspace is still shared, so this
// blocks cross-app *destruction*, not cross-app reads; per-app key confidentiality
// is the deferred isolation hardening (NEXT.md # Managed-service per-app key
// isolation, SERVICE_PROVISIONING.md # Per-app isolation). The credential is the
// per-app isolation boundary, revoked by ACL DELUSER on uninstall. The one-shot
// valkey-cli runs as the default (superuser) account, authenticated by
// REDISCLI_AUTH (clientWrapper remaps it from VALKEY_SUPERUSER_PASSWORD in the
// --env-file); the per-app password rides argv as the ACL `>password` token
// (base64url, no shell hazards) — only the superuser password is kept out of
// argv. ACL SAVE persists the user to the aclfile so it survives a service
// restart (Valkey ACLs live in the config, not the keyspace, unlike
// Postgres/MySQL accounts).
func (m *Manager) provisionValkeyACL(ctx context.Context, version, username, password string) error {
	if out, err := m.runServiceClient(ctx, "valkey", version,
		"ACL", "SETUSER", username, "on", ">"+password,
		"~*", "&*", "+@all", "-@admin", "-flushall", "-flushdb", "-swapdb",
	); err != nil {
		return fmt.Errorf("valkey acl setuser: %w\n%s", err, out)
	}
	if out, err := m.runServiceClient(ctx, "valkey", version, "ACL", "SAVE"); err != nil {
		return fmt.Errorf("valkey acl save: %w\n%s", err, out)
	}
	return nil
}

// dropServiceGrants reverses provisionServices: it drops each grant's database
// and role inside the shared instance. Best-effort — a failure is logged, never
// fatal (uninstall must always complete). FORCE terminates any straggler
// connection (the app's own containers are already down by call time).
func (m *Manager) dropServiceGrants(ctx context.Context, instanceID string, grants []store.ServiceGrant) {
	for _, g := range grants {
		if !provisionedKinds[g.Kind] {
			continue
		}
		// One one-shot run per arg set. Postgres: separate -c per statement — DROP
		// DATABASE, like CREATE, cannot run inside a transaction block, and FORCE
		// terminates any straggler connection. MySQL family: one batch run, same
		// -e shape as provisioning. Valkey: ACL DELUSER then ACL SAVE (persist the
		// removal to the aclfile).
		argSets := [][]string{{
			"-c", fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, g.DBName),
			"-c", fmt.Sprintf(`DROP ROLE IF EXISTS "%s"`, g.RoleName),
		}}
		if _, ok := mysqlTools[g.Kind]; ok {
			sql := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; DROP USER IF EXISTS '%s'@'%%'", g.DBName, g.RoleName)
			argSets = [][]string{{"-e", sql}}
		}
		if g.Kind == "valkey" {
			// DELUSER revokes in memory; SAVE persists the removal to the aclfile.
			// If DELUSER succeeds but SAVE fails (best-effort, logged below), a
			// restart reloads the user from the unchanged aclfile — but the grant
			// row is gone by then, so the password is recorded nowhere and no app
			// holds it: a harmless orphan, pruned whenever the reconcile loop that
			// owns the deferred grace-shutdown lands (NEXT.md # Managed-service
			// lifecycle gaps). Symmetric with the provisioning-side orphan.
			argSets = [][]string{
				{"ACL", "DELUSER", g.RoleName},
				{"ACL", "SAVE"},
			}
		}
		failed := false
		for _, args := range argSets {
			if out, err := m.runServiceClient(ctx, g.Kind, g.Version, args...); err != nil {
				slog.Warn("drop managed-service grant", "instance_id", instanceID,
					"service", g.LogicalName, "kind", g.Kind, "err", err, "output", strings.TrimSpace(out))
				failed = true
				break
			}
		}
		if !failed {
			slog.Info("dropped managed-service grant", "instance_id", instanceID, "service", g.LogicalName)
		}
	}
}

// reconcileServices re-asserts every recorded service instance is up at brain
// startup. `restart: unless-stopped` already keeps them alive across a daemon
// restart; this covers the case where the whole host (or Docker) was reset and
// the brain comes back first. Best-effort. Service containers carry the
// moose.service label (not moose.managed=true), so the app-orphan reaper in
// Reconcile never touches them.
func (m *Manager) reconcileServices(ctx context.Context) {
	instances, err := m.store.ListServiceInstances()
	if err != nil {
		slog.Warn("reconcile services: list", "err", err)
		return
	}
	for _, si := range instances {
		netName := serviceNetworkName(si.Kind, si.Version)
		if err := m.docker.NetworkCreate(ctx, netName, true); err != nil {
			slog.Warn("reconcile services: network", "kind", si.Kind, "version", si.Version, "err", err)
		}
		if out, err := m.docker.ServiceUp(ctx, m.serviceDir(si.Kind, si.Version), netName); err != nil {
			slog.Warn("reconcile services: up", "kind", si.Kind, "version", si.Version, "err", err, "output", out)
		}
	}
}

// writeServiceDir lays down the generated compose.yml + .env for a service
// instance under <stateDir>/services/<kind>-<version>/.
func (m *Manager) writeServiceDir(kind, version, superuserPW string) error {
	if !provisionedKinds[kind] {
		return fmt.Errorf("managed service %q is not provisioned yet", kind)
	}
	dir := m.serviceDir(kind, version)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		return err
	}
	var compose, pwVar string
	switch {
	case kind == "valkey":
		compose, pwVar = valkeyServiceCompose(version), "VALKEY_SUPERUSER_PASSWORD"
		// Bootstrap the ACL file with the default (superuser) account so valkey can
		// start; per-app users are ACL SETUSER'd in afterward and ACL SAVE'd to it.
		// The valkey entrypoint chowns the data dir to the valkey user, so it can
		// rewrite the file on ACL SAVE.
		acl := fmt.Sprintf("user default on >%s ~* &* +@all\n", superuserPW)
		if err := os.WriteFile(filepath.Join(dir, "data", "users.acl"), []byte(acl), 0o600); err != nil {
			return err
		}
	default:
		if t, ok := mysqlTools[kind]; ok {
			compose, pwVar = mysqlServiceCompose(kind, version), t.rootPWVar
		} else {
			compose, pwVar = postgresServiceCompose(version), "POSTGRES_SUPERUSER_PASSWORD"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644); err != nil {
		return err
	}
	env := pwVar + "=" + superuserPW + "\n"
	return os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600)
}

// postgresServiceCompose renders the shared-Postgres compose. The network is
// external (the brain creates it `--internal` before `up`); the service joins it
// under the postgres-<version>.moose.internal alias apps use in their DSN, and
// pg_isready backs the healthcheck the brain polls for readiness. container_name
// is the brain's inspect handle.
//
// PGDATA is pinned explicitly because the image default moved: 15/16/17 default
// to /var/lib/postgresql/data, but 18 defaults to /var/lib/postgresql/18/docker
// (major-version-specific dirs, docker-library/postgres#1259). Without the pin an
// 18 instance refuses to start at all — the image detects the mount at the old
// path and exits with "there appears to be PostgreSQL data in
// /var/lib/postgresql/data (unused mount/volume)" — so lazy spinup fails the
// readiness wait and install rolls back. Pinning keeps one template and one
// on-disk layout (services/postgres-<v>/data is the cluster dir) across every
// major. Binding the 18 parent (/var/lib/postgresql) instead is not an option:
// the 18 entrypoint drops to uid 999 before creating $PGDATA, so it cannot mkdir
// inside the 0700 dir writeServiceDir creates.
func postgresServiceCompose(version string) string {
	container := serviceContainerName("postgres", version)
	netName := serviceNetworkName("postgres", version)
	alias := serviceDNSAlias("postgres", version)
	image := serviceImageRepo["postgres"] + ":" + version
	return fmt.Sprintf(`services:
  postgres:
    image: %s
    container_name: %s
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: ${POSTGRES_SUPERUSER_PASSWORD}
      PGDATA: /var/lib/postgresql/data
    volumes:
      - ./data:/var/lib/postgresql/data
    networks:
      svc:
        aliases:
          - %s
    restart: unless-stopped
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
      timeout: 3s
      retries: 10
    labels:
      moose.service: %s
networks:
  svc:
    name: %s
    external: true
`, image, container, alias, serviceName("postgres", version), netName)
}

// mysqlServiceCompose renders the shared MySQL/MariaDB compose — same shape as
// postgresServiceCompose: external --internal network, versioned DNS alias,
// fixed container_name inspect handle. The healthcheck pings over TCP only so the
// init-time socket-only bootstrap server doesn't read as ready; $$ defers the
// env expansion to the container shell.
func mysqlServiceCompose(kind, version string) string {
	t := mysqlTools[kind]
	container := serviceContainerName(kind, version)
	netName := serviceNetworkName(kind, version)
	alias := serviceDNSAlias(kind, version)
	image := serviceImageRepo[kind] + ":" + version
	return fmt.Sprintf(`services:
  %s:
    image: %s
    container_name: %s
    environment:
      %s: ${%s}
    volumes:
      - ./data:/var/lib/mysql
    networks:
      svc:
        aliases:
          - %s
    restart: unless-stopped
    healthcheck:
      test: ["CMD-SHELL", "MYSQL_PWD=\"$$%s\" %s ping -h127.0.0.1 --protocol=TCP -uroot --silent"]
      interval: 5s
      timeout: 3s
      retries: 10
    labels:
      moose.service: %s
networks:
  svc:
    name: %s
    external: true
`, kind, image, container, t.rootPWVar, t.rootPWVar, alias, t.rootPWVar, t.admin, serviceName(kind, version), netName)
}

// valkeyServiceCompose renders the shared Valkey compose — same shape as the SQL
// services: external --internal network, versioned DNS alias, fixed
// container_name inspect handle. Valkey runs with an external aclfile on the data
// volume so per-app ACL users persist across restarts (Valkey ACLs are config,
// not keyspace); the default (superuser) account is bootstrapped into that file
// by writeServiceDir. REDISCLI_AUTH (valkey-cli honors the redis env var name)
// carries the superuser password into the service container's env so its
// healthcheck authenticates without it ever reaching argv (the brain's one-shot
// valkey-cli gets the same password via --env-file, remapped by clientWrapper).
func valkeyServiceCompose(version string) string {
	container := serviceContainerName("valkey", version)
	netName := serviceNetworkName("valkey", version)
	alias := serviceDNSAlias("valkey", version)
	image := serviceImageRepo["valkey"] + ":" + version
	return fmt.Sprintf(`services:
  valkey:
    image: %s
    container_name: %s
    command: ["valkey-server", "--aclfile", "/data/users.acl"]
    environment:
      REDISCLI_AUTH: ${VALKEY_SUPERUSER_PASSWORD}
    volumes:
      - ./data:/data
    networks:
      svc:
        aliases:
          - %s
    restart: unless-stopped
    healthcheck:
      test: ["CMD-SHELL", "valkey-cli ping | grep -q PONG"]
      interval: 5s
      timeout: 3s
      retries: 10
    labels:
      moose.service: %s
networks:
  svc:
    name: %s
    external: true
`, image, container, alias, serviceName("valkey", version), netName)
}

// sanitizeIdent reduces a manifest id to a safe SQL identifier stem: lowercase
// alphanumerics, every other rune folded to '_'. Combined with a random suffix
// it yields the per-app database/role name (e.g. kan → kan_a4f7).
func sanitizeIdent(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "app_" + out
	}
	return out
}

// randSuffix is a short hex disambiguator for per-app db/role names.
func randSuffix() string {
	buf := make([]byte, 2)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// randSecret draws n CSPRNG bytes and base64url-encodes them. Used for the
// service superuser password and per-app role passwords. base64url has no
// shell/SQL-quoting hazards (A–Za–z0–9-_).
func randSecret(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
