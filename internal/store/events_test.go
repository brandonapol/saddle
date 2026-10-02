package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

// EventsSince returns only events after the cursor, oldest first, with ids a
// caller can resume from.
func TestEventsSinceCursor(t *testing.T) {
	s := openTest(t)
	if id, err := s.LastEventID(); err != nil || id != 0 {
		t.Fatalf("empty LastEventID = %d, %v", id, err)
	}
	if es, err := s.EventsSince(0); err != nil || len(es) != 0 {
		t.Fatalf("empty EventsSince = %v, %v", es, err)
	}
	s.Event("t1", "spawn", "a")
	s.Event("t2", "landed", "b")
	cur, err := s.LastEventID()
	if err != nil {
		t.Fatal(err)
	}
	s.Event("t3", "notification", "c")

	all, err := s.EventsSince(0)
	if err != nil || len(all) != 3 {
		t.Fatalf("EventsSince(0) = %v, %v", all, err)
	}
	if all[0].Task != "t1" || all[2].Task != "t3" || all[0].ID >= all[1].ID || all[1].ID != cur {
		t.Fatalf("order/ids wrong: %+v", all)
	}
	after, err := s.EventsSince(cur)
	if err != nil || len(after) != 1 || after[0].Task != "t3" || after[0].Kind != "notification" || after[0].Data != "c" {
		t.Fatalf("EventsSince(cur) = %+v, %v", after, err)
	}
	if es, _ := s.EventsSince(after[0].ID); len(es) != 0 {
		t.Fatalf("past the end = %+v", es)
	}
	// Events(n) carries ids too, so a caller can switch to the cursor.
	last, _ := s.Events(1)
	if len(last) != 1 || last[0].ID != after[0].ID {
		t.Fatalf("Events(1) = %+v", last)
	}
}

// Today's narrator spend survives closing and reopening the store, and each
// day is kept apart.
func TestNarratorSpendPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.NarratorSpend("2026-10-02"); err != nil || v != 0 {
		t.Fatalf("unset spend = %v, %v", v, err)
	}
	if err := s.SetNarratorSpend("2026-10-02", 0.25); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNarratorSpend("2026-10-02", 0.4); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, err := s.NarratorSpend("2026-10-02"); err != nil || v != 0.4 {
		t.Fatalf("after reopen = %v, %v", v, err)
	}
	if v, _ := s.NarratorSpend("2026-10-03"); v != 0 {
		t.Fatalf("other day = %v", v)
	}
}

// UsageBuckets merges sessions into one bucket per minute, task and model.
func TestUsageBucketsSince(t *testing.T) {
	s := openTest(t)
	m := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.AddUsage("s1", false, []usage.Bucket{bucket(m, "t1", "opus", 10, 1)}))
	must(s.AddUsage("s2", false, []usage.Bucket{bucket(m, "t1", "opus", 5, 2)}))
	must(s.AddUsage("s1", false, []usage.Bucket{bucket(m.Add(-2*time.Hour), "t1", "opus", 100, 100)}))
	bs, err := s.UsageBuckets(m.Add(-time.Hour))
	must(err)
	if len(bs) != 1 || bs[0].Input != 15 || bs[0].Output != 3 || bs[0].Messages != 2 ||
		!bs[0].Minute.Equal(m) || bs[0].Task != "t1" || bs[0].Model != "opus" {
		t.Fatalf("buckets = %+v", bs)
	}
}
