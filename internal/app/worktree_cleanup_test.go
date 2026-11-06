package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
)

// #803: a task that finishes with no change has nothing to land, so its
// clean worktree goes at once.
func TestNoChangeRemovesWorktree(t *testing.T) {
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "already fixed"})
	must(t, err)
	must(t, a.Done(tk.ID, "the issue was already fixed"))
	assertNoChange(t, a, tk.ID)
	if exists(tk.Worktree) {
		t.Fatal("a no-change task kept its worktree")
	}
	if gitx.BranchExists(a.Root, tk.Branch) {
		t.Fatal("a no-change task kept a branch with no work of its own")
	}
}

// #803: kill saves uncommitted work to refs/saddle/wip/<task>, so keeping
// the dirty tree protects nothing: it goes once the snapshot is verified.
func TestKillRemovesDirtyWorktreeAfterSnapshot(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "dirty"})
	must(t, err)
	write(t, tk.Worktree, "README.md", "edited\n")
	write(t, tk.Worktree, "new.txt", "untracked\n")
	must(t, a.Kill(tk.ID, false))
	ref := wipRef(tk.ID)
	if out := git(t, a.Root, "show", ref+":new.txt"); out != "untracked" {
		t.Fatalf("snapshot new.txt = %q", out)
	}
	if out := git(t, a.Root, "show", ref+":README.md"); out != "edited" {
		t.Fatalf("snapshot README.md = %q", out)
	}
	if exists(tk.Worktree) {
		t.Fatal("kill kept a dirty worktree whose work is in the wip snapshot")
	}
	if strings.Contains(git(t, a.Root, "worktree", "list"), tk.Worktree) {
		t.Fatal("git still lists the removed worktree")
	}
}

// #803: a tree killed mid-rebase is aborted, snapshotted and removed; the
// branch, which holds unlanded work, stays.
func TestKillRemovesWorktreeMidRebase(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "rebasing"})
	must(t, err)
	write(t, tk.Worktree, "README.md", "ours\n")
	commitAll(t, tk.Worktree, "ours")
	tip := git(t, a.Root, "rev-parse", tk.Branch)
	// Integration moves on with a clashing change.
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "worktree", "add", "-q", "--detach", other, a.Cfg.Integration)
	write(t, other, "README.md", "theirs\n")
	commitAll(t, other, "theirs")
	if _, err := trainGit(a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, git(t, other, "rev-parse", "HEAD")); err != nil {
		t.Fatal(err)
	}
	git(t, a.Root, "worktree", "remove", "--force", other)
	if _, err := gitx.Run(tk.Worktree, "rebase", a.Cfg.Integration); err == nil {
		t.Fatal("rebase didn't stop on the conflict")
	}
	if !gitx.RebaseInProgress(tk.Worktree) {
		t.Fatal("not mid-rebase")
	}
	write(t, tk.Worktree, "scratch.txt", "notes\n")

	must(t, a.Kill(tk.ID, false))
	if exists(tk.Worktree) {
		t.Fatal("kill kept a worktree left mid-rebase")
	}
	if got := git(t, a.Root, "rev-parse", tk.Branch); got != tip {
		t.Fatalf("branch moved to %s, want %s", got, tip)
	}
	if out := git(t, a.Root, "show", wipRef(tk.ID)+":scratch.txt"); out != "notes" {
		t.Fatalf("snapshot scratch.txt = %q", out)
	}
}

// #803: when the snapshot can't be saved, the dirty tree stays and an event
// says why.
func TestKillKeepsWorktreeWhenSnapshotFails(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "unsaved"})
	must(t, err)
	write(t, tk.Worktree, "wip.txt", "wip\n")
	// A directory where the ref would go makes update-ref fail.
	common := git(t, a.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	must(t, os.MkdirAll(filepath.Join(common, "refs", "saddle", "wip", tk.ID, "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(common, "refs", "saddle", "wip", tk.ID, "x", "y"), []byte("junk"), 0o644))

	must(t, a.Kill(tk.ID, false))
	if !exists(filepath.Join(tk.Worktree, "wip.txt")) {
		t.Fatal("kill removed a dirty worktree it couldn't snapshot")
	}
	if !hasEvent(t, a, "worktree_kept", "snapshot") {
		t.Fatalf("no worktree_kept event: %v", eventsOf(t, a, "worktree_kept"))
	}
}
