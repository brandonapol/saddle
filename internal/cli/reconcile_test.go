package cli

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// replayFixture writes a state.db where t2's PR went red only on
// saddle/base and saddle spawned t3 to repair it.
func replayFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(store.Task{ID: "t2", Title: "x", Role: store.RoleWorker, Branch: "saddle/t2-x", Status: store.Landed, PR: "https://github.com/o/r/pull/9"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	day := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)
	for i, e := range [][3]string{
		{"t2", "done_called", "x"},
		{"t2", "landed", "abc123"},
		{"t2", "pr_layout", "abc123 published as def456 on main"},
		{"t2", "ci_red", "def456 saddle/base"},
		{"t2", "ci_repair_spawned", "t3"},
	} {
		if _, err := db.Exec(`INSERT INTO events(ts, task, kind, data) VALUES(?,?,?,?)`, day.Add(time.Duration(i)*time.Minute).Unix(), e[0], e[1], e[2]); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// saddle reconcile --replay replays a state.db offline and reports the
// divergences; it needs no saddle repo.
func TestReconcileReplay(t *testing.T) {
	t.Chdir(t.TempDir())
	path := replayFixture(t)
	out := run(t, "reconcile", "--replay", path, "--day", "2026-11-05")
	for _, want := range []string{"divergences", "legacy_only needs_fix(t2", "ci_repair_spawned t3", "legacy repairs 1 (1 of saddle/base)", "assumes"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	js := run(t, "reconcile", "--replay", path, "--day", "2026-11-05", "--json")
	var rep app.ReplayReport
	if err := json.Unmarshal([]byte(js), &rep); err != nil {
		t.Fatalf("--json: %v\n%s", err, js)
	}
	if rep.Outcomes.LegacyRepairs != 1 || len(rep.Divergences) == 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestReconcileFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	cmd := Root()
	cmd.SetArgs([]string{"reconcile", "--day", "2026-11-05"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--replay") {
		t.Errorf("--day without --replay = %v", err)
	}
	if _, _, err := replayWindow("2026-11-05", "", "", time.UTC); err != nil {
		t.Error(err)
	}
	from, until, err := replayWindow("", "2026-11-05T10:00:00Z", "2026-11-05T12:00:00Z", time.UTC)
	if err != nil || until.Sub(from) != 2*time.Hour {
		t.Errorf("from/until = %v %v %v", from, until, err)
	}
	if _, _, err := replayWindow("", "", "", time.UTC); err == nil {
		t.Error("a replay with no window is accepted")
	}
}
