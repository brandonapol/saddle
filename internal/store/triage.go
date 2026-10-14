package store

import (
	"time"
)

// TriageCall is one Jev request and how it turned out (#512). Error is
// empty when the call succeeded. InputTokens is what the API reported, so
// spend can be estimated from Jev's list price; a failure reports none.
type TriageCall struct {
	Time        time.Time
	Purpose     string
	Task        string
	Verdict     string
	Confidence  float64
	Route       string
	Latency     time.Duration
	Error       string
	InputTokens int
}

// TriageStats is the triage log rolled up.
type TriageStats struct {
	Calls       int
	Errors      int
	InputTokens int64
}

// RecordTriageCall appends one Jev call. A zero Time is stored as now.
func (s *Store) RecordTriageCall(c TriageCall) error {
	ts := c.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	_, err := s.db.Exec(`INSERT INTO triage_calls(ts, purpose, task, verdict, confidence, route, latency_ms, error, input_tokens)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		ts.Unix(), c.Purpose, c.Task, c.Verdict, c.Confidence, c.Route, c.Latency.Milliseconds(), c.Error, c.InputTokens)
	return err
}

// TriageCalls returns every recorded call, oldest first.
func (s *Store) TriageCalls() ([]TriageCall, error) {
	rows, err := s.db.Query(`SELECT ts, purpose, task, verdict, confidence, route, latency_ms, error, input_tokens
		FROM triage_calls ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriageCall
	for rows.Next() {
		var c TriageCall
		var sec, ms int64
		if err := rows.Scan(&sec, &c.Purpose, &c.Task, &c.Verdict, &c.Confidence, &c.Route, &ms, &c.Error, &c.InputTokens); err != nil {
			return nil, err
		}
		c.Time = time.Unix(sec, 0).UTC()
		c.Latency = time.Duration(ms) * time.Millisecond
		out = append(out, c)
	}
	return out, rows.Err()
}

// TriageStats sums the triage log. Errors counts calls whose error is set.
func (s *Store) TriageStats() (TriageStats, error) {
	var st TriageStats
	err := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN error != '' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(input_tokens), 0)
		FROM triage_calls`).Scan(&st.Calls, &st.Errors, &st.InputTokens)
	return st, err
}
