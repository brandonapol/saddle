package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// squashIntoMain merges a PR into origin/main the way GitHub's squash does:
// one new commit whose message is the PR's title and body. files are its
// content, which a reviewer may have edited so it matches no landed commit.
func squashIntoMain(t *testing.T, origin, msg string, files map[string]string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	for rel, body := range files {
		write(t, other, rel, body)
	}
	git(t, other, "add", "-A")
	f := filepath.Join(t.TempDir(), "msg")
	must(t, os.WriteFile(f, []byte(msg), 0o644))
	git(t, other, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-F", f)
	git(t, other, "push", "-q", "origin", "main")
}

// #331 (a): t1's PR was squash-merged with a reviewer's edit, so base's
// commit matches t1's work by neither patch-id nor content. Its Saddle-Task
// trailer, carried in from the PR body, still says t1 merged: restack skips
// t1 instead of replaying it over the reviewer's edit, and keeps t2.
func TestRestackKnowsSquashWithReviewerEditByTrailer(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, err := a.PRs()
	must(t, err)
	if log := strings.Join(ghLog(), "\n"); !strings.Contains(log, "Saddle-Task: t1") || !strings.Contains(log, "Saddle-Task: t2") {
		t.Fatalf("prs didn't put Saddle-Task trailers in the PR bodies:\n%s", log)
	}
	l := mustStack(t, a)[0]
	if a.inBase(l, "origin/main") {
		t.Fatal("t1 judged in base before it merged")
	}

	squashIntoMain(t, origin, "one (#1)\n\none summary\n\nSaddle-Task: t1\n\nCo-authored-by: Reviewer <r@example.com>\n",
		map[string]string{"one.txt": "one, as the reviewer had it\n"})
	git(t, a.Root, "fetch", "-q", "origin")
	if !a.inBase(l, "origin/main") {
		t.Fatal("inBase missed t1's squash merge with a reviewer edit")
	}
	res, err := a.Restack()
	must(t, err)
	if got := trainState(t, a, "t1"); got != TrainMerged || len(res.Merged) != 0 && strings.Join(res.Merged, ",") != "t1" {
		t.Fatalf("t1's train row = %q (restack merged %v), want merged", got, res.Merged)
	}
	if got := trainState(t, a, "t2"); got != store.TrainOK {
		t.Fatalf("t2's train row = %q, want it still in the stack", got)
	}
	if got := git(t, a.Root, "show", a.Cfg.Integration+":one.txt"); got != "one, as the reviewer had it" {
		t.Fatalf("integration's one.txt = %q, want the reviewer's", got)
	}
	if got := git(t, a.Root, "show", a.Cfg.Integration+":two.txt"); got != "two" {
		t.Fatalf("t2's work left integration: %q", got)
	}
}

// #331 (b), the #123 state: both batch-landed tasks carry the same legacy
// note. Base has only t2's squash, with a reviewer's edit. The trailer, not
// the shared note, decides: t2 merged, t1 stays.
func TestRestackDecidesIdenticalNotesByTrailer(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, err := a.Land()
	must(t, err)
	tip := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	for _, id := range []string{"t1", "t2"} {
		must(t, a.Store.SetTrain(id, store.TrainOK, tip[:7], false))
	}

	squashIntoMain(t, origin, "two (#2)\n\nSaddle-Task: t2\n", map[string]string{"two.txt": "two, reviewed\n"})
	res, err := a.Restack()
	must(t, err)
	if got := trainState(t, a, "t2"); got != TrainMerged || len(res.Merged) != 0 && strings.Join(res.Merged, ",") != "t2" {
		t.Fatalf("t2's train row = %q (restack merged %v), want merged", got, res.Merged)
	}
	if got := trainState(t, a, "t1"); got != store.TrainOK {
		t.Fatalf("t1's train row = %q, want it still in the stack", got)
	}
	if got := git(t, a.Root, "show", a.Cfg.Integration+":one.txt"); got != "one" {
		t.Fatalf("t1's work left integration: %q", got)
	}
	if got := git(t, a.Root, "show", a.Cfg.Integration+":two.txt"); got != "two, reviewed" {
		t.Fatalf("integration's two.txt = %q, want the reviewer's", got)
	}
}

// A trailer only answers for the work it is older than: a task that went on
// committing after its PR merged still has unmerged work, so the trailer
// says nothing and patch-id decides.
func TestTaskTrailerIgnoresWorkAfterTheMerge(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	squashIntoMain(t, origin, "one (#1)\n\nSaddle-Task: t1\n", map[string]string{"one.txt": "one\n"})
	git(t, a.Root, "fetch", "-q", "origin")
	git(t, a.Root, "checkout", "-q", "-b", "later", "origin/main~1")
	write(t, a.Root, "later.txt", "later\n")
	git(t, a.Root, "add", "-A")
	git(t, a.Root, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "later", "--date=2099-01-01T00:00:00")
	if a.taskTrailerOn("t1", "origin/main", "later") {
		t.Fatal("a merge older than the branch's last commit vouched for it")
	}
	git(t, a.Root, "checkout", "-q", "-b", "earlier", "origin/main~1")
	write(t, a.Root, "earlier.txt", "earlier\n")
	git(t, a.Root, "add", "-A")
	git(t, a.Root, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "earlier", "--date=2001-01-01T00:00:00")
	if !a.taskTrailerOn("t1", "origin/main", "earlier") {
		t.Fatal("t1's trailer on base wasn't found")
	}
	if a.taskTrailerOn("t11", "origin/main", "earlier") || a.taskTrailerOn("t", "origin/main", "earlier") {
		t.Fatal("t1's trailer answered for another task")
	}
}

// #331: gc uses the same check. A branch whose PR was squash-merged with a
// reviewer's edit matches base by neither content nor patch-id, but base
// names its task, so gc removes it.
func TestGCRemovesSquashWithReviewerEditByTrailer(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	task := killedWithWork(t, a, "squashed", "a.txt", "b.txt")
	write(t, other, "a.txt", "a.txt, reviewed\n")
	write(t, other, "b.txt", "b.txt\n")
	git(t, other, "add", "-A")
	git(t, other, "commit", "-qm", "squashed (#1)\n\nSaddle-Task: "+task.ID)
	git(t, other, "push", "-q", "origin", "main")
	git(t, a.Root, "fetch", "-q", "origin")

	ls, err := a.Leftovers()
	must(t, err)
	if l, ok := leftover(ls, "branch", task.Branch); !ok || l.Keep != "" {
		t.Fatalf("squash-merged branch: %+v (found %v), want removable", l, ok)
	}
}

// #331: requeue uses the same check. Integration holds t1's landing under a
// different patch (as a restack over a reviewer's edit leaves it); its
// trailer says the work is there, so requeue refuses.
func TestRequeueKnowsWorkByTrailer(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	integ := a.Cfg.Integration
	if got := git(t, a.Root, "log", "-1", "--format=%(trailers:key=Saddle-Task,valueonly)", integ); got != "t1" {
		t.Fatalf("t1's landed commit trailer = %q, want t1", got)
	}
	wt := filepath.Join(t.TempDir(), "edit")
	git(t, a.Root, "worktree", "add", "-q", "--detach", wt, integ+"^")
	write(t, wt, "one.txt", "one, reviewed\n")
	git(t, wt, "add", "-A")
	msg := git(t, a.Root, "log", "-1", "--format=%B", integ)
	asTrain(t, wt, "commit", "-q", "-m", msg)
	asTrain(t, a.Root, "update-ref", "refs/heads/"+integ, git(t, wt, "rev-parse", "HEAD"))

	err := a.Requeue("t1")
	if err == nil || !strings.Contains(err.Error(), "Saddle-Task") {
		t.Fatalf("requeue of work integration has by trailer: %v", err)
	}
}
