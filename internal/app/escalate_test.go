package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func trainEntry(t *testing.T, a *App, id string) store.TrainEntry {
	t.Helper()
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == id {
			return e
		}
	}
	t.Fatalf("%s not in the train", id)
	return store.TrainEntry{}
}

// After max_attempts failed lands, the train stops bouncing the branch back
// to its producer and escalates it to the owner as needs-you (#30).
func TestConflictEscalatesAfterMaxAttempts(t *testing.T) {
	a, ft := setup(t)
	a.Cfg.Train.MaxAttempts = 2
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	t2, _ := a.Spawn(SpawnReq{Title: "two"})
	write(t, t1.Worktree, "README.md", "one\n")
	commitAll(t, t1.Worktree, "one")
	write(t, t2.Worktree, "README.md", "two\n")
	commitAll(t, t2.Worktree, "two")
	must(t, a.Done(t1.ID, "one"))
	must(t, a.Done(t2.ID, "two"))
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Store.Task(t2.ID); got.Status != store.Conflict {
		t.Fatalf("after 1 attempt status = %s", got.Status)
	}
	_, _ = a.Store.TakeNotices(t2.ID, false)
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	woken := len(ft.sent[t2.Window])

	// The producer calls done again without fixing anything.
	must(t, a.Done(t2.ID, "two again"))
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].State != TrainEscalated {
		t.Fatalf("results = %+v", rs)
	}
	if got, _ := a.Store.Task(t2.ID); got.Status != store.NeedsYou {
		t.Fatalf("status = %s, want needs_you", got.Status)
	}
	if e := trainEntry(t, a, t2.ID); e.State != TrainEscalated || e.Attempts != 2 {
		t.Fatalf("entry = %+v", e)
	}
	if len(ft.sent[t2.Window]) != woken {
		t.Fatal("escalated producer was woken to bounce again")
	}
	if ns, _ := a.Store.TakeNotices(t2.ID, true); len(ns) != 0 {
		t.Fatalf("producer got action notices: %+v", ns)
	}
	ns, _ := a.Store.TakeNotices(OrchestratorID, true)
	if len(ns) == 0 || !strings.Contains(ns[len(ns)-1].Text, "needs you") || !strings.Contains(ns[len(ns)-1].Text, "README.md") {
		t.Fatalf("orchestrator notices = %+v", ns)
	}
}

// Persistent red tests escalate the same way.
func TestRedTestsEscalate(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.Test.Cmd = "echo red; exit 1"
	a.Cfg.Train.MaxAttempts = 2
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	write(t, t1.Worktree, "x.txt", "x\n")
	commitAll(t, t1.Worktree, "one")
	must(t, a.Done(t1.ID, "one"))
	rs, _ := a.Land()
	if len(rs) != 1 || rs[0].State != store.TestFailed {
		t.Fatalf("first land = %+v", rs)
	}
	must(t, a.Done(t1.ID, "one"))
	rs, _ = a.Land()
	if len(rs) != 1 || rs[0].State != TrainEscalated || !strings.Contains(rs[0].Note, "tests failed") {
		t.Fatalf("second land = %+v", rs)
	}
}
