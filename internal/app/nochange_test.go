package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// assertNoChange checks task finished with no change (#452): it is out of
// the train, out of the stack, holds no claims, and the orchestrator heard.
func assertNoChange(t *testing.T, a *App, task string) {
	t.Helper()
	got, err := a.Store.Task(task)
	must(t, err)
	if got.Status != store.NoChange {
		t.Fatalf("status = %q, want %q", got.Status, store.NoChange)
	}
	if got.Active() {
		t.Fatal("a no-change task still counts as active")
	}
	if inTrain(t, a, task) {
		t.Fatal("a no-change task has a train entry")
	}
	all, err := a.landedAll()
	must(t, err)
	for _, l := range all {
		if l.ID == task {
			t.Fatalf("a no-change task is in the landed stack: %+v", l)
		}
		if l.Lost != "" {
			t.Fatalf("stack has a lost layer: %s", l.Lost)
		}
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("stack flagged at risk")
	}
	cl, err := a.Store.Claims()
	must(t, err)
	if len(cl[task]) > 0 {
		t.Fatalf("a no-change task keeps its claims: %v", cl[task])
	}
	if n := noticeTexts(t, a, OrchestratorID); !strings.Contains(n, task) || !strings.Contains(n, "no change") {
		t.Fatalf("orchestrator not told:\n%s", n)
	}
}

// pickOntoIntegration lands a copy of sha's patch on integration, as the
// train would for another task carrying the same fix.
func pickOntoIntegration(t *testing.T, a *App, sha string) {
	t.Helper()
	for _, args := range [][]string{
		{"checkout", "-q", a.Cfg.Integration},
		{"cherry-pick", sha},
		{"checkout", "-q", a.Cfg.Base},
	} {
		if _, err := trainGit(a.Root, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// #452: done on a branch with no commits ahead of integration finishes the
// task with no change instead of refusing or queueing an empty landing.
func TestDoneWithNoCommitsFinishesWithNoChange(t *testing.T) {
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "already fixed", Claims: []string{"x.go"}})
	must(t, err)
	must(t, a.Done(tk.ID, "the issue was already fixed"))
	assertNoChange(t, a, tk.ID)
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 0 {
		t.Fatalf("land ran a no-change task: %+v", rs)
	}
	assertNoChange(t, a, tk.ID)
}

// #452: commits whose work integration already has count as no change too.
func TestDoneWithWorkAlreadyOnIntegrationFinishesWithNoChange(t *testing.T) {
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "dup"})
	must(t, err)
	write(t, tk.Worktree, "fix.go", "fixed\n")
	commitAll(t, tk.Worktree, "fix")
	sha := git(t, tk.Worktree, "rev-parse", "HEAD")

	// Someone else's landing already carried the same patch.
	pickOntoIntegration(t, a, sha)

	must(t, a.Done(tk.ID, "dup"))
	assertNoChange(t, a, tk.ID)
}

// #452, the t216 case: a branch queued with real commits whose work landed
// elsewhere before the train reached it rebases to nothing. The train must
// not record an empty landing (from..from) that the sentinel then reads as a
// lost layer.
func TestLandEmptyRebaseFinishesWithNoChange(t *testing.T) {
	a := trainSetup(t)
	tk := queueTask(t, a, "t216", "already fixed", map[string]string{"fix.go": "fixed\n"})
	sha := git(t, tk.Worktree, "rev-parse", "HEAD")

	// The same fix lands on integration while t216 waits in the train.
	pickOntoIntegration(t, a, sha)

	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State == store.TrainOK {
		t.Fatalf("land results = %+v, want one not-landed result", rs)
	}
	assertNoChange(t, a, tk.ID)
	ls, err := a.StackLayers()
	must(t, err)
	for _, l := range ls {
		if l.Task.ID == tk.ID {
			t.Fatalf("no-change task got a PR layer: %+v", l)
		}
	}
}
