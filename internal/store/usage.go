package store

import (
	"database/sql"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

// UsageTotal is the usage summed over one group (a model or a task).
type UsageTotal struct {
	Key string
	usage.Tokens
	Messages int64
}

// AddUsage upsert-adds delta buckets (from usage.Collector.Poll) for one agent
// session. With reset, the session's stored rows are dropped first, in the same
// transaction, so buckets that cover the session's whole transcript replace
// what an earlier process wrote instead of counting it twice.
func (s *Store) AddUsage(session string, reset bool, bs []usage.Bucket) error {
	if len(bs) == 0 && !reset {
		return nil
	}
	return s.tx(func(tx *sql.Tx) error {
		if reset {
			if _, err := tx.Exec(`DELETE FROM usage WHERE session = ?`, session); err != nil {
				return err
			}
		}
		for _, b := range bs {
			_, err := tx.Exec(`INSERT INTO usage(minute, task, model, session, input, output, cache_read, cache_creation, messages)
				VALUES(?,?,?,?,?,?,?,?,?)
				ON CONFLICT(minute, task, model, session) DO UPDATE SET
				  input = input + excluded.input,
				  output = output + excluded.output,
				  cache_read = cache_read + excluded.cache_read,
				  cache_creation = cache_creation + excluded.cache_creation,
				  messages = messages + excluded.messages`,
				b.Minute.UTC().Truncate(time.Minute).Unix(), b.Task, b.Model, session,
				b.Input, b.Output, b.CacheRead, b.CacheCreation, b.Messages)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// UsageByModel sums usage per model over minutes in [since, until). A zero
// time leaves that end of the window open.
func (s *Store) UsageByModel(since, until time.Time) ([]UsageTotal, error) {
	return s.usageBy("model", since, until)
}

// UsageByTask sums usage per task over minutes in [since, until).
func (s *Store) UsageByTask(since, until time.Time) ([]UsageTotal, error) {
	return s.usageBy("task", since, until)
}

// UsageSince sums all usage in the trailing window that starts at since.
func (s *Store) UsageSince(since time.Time) (usage.Tokens, error) {
	var t usage.Tokens
	err := s.db.QueryRow(`SELECT COALESCE(SUM(input), 0), COALESCE(SUM(output), 0),
		COALESCE(SUM(cache_read), 0), COALESCE(SUM(cache_creation), 0)
		FROM usage WHERE minute >= ?`, minuteFloor(since)).
		Scan(&t.Input, &t.Output, &t.CacheRead, &t.CacheCreation)
	return t, err
}

// UsageBuckets returns stored usage from minutes at or after since, summed
// over sessions into one bucket per minute, task and model, oldest first.
// usage.EstimateBuckets turns them into plan-limit estimates.
func (s *Store) UsageBuckets(since time.Time) ([]usage.Bucket, error) {
	rows, err := s.db.Query(`SELECT minute, task, model, SUM(input), SUM(output), SUM(cache_read), SUM(cache_creation), SUM(messages)
		FROM usage WHERE minute >= ? GROUP BY minute, task, model ORDER BY minute, task, model`, minuteFloor(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usage.Bucket
	for rows.Next() {
		var b usage.Bucket
		var minute int64
		if err := rows.Scan(&minute, &b.Task, &b.Model, &b.Input, &b.Output, &b.CacheRead, &b.CacheCreation, &b.Messages); err != nil {
			return nil, err
		}
		b.Minute = time.Unix(minute, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// usageBy groups by col, which must be a trusted column name.
func (s *Store) usageBy(col string, since, until time.Time) ([]UsageTotal, error) {
	hi := int64(1<<63 - 1)
	if !until.IsZero() {
		hi = minuteFloor(until)
	}
	rows, err := s.db.Query(`SELECT `+col+`, SUM(input), SUM(output), SUM(cache_read), SUM(cache_creation), SUM(messages)
		FROM usage WHERE minute >= ? AND minute < ? GROUP BY `+col+` ORDER BY `+col, minuteFloor(since), hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageTotal
	for rows.Next() {
		var u UsageTotal
		if err := rows.Scan(&u.Key, &u.Input, &u.Output, &u.CacheRead, &u.CacheCreation, &u.Messages); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// minuteFloor maps t to the bucket minute containing it; zero means the start of time.
func minuteFloor(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Truncate(time.Minute).Unix()
}
