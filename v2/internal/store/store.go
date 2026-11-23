package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // the database driver
)

// DBFile is the database's name inside the state directory.
const DBFile = "state.db"

// Store is one process's handle on the database. Writes go through Tx on a
// single connection; reads use a separate read-only pool.
type Store struct {
	path string
	w, r *sql.DB
	// Now is the clock. Tests replace it.
	Now func() time.Time
}

// ErrNewer means the database was written by a newer saddle than this one.
var ErrNewer = errors.New("state.db was created by a newer saddle; upgrade this binary")

// Open opens, creating or migrating if needed, the database in dir (the
// state directory, usually <repo>/.saddle2).
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, DBFile)
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	// One writer per process: SQLite allows one at a time anyway, and a
	// single connection keeps BEGIN IMMEDIATE waits inside busy_timeout.
	w.SetMaxOpenConns(1)
	s := &Store{path: path, w: w, Now: time.Now}
	if err := s.migrate(); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if s.r, err = sql.Open("sqlite", dsn(path, true)); err != nil {
		_ = w.Close()
		return nil, err
	}
	return s, nil
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// Close releases both connections.
func (s *Store) Close() error {
	var err error
	if s.r != nil {
		err = s.r.Close()
	}
	return errors.Join(err, s.w.Close())
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

func (s *Store) nowMS() int64 { return s.Now().UnixMilli() }

// migrate brings the schema up to date. An existing database is copied
// beside itself before any migration runs.
func (s *Store) migrate() error {
	var have int
	if err := s.w.QueryRow("PRAGMA user_version").Scan(&have); err != nil {
		return err
	}
	if have > len(migrations) {
		return fmt.Errorf("%w (schema %d, this binary knows %d)", ErrNewer, have, len(migrations))
	}
	if have == len(migrations) {
		return nil
	}
	if have > 0 {
		bak := fmt.Sprintf("%s.bak-schema%d-%s", s.path, have, s.Now().UTC().Format("20060102T150405Z"))
		if _, err := s.w.Exec("VACUUM INTO ?", bak); err != nil {
			return fmt.Errorf("back up before migrating: %w", err)
		}
	}
	for i := have; i < len(migrations); i++ {
		tx, err := s.w.Begin()
		if err != nil {
			return err
		}
		// Another process may have migrated while this one waited.
		var now int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&now); err != nil {
			_ = tx.Rollback()
			return err
		}
		if now > i {
			_ = tx.Rollback()
			continue
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Tx is one write transaction. It is the only way to change the database.
type Tx struct {
	tx  *sql.Tx
	now int64
}

// Now is the transaction's time in Unix milliseconds: every row it writes
// carries the same one.
func (t *Tx) Now() int64 { return t.now }

// Tx runs fn in a write transaction (BEGIN IMMEDIATE, so a read followed by
// a write inside fn can't lose another process's update). It commits when fn
// returns nil and rolls back otherwise.
func (s *Store) Tx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Tx{tx: tx, now: s.nowMS()}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Watch sends on the returned channel when another connection, in this
// process or any other, has committed a change. It polls PRAGMA data_version
// on one connection, which costs no I/O when nothing changed. Sends never
// block: several changes while the receiver is busy arrive as one. The
// channel closes when ctx ends.
func (s *Store) Watch(ctx context.Context, every time.Duration) (<-chan struct{}, error) {
	conn, err := s.r.Conn(ctx)
	if err != nil {
		return nil, err
	}
	version := func() (int64, error) {
		var v int64
		err := conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
		return v, err
	}
	last, err := version()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		defer conn.Close()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			v, err := version()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if v != last {
				last = v
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	return ch, nil
}

// RetainEventsDays is the setting that says how long events and usage rows
// are kept. Unset means 30 days.
const RetainEventsDays = "retain.events_days"

// Prune deletes events and usage older than the retention setting and
// returns how many rows went.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	var n int64
	err := s.Tx(ctx, func(t *Tx) error {
		days := 30
		if v, ok, err := t.Setting(RetainEventsDays); err != nil {
			return err
		} else if ok {
			if _, err := fmt.Sscanf(v.Value, "%d", &days); err != nil || days < 1 {
				return fmt.Errorf("setting %s = %q: want a number of days, 1 or more", RetainEventsDays, v.Value)
			}
		}
		cut := t.now - int64(days)*24*time.Hour.Milliseconds()
		for _, q := range []string{"DELETE FROM events WHERE ts < ?", "DELETE FROM usage WHERE minute < ? / 60000"} {
			res, err := t.tx.Exec(q, cut)
			if err != nil {
				return err
			}
			k, _ := res.RowsAffected()
			n += k
		}
		return nil
	})
	return n, err
}
