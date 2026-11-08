package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/brandonapol/saddle/internal/retry"
)

// Retry attempts (#826): one row per key in retry_attempts. Store is a
// retry.Store.
var _ retry.Store = (*Store)(nil)

const retryCols = `key, kind, outcome, reason, n, tries, next_at, first_err, last_err, sig, inputs, escalated, updated_at`

// RetryAttempt reads key's attempt.
func (s *Store) RetryAttempt(key string) (retry.Attempt, bool, error) {
	var a retry.Attempt
	var outcome string
	var next, updated int64
	var esc int
	err := s.db.QueryRow(`SELECT `+retryCols+` FROM retry_attempts WHERE key = ?`, key).Scan(
		&a.Key, &a.Kind, &outcome, &a.Reason, &a.N, &a.Tries, &next, &a.FirstErr, &a.LastErr, &a.Sig, &a.Inputs, &esc, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return retry.Attempt{}, false, nil
	}
	if err != nil {
		return retry.Attempt{}, false, err
	}
	a.Outcome, a.Escalated = retry.Outcome(outcome), esc != 0
	a.NextAt, a.UpdatedAt = time.UnixMilli(next), time.UnixMilli(updated)
	return a, true, nil
}

// PutRetryAttempt writes a, replacing key's row.
func (s *Store) PutRetryAttempt(a retry.Attempt) error {
	esc := 0
	if a.Escalated {
		esc = 1
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO retry_attempts(`+retryCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.Key, a.Kind, string(a.Outcome), a.Reason, a.N, a.Tries, a.NextAt.UnixMilli(), a.FirstErr, a.LastErr, a.Sig, a.Inputs, esc, a.UpdatedAt.UnixMilli())
	return err
}

// DeleteRetryAttempt drops key's attempt.
func (s *Store) DeleteRetryAttempt(key string) error {
	_, err := s.db.Exec(`DELETE FROM retry_attempts WHERE key = ?`, key)
	return err
}
