package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layoutCommits gives the repo a commit to replay (main..feat, adding
// feat.txt) and a tip to replay it onto (other, adding other.txt).
func layoutCommits(t *testing.T, a *App) (tip, from, to string) {
	t.Helper()
	from = git(t, a.Root, "rev-parse", "main")
	git(t, a.Root, "checkout", "-q", "-b", "feat", "main")
	write(t, a.Root, "feat.txt", "feat\n")
	commitAll(t, a.Root, "feat")
	to = git(t, a.Root, "rev-parse", "HEAD")
	git(t, a.Root, "checkout", "-q", "-b", "other", "main")
	write(t, a.Root, "other.txt", "other\n")
	commitAll(t, a.Root, "other")
	tip = git(t, a.Root, "rev-parse", "HEAD")
	git(t, a.Root, "checkout", "-q", "main")
	return tip, from, to
}

func assertReplayed(t *testing.T, a *App, head, tip string) {
	t.Helper()
	if parent := git(t, a.Root, "rev-parse", head+"^"); parent != tip {
		t.Fatalf("replayed head %s sits on %s, want %s", head, parent, tip)
	}
	files := git(t, a.Root, "ls-tree", "-r", "--name-only", head)
	if !strings.Contains(files, "feat.txt") || !strings.Contains(files, "other.txt") {
		t.Fatalf("replayed head is missing work:\n%s", files)
	}
}

// #669: a stale .git/worktrees entry for .saddle/layout (its directory gone,
// left locked by a half-finished run so `git worktree prune` keeps it) must
// not stop the PR layout. Only saddle's own entry is cleared: another
// missing worktree stays registered.
func TestPRsRecoverStaleLayoutWorktree(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	main := git(t, a.Root, "rev-parse", "main")
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})

	layout := a.stateDir("layout")
	git(t, a.Root, "worktree", "add", "-q", "--detach", layout, "main")
	git(t, a.Root, "worktree", "lock", layout)
	must(t, os.RemoveAll(layout))
	other := filepath.Join(t.TempDir(), "operator")
	git(t, a.Root, "worktree", "add", "-q", "--detach", other, "main")
	must(t, os.RemoveAll(other))

	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs with a stale layout worktree: %v", err)
	}
	r2 := remoteRev(t, origin, t2.Branch)
	if parent := git(t, origin, "rev-parse", r2+"^"); parent != main {
		t.Fatalf("t2's PR head is not on main: parent %s", parent)
	}
	list := git(t, a.Root, "worktree", "list", "--porcelain")
	if !strings.Contains(list, "worktree "+other) {
		t.Fatalf("an unrelated missing worktree was pruned:\n%s", list)
	}
	if strings.Contains(list, "worktree "+layout) {
		t.Fatalf("layout worktree left registered:\n%s", list)
	}
}

// #669: the layout directory vanishing between replays (as after a merge,
// when another run removed it) is rebuilt and the replay retried once.
func TestLayoutReplayRecreatesMissingDir(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	tip, from, to := layoutCommits(t, a)
	dir := ""
	head, ok, err := a.layoutReplay(&dir, tip, from, to)
	if err != nil || !ok {
		t.Fatalf("first replay: ok=%v err=%v", ok, err)
	}
	assertReplayed(t, a, head, tip)

	must(t, os.RemoveAll(dir))
	head, ok, err = a.layoutReplay(&dir, tip, from, to)
	if err != nil || !ok {
		t.Fatalf("replay after the layout dir vanished: ok=%v err=%v", ok, err)
	}
	assertReplayed(t, a, head, tip)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("layout dir not recreated: %v", err)
	}
}

// #669: a cherry-pick left in progress in the layout worktree is aborted
// before the next replay instead of failing it.
func TestReplayOntoAbortsCherryPickInProgress(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	tip, from, to := layoutCommits(t, a)
	git(t, a.Root, "checkout", "-q", "-b", "clash", "main")
	write(t, a.Root, "other.txt", "clash\n")
	commitAll(t, a.Root, "clash")
	clash := git(t, a.Root, "rev-parse", "HEAD")
	git(t, a.Root, "checkout", "-q", "main")

	dir, err := a.openLayout(tip)
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if _, err := trainGit(dir, "cherry-pick", clash); err == nil {
		t.Fatal("setup: cherry-pick was meant to conflict")
	}
	if _, err := trainGit(dir, "rev-parse", "--verify", "-q", "CHERRY_PICK_HEAD"); err != nil {
		t.Fatalf("setup: no cherry-pick in progress: %v", err)
	}

	head, ok, err := replayOnto(dir, tip, from, to)
	if err != nil || !ok {
		t.Fatalf("replay over an unfinished cherry-pick: ok=%v err=%v", ok, err)
	}
	assertReplayed(t, a, head, tip)
}
