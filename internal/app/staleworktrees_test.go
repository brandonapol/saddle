package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// fakeWorktree makes a directory under .saddle/worktrees that looks like a
// git worktree (it has a .git file), without asking git.
func fakeWorktree(t *testing.T, a *App, name string) {
	t.Helper()
	dir := a.stateDir("worktrees", name)
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: x\n"), 0o644))
}

// #803 step 5: a stale worktree is one under the worktree directory whose
// task has ended (landed, killed, no change, failed) or is gone from the
// store. Live tasks' trees, and directories not named after a task or that
// aren't worktrees, don't count.
func TestStaleWorktrees(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for id, st := range map[string]string{
		"t1": store.Running, "t2": store.Idle, "t3": store.Killed,
		"t4": store.NoChange, "t5": store.Landed, "t6": store.Failed, "t7": store.Done,
	} {
		must(t, a.Store.CreateTask(store.Task{ID: id, Title: id, Status: st}))
	}
	for _, n := range []string{"t1-live", "t2-idle", "t3-killed", "t4-nochange", "t5-landed", "t6-failed", "t7-done", "t99-gone"} {
		fakeWorktree(t, a, n)
	}
	must(t, os.MkdirAll(a.stateDir("worktrees", "t98-not-a-tree"), 0o755))
	fakeWorktree(t, a, "scratch")

	n, err := a.StaleWorktrees()
	must(t, err)
	if n != 5 { // t3, t4, t5, t6, t99
		t.Fatalf("StaleWorktrees = %d, want 5", n)
	}
}

// No worktree directory at all is zero, not an error.
func TestStaleWorktreesNoDir(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, os.RemoveAll(a.stateDir("worktrees")))
	if n, err := a.StaleWorktrees(); err != nil || n != 0 {
		t.Fatalf("StaleWorktrees = %d, %v; want 0, nil", n, err)
	}
}
