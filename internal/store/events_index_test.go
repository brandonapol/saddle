package store

import (
	"fmt"
	"strings"
	"testing"
)

// seedEvents inserts n events of kind "tool" spread over 500 tasks in one
// transaction, the bulk of a long-lived state.db.
func seedEvents(t testing.TB, s *Store, n int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st, err := tx.Prepare(`INSERT INTO events(ts, task, kind, data) VALUES(0, ?, 'tool', 'Edit')`)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := st.Exec(fmt.Sprintf("n%d", i%500)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// #778: the per-task lookups answer what a full scan did and read only the
// rows they return.
func TestTaskEventLookups(t *testing.T) {
	s := openTest(t)
	seedEvents(t, s, 2000)
	s.Event("t1", "repair_spawned", "t9")
	s.Event("t2", "repair_spawned", "t8")
	s.Event("t1", "repair_failed", "no slot")
	s.Event("t1", "repair_spawned", "t10")
	seedEvents(t, s, 2000)

	before := s.EventRowsRead()
	es, err := s.TaskEvents("t1", "repair_spawned")
	if err != nil || len(es) != 2 || es[0].Data != "t9" || es[1].Data != "t10" {
		t.Fatalf("TaskEvents = %+v, %v; want t9 then t10", es, err)
	}
	e, ok, err := s.LastTaskEvent("t1", "repair_failed", "repair_spawned")
	if err != nil || !ok || e.Kind != "repair_spawned" || e.Data != "t10" {
		t.Fatalf("LastTaskEvent = %+v, %v, %v; want the t10 spawn", e, ok, err)
	}
	if _, ok, err := s.LastTaskEvent("t3", "repair_spawned"); ok || err != nil {
		t.Fatalf("LastTaskEvent for a task with none = %v, %v", ok, err)
	}
	e, ok, err = s.LastEventWithData("repair_spawned", "t8")
	if err != nil || !ok || e.Task != "t2" {
		t.Fatalf("LastEventWithData = %+v, %v, %v; want t2", e, ok, err)
	}
	if read := s.EventRowsRead() - before; read != 4 {
		t.Fatalf("lookups read %d event rows, want 4 (only the rows returned)", read)
	}
}

// The lookups must use the events(kind, task) index, not scan the table.
func TestTaskEventQueriesUseIndex(t *testing.T) {
	s := openTest(t)
	seedEvents(t, s, 1000)
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT ` + eventCols + ` FROM events WHERE task = ? AND kind IN (?) ORDER BY id`, []any{"t1", "landed"}},
		{`SELECT ` + eventCols + ` FROM events WHERE task = ? AND kind IN (?,?) ORDER BY id DESC LIMIT 1`, []any{"t1", "a", "b"}},
		{`SELECT ` + eventCols + ` FROM events WHERE kind = ? AND data = ? ORDER BY id DESC LIMIT 1`, []any{"repair_spawned", "t9"}},
	} {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		got := strings.Join(plan, "; ")
		if !strings.Contains(got, "events_kind_task") || strings.Contains(got, "SCAN events") {
			t.Errorf("plan for %q = %q, want a search on events_kind_task", q.sql, got)
		}
	}
}

// BenchmarkLastTaskEvent100k is one repair-bookkeeping lookup over a
// 100k-row events table, which a full Events(-1) load took ~0.2s for (#778).
func BenchmarkLastTaskEvent100k(b *testing.B) {
	s := openTest(b)
	seedEvents(b, s, 100000)
	s.Event("t1", "repair_spawned", "t9")
	b.ResetTimer()
	for range b.N {
		if _, ok, err := s.LastTaskEvent("t1", "repair_spawned"); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}

// BenchmarkEventsAll100k is the full load the lookups replaced, for scale.
func BenchmarkEventsAll100k(b *testing.B) {
	s := openTest(b)
	seedEvents(b, s, 100000)
	b.ResetTimer()
	for range b.N {
		if _, err := s.Events(-1); err != nil {
			b.Fatal(err)
		}
	}
}
