package store

import (
	"strings"
	"time"
)

// The watcher heartbeat table (#789) is created on first write rather than
// by a migration: it is a cache of what a running engine last saw, an older
// binary never reads it, and losing it loses nothing.
const watchersTable = `CREATE TABLE IF NOT EXISTS watcher_ticks(
  name TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  pid INTEGER NOT NULL DEFAULT 0,
  restarts INTEGER NOT NULL DEFAULT 0,
  last_panic TEXT NOT NULL DEFAULT ''
)`

// WatcherTick is one background watcher's last heartbeat.
type WatcherTick struct {
	Name      string
	At        time.Time
	PID       int // process that ran it
	Restarts  int
	LastPanic string
}

// TickWatcher records watcher t's heartbeat.
func (s *Store) TickWatcher(t WatcherTick) error {
	if _, err := s.db.Exec(watchersTable); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO watcher_ticks(name, ts, pid, restarts, last_panic) VALUES(?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET ts = excluded.ts, pid = excluded.pid, restarts = excluded.restarts, last_panic = excluded.last_panic`,
		t.Name, t.At.UnixNano(), t.PID, t.Restarts, t.LastPanic)
	return err
}

// WatcherTicks returns every recorded heartbeat by name, none when no engine
// has ever recorded one.
func (s *Store) WatcherTicks() ([]WatcherTick, error) {
	rows, err := s.db.Query(`SELECT name, ts, pid, restarts, last_panic FROM watcher_ticks ORDER BY name`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []WatcherTick
	for rows.Next() {
		var t WatcherTick
		var ns int64
		if err := rows.Scan(&t.Name, &ns, &t.PID, &t.Restarts, &t.LastPanic); err != nil {
			return nil, err
		}
		t.At = time.Unix(0, ns)
		out = append(out, t)
	}
	return out, rows.Err()
}
