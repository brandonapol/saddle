package gc

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// openDB makes a migrated state.db and a second connection to seed it with
// rows at chosen times.
func openDB(t *testing.T) (path string, db *sql.DB) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	db, err = open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return path, db
}

func execSQL(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// #802: old noisy events (ci_error and friends) are pruned; landed rows and
// recent noise are kept.
func TestPruneEventsDropsOldNoiseKeepsLanded(t *testing.T) {
	t.Parallel()
	path, db := openDB(t)
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour).Unix()
	fresh := now.Add(-time.Hour).Unix()
	ev := `INSERT INTO events(ts, task, kind, data) VALUES(?,?,?,?)`
	execSQL(t, db, ev, old, "t1", "ci_error", "gh: 502")
	execSQL(t, db, ev, old, "t1", "tool", "x")
	execSQL(t, db, ev, old, "t1", "ref", "x")
	execSQL(t, db, ev, old, "t1", "pr_layout", "x") // the shadow replay reads it
	execSQL(t, db, ev, old, "t1", "landed", "abc")
	execSQL(t, db, ev, fresh, "t2", "ci_error", "gh: 502")

	res, err := PruneDB(path, now, Retention{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 3 {
		t.Fatalf("pruned %d events, want 3", res.Events)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM events WHERE kind IN ('landed', 'pr_layout')`); n != 2 {
		t.Fatalf("landed and pr_layout rows = %d, want 2", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM events WHERE kind = 'ci_error'`); n != 1 {
		t.Fatalf("ci_error rows = %d, want the fresh one", n)
	}
}

// #802: delivered notices past retention go; undelivered ones never do.
func TestPruneNoticesDropsOldDelivered(t *testing.T) {
	t.Parallel()
	path, db := openDB(t)
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour).Unix()
	n := `INSERT INTO notices(ts, task, kind, text, delivered) VALUES(?,?,?,?,?)`
	execSQL(t, db, n, old, "t1", "info", "read", 1)
	execSQL(t, db, n, old, "t1", "action", "unread", 0)
	execSQL(t, db, n, now.Unix(), "t1", "info", "read today", 1)

	res, err := PruneDB(path, now, Retention{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Notices != 1 {
		t.Fatalf("pruned %d notices, want 1", res.Notices)
	}
	if c := count(t, db, `SELECT COUNT(*) FROM notices WHERE text = 'unread'`); c != 1 {
		t.Fatal("pruned an undelivered notice")
	}
}

// #802: chat keeps its newest rows; older ones past retention go.
func TestPruneChatKeepsNewest(t *testing.T) {
	t.Parallel()
	path, db := openDB(t)
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour).Unix()
	for i := range 5 {
		execSQL(t, db, `INSERT INTO chat(ts, role, text) VALUES(?,?,?)`, old, "user", i)
	}
	res, err := PruneDB(path, now, Retention{ChatKeep: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chat != 3 {
		t.Fatalf("pruned %d chat rows, want 3", res.Chat)
	}
	if c := count(t, db, `SELECT COUNT(*) FROM chat WHERE text IN ('3', '4')`); c != 2 {
		t.Fatal("chat lost its newest rows")
	}
}
