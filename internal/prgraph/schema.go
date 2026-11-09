package prgraph

import (
	"context"
	"database/sql"
	"fmt"
)

// SchemaVersion is prgraph's own migration version, kept in prgraph_meta
// apart from the host's.
const SchemaVersion = 2

// schemaV1 is ADR 0009 §1's tables. Every table is prefixed prgraph_ so the
// package can later move to its own database file. Two attempt columns go
// beyond the ADR's sketch: last (the latest outcome kind, so a forge wait is
// told apart from a failure) and waits (consecutive forge waits, which never
// count toward a cap).
const schemaV1 = `
CREATE TABLE IF NOT EXISTS prgraph_meta(
  key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS prgraph_change(
  id TEXT PRIMARY KEY, branch TEXT NOT NULL UNIQUE, base TEXT NOT NULL,
  head TEXT NOT NULL,
  seq INTEGER NOT NULL,
  pr INTEGER,
  state TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
  since INTEGER NOT NULL, deadline INTEGER, retry_at INTEGER,
  automerge INTEGER NOT NULL DEFAULT 1, hold_until INTEGER,
  gen INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS prgraph_edge(
  change TEXT NOT NULL, dep TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('declared','inferred','verified')),
  source TEXT NOT NULL,
  PRIMARY KEY(change, dep));
CREATE TABLE IF NOT EXISTS prgraph_check(
  head TEXT NOT NULL, name TEXT NOT NULL, status TEXT NOT NULL,
  required INTEGER NOT NULL, host_owned INTEGER NOT NULL, observed_at INTEGER,
  PRIMARY KEY(head, name));
CREATE TABLE IF NOT EXISTS prgraph_gate(
  head TEXT NOT NULL, onto TEXT NOT NULL, status TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '', ran_at INTEGER,
  PRIMARY KEY(head, onto));
CREATE TABLE IF NOT EXISTS prgraph_attempt(
  key TEXT PRIMARY KEY, kind TEXT NOT NULL, inputs TEXT NOT NULL DEFAULT '',
  n INTEGER NOT NULL DEFAULT 0, waits INTEGER NOT NULL DEFAULT 0,
  last TEXT NOT NULL DEFAULT '', next_at INTEGER,
  first_err TEXT NOT NULL DEFAULT '', last_err TEXT NOT NULL DEFAULT '',
  escalated INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS prgraph_tick(
  id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, input_hash TEXT NOT NULL,
  input BLOB NOT NULL, plan BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS prgraph_tick_ts ON prgraph_tick(ts);
`

// schemaV2 narrows prgraph_edge to declared after edges (ADR 0012 rule 8,
// #934): the kind and source columns go, and so does every row that wasn't
// kind 'declared' from source 'after'. SQLite can't drop a column with a
// CHECK on it, so the table is rebuilt.
const schemaV2 = `
CREATE TABLE prgraph_edge_v2(
  change TEXT NOT NULL, dep TEXT NOT NULL,
  PRIMARY KEY(change, dep));
INSERT INTO prgraph_edge_v2(change, dep)
  SELECT change, dep FROM prgraph_edge WHERE kind = 'declared' AND source = 'after';
DROP TABLE prgraph_edge;
ALTER TABLE prgraph_edge_v2 RENAME TO prgraph_edge;
`

// Migrate creates or upgrades prgraph's tables in db, whatever database the
// host passes. It is idempotent.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("prgraph: migrate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
		return fmt.Errorf("prgraph: migrate v1: %w", err)
	}
	if old, err := edgeHasKind(ctx, tx); err != nil {
		return fmt.Errorf("prgraph: migrate v2: %w", err)
	} else if old {
		if _, err := tx.ExecContext(ctx, schemaV2); err != nil {
			return fmt.Errorf("prgraph: migrate v2: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO prgraph_meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fmt.Sprint(SchemaVersion)); err != nil {
		return fmt.Errorf("prgraph: migrate: version: %w", err)
	}
	return tx.Commit()
}

// edgeHasKind reports whether prgraph_edge still has v1's kind column.
func edgeHasKind(ctx context.Context, tx *sql.Tx) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info('prgraph_edge')`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == "kind" {
			return true, nil
		}
	}
	return false, rows.Err()
}
