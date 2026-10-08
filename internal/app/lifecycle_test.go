package app

import (
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withOrigin gives a's repo a bare origin and returns a second clone that can
// push commits local main doesn't have.
func withOrigin(t *testing.T, a *App) (other string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, a.Root, "init", "-q", "--bare", "-b", "main", origin)
	git(t, a.Root, "remote", "add", "origin", origin)
	git(t, a.Root, "push", "-q", "-u", "origin", "main")
	other = filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	return other
}

func pushCommit(t *testing.T, dir, file string) {
	t.Helper()
	write(t, dir, file, file+"\n")
	commitAll(t, dir, "add "+file)
	git(t, dir, "push", "-q", "origin", "main")
}

func hasEvent(t *testing.T, a *App, kind, sub string) bool {
	t.Helper()
	es, err := a.Store.Events(500)
	must(t, err)
	for _, e := range es {
		if e.Kind == kind && strings.Contains(e.Data, sub) {
			return true
		}
	}
	return false
}

// #84: integration is cut from origin/<base>, not a stale local base, and
// falling behind origin is reported once.
func TestIntegrationCutFromOrigin(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	pushCommit(t, other, "upstream1.txt")

	if _, err := a.Spawn(SpawnReq{Title: "one"}); err != nil {
		t.Fatal(err)
	}
	tip := git(t, other, "rev-parse", "HEAD")
	if mb := git(t, a.Root, "merge-base", a.Cfg.Integration, tip); mb != tip {
		t.Fatalf("integration merge-base = %s, want origin/main %s", mb, tip)
	}

	pushCommit(t, other, "upstream2.txt")
	if _, err := a.Spawn(SpawnReq{Title: "two"}); err != nil {
		t.Fatal(err)
	}
	if !hasEvent(t, a, "integration_behind", "integration behind origin/main by 1") {
		t.Fatal("no integration_behind event")
	}
	if _, err := a.Spawn(SpawnReq{Title: "three"}); err != nil {
		t.Fatal(err)
	}
	es, _ := a.Store.Events(500)
	n := 0
	for _, e := range es {
		if e.Kind == "integration_behind" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("integration_behind events = %d, want 1", n)
	}
	if ws := a.Warnings(); len(ws) != 1 || ws[0] != "integration behind origin/main by 1" {
		t.Fatalf("warnings = %q", ws)
	}
}

// #91: a spawn that fails after its row exists is marked failed with the
// reason, leaves no branch, and a claim conflict leaves no row at all.
func TestFailedSpawnIsRecorded(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	id, err := a.Store.NextID()
	must(t, err)
	wt := a.stateDir("worktrees", id+"-"+slug("blocked"))
	write(t, wt, "squatter.txt", "in the way\n")
	_, err = a.Spawn(SpawnReq{Title: "blocked", Claims: []string{"billing/**"}})
	if err == nil {
		t.Fatal("spawn into an occupied path succeeded")
	}
	got, err := a.Store.Task(id)
	must(t, err)
	if got.Status != StatusFailed {
		t.Fatalf("status = %s, want %s", got.Status, StatusFailed)
	}
	if !hasEvent(t, a, "spawn_failed", "worktree") {
		t.Fatal("no spawn_failed event with the error")
	}
	if gitx.BranchExists(a.Root, got.Branch) {
		t.Fatalf("branch %s left behind", got.Branch)
	}
	if cl, _ := a.Store.Claims(); len(cl[id]) > 0 {
		t.Fatalf("failed spawn kept claims %v", cl[id])
	}

	// The retry gets a fresh id and a hint about the failure.
	t2, err := a.Spawn(SpawnReq{Title: "blocked", Claims: []string{"billing/**"}})
	must(t, err)
	if !hasEvent(t, a, "spawn", id+" failed") {
		t.Fatalf("retry %s carries no hint about %s", t2.ID, id)
	}

	before, _ := a.Store.Tasks()
	if _, err := a.Spawn(SpawnReq{Title: "dup", Claims: []string{"billing/meter.go"}}); err == nil || !strings.Contains(err.Error(), "claim conflict") {
		t.Fatalf("conflicting spawn: err = %v", err)
	}
	if after, _ := a.Store.Tasks(); len(after) != len(before) {
		t.Fatalf("claim conflict created a task row: %d -> %d", len(before), len(after))
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// #92: kill cleans up after itself unless the branch holds work.
func TestKillCleansUp(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	empty, err := a.Spawn(SpawnReq{Title: "empty"})
	must(t, err)
	must(t, a.Kill(empty.ID, false))
	if exists(empty.Worktree) || gitx.BranchExists(a.Root, empty.Branch) {
		t.Fatal("killed task without commits left its worktree or branch")
	}

	work, err := a.Spawn(SpawnReq{Title: "work"})
	must(t, err)
	write(t, work.Worktree, "work.txt", "work\n")
	commitAll(t, work.Worktree, "work")
	must(t, a.Kill(work.ID, false))
	if exists(work.Worktree) {
		t.Fatal("killed task kept its worktree")
	}
	if !gitx.BranchExists(a.Root, work.Branch) {
		t.Fatal("killed task lost a branch with commits")
	}

	kept, err := a.Spawn(SpawnReq{Title: "kept"})
	must(t, err)
	must(t, a.Kill(kept.ID, true))
	if !exists(kept.Worktree) {
		t.Fatal("kill --keep removed the worktree")
	}
}

// #92: gc removes orphaned worktrees, branches and refs/saddle leftovers.
func TestGCRemovesLeftovers(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	live, err := a.Spawn(SpawnReq{Title: "live"})
	must(t, err)
	orphan := a.stateDir("worktrees", "t99-orphan")
	git(t, a.Root, "worktree", "add", "-q", "-b", "saddle/t99-orphan", orphan, a.Cfg.Integration)
	git(t, a.Root, "update-ref", "refs/saddle/wip/t99", "HEAD")
	git(t, a.Root, "update-ref", "refs/saddle/wip/"+live.ID, "HEAD")

	ls, err := a.Leftovers()
	must(t, err)
	if len(ls) != 3 {
		t.Fatalf("leftovers = %+v, want orphan worktree, branch and ref", ls)
	}
	if _, err := a.GC(); err != nil {
		t.Fatal(err)
	}
	if exists(orphan) || gitx.BranchExists(a.Root, "saddle/t99-orphan") {
		t.Fatal("gc left the orphaned worktree or branch")
	}
	if _, err := gitx.RevParse(a.Root, "refs/saddle/wip/t99"); err == nil {
		t.Fatal("gc left refs/saddle/wip/t99")
	}
	if _, err := gitx.RevParse(a.Root, "refs/saddle/wip/"+live.ID); err != nil {
		t.Fatal("gc removed a live task's ref")
	}
	if !exists(live.Worktree) || !gitx.BranchExists(a.Root, live.Branch) {
		t.Fatal("gc touched a live task")
	}
	if strings.Contains(git(t, a.Root, "worktree", "list"), "t99-orphan") {
		t.Fatal("orphan still registered with git")
	}
}

// #93: the headless orchestrator never gets keys typed into a stale window.
func TestOrchestratorGetsNoWakeKeys(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	ft.windows["@0"] = true // the TUI's own window, recorded by an old-style saddle up
	must(t, a.Store.CreateTask(store.Task{ID: OrchestratorID, Title: "orchestrator", Role: store.RoleOrchestrator,
		Worktree: a.Root, Window: "@0", Status: store.Idle}))
	must(t, a.Notify(OrchestratorID, store.NoticeAction, "t1 is done"))
	if len(ft.sent["@0"]) > 0 {
		t.Fatalf("typed into the orchestrator's window: %q", ft.sent["@0"])
	}
	if err := a.SendKeys(OrchestratorID, "hi", nil); err == nil || len(ft.sent["@0"]) > 0 {
		t.Fatalf("SendKeys to the orchestrator: err = %v", err)
	}
	_, _, err := a.Orchestrator()
	must(t, err)
	if o, _ := a.Store.Task(OrchestratorID); o.Window != "" {
		t.Fatalf("orchestrator window = %q after Orchestrator()", o.Window)
	}
}

// #93: a window id that now belongs to something else is never typed into.
func TestNoKeysIntoForeignWindow(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	w, err := a.Spawn(SpawnReq{Title: "worker"})
	must(t, err)
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	ft.names[w.Window] = "zsh" // tmux reused the id for a window saddle didn't open
	must(t, a.Notify(w.ID, store.NoticeAction, "rebase"))
	if len(ft.sent[w.Window]) > 0 {
		t.Fatalf("typed into a foreign window: %q", ft.sent[w.Window])
	}
	ft.names[w.Window] = w.ID + "-worker"
	must(t, a.Notify(w.ID, store.NoticeAction, "rebase"))
	if len(ft.sent[w.Window]) != 1 {
		t.Fatal("own window was not woken")
	}
}

// killedWithWork spawns a task, commits each file in files on it, and kills it,
// leaving its branch (which holds work) behind.
func killedWithWork(t *testing.T, a *App, title string, files ...string) store.Task {
	t.Helper()
	task, err := a.Spawn(SpawnReq{Title: title})
	must(t, err)
	for _, f := range files {
		write(t, task.Worktree, f, f+"\n")
		commitAll(t, task.Worktree, "add "+f)
	}
	must(t, a.Kill(task.ID, false))
	return task
}

// leftover finds name among ls.
func leftover(ls []Leftover, kind, name string) (Leftover, bool) {
	for _, l := range ls {
		if l.Kind == kind && l.Name == name {
			return l, true
		}
	}
	return Leftover{}, false
}

// #158: a branch whose PR was squash-merged into main has different SHAs than
// main, but its work is there; gc removes it.
func TestGCRemovesSquashMergedBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	task := killedWithWork(t, a, "squashed", "a.txt", "b.txt")
	// The PR lands as one squash commit with the same content.
	write(t, other, "a.txt", "a.txt\n")
	write(t, other, "b.txt", "b.txt\n")
	commitAll(t, other, "squashed (#1)")
	git(t, other, "push", "-q", "origin", "main")
	git(t, a.Root, "fetch", "-q", "origin")

	ls, err := a.Leftovers()
	must(t, err)
	l, ok := leftover(ls, "branch", task.Branch)
	if !ok || l.Keep != "" {
		t.Fatalf("squash-merged branch: %+v (found %v), want removable", l, ok)
	}
	_, err = a.GC()
	must(t, err)
	if gitx.BranchExists(a.Root, task.Branch) {
		t.Fatal("gc kept a squash-merged branch")
	}
}

// #158: a stacked branch squash-merged after the branch below it is removed too.
func TestGCRemovesStackedSquashMergedBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	task := killedWithWork(t, a, "stacked", "a.txt", "b.txt")
	write(t, other, "a.txt", "a.txt\n")
	commitAll(t, other, "lower (#1)")
	write(t, other, "unrelated.txt", "x\n")
	commitAll(t, other, "someone else (#2)")
	write(t, other, "b.txt", "b.txt\n")
	commitAll(t, other, "upper (#3)")
	git(t, other, "push", "-q", "origin", "main")
	git(t, a.Root, "fetch", "-q", "origin")

	_, err := a.GC()
	must(t, err)
	if gitx.BranchExists(a.Root, task.Branch) {
		t.Fatal("gc kept a branch whose commits all landed on main")
	}
}

// #158: real unmerged work is kept, with the reason.
func TestGCKeepsUnmergedWork(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	task := killedWithWork(t, a, "partial", "a.txt", "b.txt")
	write(t, other, "a.txt", "a.txt\n") // only the first commit landed
	commitAll(t, other, "partial (#1)")
	git(t, other, "push", "-q", "origin", "main")
	git(t, a.Root, "fetch", "-q", "origin")

	ls, err := a.GC()
	must(t, err)
	l, ok := leftover(ls, "branch", task.Branch)
	if !ok || !strings.Contains(l.Keep, "1 commit(s)") {
		t.Fatalf("partly landed branch: %+v (found %v), want kept with 1 commit unmerged", l, ok)
	}
	if !gitx.BranchExists(a.Root, task.Branch) {
		t.Fatal("gc deleted a branch with unmerged work")
	}
}

// #158: a worktree with uncommitted changes is never removed, and neither is
// its branch.
func TestGCKeepsDirtyWorktree(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	task, err := a.Spawn(SpawnReq{Title: "dirty"})
	must(t, err)
	must(t, a.Kill(task.ID, true))
	write(t, task.Worktree, "wip.txt", "wip\n")

	ls, err := a.GC()
	must(t, err)
	if l, ok := leftover(ls, "worktree", task.Worktree); !ok || l.Keep == "" {
		t.Fatalf("dirty worktree: %+v (found %v), want kept", l, ok)
	}
	if !exists(filepath.Join(task.Worktree, "wip.txt")) || !gitx.BranchExists(a.Root, task.Branch) {
		t.Fatal("gc removed a dirty worktree or its checked-out branch")
	}
}

// #158: a killed task's clean worktree is removed; its branch goes too once
// its work is merged.
func TestGCRemovesKilledWorktree(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	task, err := a.Spawn(SpawnReq{Title: "kept"})
	must(t, err)
	write(t, task.Worktree, "k.txt", "k.txt\n")
	commitAll(t, task.Worktree, "k")
	must(t, a.Kill(task.ID, true))
	pushCommit(t, other, "k.txt")
	git(t, a.Root, "fetch", "-q", "origin")

	_, err = a.GC()
	must(t, err)
	if exists(task.Worktree) || gitx.BranchExists(a.Root, task.Branch) {
		t.Fatal("gc left a killed task's worktree or merged branch")
	}
}

// #158: gc never deletes the branch checked out in the main repo.
func TestGCKeepsCheckedOutBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	task := killedWithWork(t, a, "checked out", "c.txt")
	git(t, a.Root, "checkout", "-q", task.Branch)
	defer git(t, a.Root, "checkout", "-q", "main")
	_, err := a.GC()
	must(t, err)
	if !gitx.BranchExists(a.Root, task.Branch) {
		t.Fatal("gc deleted the checked-out branch")
	}
}

// #158: the task's PR being merged into base, at the branch's tip, is enough.
func TestGCRemovesBranchOfMergedPR(t *testing.T) {
	a, _ := setup(t)
	task := killedWithWork(t, a, "merged pr", "m.txt")
	tip := git(t, a.Root, "rev-parse", task.Branch)
	must(t, a.Store.SetField(task.ID, "pr", "https://github.com/o/r/pull/7"))
	prev := prHead
	defer func() { prHead = prev }()
	prHead = func(_, url string) (PRHead, error) {
		return PRHead{State: "MERGED", BaseRefName: "main", HeadRefOid: tip}, nil
	}
	_, err := a.GC()
	must(t, err)
	if gitx.BranchExists(a.Root, task.Branch) {
		t.Fatal("gc kept the branch of a merged PR")
	}

	// A branch that moved past its merged PR keeps the extra work.
	task2 := killedWithWork(t, a, "moved on", "n.txt", "n2.txt")
	must(t, a.Store.SetField(task2.ID, "pr", "https://github.com/o/r/pull/8"))
	head := git(t, a.Root, "rev-parse", task2.Branch+"~1")
	prHead = func(_, url string) (PRHead, error) {
		return PRHead{State: "MERGED", BaseRefName: "main", HeadRefOid: head}, nil
	}
	_, err = a.GC()
	must(t, err)
	if !gitx.BranchExists(a.Root, task2.Branch) {
		t.Fatal("gc deleted commits made after the PR merged")
	}
}

// #158: a merged branch's copy on origin is deleted too; an unmerged one stays.
func TestGCRemovesMergedOriginBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	merged := killedWithWork(t, a, "merged", "a.txt")
	open := killedWithWork(t, a, "open", "o.txt")
	if _, err := trainGit(a.Root, "push", "-q", "origin", merged.Branch, open.Branch); err != nil {
		t.Fatal(err)
	}
	git(t, a.Root, "branch", "-D", merged.Branch) // only origin has it now
	pushCommit(t, other, "a.txt")
	git(t, a.Root, "fetch", "-q", "origin")

	ls, err := a.Leftovers()
	must(t, err)
	if l, ok := leftover(ls, "remote", "origin/"+merged.Branch); !ok || l.Keep != "" {
		t.Fatalf("merged origin branch: %+v (found %v), want removable", l, ok)
	}
	_, err = a.GC()
	must(t, err)
	if out := git(t, a.Root, "ls-remote", "origin", "refs/heads/"+merged.Branch); out != "" {
		t.Fatal("gc left the merged branch on origin")
	}
	if out := git(t, a.Root, "ls-remote", "origin", "refs/heads/"+open.Branch); out == "" {
		t.Fatal("gc deleted an unmerged branch on origin")
	}
}

// #158: doctor's count is what gc removes, so it clears after gc; kept work
// is counted apart.
func TestGCCountsMatchGC(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	killedWithWork(t, a, "merged", "a.txt")
	killedWithWork(t, a, "unmerged", "u.txt")
	pushCommit(t, other, "a.txt")
	git(t, a.Root, "fetch", "-q", "origin")

	remove, kept, err := a.GCCounts()
	must(t, err)
	if remove != 1 || kept != 1 {
		t.Fatalf("GCCounts = %d removable, %d kept; want 1, 1", remove, kept)
	}
	ls, err := a.GC()
	must(t, err)
	removed := 0
	for _, l := range ls {
		if l.Keep == "" {
			removed++
		}
	}
	if removed != remove {
		t.Fatalf("gc removed %d, doctor counted %d", removed, remove)
	}
	if remove, kept, err = a.GCCounts(); err != nil || remove != 0 || kept != 1 {
		t.Fatalf("after gc: %d removable, %d kept, %v; want 0, 1", remove, kept, err)
	}
}
