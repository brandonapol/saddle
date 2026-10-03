package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// t1 lands in two commits and is squash-merged into origin/main; restack must
// drop it, lay t2 and t3 linearly on the new main and retarget t2's PR.
func TestRestackAfterSquashMerge(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Output = "single" // pins the one linear stack this test was written for (#52)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "a\n"}, map[string]string{"one.txt": "a\nb\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	t3 := landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	t2, _ = a.Store.Task(t2.ID)
	before := len(ghLog())

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	git(t, other, "merge", "-q", "--squash", "origin/"+t1.Branch)
	git(t, other, "commit", "-qm", "one (#1)")
	git(t, other, "push", "-q", "origin", "main")
	squash := git(t, other, "rev-parse", "HEAD")

	res, err := a.Restack()
	if err != nil {
		t.Fatal(err)
	}
	integ := a.Cfg.Integration
	if got := git(t, a.Root, "rev-parse", "origin/main"); got != squash {
		t.Fatalf("origin/main not fetched: %s", got)
	}
	if m := git(t, a.Root, "rev-list", "--merges", "origin/main.."+integ); m != "" {
		t.Fatalf("merge commits on integration: %s", m)
	}
	if git(t, a.Root, "merge-base", "origin/main", integ) != squash {
		t.Fatal("integration is not on top of origin/main")
	}
	if n := git(t, a.Root, "rev-list", "--count", integ, "--", "one.txt"); n != "1" {
		t.Fatalf("t1's change appears in %s commits", n)
	}
	if n := git(t, a.Root, "rev-list", "--count", "origin/main.."+integ); n != "2" {
		t.Fatalf("integration has %s commits over main, want 2", n)
	}
	for tk, rev := range map[string]string{t2.Branch: integ + "~1", t3.Branch: integ} {
		want := git(t, a.Root, "rev-parse", rev)
		if got := git(t, a.Root, "rev-parse", tk); got != want {
			t.Fatalf("%s = %s, want %s", tk, got, want)
		}
		if got := remoteRev(t, origin, tk); got != want {
			t.Fatalf("remote %s = %s, want %s", tk, got, want)
		}
	}
	for _, r := range [][3]string{{"origin/main", t2.Branch, "two"}, {t2.Branch, t3.Branch, "three"}} {
		if got := git(t, a.Root, "log", "--format=%s", r[0]+".."+r[1]); got != r[2] {
			t.Fatalf("%s..%s = %q, want only %q", r[0], r[1], got, r[2])
		}
	}

	calls := ghLog()[before:]
	for _, c := range calls {
		if strings.HasPrefix(c, "pr view ") {
			continue // reading its state is how restack knows it merged
		}
		if strings.Contains(c, t1.PR+" ") || strings.HasSuffix(c, t1.PR) {
			t.Fatalf("merged t1 PR touched: %s", c)
		}
	}
	if !contains(calls, "pr edit "+t2.PR+" --base main") {
		t.Fatalf("t2 PR not retargeted to main: %v", calls)
	}
	if len(res.Merged) != 1 || res.Merged[0] != t1.ID {
		t.Fatalf("merged = %v", res.Merged)
	}
	if !hasEvent(t, a, "restack", t2.Branch) || !hasEvent(t, a, "restack", integ) {
		t.Fatal("ref moves not recorded in events")
	}
	// The stack is consistent again, so prs accepts it.
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs after restack: %v", err)
	}
}

// A landed commit that conflicts with the new base stops restack before any
// ref moves, and the conflict goes back to its task.
func TestRestackConflictReturnsToOwner(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"README.md": "one\n"})
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "README.md", "main\n")
	commitAll(t, other, "main edits readme")
	git(t, other, "push", "-q", "origin", "main")

	_, err := a.Restack()
	if err == nil || !strings.Contains(err.Error(), t1.ID) || !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("restack conflict: err = %v", err)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != before {
		t.Fatal("integration moved despite the conflict")
	}
	if got := git(t, a.Root, "rev-parse", t1.Branch); got != before {
		t.Fatal("t1's branch moved despite the conflict")
	}
	ns, _ := a.Store.TakeNotices(t1.ID, false)
	if len(ns) == 0 || !strings.Contains(ns[len(ns)-1].Text, "README.md") {
		t.Fatalf("conflict notice = %+v", ns)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Every landed task was squash-merged into main and recorded with a
// single-SHA merged note on an old main commit; integration sits at the old
// main, and then a human PR merges to main. Integration has nothing main
// lacks, so restack fast-forwards it rather than counting main's own squash
// commits as work it would drop.
func TestRestackFastForwardsIntegrationBehindBase(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	old := git(t, a.Root, "rev-parse", "origin/main")

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	for _, tk := range []string{t1.Branch, t2.Branch} {
		git(t, other, "fetch", "-q", a.Root, tk)
		git(t, other, "merge", "-q", "--squash", "FETCH_HEAD")
		git(t, other, "commit", "-qm", "squash "+tk)
	}
	git(t, other, "push", "-q", "origin", "main")
	for _, id := range []string{t1.ID, t2.ID} {
		if err := a.Store.SetTrain(id, TrainMerged, old, false); err != nil {
			t.Fatal(err)
		}
		// Its landing's commit is gone too, so the note can't be recovered.
		a.Store.Event(id, "landed", strings.Repeat("d", 40))
	}
	// Integration was restacked onto that main earlier.
	git(t, a.Root, "fetch", "-q", "origin")
	if _, err := trainGit(a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, "origin/main"); err != nil {
		t.Fatal(err)
	}

	write(t, other, "human.txt", "human\n")
	commitAll(t, other, "human PR (#164)")
	git(t, other, "push", "-q", "origin", "main")
	head := git(t, other, "rev-parse", "HEAD")

	// prs and land must not trip over the same stale notes.
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs with integration behind main: %v", err)
	}
	if _, err := a.Restack(); err != nil {
		t.Fatalf("restack refused an integration main already holds: %v", err)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != head {
		t.Fatalf("integration = %s, want fast-forwarded to main %s", got, head)
	}
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs after restack: %v", err)
	}
	if n := git(t, a.Root, "rev-list", "--count", "origin/main.."+a.Cfg.Integration); n != "1" {
		t.Fatalf("integration has %s commits over main, want only t3's", n)
	}
}

// A commit on integration that no landed task owns and base doesn't have
// still stops restack: dropping it would lose work.
func TestRestackRefusesUnownedIntegrationCommit(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})

	wt := filepath.Join(t.TempDir(), "integ")
	git(t, a.Root, "worktree", "add", "-q", wt, a.Cfg.Integration)
	write(t, wt, "stray.txt", "stray\n")
	git(t, wt, "add", "-A")
	if _, err := trainGit(wt, "commit", "-qm", "stray commit"); err != nil {
		t.Fatal(err)
	}
	git(t, a.Root, "worktree", "remove", "--force", wt)
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "human.txt", "human\n")
	commitAll(t, other, "human PR")
	git(t, other, "push", "-q", "origin", "main")

	_, err := a.Restack()
	if err == nil || !strings.Contains(err.Error(), "no landed task owns") || !strings.Contains(err.Error(), "1 commit ") {
		t.Fatalf("restack err = %v, want refusal over the 1 stray commit", err)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != before {
		t.Fatal("integration moved despite the stray commit")
	}
}
