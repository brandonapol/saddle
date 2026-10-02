package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// These tests cover #119 and #123: the stack has to survive people using
// GitHub (merging in the UI, squash merges, merging stacked PRs into each
// other, closing PRs, deleting branches) and killed tasks, without anyone
// editing state.db.

var errFakeGH = errors.New("gh: can't reach GitHub")

// trainState is the train entry's state for task.
func trainState(t *testing.T, a *App, task string) string {
	t.Helper()
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == task {
			return e.State
		}
	}
	t.Fatalf("%s has no train entry", task)
	return ""
}

// trainNote is the train entry's note for task.
func trainNote(t *testing.T, a *App, task string) string {
	t.Helper()
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == task {
			return e.Note
		}
	}
	t.Fatalf("%s has no train entry", task)
	return ""
}

// asTrain runs git as the merge train, which the ref guard lets move any branch.
func asTrain(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := trainGit(dir, args...)
	must(t, err)
	return out
}

// moveMain pushes an unrelated commit to origin/main from a fresh clone.
func moveMain(t *testing.T, origin, file string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	write(t, other, file, "main\n")
	commitAll(t, other, "main moves: "+file)
	git(t, other, "push", "-q", "origin", "main")
	return git(t, other, "rev-parse", "HEAD")
}

// prCalls are the gh calls that touched the PR at url.
func prCalls(calls []string, url string) []string {
	var out []string
	for _, c := range calls {
		if strings.Contains(c, " "+url+" ") || strings.HasSuffix(c, " "+url) {
			out = append(out, c)
		}
	}
	return out
}

// #123: two tasks landed in one batch each record their own from..to, a
// restack keeps both on integration, and prs opens a PR for each.
func TestBatchLandKeepsEveryTask(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"}, map[string]string{"two.txt": "two\nmore\n"})
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 2 || rs[0].State != store.TrainOK || rs[1].State != store.TrainOK {
		t.Fatalf("batch land = %+v", rs)
	}
	from1, to1, ok1 := strings.Cut(trainNote(t, a, "t1"), "..")
	from2, to2, ok2 := strings.Cut(trainNote(t, a, "t2"), "..")
	if !ok1 || !ok2 || from1 == to1 || from2 == to2 || to1 != from2 || to1 == to2 {
		t.Fatalf("batch notes: t1 %s..%s, t2 %s..%s", from1, to1, from2, to2)
	}

	moveMain(t, origin, "news.txt")
	res, err := a.Restack()
	must(t, err)
	if len(res.Merged) != 0 {
		t.Fatalf("restack judged %v merged", res.Merged)
	}
	assertStackKeeps(t, a, map[string]string{"t1": "one", "t2": "two"})
}

// assertStackKeeps checks that integration holds each task's work once and
// that prs gives each task a PR showing only its own commits.
func assertStackKeeps(t *testing.T, a *App, titles map[string]string) {
	t.Helper()
	integ := a.Cfg.Integration
	for id, title := range titles {
		if n := git(t, a.Root, "rev-list", "--count", "origin/main.."+integ, "--", title+".txt"); n == "0" {
			t.Errorf("%s's work is not on integration", id)
		}
	}
	urls, err := a.PRs()
	if err != nil {
		t.Fatalf("PRs: %v", err)
	}
	if len(urls) != len(titles) {
		t.Fatalf("PRs = %v, want one per task %v", urls, titles)
	}
	base := "origin/main"
	for _, l := range mustStack(t, a) {
		want := titles[l.ID]
		got := git(t, a.Root, "log", "--format=%s", base+".."+l.To)
		for _, s := range strings.Split(got, "\n") {
			if !strings.HasPrefix(s, want) {
				t.Errorf("%s's PR shows %q, want only %q commits", l.ID, got, want)
			}
		}
		if l.PR == "" {
			t.Errorf("%s has no PR", l.ID)
		}
		base = l.To
	}
}

func mustStack(t *testing.T, a *App) []landedTask {
	t.Helper()
	s, err := a.landedStack()
	must(t, err)
	return s
}

// #123, the 2026-10-02 state: an old binary recorded the same single SHA as
// both batch-landed tasks' note. Restack must not judge the second one merged
// (empty range) and drop it; it recovers each task's range from its landing.
func TestRestackRecoversBatchLandedLegacyNotes(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"}, map[string]string{"two.txt": "two\nmore\n"})
	_, err := a.Land()
	must(t, err)
	tip := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	for _, id := range []string{"t1", "t2"} {
		must(t, a.Store.SetTrain(id, store.TrainOK, tip[:7], false))
	}

	moveMain(t, origin, "news.txt")
	res, err := a.Restack()
	must(t, err)
	if len(res.Merged) != 0 {
		t.Fatalf("restack judged %v merged and skipped it", res.Merged)
	}
	assertStackKeeps(t, a, map[string]string{"t1": "one", "t2": "two"})
}

// #123, after the bad restack: t2's note is an empty range and its commits
// are gone from integration. The stack reports it and restack puts t2's work
// back, so nobody has to requeue the train row by hand.
func TestRestackRestoresTaskDroppedAsMerged(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, err := a.Land()
	must(t, err)
	_, to1, _ := strings.Cut(trainNote(t, a, "t1"), "..")
	asTrain(t, a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, to1)
	must(t, a.Store.SetTrain("t2", store.TrainOK, to1+".."+to1, false))

	ls, err := a.StackLayers()
	must(t, err)
	if len(ls) != 2 || ls[1].Task.ID != "t2" || ls[1].Problem == "" {
		t.Fatalf("layers = %+v, want t2 reported", ls)
	}
	res, err := a.Restack()
	must(t, err)
	if len(res.Merged) != 0 {
		t.Fatalf("restack merged = %v", res.Merged)
	}
	assertStackKeeps(t, a, map[string]string{"t1": "one", "t2": "two"})
}

// #119.1 and the owner's second hand edit: a killed task with a PR and a
// landed train row leaves the stack by itself. Its train row becomes
// superseded, its PR is never touched again, and restack drops its commits
// instead of letting prs bundle them into the next task's PR.
func TestKilledLandedTaskLeavesStack(t *testing.T) {
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	_, err := a.PRs()
	must(t, err)
	t2, _ := a.Store.Task("t2")
	must(t, a.Kill("t2", false))
	before := len(ghLog())

	// t3's PR would show t2's commit: prs publishes t1, holds t3.
	urls, err := a.PRs()
	if err == nil || !strings.Contains(err.Error(), "t3") || !strings.Contains(err.Error(), "restack") {
		t.Fatalf("PRs with a killed task in the stack: %v, %v", urls, err)
	}
	if got := trainState(t, a, "t2"); got != TrainSuperseded || TrainSuperseded != "superseded" {
		t.Fatalf("t2's train row = %q, want superseded", got)
	}
	res, err := a.Restack()
	must(t, err)
	if strings.Join(res.Superseded, ",") != "t2" {
		t.Fatalf("restack superseded = %v, want [t2]", res.Superseded)
	}
	if n := git(t, a.Root, "rev-list", "--count", "origin/main.."+a.Cfg.Integration, "--", "two.txt"); n != "0" {
		t.Fatal("killed t2's commit is still on integration")
	}
	assertStackKeeps(t, a, map[string]string{"t1": "one", "t3": "three"})
	if c := prCalls(ghLog()[before:], t2.PR); len(c) > 0 {
		t.Fatalf("killed t2's PR touched: %v", c)
	}
}

// The owner already marked rows 'superseded' in state.db by hand; that
// literal keeps working.
func TestHandSupersededRowLeavesStack(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	must(t, a.Store.SetTrain("t1", "superseded", trainNote(t, a, "t1"), false))
	if s := mustStack(t, a); len(s) != 1 || s[0].ID != "t2" {
		t.Fatalf("stack = %+v, want only t2", s)
	}
}

// #119.6 and the owner's first hand edit: restack skips a closed PR and a
// PR merged into main instead of failing on "Cannot change the base branch of
// a closed pull request", and retargets the open PRs above them.
func TestRestackSkipsClosedAndMergedPRs(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	landTask(t, a, "t4", "four", map[string]string{"four.txt": "four\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	t3, _ := a.Store.Task("t3")
	t4, _ := a.Store.Task("t4")

	// The owner squash-merges t1's PR in the UI and closes t2's.
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	git(t, other, "merge", "-q", "--squash", "origin/"+t1.Branch)
	git(t, other, "commit", "-qm", "one (#1)")
	git(t, other, "push", "-q", "origin", "main")
	setPR(t, t1.PR, "MERGED", "main")
	setPR(t, t2.PR, "CLOSED", t1.Branch)
	before := len(ghLog())

	res, err := a.Restack()
	if err != nil {
		t.Fatalf("Restack: %v", err)
	}
	calls := ghLog()[before:]
	for _, tk := range []store.Task{t1, t2} {
		for _, c := range prCalls(calls, tk.PR) {
			if strings.HasPrefix(c, "pr edit") {
				t.Fatalf("restack edited %s's %s PR: %s", tk.ID, trainState(t, a, tk.ID), c)
			}
		}
	}
	if !contains(calls, "pr edit "+t3.PR+" --base main") || !contains(calls, "pr edit "+t4.PR+" --base "+t3.Branch) {
		t.Fatalf("open PRs not retargeted: %v", calls)
	}
	if got := trainState(t, a, "t1"); got != TrainMerged {
		t.Fatalf("t1's train row = %q, want %s", got, TrainMerged)
	}
	if got := trainState(t, a, "t2"); got != TrainSuperseded {
		t.Fatalf("t2's train row = %q, want %s", got, TrainSuperseded)
	}
	if strings.Join(res.Superseded, ",") != "t2" {
		t.Fatalf("superseded = %v", res.Superseded)
	}
	assertStackKeeps(t, a, map[string]string{"t3": "three", "t4": "four"})
}

// Even if GitHub can't be asked first, a closed PR's retarget failure skips
// that PR instead of failing the restack.
func TestRestackToleratesRetargetOfClosedPR(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	setPR(t, t1.PR, "CLOSED", "main")
	moveMain(t, origin, "news.txt")
	prev := prView
	prView = func(string, string) (PRInfo, error) { return PRInfo{}, errFakeGH }
	defer func() { prView = prev }()
	if _, err := a.Restack(); err != nil {
		t.Fatalf("Restack: %v", err)
	}
	if got := trainState(t, a, "t1"); got != TrainSuperseded {
		t.Fatalf("t1's train row = %q, want %s", got, TrainSuperseded)
	}
}

// #119.2: a PR merged into the stacked base below it instead of main is
// reported once with the fix and re-landed as a fresh PR; nobody clears
// tasks.pr by hand.
func TestPRMergedIntoStackedBaseGetsFreshPR(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	_, to1, _ := strings.Cut(trainNote(t, a, "t1"), "..")

	// The owner merges t2's PR into t1's branch on GitHub.
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	git(t, other, "checkout", "-q", t1.Branch)
	git(t, other, "merge", "-q", "--no-ff", "-m", "Merge pull request #2", "origin/"+t2.Branch)
	git(t, other, "push", "-q", "origin", t1.Branch)
	setPR(t, t2.PR, "MERGED", t1.Branch)
	_, _ = a.Store.TakeNotices(OrchestratorID, false)

	urls, err := a.PRs()
	if err != nil {
		t.Fatalf("PRs: %v", err)
	}
	nt2, _ := a.Store.Task("t2")
	if nt2.PR == "" || nt2.PR == t2.PR || len(urls) != 2 {
		t.Fatalf("t2's PR = %q (was %s), urls %v; want a fresh PR", nt2.PR, t2.PR, urls)
	}
	if got := remoteRev(t, origin, t1.Branch); got != to1 {
		t.Fatalf("origin %s = %s, want t1's landed %s", t1.Branch, got, to1)
	}
	ns, _ := a.Store.TakeNotices(OrchestratorID, false)
	var told []string
	for _, n := range ns {
		if strings.Contains(n.Text, t2.PR) {
			told = append(told, n.Text)
		}
	}
	if len(told) != 1 || !strings.Contains(told[0], "fresh PR") || !strings.Contains(told[0], t1.Branch) {
		t.Fatalf("orchestrator told %v, want one notice with the fix", told)
	}
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	ns, _ = a.Store.TakeNotices(OrchestratorID, false)
	for _, n := range ns {
		if strings.Contains(n.Text, t2.PR) {
			t.Fatalf("reported twice: %s", n.Text)
		}
	}
}

// A PR merged into its stacked base whose work reached main anyway (the base
// PR merged later) simply leaves the stack.
func TestPRMergedIntoStackedBaseThenMainIsMerged(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	git(t, other, "checkout", "-q", t1.Branch)
	git(t, other, "merge", "-q", "--no-ff", "-m", "Merge pull request #2", "origin/"+t2.Branch)
	git(t, other, "checkout", "-q", "main")
	git(t, other, "merge", "-q", "--squash", t1.Branch)
	git(t, other, "commit", "-qm", "one and two (#1)")
	git(t, other, "push", "-q", "origin", "main", t1.Branch)
	setPR(t, t1.PR, "MERGED", "main")
	setPR(t, t2.PR, "MERGED", t1.Branch)

	res, err := a.Restack()
	must(t, err)
	for _, id := range []string{"t1", "t2"} {
		if got := trainState(t, a, id); got != TrainMerged {
			t.Fatalf("%s's train row = %q, want merged", id, got)
		}
	}
	if len(res.Superseded) != 0 {
		t.Fatalf("superseded = %v", res.Superseded)
	}
	assertStackKeeps(t, a, map[string]string{"t3": "three"})
}

// #119.3: a landed branch deleted locally is recreated at its landed commit
// instead of blocking prs and restack.
func TestDeletedLandedBranchIsRestored(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, to1, _ := strings.Cut(trainNote(t, a, "t1"), "..")
	asTrain(t, a.Root, "branch", "-D", t1.Branch)

	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs with a deleted branch: %v", err)
	}
	if got := git(t, a.Root, "rev-parse", t1.Branch); got != to1 {
		t.Fatalf("%s = %s, want landed %s", t1.Branch, got, to1)
	}
	if !hasEvent(t, a, "branch_restored", t1.Branch) {
		t.Fatal("restoring the branch was not recorded")
	}
	asTrain(t, a.Root, "branch", "-D", t1.Branch)
	moveMain(t, origin, "news.txt")
	if _, err := a.Restack(); err != nil {
		t.Fatalf("Restack with a deleted branch: %v", err)
	}
}

// #119.4: a flag freezes land only for queued work that touches the broken
// layers; a task that doesn't lands.
func TestFlagHoldsOnlyDependentLand(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"one.txt": "one\ntwo\n"})
	queueTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	must(t, a.SetFlag(StackFlag{Task: t1.ID, Cause: "GitHub reports its PR conflicts with its base"}))

	rs, err := a.Land()
	if err != nil {
		t.Fatalf("Land while flagged: %v", err)
	}
	got := map[string]string{}
	for _, r := range rs {
		got[r.Task] = r.State
	}
	if got["t3"] != store.TrainOK || got["t2"] != TrainHeld {
		t.Fatalf("land while flagged = %+v, want t3 landed and t2 held", rs)
	}
	if trainState(t, a, "t2") != store.Queued {
		t.Fatal("held task left the queue")
	}
	must(t, a.ClearFlag())
	rs, err = a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].Task != "t2" || rs[0].State != store.TrainOK {
		t.Fatalf("land after the flag cleared = %+v", rs)
	}
}

// #119.4: prs publishes the healthy layers below a broken one.
func TestPRsPublishesLayersBelowFlag(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	must(t, a.SetFlag(StackFlag{Task: t2.ID, Cause: "GitHub reports its PR conflicts with its base"}))
	urls, err := a.PRs()
	if err == nil || !strings.Contains(err.Error(), "t2") {
		t.Fatalf("PRs flagged at t2: %v", err)
	}
	if len(urls) != 1 || remoteRev(t, origin, t1.Branch) == "" || remoteRev(t, origin, t2.Branch) != "" {
		t.Fatalf("PRs = %v; want only t1 published", urls)
	}
}

// #119.5: unstack drops a task from the stack by task id or PR, and ack
// lifts the freeze of the current flag, both without touching state.db.
func TestUnstackAndAck(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")

	for _, ref := range []string{t1.PR, "#2", "t3"} {
		if _, err := a.Unstack(ref); err != nil {
			t.Fatalf("Unstack(%q): %v", ref, err)
		}
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if got := trainState(t, a, id); got != TrainSuperseded {
			t.Fatalf("%s = %q after unstack", id, got)
		}
	}
	if !hasEvent(t, a, "unstacked", "") {
		t.Fatal("unstack not recorded")
	}
	if _, err := a.Unstack("#99"); err == nil {
		t.Fatal("unstack of an unknown PR succeeded")
	}

	if _, err := a.AckFlag(); err == nil {
		t.Fatal("ack with no flag succeeded")
	}
	must(t, a.SetFlag(StackFlag{Task: "t1", Cause: "x"}))
	f, err := a.AckFlag()
	if err != nil || !f.Acked {
		t.Fatalf("AckFlag = %+v, %v", f, err)
	}
	if err := a.checkFlag(); err != nil {
		t.Fatalf("acked flag still freezes: %v", err)
	}
}

// The owner's third hand edit: a landed task whose work is not on
// integration is requeued with one call and lands again; requeueing work
// integration already has is refused.
func TestRequeueLandsAgain(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if err := a.Requeue("t2"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("requeue of work on integration: %v", err)
	}
	from2, _, _ := strings.Cut(trainNote(t, a, "t2"), "..")
	asTrain(t, a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, from2)

	must(t, a.Requeue("t2"))
	if got := trainState(t, a, "t2"); got != store.Queued {
		t.Fatalf("t2's train row = %q, want queued", got)
	}
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].Task != "t2" || rs[0].State != store.TrainOK {
		t.Fatalf("land after requeue = %+v", rs)
	}
	assertStackKeeps(t, a, map[string]string{"t1": "one", "t2": "two"})
}
