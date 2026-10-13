package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// orphanRepair lands t1 and t2, publishes them, closes t1's window, moves
// main under t1's commit and restacks, so t1 is repairing with one repair.
func orphanRepair(t *testing.T) (*App, store.Task, store.Task) {
	t.Helper()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1, err := a.Spawn(SpawnReq{ID: "t1", Title: "one", Prompt: "Make README say one.", Claims: []string{"docs/**"}})
	must(t, err)
	write(t, t1.Worktree, "README.md", "one\n")
	commitAll(t, t1.Worktree, "one")
	must(t, a.Done(t1.ID, "README now says one."))
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	moveMain(t, origin, "README.md")
	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	rs := repairTasks(t, a, t1.ID)
	if len(rs) != 1 {
		t.Fatalf("repair tasks = %+v, want one", rs)
	}
	return a, t1, rs[0]
}

// #337: the owner killed t1's repair because t1's work was superseded. The
// next restack must not spawn another, and says why once. `saddle repair`
// still spawns one by hand: the escape hatch.
func TestRestackDoesNotRespawnKilledRepair(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	must(t, a.Kill(r.ID, false))

	for range 2 {
		if _, err := a.Restack(); err != nil {
			t.Fatal(err)
		}
	}
	if rs := repairTasks(t, a, t1.ID); len(rs) != 1 {
		t.Fatalf("restack respawned a repair the owner killed: %+v", rs)
	}
	var why []string
	es, err := a.Store.Events(-1)
	must(t, err)
	for _, e := range es {
		if e.Task == t1.ID && e.Kind == eventRepairDeclined {
			why = append(why, e.Data)
		}
	}
	if len(why) != 1 || !strings.Contains(why[0], r.ID) || !strings.Contains(why[0], "saddle repair "+t1.ID) {
		t.Fatalf("declined events = %q, want one naming %s and `saddle repair %s`", why, r.ID, t1.ID)
	}

	res, err := a.Repair(t1.ID, false)
	if err != nil {
		t.Fatalf("repair by hand after a kill: %v", err)
	}
	if !res.Created || res.Repair == r.ID {
		t.Fatalf("repair by hand = %+v, want a new repair task", res)
	}
}

// A repair killed by hand once is declined; a later hand-spawned repair
// that is killed too is declined again, and one still live is not.
func TestRepairDeclined(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	if _, ok := a.repairDeclined(t1.ID); ok {
		t.Fatal("a live repair reads as declined")
	}
	must(t, a.Kill(r.ID, false))
	if got, ok := a.repairDeclined(t1.ID); !ok || got != r.ID {
		t.Fatalf("repairDeclined = %q, %v; want %s, true", got, ok, r.ID)
	}
}

// Repairs per task are capped per day, however they end.
func TestRepairDailyCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	ev := func(ago time.Duration, task, kind string) store.Event {
		return store.Event{TS: now.Add(-ago), Task: task, Kind: kind}
	}
	es := []store.Event{
		ev(30*time.Hour, "t1", eventRepairSpawned),
		ev(5*time.Hour, "t1", eventRepairSpawned),
		ev(time.Hour, "t2", eventRepairSpawned),
	}
	if repairsToday(es, "t1", now) != 1 {
		t.Fatal("a repair older than a day counted")
	}
	es = append(es, ev(time.Hour, "t1", eventRepairSpawned))
	if n := repairsToday(es, "t1", now); n != 2 || n < repairDailyCap {
		t.Fatalf("repairsToday = %d, want 2 (the cap is %d)", n, repairDailyCap)
	}
}

// #338: a canceled run (a force-push cancels it and leaves a red matrix
// check) is nothing a fix task can fix: no worker is spawned.
func TestCIFixSkipsCanceledRun(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	ciTask(t, a, "t1", store.Landed, "https://github.com/o/r/pull/1")
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/1", "fail", ciJob1)
	gh.out["run view --job 200 --log-failed"] = "test\tRun go test\t2026-10-01T17:03:02.0529854Z Error: The operation was canceled.\n"
	newCI(t, a, gh).Poll(context.Background())

	if _, err := a.Store.Task("t2"); err == nil {
		t.Fatal("a fix task was spawned for a canceled run")
	}
	if !hasEvent(t, a, eventCIRepairSkipped, "canceled") {
		t.Fatal("no ci_repair_skipped event says the run was canceled")
	}
}

// #338: the PR body carries the attribution footer and the repo's
// attribution check fails on it. No commit can fix that, so instead of a
// worker saddle strips the footer from the body and reruns the job.
func TestCIFixBodyCheckStripsFooterAndReruns(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	_, ghLog := originWithGh(t, a)
	pr := "https://github.com/o/r/pull/1"
	ciTask(t, a, "t1", store.Landed, pr)
	must(t, os.WriteFile(filepath.Join(fakeGHDir(t), "view-1"),
		[]byte(`{"body":"Meter bills by the minute.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)"}`), 0o644))
	gh := newCIGH()
	gh.checks(pr, "fail", ciJob1)
	gh.out["run view --job 200 --log-failed"] = "check-misc\tmake check/attribution\t2026-10-01T17:03:02.0529854Z PR body contains an AI attribution footer\n"
	newCI(t, a, gh).Poll(context.Background())

	if _, err := a.Store.Task("t2"); err == nil {
		t.Fatal("a fix task was spawned for a PR-body check")
	}
	calls := strings.Join(ghLog(), "\n")
	if !strings.Contains(calls, "pr edit "+pr+" --body Meter bills by the minute.") || strings.Contains(calls, "--body Meter bills by the minute.\n\n🤖") {
		t.Fatalf("the footer wasn't stripped from the PR body; gh calls:\n%s", calls)
	}
	if !strings.Contains(calls, "run rerun 100 --failed") {
		t.Fatalf("the failed job wasn't rerun; gh calls:\n%s", calls)
	}
}

// A red layer whose PR head lacks the layer below it is red because of its
// base, not its own work: no repair, a notice instead. Once its head has
// the layer below, a red check gets its repair as before.
func TestCIRedRepairSkipsMissingStackBase(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	originWithGh(t, a)
	a.Cfg.Train.Output = "single"
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	stack, err := a.landedStack()
	must(t, err)
	if len(stack) != 2 || stack[1].ID != "t2" {
		t.Fatalf("stack = %+v, want t1 then t2", stack)
	}
	t2 := stack[1]
	// t2's commit alone on main: a PR head without t1.
	dir := filepath.Join(t.TempDir(), "bare")
	git(t, a.Root, "worktree", "add", "-q", "--detach", dir, "origin/main")
	git(t, dir, "cherry-pick", t2.From+".."+t2.To)
	lacking := git(t, dir, "rev-parse", "HEAD")
	git(t, a.Root, "worktree", "remove", "--force", dir)

	fails := []CIRedFailure{{Check: "ci / test", Step: "Run go test", LogTail: "--- FAIL: TestTwo\n"}}
	red := func(head string) string {
		t.Helper()
		must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t2", PR: t2.PR, Head: head, Checks: []string{"ci / test"}}}}))
		note, err := a.CIRedRepair("t2", fails)
		must(t, err)
		return note
	}
	fixes := func() int {
		ts, err := a.Store.Tasks()
		must(t, err)
		n := 0
		for _, x := range ts {
			if strings.HasPrefix(x.Title, "Fix CI for t2 ") {
				n++
			}
		}
		return n
	}

	note := red(lacking)
	if fixes() != 0 {
		t.Fatal("a repair was spawned for a PR head that lacks the layer below it")
	}
	if !strings.Contains(note, "t1") || !hasEvent(t, a, eventCIRepairSkipped, "t1") {
		t.Fatalf("note = %q; want it and a ci_repair_skipped event to name t1", note)
	}

	red(t2.To)
	if fixes() != 1 {
		t.Fatal("a red head that has its base got no repair")
	}
}
