package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestTriageCallStats(t *testing.T) {
	s := openTest(t)
	if st, err := s.TriageStats(); err != nil || st != (TriageStats{}) {
		t.Fatalf("empty = %+v, %v", st, err)
	}
	when := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ok := TriageCall{
		Time: when, Purpose: "screen", Task: "t3", Verdict: "working",
		Confidence: 0.9, Route: "drop", Latency: 20 * time.Millisecond, InputTokens: 400,
	}
	if err := s.RecordTriageCall(ok); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTriageCall(TriageCall{Purpose: "needs_user", Task: "t0", Error: "jev: 401 Unauthorized"}); err != nil {
		t.Fatal(err)
	}
	st, err := s.TriageStats()
	if err != nil || st.Calls != 2 || st.Errors != 1 || st.InputTokens != 400 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	got, err := s.TriageCalls()
	if err != nil || len(got) != 2 {
		t.Fatalf("calls = %+v, %v", got, err)
	}
	if got[0] != ok {
		t.Fatalf("first = %+v, want %+v", got[0], ok)
	}
	if got[1].Error == "" || got[1].Task != "t0" || got[1].Purpose != "needs_user" {
		t.Fatalf("failure = %+v", got[1])
	}
}

// A database from before the triage log gains the table on open.
func TestTriageCallsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:len(migrations)-1] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = ` + fmt.Sprint(len(migrations)-1)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.RecordTriageCall(TriageCall{Purpose: "screen", Task: "t1", Error: "jev: down"}); err != nil {
		t.Fatal(err)
	}
	st, err := s.TriageStats()
	if err != nil || st.Calls != 1 || st.Errors != 1 {
		t.Fatalf("after migrate = %+v, %v", st, err)
	}
}
