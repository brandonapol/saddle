package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func bucket(minute time.Time, task, model string, in, out int64) usage.Bucket {
	return usage.Bucket{
		Key:      usage.Key{Minute: minute, Task: task, Model: model},
		Tokens:   usage.Tokens{Input: in, Output: out, CacheRead: 10 * in},
		Messages: 1,
	}
}

// A database created before the usage table existed upgrades in place.
func TestUsageMigrationFromV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks(id, title, status, created_at, updated_at) VALUES('t1', 'x', 'running', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(migrations) {
		t.Fatalf("user_version = %d, want %d", v, len(migrations))
	}
	if _, err := s.Task("t1"); err != nil {
		t.Fatalf("existing task lost: %v", err)
	}
	m := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if err := s.AddUsage("s1", false, []usage.Bucket{bucket(m, "t1", "opus", 1, 2)}); err != nil {
		t.Fatal(err)
	}
	// Reopening is a no-op migration.
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	got, err := s.UsageSince(time.Time{})
	if err != nil || got.Input != 1 || got.Output != 2 {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
}

func TestAddUsageAddsAndResets(t *testing.T) {
	s := openTest(t)
	m := time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC) // truncated to 10:00
	d := []usage.Bucket{bucket(m, "t1", "opus", 5, 7), bucket(m, "t1", "haiku", 1, 1)}

	// Deltas accumulate.
	must(t, s.AddUsage("s1", false, d))
	must(t, s.AddUsage("s1", false, d[:1]))
	byModel, err := s.UsageByModel(time.Time{}, time.Time{})
	must(t, err)
	if len(byModel) != 2 || byModel[1].Key != "opus" || byModel[1].Input != 10 || byModel[1].Output != 14 ||
		byModel[1].CacheRead != 100 || byModel[1].Messages != 2 {
		t.Fatalf("after two deltas: %+v", byModel)
	}

	// A reset replays the session's full transcript; doing it repeatedly
	// (e.g. every restart) never double counts.
	full := []usage.Bucket{bucket(m, "t1", "opus", 10, 14), bucket(m, "t1", "haiku", 1, 1)}
	full[0].Messages = 2
	for range 3 {
		must(t, s.AddUsage("s1", true, full))
	}
	again, err := s.UsageByModel(time.Time{}, time.Time{})
	must(t, err)
	if len(again) != 2 || again[1] != byModel[1] || again[0] != byModel[0] {
		t.Fatalf("after repeated resets: %+v, want %+v", again, byModel)
	}

	// Empty, non-reset deltas are a no-op; resets leave other sessions alone.
	must(t, s.AddUsage("s1", false, nil))
	must(t, s.AddUsage("s2", false, []usage.Bucket{bucket(m, "t2", "opus", 3, 3)}))
	must(t, s.AddUsage("s1", true, full))
	byTask, err := s.UsageByTask(time.Time{}, time.Time{})
	must(t, err)
	if len(byTask) != 2 || byTask[0].Key != "t1" || byTask[0].Input != 11 || byTask[1].Key != "t2" || byTask[1].Input != 3 {
		t.Fatalf("by task: %+v", byTask)
	}
}

func TestUsageWindows(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 1, 8, 12, 0, 0, 0, time.UTC)
	must(t, s.AddUsage("s1", false, []usage.Bucket{
		bucket(now.Add(-8*24*time.Hour), "t1", "opus", 1000, 0), // outside 7d
		bucket(now.Add(-6*time.Hour), "t1", "opus", 100, 0),     // inside 7d, outside 5h
		bucket(now.Add(-5*time.Hour), "t2", "sonnet", 10, 0),    // on the 5h edge: inside
		bucket(now.Add(-time.Minute), "t2", "opus", 1, 0),
	}))

	in := func(d time.Duration) int64 {
		t.Helper()
		tok, err := s.UsageSince(now.Add(-d))
		must(t, err)
		return tok.Input
	}
	if got := in(5 * time.Hour); got != 11 {
		t.Fatalf("5h input = %d, want 11", got)
	}
	if got := in(7 * 24 * time.Hour); got != 111 {
		t.Fatalf("7d input = %d, want 111", got)
	}
	if tok, err := s.UsageSince(now.Add(time.Hour)); err != nil || tok.Total() != 0 {
		t.Fatalf("empty window = %+v, %v", tok, err)
	}

	// Bounded window: [now-6h, now-1h) holds the 6h and 5h buckets.
	byModel, err := s.UsageByModel(now.Add(-6*time.Hour), now.Add(-time.Hour))
	must(t, err)
	if len(byModel) != 2 || byModel[0].Key != "opus" || byModel[0].Input != 100 || byModel[1].Key != "sonnet" || byModel[1].Input != 10 {
		t.Fatalf("bounded by model: %+v", byModel)
	}
	byTask, err := s.UsageByTask(now.Add(-6*time.Hour), time.Time{})
	must(t, err)
	if len(byTask) != 2 || byTask[0].Input != 100 || byTask[1].Input != 11 {
		t.Fatalf("by task since 6h: %+v", byTask)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// An older binary, which knows fewer migrations, must not lower user_version
// on a database a newer binary already migrated. Otherwise the newer binary
// re-runs its migrations and fails with "table already exists".
func TestOlderBinaryDoesNotDowngradeSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	all := migrations
	migrations = all[:len(all)-1]
	s, err = Open(path)
	migrations = all
	if err != nil {
		t.Fatalf("older binary open: %v", err)
	}
	_ = s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("newer binary reopen: %v", err)
	}
	_ = s.Close()
}
