package lifecycle

// Moving an app's managed database to a new major version during an update
// (#635, SERVICE_PROVISIONING.md # At app update, different major version).
//
// A new version of an app can ask for a newer major of a database it already
// has, for example Postgres 15 to 18. The update moves the data as one of its
// steps, after the pre-update snapshot (#347), which already holds a dump of
// the database from the old major:
//
//  1. Start the new major's shared instance if it does not run yet.
//  2. Make the app's role (user) and database on it, with the same names and
//     password as on the old major.
//  3. Restore the snapshot's dump into it, with the new major's own tools.
//  4. Point the grant at the new major and keep the old one as retired, in one
//     store write. The new .env then names the new host.
//
// The old database is not changed. A failed update drops the database on the
// new major and points the grant back, and the old version runs on its old
// data. For 7 days the old database stays, so the rollback by hand can point
// the app back at it. Then the prune drops it (pruneRetiredGrants).
//
// The names stay the same on purpose. A MySQL or MariaDB dump names its user
// in the DEFINER of each view, routine and trigger, and a Postgres dump makes
// the app's role the owner of each object, so the dump restores as it is only
// for the same names. A database or role name that another grant already
// holds on the new major is refused: the move never drops or shares another
// app's database.
//
// Only a newer major of the same engine is moved. MySQL to MariaDB (or the
// reverse) is not a major move, and a dump from a newer major may not restore
// into an older one, so both are refused when the update is classified.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

// movedService is one managed service an update moves to another major
// version. It is written to the update's record before anything is made on
// the new major, so a rollback knows what to drop and to point back.
type movedService struct {
	Service string `json:"service"` // the manifest's logical name
	Kind    string `json:"kind"`
	From    string `json:"from"`
	To      string `json:"to"`
	// Started: the names were free on To, and the move may have made the
	// database there. Only then does a rollback drop it: before that, a
	// database on To with these names is another app's.
	Started bool `json:"started,omitempty"`
}

// serviceMoveError is a failed database move, with a plain sentence the
// dashboard shows as it is.
type serviceMoveError struct {
	plain string
	err   error
}

func (e *serviceMoveError) Error() string { return e.plain + ": " + e.err.Error() }
func (e *serviceMoveError) Unwrap() error { return e.err }

// engineNames are the names the dashboard says for each engine.
var engineNames = map[string]string{"postgres": "Postgres", "mysql": "MySQL", "mariadb": "MariaDB", "valkey": "Valkey"}

func engineName(kind string) string {
	if n, ok := engineNames[kind]; ok {
		return n
	}
	return kind
}

// serviceMoves lists the managed services the new manifest moves to another
// version, sorted by logical name. It returns a plain refusal for a move the
// box cannot make: another engine, an older version, or a version change of a
// service with no per-app dump (Valkey).
func serviceMoves(grants []store.ServiceGrant, newMan *manifest.Manifest) ([]movedService, error) {
	var out []movedService
	for _, g := range grants {
		dep, ok := newMan.Services[g.LogicalName]
		if !ok {
			continue
		}
		kind, version := normalizeEngine(dep.Type, dep.Version)
		switch {
		case kind == g.Kind && version == g.Version:
			continue
		case kind != g.Kind:
			return nil, fmt.Errorf("This version keeps its data in %s, and the app has it in %s. moose cannot move the data from one database engine to another, so it does not apply this version",
				engineName(kind), engineName(g.Kind))
		case g.Kind == "valkey":
			return nil, fmt.Errorf("This version needs %s %s, and the app has %s %s. moose cannot move it to another version", engineName(kind), version, engineName(g.Kind), g.Version)
		case compareVersions(version, g.Version) < 0:
			return nil, fmt.Errorf("This version needs an older %s (%s) than the app has (%s). moose cannot move the data to an older version, so it does not apply this version",
				engineName(kind), version, g.Version)
		}
		out = append(out, movedService{Service: g.LogicalName, Kind: g.Kind, From: g.Version, To: version})
	}
	return out, nil
}

// compareVersions compares two dotted version numbers ("15", "8.0", "10.11")
// part by part as numbers. A part that is not a number compares as text.
func compareVersions(a, b string) int {
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(ap), len(bp)); i++ {
		var x, y string
		if i < len(ap) {
			x = ap[i]
		}
		if i < len(bp) {
			y = bp[i]
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case x == y:
			continue
		case xerr == nil && yerr == nil:
			if xn < yn {
				return -1
			}
			if xn > yn {
				return 1
			}
		case x == "":
			return -1
		case y == "":
			return 1
		default:
			return strings.Compare(x, y)
		}
	}
	return 0
}

// moveServices moves each service in moves to its new version: the database
// on the new major is made and filled from the snapshot's dump, then the
// grant is pointed at it. rec.MovedServices is written first, so a rollback
// drops what was made. The caller holds the instance's lock and has stopped
// the app.
func (m *Manager) moveServices(ctx context.Context, id string, moves []movedService, snap Snapshot, base int, rec *updateRecord) error {
	if len(moves) == 0 {
		return nil
	}
	rec.MovedServices = slices.Clone(moves)
	if err := m.writeUpdateRecord(id, *rec); err != nil {
		return err
	}
	grants, err := m.store.GetServiceGrants(id)
	if err != nil {
		return fmt.Errorf("load service grants: %w", err)
	}
	byName := map[string]store.ServiceGrant{}
	for _, g := range grants {
		byName[g.LogicalName] = g
	}
	dumps := map[string]SnapshotDump{}
	for _, d := range snap.Dumps {
		dumps[d.Service] = d
	}
	for i, mv := range moves {
		g, ok := byName[mv.Service]
		if !ok || g.Version != mv.From {
			return fmt.Errorf("the %s grant is not on %s %s", mv.Service, mv.Kind, mv.From)
		}
		plain := fmt.Sprintf("moose could not move the app's data to %s %s", engineName(mv.Kind), mv.To)
		d, ok := dumps[mv.Service]
		if !ok || d.Kind != g.Kind || d.Version != g.Version || d.DBName != g.DBName {
			return &serviceMoveError{plain, fmt.Errorf("the snapshot holds no dump of the %s database", mv.Service)}
		}
		taken, err := m.reserveMoveNames(id, g.Kind, mv.To, g.DBName, g.RoleName)
		if err != nil {
			return err
		}
		if taken {
			return &serviceMoveError{
				fmt.Sprintf("moose could not move the app's data to %s %s, because another app's database there has the same name", engineName(g.Kind), mv.To),
				fmt.Errorf("%s or %s is taken on %s %s", g.DBName, g.RoleName, g.Kind, mv.To),
			}
		}
		if err := m.checkMoveSpace(ctx, g); err != nil {
			return err
		}
		rec.MovedServices[i].Started = true
		if err := m.writeUpdateRecord(id, *rec); err != nil {
			return err
		}
		if err := m.moveService(ctx, g, mv.To, filepath.Join(m.snapshotsDir(id), d.File), base); err != nil {
			var me *serviceMoveError
			if errors.As(err, &me) {
				return err
			}
			return &serviceMoveError{plain, err}
		}
		if err := m.store.MoveServiceGrant(id, mv.Service, mv.To, time.Now()); err != nil {
			return fmt.Errorf("point the %s grant at %s %s: %w", mv.Service, mv.Kind, mv.To, err)
		}
		slog.Info("managed database moved to a new version", "instance_id", id, "service", mv.Service,
			"kind", mv.Kind, "version", mv.From, "new_version", mv.To)
	}
	return nil
}

// reserveMoveNames holds dbName and roleName on kind and version for
// instance id, until releaseMoveNames. It reports true, and holds nothing,
// when a grant in the store or another update that runs now holds either
// name there. The check and the hold are one step under moveMu, so two
// updates cannot both pass it for the same names.
func (m *Manager) reserveMoveNames(id, kind, version, dbName, roleName string) (bool, error) {
	m.moveMu.Lock()
	defer m.moveMu.Unlock()
	keys := []string{kind + "/" + version + "/" + dbName, kind + "/" + version + "/" + roleName}
	for _, k := range keys {
		if owner, ok := m.moveNames[k]; ok && owner != id {
			return true, nil
		}
	}
	taken, err := m.store.ServiceNamesTaken(kind, version, dbName, roleName)
	if err != nil || taken {
		return taken, err
	}
	if m.moveNames == nil {
		m.moveNames = map[string]string{}
	}
	for _, k := range keys {
		m.moveNames[k] = id
	}
	return false, nil
}

// releaseMoveNames lets go of every name instance id holds for a database
// move. The update calls it when it ends, after any rollback: by then the
// names are in the store (the grant points there) or dropped.
func (m *Manager) releaseMoveNames(id string) {
	m.moveMu.Lock()
	defer m.moveMu.Unlock()
	for k, owner := range m.moveNames {
		if owner == id {
			delete(m.moveNames, k)
		}
	}
}

// moveService makes g's database on version to and fills it from the dump
// at dumpPath, with the new major's own tools. The old database is not
// touched. The caller checked that no grant holds g's names on to.
func (m *Manager) moveService(ctx context.Context, g store.ServiceGrant, to, dumpPath string, base int) error {
	if err := m.ensureServiceInstance(ctx, g.Kind, to, base); err != nil {
		return fmt.Errorf("ensure %s-%s: %w", g.Kind, to, err)
	}
	n := g
	n.Version = to
	// No grant holds these names on the new major, so anything there with
	// them is left from an earlier move of this app that did not finish.
	if err := m.dropGrantDB(ctx, n); err != nil {
		return fmt.Errorf("clear an earlier try: %w", err)
	}
	if err := m.createGrantRole(ctx, n); err != nil {
		return err
	}
	// restoreDatabase makes the database with the role as its owner and
	// fills it, the same as a snapshot restore on the same major.
	if err := m.restoreDatabase(ctx, n, dumpPath); err != nil {
		return err
	}
	return nil
}

// checkMoveSpace refuses a move that would leave the drive past the
// disk-full threshold: the database on the new major takes about as much
// room as the old one, which stays for 7 days.
func (m *Manager) checkMoveSpace(ctx context.Context, g store.ServiceGrant) error {
	est, err := m.dumpEstimate(ctx, g)
	if err != nil {
		return fmt.Errorf("measure the %s database: %w", g.LogicalName, err)
	}
	if mysqlTools[g.Kind].dump != "" {
		est /= dumpTextFactor // the database's own size, not its dump's
	}
	if err := m.checkSnapshotSpace(m.stateDir, est); err != nil {
		return &serviceMoveError{
			fmt.Sprintf("There is not enough free space on the drive to move the app's data to %s, so moose did not update the app. Free up some space and try again", engineName(g.Kind)),
			err,
		}
	}
	return nil
}

// createGrantRole makes g's role (Postgres) or user (MySQL family), with its
// password, on g's version. The database is made by restoreDatabase. A
// MySQL-family grant is kept by name, so it holds for the database made
// after it.
func (m *Manager) createGrantRole(ctx context.Context, g store.ServiceGrant) error {
	switch {
	case g.Kind == "postgres":
		if out, err := m.runServiceClient(ctx, "postgres", g.Version,
			"-c", fmt.Sprintf(`CREATE ROLE "%s" LOGIN PASSWORD '%s'`, g.RoleName, g.Password),
		); err != nil {
			return fmt.Errorf("psql: %w\n%s", err, strings.TrimSpace(out))
		}
	case mysqlTools[g.Kind].dump != "":
		sql := fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'; GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'",
			g.RoleName, g.Password, g.DBName, g.RoleName)
		if out, err := m.runServiceClient(ctx, g.Kind, g.Version, "-e", sql); err != nil {
			return fmt.Errorf("%s: %w\n%s", mysqlTools[g.Kind].client, err, strings.TrimSpace(out))
		}
	default:
		return fmt.Errorf("no database move for managed service kind %q", g.Kind)
	}
	return nil
}

// dropGrantDB drops g's database and role on g's version, and returns the
// error. dropServiceGrants is the same, but only logs.
func (m *Manager) dropGrantDB(ctx context.Context, g store.ServiceGrant) error {
	var args []string
	switch {
	case g.Kind == "postgres":
		args = []string{
			"-c", fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, g.DBName),
			"-c", fmt.Sprintf(`DROP ROLE IF EXISTS "%s"`, g.RoleName),
		}
	case mysqlTools[g.Kind].dump != "":
		args = []string{"-e", fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; DROP USER IF EXISTS '%s'@'%%'", g.DBName, g.RoleName)}
	default:
		return fmt.Errorf("no database move for managed service kind %q", g.Kind)
	}
	if out, err := m.runServiceClient(ctx, g.Kind, g.Version, args...); err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(out))
	}
	return nil
}

// moveServicesBack is the rollback of moveServices: the database on the new
// major is dropped and the grant points at the old one again, which the
// update never changed. It can run again. The caller restores the snapshot
// after it, so the old database also holds what the dump holds.
func (m *Manager) moveServicesBack(ctx context.Context, id string, moves []movedService) error {
	if len(moves) == 0 {
		return nil
	}
	grants, err := m.store.GetServiceGrants(id)
	if err != nil {
		return err
	}
	byName := map[string]store.ServiceGrant{}
	for _, g := range grants {
		byName[g.LogicalName] = g
	}
	for _, mv := range moves {
		g, ok := byName[mv.Service]
		if !ok {
			return fmt.Errorf("the %s grant is gone", mv.Service)
		}
		if mv.Started {
			n := g
			n.Version = mv.To
			if err := m.dropGrantDB(ctx, n); err != nil {
				// The app does not use it any more. Say so and go on: the
				// old database is what matters now.
				slog.Warn("update rollback: drop the database on the new version", "instance_id", id, "service", mv.Service,
					"kind", mv.Kind, "new_version", mv.To, "err", err)
			}
		}
		if err := m.store.MoveServiceGrantBack(id, mv.Service, mv.From); err != nil {
			return fmt.Errorf("point the %s grant back at %s %s: %w", mv.Service, mv.Kind, mv.From, err)
		}
		slog.Info("managed database moved back to its old version", "instance_id", id, "service", mv.Service,
			"kind", mv.Kind, "version", mv.From)
	}
	return nil
}

// retiredGrantsKept reports whether the old database of every move is still
// kept, so a rollback by hand can point the app back at it.
func (m *Manager) retiredGrantsKept(id string, moves []movedService) (bool, error) {
	if len(moves) == 0 {
		return true, nil
	}
	retired, err := m.store.GetRetiredServiceGrants(id)
	if err != nil {
		return false, err
	}
	for _, mv := range moves {
		found := false
		for _, r := range retired {
			if r.LogicalName == mv.Service && r.Version == mv.From {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// pruneRetiredGrants drops the old databases of an instance once nothing can
// roll back to them: 7 days after the move, or as soon as there is no previous
// generation to roll back to. The caller holds the instance's lock and has
// checked that no update or rollback is under way. A drop that fails is
// tried again at the next pass.
func (m *Manager) pruneRetiredGrants(ctx context.Context, id string, now time.Time) {
	retired, err := m.store.GetRetiredServiceGrants(id)
	if err != nil {
		slog.Warn("snapshot prune: read the retired databases", "instance_id", id, "err", err)
		return
	}
	if len(retired) == 0 {
		return
	}
	prev, err := m.store.GetInstancePrevImages(id)
	if err != nil {
		slog.Warn("snapshot prune: read the previous image pins", "instance_id", id, "err", err)
		return
	}
	for _, r := range retired {
		if len(prev) > 0 && now.Sub(r.RetiredAt) < SnapshotKeep {
			continue
		}
		if err := m.dropGrantDB(ctx, r.ServiceGrant); err != nil {
			slog.Warn("snapshot prune: drop an old database", "instance_id", id, "service", r.LogicalName,
				"kind", r.Kind, "version", r.Version, "err", err)
			continue
		}
		if err := m.store.DeleteRetiredServiceGrant(id, r.LogicalName, r.Version); err != nil {
			slog.Warn("snapshot prune: forget an old database", "instance_id", id, "service", r.LogicalName, "err", err)
			continue
		}
		slog.Info("snapshot prune: old database dropped", "instance_id", id, "service", r.LogicalName,
			"kind", r.Kind, "version", r.Version)
	}
}
