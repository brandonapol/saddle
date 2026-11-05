package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// deadPid is the pid of a process that has exited.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	must(t, cmd.Run())
	return cmd.Process.Pid
}

// putJournal writes j as the restack journal as-is, pid and all, as a
// process that died mid-restack left it.
func putJournal(t *testing.T, a *App, j restackJournal) {
	t.Helper()
	b, err := json.Marshal(j)
	must(t, err)
	must(t, os.WriteFile(a.stateDir(RestackJournalFile), b, 0o644))
}

// #777: a journal from a dead process, one of whose refs no longer matches
// its CAS, doesn't block land: land rolls it back on its own and goes on.
func TestLandAutoAbortsDeadRestackJournal(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.CloseOnLand = false
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	base := git(t, a.Root, "rev-parse", "main")
	old1 := git(t, a.Root, "rev-parse", t1.Branch)
	old2 := git(t, a.Root, "rev-parse", t2.Branch)
	next1 := git(t, a.Root, "commit-tree", old1+"^{tree}", "-p", base, "-m", "rebuilt one")
	next2 := git(t, a.Root, "commit-tree", old2+"^{tree}", "-p", next1, "-m", "rebuilt two")
	landed, err := a.landedStack()
	must(t, err)
	integ := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	j := restackJournal{
		Pid: deadPid(t),
		Plan: []restacked{
			{landedTask: landed[0], NewFrom: base, NewTo: next1},
			{landedTask: landed[1], NewFrom: next1, NewTo: next2},
		},
		Moves: []RestackMove{
			{Task: t1.ID, Ref: "refs/heads/" + t1.Branch, Old: old1, New: next1},
			{Task: t2.ID, Ref: "refs/heads/" + t2.Branch, Old: old2, New: next2},
			{Ref: "refs/heads/" + a.Cfg.Integration, Old: integ, New: next2},
		},
	}
	// The transaction committed, then a live agent moved t2's branch.
	must(t, a.moveRestackRefs(j.Moves, false))
	moved := git(t, a.Root, "commit-tree", next2+"^{tree}", "-p", next2, "-m", "agent commit")
	asTrain(t, a.Root, "update-ref", "refs/heads/"+t2.Branch, moved)
	putJournal(t, a, j)

	queueTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	rs, err := a.Land()
	if err != nil {
		t.Fatalf("land with a dead journal: %v", err)
	}
	if len(rs) != 1 || rs[0].Task != "t3" || rs[0].State != store.TrainOK {
		t.Fatalf("land = %+v", rs)
	}
	if RestackInterrupted(a.Root) {
		t.Fatal("journal not cleared")
	}
	if got := git(t, a.Root, "rev-parse", t1.Branch); got != old1 {
		t.Fatalf("t1 = %s, want its old tip %s back", got, old1)
	}
	if got := git(t, a.Root, "rev-parse", t2.Branch); got != moved {
		t.Fatalf("t2 = %s, want the agent's commit %s kept", got, moved)
	}
	if evs := eventsOf(t, a, EventRestackAutoAborted); len(evs) != 1 {
		t.Fatalf("restack_auto_aborted events = %v", evs)
	}
}

// #777: abort skips a dirty checkout instead of failing, completes, and
// names the checkout it left alone.
func TestAutoAbortSkipsDirtyCheckout(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.CloseOnLand = false
	task := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landed, err := a.landedStack()
	must(t, err)
	old := landed[0].To
	base := git(t, a.Root, "rev-parse", "main")
	next := git(t, a.Root, "commit-tree", old+"^{tree}", "-p", base, "-m", "rebuilt one")
	j := restackJournal{Pid: deadPid(t), Plan: []restacked{{landedTask: landed[0], NewFrom: base, NewTo: next}}, Moves: []RestackMove{
		{Task: task.ID, Ref: "refs/heads/" + task.Branch, Old: old, New: next},
		{Ref: "refs/heads/" + a.Cfg.Integration, Old: old, New: next},
	}, Checkouts: []store.Task{task}}
	// Died after detaching the checkout and committing the refs; then the
	// agent wrote to its checkout.
	asTrain(t, task.Worktree, "checkout", "-q", "--detach")
	must(t, a.moveRestackRefs(j.Moves, false))
	write(t, task.Worktree, "wip.txt", "wip\n")
	putJournal(t, a, j)

	unlock, ok, err := a.TryLockTrain()
	if err != nil || !ok {
		t.Fatalf("TryLockTrain: ok=%t err=%v", ok, err)
	}
	unlock()
	if RestackInterrupted(a.Root) {
		t.Fatal("journal not cleared")
	}
	for _, ref := range []string{task.Branch, a.Cfg.Integration} {
		if got := git(t, a.Root, "rev-parse", ref); got != old {
			t.Fatalf("%s = %s, want %s", ref, got, old)
		}
	}
	if _, err := os.Stat(task.Worktree + "/wip.txt"); err != nil {
		t.Fatalf("dirty checkout's work lost: %v", err)
	}
	evs := eventsOf(t, a, EventRestackAutoAborted)
	if len(evs) != 1 || !strings.Contains(evs[0], task.Worktree) {
		t.Fatalf("restack_auto_aborted events = %v, want one naming %s", evs, task.Worktree)
	}
}

// #777: a journal whose writer is still alive is left for it.
func TestLiveRestackJournalStillBlocks(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	putJournal(t, a, restackJournal{Pid: os.Getpid()})
	if _, ok, err := a.TryLockTrain(); ok || err == nil || !strings.Contains(err.Error(), "--continue") {
		t.Fatalf("live journal: ok=%t err=%v", ok, err)
	}
	if !RestackInterrupted(a.Root) {
		t.Fatal("live journal removed")
	}
}

// #777: a restack whose ref transaction fails rolls its journal back at
// once, so even a live process (the daemon) leaves nothing to recover.
func TestFailedRestackTransactionLeavesNoJournal(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landed, err := a.landedStack()
	must(t, err)
	old := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	base := git(t, a.Root, "rev-parse", "main")
	plan := []restacked{
		{landedTask: landed[0], NewFrom: base, NewTo: old},
		{landedTask: landed[1], NewFrom: old, NewTo: landed[0].To},
	}
	asTrain(t, a.Root, "update-ref", "refs/heads/"+landed[1].Branch, base)
	var res RestackResult
	if err := a.moveStack(plan, old, base, &res); err == nil {
		t.Fatal("expected compare-and-swap failure")
	}
	if RestackInterrupted(a.Root) {
		t.Fatal("failed transaction left its journal behind")
	}
}

// #777: saddle init, which doctor --fix runs, rolls back a dead journal.
func TestInitRollsBackDeadRestackJournal(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	putJournal(t, a, restackJournal{Pid: deadPid(t)})
	must(t, a.Init())
	if RestackInterrupted(a.Root) {
		t.Fatal("init left the dead journal")
	}
	if evs := eventsOf(t, a, EventRestackAutoAborted); len(evs) != 1 {
		t.Fatalf("restack_auto_aborted events = %v", evs)
	}
}
