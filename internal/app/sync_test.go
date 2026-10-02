package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
)

// commitOnIntegration commits files on top of integration from a scratch
// worktree and moves integration there as the train, returning the commit.
func commitOnIntegration(t *testing.T, a *App, msg string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scratch")
	git(t, a.Root, "worktree", "add", "-q", "--detach", dir, a.Cfg.Integration)
	defer git(t, a.Root, "worktree", "remove", "--force", dir)
	for f, body := range files {
		write(t, dir, f, body)
	}
	commitAll(t, dir, msg)
	head := git(t, dir, "rev-parse", "HEAD")
	asTrain(t, a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, head)
	return head
}

// syncSquashed cuts a task, gives it two commits, squash-lands their work on
// integration (after unrelated work when between is set), then adds the
// task's own commit. Integration is ahead of the task's merge-base by both.
func syncSquashed(t *testing.T, between bool) (*App, string) {
	t.Helper()
	a, _ := setup(t)
	must(t, a.Init())
	must(t, a.ensureIntegration())
	tk, err := a.Spawn(SpawnReq{Title: "late"})
	must(t, err)
	write(t, tk.Worktree, "events_test.go", "one\n")
	commitAll(t, tk.Worktree, "pre-squash 1")
	write(t, tk.Worktree, "events_test.go", "one\ntwo\n")
	write(t, tk.Worktree, "usage.go", "usage\n")
	commitAll(t, tk.Worktree, "pre-squash 2")
	if between {
		commitOnIntegration(t, a, "unrelated", map[string]string{"other.go": "other\n"})
	}
	commitOnIntegration(t, a, "usage and narrator (#130)", map[string]string{"events_test.go": "one\ntwo\n", "usage.go": "usage\n"})
	commitOnIntegration(t, a, "t34 (#131)", map[string]string{"t34.go": "t34\n"})
	write(t, tk.Worktree, "mine.go", "mine\n")
	commitAll(t, tk.Worktree, "task's own work")
	return a, tk.ID
}

// #132: the task was cut from commits integration later squash-landed under
// another SHA. Sync replays only the task's own commit, like the hand fix
// `git rebase --onto <integration> <squashed tip>`.
func TestSyncSkipsSquashLandedCommits(t *testing.T) {
	for _, between := range []bool{false, true} {
		name := "same tree"
		if between {
			name = "by patch-id"
		}
		t.Run(name, func(t *testing.T) {
			a, id := syncSquashed(t, between)
			t.Setenv("SADDLE_TASK", id)
			rr, err := a.Sync(id)
			if err != nil || !rr.OK {
				t.Fatalf("sync = %+v, %v", rr, err)
			}
			tk, _ := a.Store.Task(id)
			if gitx.RebaseInProgress(tk.Worktree) {
				t.Fatal("left mid-rebase")
			}
			if got := git(t, tk.Worktree, "log", "--format=%s", a.Cfg.Integration+"..HEAD"); got != "task's own work" {
				t.Fatalf("replayed commits = %q, want only the task's own", got)
			}
			if rr.Skipped != 2 {
				t.Fatalf("skipped = %d, want 2", rr.Skipped)
			}
		})
	}
}

// A sync that fails without conflicts (here the ref guard refuses to move the
// branch) aborts rather than leaving the worktree mid-rebase.
func TestSyncAbortsFailedRebase(t *testing.T) {
	a, _ := setup(t)
	must(t, a.Init())
	must(t, a.ensureIntegration())
	tk, err := a.Spawn(SpawnReq{Title: "guarded"})
	must(t, err)
	write(t, tk.Worktree, "mine.go", "mine\n")
	commitAll(t, tk.Worktree, "mine")
	before := git(t, tk.Worktree, "rev-parse", "HEAD")
	commitOnIntegration(t, a, "theirs", map[string]string{"theirs.go": "theirs\n"})
	t.Setenv("SADDLE_TASK", "t9")
	if _, err := a.Sync(tk.ID); err == nil {
		t.Fatal("sync as another task succeeded")
	}
	if gitx.RebaseInProgress(tk.Worktree) {
		t.Fatal("failed sync left the worktree mid-rebase")
	}
	if got := git(t, tk.Worktree, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD = %s, want %s", got, before)
	}
}

// A conflicting sync stops at the conflict for the agent to resolve, and a
// second sync reports the rebase in progress instead of stacking on it.
func TestSyncNeverStacksOnARebaseInProgress(t *testing.T) {
	a, _ := setup(t)
	must(t, a.Init())
	must(t, a.ensureIntegration())
	tk, err := a.Spawn(SpawnReq{Title: "clash"})
	must(t, err)
	write(t, tk.Worktree, "README.md", "mine\n")
	commitAll(t, tk.Worktree, "mine")
	commitOnIntegration(t, a, "theirs", map[string]string{"README.md": "theirs\n"})
	t.Setenv("SADDLE_TASK", tk.ID)
	rr, err := a.Sync(tk.ID)
	if err != nil || rr.OK || len(rr.Conflicts) != 1 {
		t.Fatalf("sync = %+v, %v", rr, err)
	}
	if !gitx.RebaseInProgress(tk.Worktree) {
		t.Fatal("conflict was not left for the agent to resolve")
	}
	_, err = a.Sync(tk.ID)
	if err == nil || !strings.Contains(err.Error(), "rebase --abort") {
		t.Fatalf("second sync err = %v, want a rebase-in-progress error naming the way out", err)
	}
}
