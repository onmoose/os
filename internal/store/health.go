package store

import (
	"strings"
	"time"

	"github.com/onmoose/moose/internal/health"
)

// IntegrityCheck runs SQLite's PRAGMA integrity_check and returns its result.
// A sound database returns the single line "ok"; a corrupt one returns one or
// more rows describing the damage (capped by SQLite at 100). The rows are
// joined with newlines, so the result is "ok" exactly when the database is
// sound and a multi-line corruption report otherwise — the caller compares
// against "ok" and uses the report as the issue's details.
//
// This is the persistence-boundary detector for the locus-C brain-db-corrupt
// health issue (HEALTH.md # Detector catalog): the SQLite query lives here, in
// the store, not in the health package. The check is read-only and runs against
// the live database on the brain's single serialized connection.
func (s *Store) IntegrityCheck() (string, error) {
	rows, err := s.db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

// UpsertHealthIssue inserts or replaces one health issue row. Called on every
// Raise — both on first raise (new row) and on re-raise (updates last_checked_at
// and details). Idempotent.
func (s *Store) UpsertHealthIssue(h health.Issue) error {
	boolInt := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO health_issues
		 (id, instance_key, category, severity, tier,
		  blocks_writes, blocks_apps, blocks_users,
		  summary, details, raised_at, last_checked_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.InstanceKey, string(h.Category), string(h.Severity), h.Tier,
		boolInt(h.BlocksWrites), boolInt(h.BlocksApps), boolInt(h.BlocksUsers),
		h.Summary, h.Details,
		h.RaisedAt.UnixMilli(), h.LastCheckedAt.UnixMilli(),
	)
	return err
}

// DeleteHealthIssue removes a health issue. No-op if the row doesn't exist
// (idempotent delete is intentional — Clear after restart is safe).
func (s *Store) DeleteHealthIssue(id, instanceKey string) error {
	_, err := s.db.Exec(
		`DELETE FROM health_issues WHERE id=? AND instance_key=?`, id, instanceKey)
	return err
}

// BatchUpsertAndDelete runs all upserts and deletes in a single transaction.
// Used by ApplyFindings so a crash mid-reconcile can't leave SQLite torn.
func (s *Store) BatchUpsertAndDelete(upserts []health.Issue, deletes []health.IssueKey) error {
	if len(upserts) == 0 && len(deletes) == 0 {
		return nil
	}
	boolInt := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, h := range upserts {
		_, err := tx.Exec(
			`INSERT OR REPLACE INTO health_issues
			 (id, instance_key, category, severity, tier,
			  blocks_writes, blocks_apps, blocks_users,
			  summary, details, raised_at, last_checked_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			h.ID, h.InstanceKey, string(h.Category), string(h.Severity), h.Tier,
			boolInt(h.BlocksWrites), boolInt(h.BlocksApps), boolInt(h.BlocksUsers),
			h.Summary, h.Details,
			h.RaisedAt.UnixMilli(), h.LastCheckedAt.UnixMilli(),
		)
		if err != nil {
			return err
		}
	}
	for _, k := range deletes {
		if _, err := tx.Exec(
			`DELETE FROM health_issues WHERE id=? AND instance_key=?`, k.ID, k.InstanceKey,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListHealthIssues returns every active health issue, ordered by raised_at.
// Used only at brain startup to restore the in-memory registry.
func (s *Store) ListHealthIssues() ([]health.Issue, error) {
	rows, err := s.db.Query(
		`SELECT id, instance_key, category, severity, tier,
		        blocks_writes, blocks_apps, blocks_users,
		        summary, details, raised_at, last_checked_at
		 FROM health_issues ORDER BY raised_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []health.Issue
	for rows.Next() {
		var h health.Issue
		var category, severity string
		var blocksWrites, blocksApps, blocksUsers int
		var raisedAt, lastCheckedAt int64
		if err := rows.Scan(
			&h.ID, &h.InstanceKey, &category, &severity, &h.Tier,
			&blocksWrites, &blocksApps, &blocksUsers,
			&h.Summary, &h.Details, &raisedAt, &lastCheckedAt,
		); err != nil {
			return nil, err
		}
		h.Category = health.Category(category)
		h.Severity = health.Severity(severity)
		h.BlocksWrites = blocksWrites != 0
		h.BlocksApps = blocksApps != 0
		h.BlocksUsers = blocksUsers != 0
		h.RaisedAt = time.UnixMilli(raisedAt).UTC()
		h.LastCheckedAt = time.UnixMilli(lastCheckedAt).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}
