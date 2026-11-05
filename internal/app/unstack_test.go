package app

import (
	"github.com/brandonapol/saddle/internal/store"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// #774: t2 runs after t1. The owner unstacks t1, whose PR stays open and
// will never merge. The next restack mustn't hold t2 on it: the edge is
// dropped, logged, and t2 goes onto main.
func TestUnstackedDependencyDoesNotHoldLayer(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	t1 := landSpec(t, a, SpawnReq{ID: "t1", Title: "one"}, map[string]string{"one.txt": "one\n"})
	t2 := landSpec(t, a, SpawnReq{ID: "t2", Title: "two", After: []string{"t1"}}, map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	t2, _ = a.Store.Task(t2.ID)
	setPR(t, t2.PR, "OPEN", t1.Branch)
	if _, err := a.Unstack(t1.ID); err != nil {
		t.Fatal(err)
	}
	setPR(t, t1.PR, "OPEN", "main") // closing it failed, or it was left open
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got := baseEdits(ghLog()[before:])[t2.PR]; got != "main" {
		t.Fatalf("t2's base = %q after its dependency was unstacked, want main", got)
	}
	if hasEvent(t, a, EventDependencyHold, t1.PR) {
		t.Fatal("t2 was held on an unstacked dependency")
	}
	if !eventWith(t, a, "t2", EventDependencyDropped, "t1") {
		t.Fatal("no dep_dropped event naming t1")
	}
}

// #774: unstack is the escape hatch the repair notices name, so it must
// work on a repairing entry: it leaves for good, its PR is closed, and no
// repair is owed any more.
func TestUnstackRepairingEntry(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	t1, _ = a.Store.Task(t1.ID)
	must(t, a.Kill(r.ID, false)) // declined: the notice says to unstack it
	ghLog := func() []string {
		b, err := os.ReadFile(filepath.Join(a.Root, ".git", FakeGHSubdir, "gh.log"))
		must(t, err)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}

	res, err := a.UnstackTask(t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := trainState(t, a, t1.ID); got != TrainSuperseded {
		t.Fatalf("t1 train state = %q after unstack, want %s", got, TrainSuperseded)
	}
	if !res.Closed || !strings.Contains(res.Note, t1.PR) {
		t.Fatalf("unstack result = %+v, want t1's PR %s closed", res, t1.PR)
	}
	if !slices.ContainsFunc(ghLog(), func(c string) bool { return strings.HasPrefix(c, "pr close "+t1.PR) }) {
		t.Fatalf("t1's PR wasn't closed: %q", ghLog())
	}
	if slices.ContainsFunc(a.PendingRepairs(), func(p PendingRepair) bool { return p.Task == t1.ID }) {
		t.Fatal("an unstacked task still has a pending repair")
	}
}

// #774: a branch escalated after failing to land is dropped by unstack too.
func TestUnstackEscalatedEntry(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	originWithGh(t, a)
	t1, err := a.Spawn(SpawnReq{ID: "t1", Title: "one"})
	must(t, err)
	must(t, a.Store.Enqueue(t1.ID))
	must(t, a.Store.SetTrain(t1.ID, TrainEscalated, "README.md", false))

	res, err := a.UnstackTask(t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := trainState(t, a, t1.ID); got != TrainSuperseded {
		t.Fatalf("t1 train state = %q after unstack, want %s", got, TrainSuperseded)
	}
	if res.Note == "" {
		t.Fatal("unstack says nothing about t1's PR")
	}
}

// routeAroundByHand takes t out of the stack as a route-around's standalone
// PR: saddle still means to merge it, so layers after it wait on it.
func routeAroundByHand(t *testing.T, a *App, task store.Task) {
	t.Helper()
	must(t, a.Store.SetTrain(task.ID, TrainSuperseded, trainNoteOf(t, a, task.ID), false))
	must(t, a.setStallHeld([]StallRoute{{Red: task.ID, PR: task.PR}}))
}

func trainNoteOf(t *testing.T, a *App, task string) string {
	t.Helper()
	e, ok := a.trainEntry(task)
	if !ok {
		t.Fatalf("%s has no train entry", task)
	}
	return e.Note
}
