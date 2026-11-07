package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

type replayEv struct {
	at         string // 15:04:05 on 2026-11-05, UTC
	task, kind string
	data       string
}

// writeReplayDB writes a state.db with tasks and timestamped events, as an
// old saddle left it.
func writeReplayDB(t *testing.T, tasks []store.Task, evs []replayEv) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		must(t, s.CreateTask(tk))
	}
	must(t, s.Close())
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, e := range evs {
		at, err := time.Parse("2006-01-02 15:04:05", "2026-11-05 "+e.at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO events(ts, task, kind, data) VALUES(?,?,?,?)`, at.Unix(), e.task, e.kind, e.data); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// The 2026-11-05 incident, in miniature. t396's PR went red only on
// saddle/base, the status saddle posts itself, and legacy spawned t402 to
// repair it; t397's PR (#814) went red on real checks and legacy spawned
// no repair for it. The replay reports both, and prgraph never asks to fix
// saddle/base.
func TestReplayShadow1105(t *testing.T) {
	t.Parallel()
	w := func(id, branch, pr string) store.Task {
		return store.Task{ID: id, Title: id, Role: store.RoleWorker, Branch: branch, Status: store.Landed, PR: pr}
	}
	path := writeReplayDB(t, []store.Task{
		w("t395", "saddle/t395-labels", "https://github.com/o/r/pull/811"),
		w("t396", "saddle/t396-governor", "https://github.com/o/r/pull/812"),
		w("t397", "saddle/t397-autopilot", "https://github.com/o/r/pull/814"),
	}, []replayEv{
		{"14:09:07", "t395", "done_called", "labels"},
		{"14:52:17", "t395", "done", "labels"},
		{"15:00:53", "t395", "landed", "71532e456f14"},
		{"15:39:47", "t396", "done_called", "governor"},
		{"15:48:43", "t396", "done", "governor"},
		{"15:57:56", "t396", "landed", "8dcb9986174a"},
		{"16:29:30", "t397", "done_called", "autopilot"},
		{"16:48:29", "t397", "done", "autopilot"},
		{"16:57:32", "t397", "landed", "0c903329e058"},
		{"18:09:34", "t396", "depends_on", "t395 (inferred: t396's task text names t395)"},
		{"18:09:51", "t395", "pr_layout", "71532e456f14 published as a2e65ad3b597 on main"},
		{"18:09:55", "t396", "pr_layout", "8dcb9986174a published as b63678e8f55e on saddle/t395-labels (t396 runs after t395)"},
		{"18:10:03", "t397", "pr_layout", "0c903329e058 published as dec6fd8fec79 on main"},
		{"18:26:34", "t396", "ci_red", "67625df31f49 saddle/base"},
		{"18:26:41", "t396", "ci_repair_spawned", "t402"},
		{"18:26:46", "t397", "ci_red", "48e00fba8972 ci / lint, ci / test"},
		{"18:31:56", "t396", "ci_repair_spawned", "t403"},
	})
	from := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	rep, err := ReplayShadow(context.Background(), path, ReplayOptions{From: from, Until: from.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	has := func(side string, kind prgraph.ActionKind, change string) bool {
		for _, d := range rep.Divergences {
			if d.Side == side && d.Kind == kind && d.Change == change {
				return true
			}
		}
		return false
	}
	var lines []string
	for _, d := range rep.Divergences {
		lines = append(lines, d.Change+" "+d.Line())
	}
	all := strings.Join(lines, "\n")
	if !has(ShadowLegacyOnly, prgraph.ActNeedsFix, "t396") {
		t.Errorf("want legacy_only needs_fix(t396) (t402/t403 repaired saddle/base); got\n%s", all)
	}
	if !has(ShadowPrgraphOnly, prgraph.ActNeedsFix, "t397") {
		t.Errorf("want prgraph_only needs_fix(t397) (#814 red on real checks); got\n%s", all)
	}
	if has(ShadowPrgraphOnly, prgraph.ActNeedsFix, "t396") || strings.Contains(all, "check:saddle/base") {
		t.Errorf("prgraph asked to fix saddle/base:\n%s", all)
	}
	if rep.Outcomes.LegacyRepairs != 2 || rep.Outcomes.LegacyHostRepairs != 2 {
		t.Errorf("outcomes = %+v; want 2 legacy repairs, both of saddle/base", rep.Outcomes)
	}
	if rep.Outcomes.PrgraphFixes != 1 {
		t.Errorf("outcomes = %+v; want prgraph to ask one fix (t397)", rep.Outcomes)
	}
}

// A replay never writes to the database it reads.
func TestReplayShadowLeavesSourceAlone(t *testing.T) {
	t.Parallel()
	path := writeReplayDB(t, []store.Task{{ID: "t1", Title: "x", Role: store.RoleWorker, Branch: "saddle/t1-x", Status: store.Landed}},
		[]replayEv{{"10:00:00", "t1", "done_called", "x"}, {"10:10:00", "t1", "landed", "abc"}})
	before := fileSum(t, path)
	from := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	if _, err := ReplayShadow(context.Background(), path, ReplayOptions{From: from, Until: from.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if fileSum(t, path) != before {
		t.Error("the replay changed the database it read")
	}
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
