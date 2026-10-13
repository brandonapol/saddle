package app

import (
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
