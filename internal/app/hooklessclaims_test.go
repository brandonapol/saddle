package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/store"
)

// #489: a hookless agent's done is refused while its branch changes a file
// another task claims, naming the file and owner; it goes through once the
// owner releases the file.
func TestDoneRefusesHooklessBranchTouchingClaimedFile(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "meter", Claims: []string{"billing/meter.go"}})
	must(t, err)
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	write(t, c.Worktree, "billing/meter.go", "package billing\n\nfunc Meter() int { return 2 }\n")
	write(t, c.Worktree, "docs/new.md", "new\n")
	commitAll(t, c.Worktree, "codex edits")
	err = a.Done(c.ID, "did it")
	if err == nil || !strings.Contains(err.Error(), "billing/meter.go") || !strings.Contains(err.Error(), owner.ID) ||
		!strings.Contains(err.Error(), agent.AllowClaimedEnv) || strings.Contains(err.Error(), "docs/new.md") {
		t.Fatalf("done = %v", err)
	}
	if got, _ := a.Store.Task(c.ID); got.Status == store.Done {
		t.Fatal("refused done still queued the task")
	}
	must(t, a.Store.Release(owner.ID))
	must(t, a.Done(c.ID, "did it"))
}

// #489: SADDLE_ALLOW_CLAIMED=1 is done's escape hatch (#119). The override
// is recorded and the orchestrator hears which files belong to whom.
func TestDoneAllowClaimedOverridesAndTellsOrchestrator(t *testing.T) {
	t.Setenv(agent.AllowClaimedEnv, "1")
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "meter", Claims: []string{"billing/meter.go"}})
	must(t, err)
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	write(t, c.Worktree, "billing/meter.go", "package billing\n\nfunc Meter() int { return 2 }\n")
	write(t, c.Worktree, "docs/new.md", "new\n")
	commitAll(t, c.Worktree, "codex edits")
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	must(t, a.Done(c.ID, "did it"))
	if !hasEvent(t, a, EventClaimsOverridden, "billing/meter.go") {
		t.Error("override not recorded")
	}
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	text := store.FormatNotices(ns)
	if !strings.Contains(text, "billing/meter.go") || !strings.Contains(text, owner.ID) || strings.Contains(text, "docs/new.md") {
		t.Errorf("orchestrator notices:\n%s", text)
	}
}

// #489: the pre-commit check refuses a hookless task's commit of a file
// another task claims, naming the owner, and claims an unowned file for the
// task, as a hooked agent's first write would. allow commits anyway and is
// recorded.
func TestCheckCommitEnforcesClaimsForHooklessTask(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "meter", Claims: []string{"billing/meter.go"}})
	must(t, err)
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	err = a.CheckCommit(c.ID, []string{"docs/new.md", "billing/meter.go"}, false)
	if err == nil || !strings.Contains(err.Error(), "billing/meter.go is claimed by "+owner.ID) || !strings.Contains(err.Error(), agent.AllowClaimedEnv) {
		t.Fatalf("commit check = %v", err)
	}
	all, err := a.Store.Claims()
	must(t, err)
	if _, ok := claims.Covering(all[c.ID], "docs/new.md"); !ok {
		t.Errorf("unowned file not claimed for the committer: %v", all[c.ID])
	}
	must(t, a.CheckCommit(c.ID, []string{"docs/new.md"}, false))
	must(t, a.CheckCommit(c.ID, []string{"billing/meter.go"}, true))
	if !hasEvent(t, a, EventClaimsOverridden, "billing/meter.go") {
		t.Error("override not recorded")
	}
}

// #489: claude and grok workers' hooks already enforce claims, so the commit
// check leaves them alone, as it does a task saddle doesn't know.
func TestCheckCommitLeavesHookedTasksAlone(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	_, err := a.Spawn(SpawnReq{Title: "meter", Claims: []string{"billing/meter.go"}})
	must(t, err)
	cl, err := a.Spawn(SpawnReq{Title: "claude work"})
	must(t, err)
	must(t, a.CheckCommit(cl.ID, []string{"billing/meter.go"}, false))
	must(t, a.CheckCommit("t999", []string{"billing/meter.go"}, false))
}
