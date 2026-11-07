package store

import (
	"strings"
	"testing"
)

// #778 follow-up: EventsOfKinds returns every task's events of the given
// kinds, oldest first, and reads only those rows.
func TestEventsOfKinds(t *testing.T) {
	s := openTest(t)
	seedEvents(t, s, 2000)
	s.Event("t1", "landed", "a")
	s.Event("t2", "hung_keep", "why")
	s.Event("t2", "landed", "b")
	seedEvents(t, s, 2000)
	s.Event("t1", "landed", "c")

	before := s.EventRowsRead()
	es, err := s.EventsOfKinds("landed", "hung_keep")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range es {
		got = append(got, e.Task+":"+e.Kind+":"+e.Data)
	}
	want := "t1:landed:a t2:hung_keep:why t2:landed:b t1:landed:c"
	if strings.Join(got, " ") != want {
		t.Fatalf("EventsOfKinds = %v, want %s", got, want)
	}
	if read := s.EventRowsRead() - before; read != 4 {
		t.Fatalf("read %d event rows, want 4", read)
	}
	if es, err := s.EventsOfKinds(); es != nil || err != nil {
		t.Fatalf("no kinds = %v, %v", es, err)
	}
}

// EventsOfKinds must search the events(kind, task) index, not scan the table.
func TestEventsOfKindsUsesIndex(t *testing.T) {
	s := openTest(t)
	seedEvents(t, s, 1000)
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	for _, kinds := range [][]string{{"landed"}, {"hung_keep", "worker_takeover"}} {
		q, args := eventsOfKindsQuery(kinds)
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
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
			t.Errorf("plan for %v = %q, want a search on events_kind_task", kinds, got)
		}
	}
}

// BenchmarkEventsOfKinds100k is one kind lookup over a 100k-row events
// table; compare BenchmarkEventsAll100k, the full load it replaces.
func BenchmarkEventsOfKinds100k(b *testing.B) {
	s := openTest(b)
	seedEvents(b, s, 100000)
	s.Event("t1", "landed", "x")
	b.ResetTimer()
	for range b.N {
		if es, err := s.EventsOfKinds("landed"); len(es) != 1 || err != nil {
			b.Fatal(es, err)
		}
	}
}
