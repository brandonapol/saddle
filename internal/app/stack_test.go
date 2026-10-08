package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// t1 lands in two commits and is squash-merged into origin/main; restack must
// drop it, lay t2 and t3 linearly on the new main and retarget t2's PR.
func TestRestackAfterSquashMerge(t *testing.T) {
	t.Parallel()
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
// ref moves, and the conflict goes back to its task while its agent is alive
// (an orphaned one gets a repair task instead: TestRestackOrphanConflictSpawnsOneRepair).
func TestRestackConflictReturnsToOwner(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.CloseOnLand = false
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
	t.Parallel()
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
	t.Parallel()
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

// #219: killing a landed task that has no PR only closes its agent. Its work
// is on integration alone, so restack keeps it, and a later task that needs
// it still finds it there.
func TestRestackKeepsKilledUnpublishedTask(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	must(t, a.Kill("t1", false))
	moveMain(t, origin, "news.txt")

	res, err := a.Restack()
	must(t, err)
	if len(res.Superseded) != 0 || len(res.Merged) != 0 {
		t.Fatalf("restack superseded %v, merged %v; want neither", res.Superseded, res.Merged)
	}
	if got := trainState(t, a, "t1"); got != store.TrainOK {
		t.Fatalf("t1's train row = %q, want still in the stack", got)
	}
	for _, f := range []string{"one.txt", "two.txt", "news.txt"} {
		if n := git(t, a.Root, "rev-list", "--count", a.Cfg.Integration, "--", f); n == "0" {
			t.Errorf("integration lost %s", f)
		}
	}
	if res.Backup == "" || git(t, a.Root, "rev-parse", res.Backup) == git(t, a.Root, "rev-parse", a.Cfg.Integration) {
		t.Fatalf("no backup of the rewritten integration: %q", res.Backup)
	}
}

// #219: with rerere replaying an old resolution, cherry-pick stops with the
// conflict already resolved and staged. Restack used to read that as an
// empty commit, skip it, and mark the task merged; it must stop on it as a
// conflict, which with t1's agent gone means a repair task.
func TestRestackDoesNotSkipRerereResolvedPick(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	git(t, a.Root, "config", "rerere.enabled", "true")
	git(t, a.Root, "config", "rerere.autoupdate", "true")
	t1 := landTask(t, a, "t1", "one", map[string]string{"shared.txt": "one\n"})
	moveMain(t, origin, "shared.txt")
	git(t, a.Root, "fetch", "-q", "origin")

	// Someone resolved this very conflict once, so rerere remembers it.
	wt := filepath.Join(t.TempDir(), "scratch")
	git(t, a.Root, "worktree", "add", "-q", "--detach", wt, "origin/main")
	if _, err := gitx.Run(wt, "cherry-pick", t1.Branch); err == nil {
		t.Fatal("the pick didn't conflict")
	}
	write(t, wt, "shared.txt", "main\none\n")
	git(t, wt, "add", "shared.txt")
	git(t, wt, "-c", "core.editor=true", "cherry-pick", "--continue")
	git(t, a.Root, "worktree", "remove", "--force", wt)

	// t1's agent is gone, so the conflict goes to a repair task (#172).
	res, err := a.Restack()
	must(t, err)
	if len(res.Merged) != 0 || strings.Join(res.Repairing, ",") != "t1" {
		t.Fatalf("restack merged %v, repairing %v; want t1 sent to a repair", res.Merged, res.Repairing)
	}
	if got := trainState(t, a, "t1"); got != TrainRepairing {
		t.Fatalf("t1's train row = %q, want repairing", got)
	}
}

// #219: the integrity check refuses a rebuilt stack that lost a task or one
// of its commits, whatever replay did.
func TestVerifyKeptRefusesLostWork(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	stack := mustStack(t, a)
	base := git(t, a.Root, "rev-parse", "origin/main")
	keep := []restacked{
		{landedTask: stack[0], NewFrom: stack[0].From, NewTo: stack[0].To},
		{landedTask: stack[1], NewFrom: stack[1].From, NewTo: stack[1].To},
	}
	must(t, a.verifyKept(stack, keep, nil, base))

	if err := a.verifyKept(stack, keep[:1], nil, base); err == nil || !strings.Contains(err.Error(), "t2") {
		t.Fatalf("t2 left out: err = %v", err)
	}
	if err := a.verifyKept(stack, keep[:1], []string{"t2"}, base); err != nil {
		t.Fatalf("t2 leaving for a repair: %v", err)
	}
	// t2 "replayed" to an empty range on t1: its commit is gone.
	gone := []restacked{keep[0], {landedTask: stack[1], NewFrom: stack[0].To, NewTo: stack[0].To}}
	if err := a.verifyKept(stack, gone, nil, base); err == nil || !strings.Contains(err.Error(), "t2 (commit") {
		t.Fatalf("t2's commit dropped: err = %v", err)
	}
}

// #205: restack with nothing to do moves no ref, and a second restack after
// a real one moves nothing either.
func TestRestackMovesNothingWhenNothingMoved(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, err := a.PRs()
	must(t, err)
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	res, err := a.Restack()
	must(t, err)
	if len(res.Moves) != 0 || res.Backup != "" {
		t.Fatalf("restack with main unchanged moved %+v", res.Moves)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != before {
		t.Fatal("integration rewritten with nothing to do")
	}

	moveMain(t, origin, "news.txt")
	res, err = a.Restack()
	must(t, err)
	if len(res.Moves) == 0 {
		t.Fatal("restack after main moved moved nothing")
	}
	res, err = a.Restack()
	must(t, err)
	if len(res.Moves) != 0 {
		t.Fatalf("second restack moved %+v", res.Moves)
	}
}
