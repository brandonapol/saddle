package store

import (
	"context"
	"database/sql"
)

// Bounds on what one snapshot reads, whatever the database holds.
const (
	MaxLive   = 500  // live tasks
	MaxEnded  = 50   // most recent merged or closed tasks
	MaxOpen   = 200  // open questions, retries
	MaxEvents = 1000 // events after the cursor
)

// Snapshot is everything `status` and the TUI show, read in one
// transaction so it is consistent. It is a value: nothing in it changes
// after it is returned.
type Snapshot struct {
	At        int64      // when it was read, Unix milliseconds
	Live      []Task     // not merged or closed, oldest first
	Ended     []Task     // merged or closed, most recent first
	Questions []Question // unanswered, oldest first
	Settings  map[string]Setting
	Providers []Provider
	Retries   []Retry
	Events    []Event // after the cursor, oldest first
	// Cursor is the id to pass as since next time.
	Cursor int64
	// Gap is true when events after the cursor were not all returned,
	// because they were pruned or there were more than MaxEvents. The
	// reader must then treat everything it remembers as stale.
	Gap bool
}

// The snapshot's queries. Each must use an index: TestSnapshotQueriesUseIndexes
// runs EXPLAIN QUERY PLAN on every one.
var snapshotQueries = map[string]string{
	"live":      "SELECT " + taskCols + " FROM tasks WHERE state NOT IN ('merged','closed') ORDER BY id LIMIT ?",
	"ended":     "SELECT " + taskCols + " FROM tasks WHERE state IN ('merged','closed') ORDER BY since DESC LIMIT ?",
	"questions": "SELECT id, task, body, options, asked FROM questions WHERE answer IS NULL ORDER BY id LIMIT ?",
	"events":    "SELECT id, ts, task, kind, data FROM events WHERE id > ? ORDER BY id LIMIT ?",
	"oldest":    "SELECT COALESCE(MIN(id), 0), COALESCE(MAX(id), 0) FROM events",
	"pending":   pendingQuery,
}

// Snapshot reads the current state and the events after since. Pass 0 the
// first time: no events are returned then, only the cursor to start from.
func (s *Store) Snapshot(ctx context.Context, since int64) (Snapshot, error) {
	snap := Snapshot{At: s.nowMS(), Settings: map[string]Setting{}}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snap, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, snapshotQueries["live"], MaxLive)
	if err != nil {
		return snap, err
	}
	if snap.Live, err = scanTasks(rows); err != nil {
		return snap, err
	}
	if rows, err = tx.QueryContext(ctx, snapshotQueries["ended"], MaxEnded); err != nil {
		return snap, err
	}
	if snap.Ended, err = scanTasks(rows); err != nil {
		return snap, err
	}

	if rows, err = tx.QueryContext(ctx, snapshotQueries["questions"], MaxOpen); err != nil {
		return snap, err
	}
	for rows.Next() {
		var q Question
		var opts string
		if err := rows.Scan(&q.ID, &q.Task, &q.Body, &opts, &q.Asked); err != nil {
			rows.Close()
			return snap, err
		}
		if q.Options, err = fromJSON[string](opts, "questions.options"); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Questions = append(snap.Questions, q)
	}
	if err := closeRows(rows); err != nil {
		return snap, err
	}

	// settings, providers and retries are small by construction: one row
	// per key, provider or failing action.
	if rows, err = tx.QueryContext(ctx, "SELECT key, value, updated, updated_by FROM settings"); err != nil {
		return snap, err
	}
	for rows.Next() {
		var st Setting
		if err := rows.Scan(&st.Key, &st.Value, &st.Updated, &st.UpdatedBy); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Settings[st.Key] = st
	}
	if err := closeRows(rows); err != nil {
		return snap, err
	}
	if rows, err = tx.QueryContext(ctx, "SELECT name, available, out_until, detail FROM providers ORDER BY name"); err != nil {
		return snap, err
	}
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Name, &p.Available, &p.OutUntil, &p.Detail); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Providers = append(snap.Providers, p)
	}
	if err := closeRows(rows); err != nil {
		return snap, err
	}
	if rows, err = tx.QueryContext(ctx, "SELECT key, inputs_hash, attempts, next_at, last_error FROM retries ORDER BY key LIMIT ?", MaxOpen); err != nil {
		return snap, err
	}
	for rows.Next() {
		var r Retry
		if err := rows.Scan(&r.Key, &r.InputsHash, &r.Attempts, &r.NextAt, &r.LastError); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Retries = append(snap.Retries, r)
	}
	if err := closeRows(rows); err != nil {
		return snap, err
	}

	var oldest, newest int64
	if err := tx.QueryRowContext(ctx, snapshotQueries["oldest"]).Scan(&oldest, &newest); err != nil {
		return snap, err
	}
	snap.Cursor = newest
	if since <= 0 {
		return snap, nil
	}
	if since > newest {
		// The cursor is from another database, or events were deleted.
		snap.Gap = true
		return snap, nil
	}
	// Events (since, oldest) were pruned before this reader saw them.
	if oldest > since+1 {
		snap.Gap = true
	}
	if rows, err = tx.QueryContext(ctx, snapshotQueries["events"], since, MaxEvents+1); err != nil {
		return snap, err
	}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Task, &e.Kind, &e.Data); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Events = append(snap.Events, e)
	}
	if err := closeRows(rows); err != nil {
		return snap, err
	}
	if len(snap.Events) > MaxEvents {
		// More than one read's worth: hand back none and say so, rather
		// than a partial history the reader might take for complete.
		snap.Events, snap.Gap = nil, true
	}
	return snap, nil
}

func closeRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}
