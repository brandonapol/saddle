package prgraph

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SQLStore is the Store over prgraph's own tables in the host's *sql.DB
// (ADR 0009 §1). Times are stored as Unix seconds; a zero time is NULL.
type SQLStore struct {
	DB *sql.DB
	// Policy is the host's configuration; Load returns it with the rows.
	Policy Policy
}

var _ Store = (*SQLStore)(nil)

// OpenSQLStore migrates prgraph's tables in db and returns a store over them.
func OpenSQLStore(ctx context.Context, db *sql.DB, p Policy) (*SQLStore, error) {
	if err := Migrate(ctx, db); err != nil {
		return nil, err
	}
	return &SQLStore{DB: db, Policy: p}, nil
}

func unix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func fromUnix(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0).UTC()
}

// Load returns the stored changes, edges and attempts, each sorted by key,
// with the store's policy.
func (s *SQLStore) Load(ctx context.Context) (State, error) {
	st := State{Policy: s.Policy}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, branch, base, head, seq, pr, state, reason,
		since, deadline, retry_at, automerge, hold_until, gen FROM prgraph_change ORDER BY id`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var c Change
		var pr, since, deadline, retry, hold sql.NullInt64
		var am int
		if err := rows.Scan(&c.ID, &c.Branch, &c.Base, &c.Head, &c.Seq, &pr, &c.State, &c.Reason,
			&since, &deadline, &retry, &am, &hold, &c.Gen); err != nil {
			_ = rows.Close()
			return st, err
		}
		c.PR = int(pr.Int64)
		c.Since, c.Deadline, c.RetryAt, c.HoldUntil = fromUnix(since), fromUnix(deadline), fromUnix(retry), fromUnix(hold)
		c.Automerge = am != 0
		st.Changes = append(st.Changes, c)
	}
	if err := closeRows(rows); err != nil {
		return st, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT change, dep, kind, source FROM prgraph_edge ORDER BY change, dep`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.Change, &e.Dep, &e.Kind, &e.Source); err != nil {
			_ = rows.Close()
			return st, err
		}
		st.Edges = append(st.Edges, e)
	}
	if err := closeRows(rows); err != nil {
		return st, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT key, kind, inputs, n, waits, last, next_at,
		first_err, last_err, escalated FROM prgraph_attempt ORDER BY key`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var a Attempt
		var next sql.NullInt64
		var esc int
		if err := rows.Scan(&a.Key, &a.Kind, &a.Inputs, &a.N, &a.Waits, &a.Last, &next,
			&a.FirstErr, &a.LastErr, &esc); err != nil {
			_ = rows.Close()
			return st, err
		}
		a.NextAt, a.Escalated = fromUnix(next), esc != 0
		st.Attempts = append(st.Attempts, a)
	}
	return st, closeRows(rows)
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// PutChange inserts or replaces a change row: the host's intent.
func (s *SQLStore) PutChange(ctx context.Context, c Change) error {
	var pr any
	if c.PR != 0 {
		pr = c.PR
	}
	am := 0
	if c.Automerge {
		am = 1
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO prgraph_change(id, branch, base, head, seq, pr, state,
		reason, since, deadline, retry_at, automerge, hold_until, gen) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET branch=excluded.branch, base=excluded.base, head=excluded.head,
		seq=excluded.seq, pr=excluded.pr, state=excluded.state, reason=excluded.reason, since=excluded.since,
		deadline=excluded.deadline, retry_at=excluded.retry_at, automerge=excluded.automerge,
		hold_until=excluded.hold_until, gen=excluded.gen`,
		c.ID, c.Branch, c.Base, c.Head, c.Seq, pr, string(c.State), c.Reason, unix(c.Since),
		unix(c.Deadline), unix(c.RetryAt), am, unix(c.HoldUntil), c.Gen)
	if err != nil {
		return fmt.Errorf("prgraph: put change %s: %w", c.ID, err)
	}
	return nil
}

// PutEdge inserts or replaces one dependency edge.
func (s *SQLStore) PutEdge(ctx context.Context, e Edge) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO prgraph_edge(change, dep, kind, source) VALUES(?,?,?,?)
		ON CONFLICT(change, dep) DO UPDATE SET kind=excluded.kind, source=excluded.source`,
		e.Change, e.Dep, string(e.Kind), e.Source)
	if err != nil {
		return fmt.Errorf("prgraph: put edge %s->%s: %w", e.Change, e.Dep, err)
	}
	return nil
}

// Transition applies a to its row with UPDATE ... WHERE gen = a.Guard.Gen.
// Each "id=head" in a.Guard.Deps must still name a non-terminal row with
// that head. A stale guard is (false, nil).
func (s *SQLStore) Transition(ctx context.Context, a Action, now time.Time) (bool, error) {
	if a.Kind != ActTransition {
		return false, fmt.Errorf("prgraph: %s is not a transition", a)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, d := range a.Guard.Deps {
		id, head, _ := strings.Cut(d, "=")
		var h, st string
		err := tx.QueryRowContext(ctx, `SELECT head, state FROM prgraph_change WHERE id = ?`, id).Scan(&h, &st)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if h != head || ChangeState(st).Terminal() {
			return false, nil
		}
	}
	q := `UPDATE prgraph_change SET state = ?, reason = ?, since = ?, deadline = ?, retry_at = ?, gen = gen + 1`
	args := []any{string(a.To), a.Reason, now.Unix(), unix(a.Deadline), unix(a.RetryAt)}
	if a.Head != "" {
		q += `, head = ?`
		args = append(args, a.Head)
	}
	q += ` WHERE id = ? AND gen = ?`
	args = append(args, a.Change, a.Guard.Gen)
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	return true, tx.Commit()
}

// PutAttempt upserts one attempt row.
func (s *SQLStore) PutAttempt(ctx context.Context, att Attempt) error {
	esc := 0
	if att.Escalated {
		esc = 1
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO prgraph_attempt(key, kind, inputs, n, waits, last, next_at,
		first_err, last_err, escalated) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET kind=excluded.kind, inputs=excluded.inputs, n=excluded.n,
		waits=excluded.waits, last=excluded.last, next_at=excluded.next_at, first_err=excluded.first_err,
		last_err=excluded.last_err, escalated=excluded.escalated`,
		att.Key, att.Kind, att.Inputs, att.N, att.Waits, string(att.Last), unix(att.NextAt),
		att.FirstErr, att.LastErr, esc)
	if err != nil {
		return fmt.Errorf("prgraph: put attempt %s: %w", att.Key, err)
	}
	return nil
}

// RecordTick stores one tick's input and plan.
func (s *SQLStore) RecordTick(ctx context.Context, t Tick) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO prgraph_tick(ts, input_hash, input, plan) VALUES(?,?,?,?)`,
		t.TS.UnixNano(), t.InputHash, t.Input, t.Plan)
	if err != nil {
		return fmt.Errorf("prgraph: record tick: %w", err)
	}
	return nil
}

// Ticks returns the recorded ticks with from <= ts < until, oldest first.
func (s *SQLStore) Ticks(ctx context.Context, from, until time.Time) ([]Tick, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT ts, input_hash, input, plan FROM prgraph_tick
		WHERE ts >= ? AND ts < ? ORDER BY ts, id`, from.UnixNano(), until.UnixNano())
	if err != nil {
		return nil, err
	}
	var out []Tick
	for rows.Next() {
		var t Tick
		var ts int64
		if err := rows.Scan(&ts, &t.InputHash, &t.Input, &t.Plan); err != nil {
			_ = rows.Close()
			return nil, err
		}
		t.TS = time.Unix(0, ts).UTC()
		out = append(out, t)
	}
	return out, closeRows(rows)
}

// PruneTicks deletes the ticks recorded before before and reports how many.
func (s *SQLStore) PruneTicks(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM prgraph_tick WHERE ts < ?`, before.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
