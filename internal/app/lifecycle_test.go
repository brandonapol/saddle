package app

import (
	"github.com/brandonapol/saddle/internal/gitx"
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
