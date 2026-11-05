package app

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/store"
)

// fillCap spawns a busy worker and sets the cap so no other spawn fits.
func fillCap(t *testing.T, a *App) store.Task {
	t.Helper()
	busy, err := a.Spawn(SpawnReq{Title: "busy elsewhere", Claims: []string{"elsewhere/**"}})
	must(t, err)
	n, err := a.activeWorkers()
	must(t, err)
	a.Cfg.Concurrency = n
	return busy
}

// ciFixes lists the CI repair tasks spawned for task.
func ciFixes(t *testing.T, a *App, task string) []store.Task {
	t.Helper()
	ts, err := a.Store.Tasks()
	must(t, err)
	var out []store.Task
	for _, x := range ts {
		if strings.HasPrefix(x.Title, "Fix CI for "+task+" ") {
			out = append(out, x)
		}
	}
	return out
}

// redT2 lands t1 and t2, publishes them and marks t2 red on its head.
func redT2(t *testing.T) (*App, []CIRedFailure) {
	t.Helper()
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
	t2 := stack[1]
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t2", PR: t2.PR, Head: t2.To, Checks: []string{"ci / test"}}}}))
	return a, []CIRedFailure{{Check: "ci / test", Step: "Run go test", LogTail: "--- FAIL: TestTwo\n"}}
}

// #768: a CI repair whose spawn hits the concurrency cap is recorded as
// pending on the red head and retried on that same head; the old code
// waited for a new head that might never come.
func TestCIRedRepairAtCapRetriesOnSameHead(t *testing.T) {
	t.Parallel()
	a, fails := redT2(t)
	busy := fillCap(t, a)
	_ = orchNotices(t, a, "")

	for range 2 {
		if _, err := a.CIRedRepair("t2", fails); err != nil {
			t.Fatalf("a spawn at the cap is pending, not an error: %v", err)
		}
	}
	if n := len(ciFixes(t, a, "t2")); n != 0 {
		t.Fatalf("%d repairs spawned at the cap", n)
	}
	if due := a.CIRepairsDue(); !slices.Equal(due, []string{"t2"}) {
		t.Fatalf("due = %v, want t2's pending repair", due)
	}
	if got := orchNotices(t, a, "couldn't spawn its repair"); len(got) != 1 || strings.Contains(got[0], "next ci-red cycle") {
		t.Fatalf("notices = %q, want one, saying the repair is retried", got)
	}

	must(t, a.Kill(busy.ID, false))
	if _, err := a.CIRedRepair("t2", fails); err != nil {
		t.Fatal(err)
	}
	if n := len(ciFixes(t, a, "t2")); n != 1 {
		t.Fatalf("repairs after a slot freed = %d, want 1", n)
	}
	if due := a.CIRepairsDue(); len(due) != 0 {
		t.Fatalf("due = %v after the repair spawned", due)
	}
	s, err := a.CIRed()
	must(t, err)
	if slices.ContainsFunc(s.Repairs, func(r CIRepairRecord) bool { return r.State == ciRepairPending }) {
		t.Fatalf("pending record left after the spawn: %+v", s.Repairs)
	}
}

// #768: the sentinel's StallTick retries a pending CI repair from what it
// recorded, log tail included, without waiting for a new red poll.
func TestStallTickRetriesPendingCIRepair(t *testing.T) {
	t.Parallel()
	a, fails := redT2(t)
	busy := fillCap(t, a)
	if _, err := a.CIRedRepair("t2", fails); err != nil {
		t.Fatal(err)
	}
	must(t, a.Kill(busy.ID, false))
	_, _ = a.StallTick(time.Now())
	fx := ciFixes(t, a, "t2")
	if len(fx) != 1 {
		t.Fatalf("repairs after StallTick = %d, want 1", len(fx))
	}
	if !strings.Contains(fx[0].Prompt, "TestTwo") {
		t.Fatalf("retried repair's prompt lacks the log tail:\n%s", fx[0].Prompt)
	}
}

// #768: repairs jump the queue. With a repair pending on the cap and one
// free slot, autopilot's ticket gives the slot to the repair and waits.
func TestAutopilotYieldsSlotToPendingRepair(t *testing.T) {
	t.Parallel()
	a, fails := redT2(t)
	fillCap(t, a)
	if _, err := a.CIRedRepair("t2", fails); err != nil {
		t.Fatal(err)
	}
	a.Cfg.Concurrency++ // one slot frees up
	env := &autopilotEnv{a: a}
	_, err := env.Spawn(autopilot.SpawnReq{Title: "a fresh ticket", Prompt: "new work", Issue: 9, Claims: []string{"fresh/**"}})
	if !errors.Is(err, autopilot.ErrAtCap) {
		t.Fatalf("ticket spawn = %v, want it to wait at the cap", err)
	}
	if n := len(ciFixes(t, a, "t2")); n != 1 {
		t.Fatalf("pending repair spawns = %d, want the repair to take the slot", n)
	}
	ts, err := a.Store.Tasks()
	must(t, err)
	if slices.ContainsFunc(ts, func(x store.Task) bool { return x.Title == "a fresh ticket" }) {
		t.Fatal("the ticket spawned ahead of the pending repair")
	}
}

// #795: a land conflict's repair that can't spawn (the cap) leaves the
// original's claims in place, so a fresh spawn can't take the hot file;
// once a slot frees, the retry spawns the repair and the claims move to it.
func TestEnsureRepairKeepsClaimsUntilSpawned(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	a.Cfg.Test.Cmd = "none"
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	t2, _ := a.Spawn(SpawnReq{Title: "two", Claims: []string{"two/**"}})
	write(t, t1.Worktree, "README.md", "one\n")
	commitAll(t, t1.Worktree, "one")
	write(t, t2.Worktree, "README.md", "two\n")
	commitAll(t, t2.Worktree, "two")
	must(t, a.Done(t1.ID, "one"))
	must(t, a.Done(t2.ID, "two"))
	must(t, ft.KillWindow(t2.Window))
	busy := fillCap(t, a)
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	if reps := repairTasks(t, a, t2.ID); len(reps) != 0 {
		t.Fatalf("repair spawned at the cap: %+v", reps)
	}
	cl, _ := a.Store.Claims()
	if !slices.Contains(cl[t2.ID], "two/**") {
		t.Fatalf("t2's claims = %v; a failed repair spawn released them", cl[t2.ID])
	}
	a.Cfg.Concurrency += 5
	if _, err := a.Spawn(SpawnReq{Title: "fresh", Claims: []string{"two/x.go"}}); err == nil || !strings.Contains(err.Error(), t2.ID) {
		t.Fatalf("fresh spawn on t2's files = %v, want a conflict naming %s", err, t2.ID)
	}
	must(t, a.Kill(busy.ID, false))
	a.RetryRepairs()
	reps := repairTasks(t, a, t2.ID)
	if len(reps) != 1 {
		t.Fatalf("repairs after the retry = %+v, want one", reps)
	}
	cl, _ = a.Store.Claims()
	if len(cl[t2.ID]) != 0 || !slices.Contains(cl[reps[0].ID], "two/**") || !slices.Contains(cl[reps[0].ID], "README.md") {
		t.Fatalf("claims = %v, want t2's moved to %s", cl, reps[0].ID)
	}
}

// #768 and #795 on the restack path: a repairing task whose repair hits the
// cap reserves its files against fresh spawns, and StallTick retries it
// without waiting for a fully successful restack.
func TestRestackRepairAtCapIsReservedAndRetried(t *testing.T) {
	t.Parallel()
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
	busy := fillCap(t, a)
	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got := trainState(t, a, t1.ID); got != TrainRepairing {
		t.Fatalf("t1 = %s, want repairing", got)
	}
	if rs := repairTasks(t, a, t1.ID); len(rs) != 0 {
		t.Fatalf("repair spawned at the cap: %+v", rs)
	}
	a.Cfg.Concurrency += 5
	for _, c := range []string{"README.md", "docs/a.md"} {
		_, err := a.Spawn(SpawnReq{Title: "fresh " + c, Claims: []string{c}, Confirm: true})
		if err == nil || !strings.Contains(err.Error(), t1.ID) {
			t.Fatalf("fresh spawn on %s = %v, want it refused for %s's pending repair", c, err, t1.ID)
		}
	}
	must(t, a.Kill(busy.ID, false))
	_, _ = a.StallTick(time.Now())
	rs := repairTasks(t, a, t1.ID)
	if len(rs) != 1 {
		t.Fatalf("repairs after StallTick = %+v, want one", rs)
	}
	cl, _ := a.Store.Claims()
	if !slices.Contains(cl[rs[0].ID], "README.md") || !slices.Contains(cl[rs[0].ID], "docs/**") {
		t.Fatalf("repair claims = %v", cl[rs[0].ID])
	}
	if p := a.PendingRepairs(); len(p) != 0 {
		t.Fatalf("pending after the spawn: %+v", p)
	}
}
