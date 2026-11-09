package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

// The merge queue (ADR 0012 rule 5, #935) over pr_open tasks, against an
// in-memory forge whose PRs live on a real bare origin: merges squash into
// the origin's main, mergeability is git's.

type qPR struct {
	n            int
	url          string
	head, base   string
	state        string
	draft        bool
	labels       []string
	checks       string // pass, fail, pending; "" none
	checksAt     string // the head the checks ran on; "" any head
	mergedHeadAt string
}

type qForge struct {
	origin     string
	prs        []*qPR
	strict     bool // a PR behind main is BEHIND (branch protection: up to date)
	baseChecks []forge.CheckRun
	merges     []string
	baseEdits  []string
}

func (f *qForge) rev(ref string) string {
	out, _ := gitx.Run(f.origin, "rev-parse", "--verify", "--quiet", "refs/heads/"+ref)
	return out
}

func (f *qForge) view(p *qPR) forge.PR {
	out := forge.PR{Number: p.n, URL: p.url, State: p.state, Draft: p.draft, Base: p.base, Head: p.head, Labels: p.labels}
	if p.state != forge.StateOpen {
		out.HeadSHA = p.mergedHeadAt
		return out
	}
	head, base := f.rev(p.head), f.rev(p.base)
	out.HeadSHA = head
	out.Mergeable, out.MergeState = forge.Mergeable, "CLEAN"
	if _, err := gitx.Run(f.origin, "merge-tree", "--write-tree", base, head); err != nil {
		out.Mergeable, out.MergeState = forge.Conflicting, "DIRTY"
	} else if _, err := gitx.Run(f.origin, "merge-base", "--is-ancestor", base, head); err != nil && f.strict {
		out.MergeState = "BEHIND"
	}
	st := p.checks
	if p.checksAt != "" && p.checksAt != head && st != "" {
		st = "pending" // a push starts CI again
	}
	switch st {
	case "pass":
		out.Checks = []forge.CheckRun{{Kind: forge.KindJob, Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}}
	case "fail":
		out.Checks = []forge.CheckRun{{Kind: forge.KindJob, Name: "ci", Status: "COMPLETED", Conclusion: "FAILURE"}}
	case "pending":
		out.Checks = []forge.CheckRun{{Kind: forge.KindJob, Name: "ci", Status: "IN_PROGRESS"}}
	}
	return out
}

func (f *qForge) find(ref string) *qPR {
	for _, p := range f.prs {
		if p.url == ref {
			return p
		}
	}
	return nil
}

func (f *qForge) OpenPRs(context.Context) ([]forge.PR, error) {
	var out []forge.PR
	for _, p := range f.prs {
		if p.state == forge.StateOpen {
			out = append(out, f.view(p))
		}
	}
	return out, nil
}

func (f *qForge) PR(_ context.Context, ref string, _ forge.PRFields) (forge.PR, error) {
	p := f.find(ref)
	if p == nil {
		return forge.PR{}, fmt.Errorf("no PR %s", ref)
	}
	return f.view(p), nil
}

func (f *qForge) MergeSettings(context.Context) (forge.MergeSettings, error) {
	return forge.MergeSettings{Squash: true}, nil
}

func (f *qForge) MergePR(_ context.Context, ref, method, head string) error {
	p := f.find(ref)
	if p == nil || p.state != forge.StateOpen {
		return fmt.Errorf("PR %s is not open", ref)
	}
	h, b := f.rev(p.head), f.rev(p.base)
	if head != "" && head != h {
		return fmt.Errorf("head branch was modified")
	}
	tree, err := gitx.Run(f.origin, "merge-tree", "--write-tree", b, h)
	if err != nil {
		return fmt.Errorf("not mergeable")
	}
	tree, _, _ = strings.Cut(tree, "\n")
	c, err := gitx.Run(f.origin, "commit-tree", tree, "-p", b, "-m", fmt.Sprintf("%s (#%d)", p.head, p.n))
	if err != nil {
		return err
	}
	if _, err := gitx.Run(f.origin, "update-ref", "refs/heads/"+p.base, c, b); err != nil {
		return err
	}
	p.state, p.mergedHeadAt = forge.StateMerged, h
	f.merges = append(f.merges, fmt.Sprintf("#%d %s", p.n, method))
	return nil
}

func (f *qForge) SetPRBase(_ context.Context, ref, base string) error {
	p := f.find(ref)
	if p == nil {
		return fmt.Errorf("no PR %s", ref)
	}
	p.base = base
	f.baseEdits = append(f.baseEdits, fmt.Sprintf("#%d %s", p.n, base))
	return nil
}

func (f *qForge) BranchChecks(context.Context, string) ([]forge.CheckRun, error) {
	return f.baseChecks, nil
}

// queueSetup is an App with a bare origin holding main, auto-merge on, and a
// queue over a fake forge on that origin.
func queueSetup(t *testing.T) (*App, *qForge, *MergeQueue) {
	t.Helper()
	a, _ := setup(t)
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, a.Root, "remote", "add", "origin", origin)
	git(t, a.Root, "push", "-q", "-u", "origin", a.Cfg.Base)
	a.Cfg.Train.AutoMerge = true
	f := &qForge{origin: origin}
	q := a.NewMergeQueue(f)
	return a, f, q
}

// prOpenTask spawns id (after deps), commits files on its branch, pushes it
// and opens its PR against its base: a task done published.
func prOpenTask(t *testing.T, a *App, f *qForge, id string, files map[string]string, after ...string) store.Task {
	t.Helper()
	tk, err := a.Spawn(SpawnReq{ID: id, Title: id + " work", After: after})
	must(t, err)
	commitFiles(t, tk.Worktree, id+" work", files)
	gitEnv(t, a.Root, []string{"SADDLE_TRAIN=1"}, "push", "-q", "origin", "refs/heads/"+tk.Branch+":refs/heads/"+tk.Branch)
	got, err := a.Store.Task(id)
	must(t, err)
	n := len(f.prs) + 1
	p := &qPR{n: n, url: fmt.Sprintf("https://github.com/o/r/pull/%d", n), head: tk.Branch, base: a.taskBase(got),
		state: forge.StateOpen, checks: "pass"}
	f.prs = append(f.prs, p)
	must(t, a.Store.SetField(id, "pr", p.url))
	must(t, a.Store.SetStatus(id, store.PROpen))
	got, err = a.Store.Task(id)
	must(t, err)
	return got
}

func tick(t *testing.T, q *MergeQueue) QueueTick {
	t.Helper()
	tk, err := q.Tick(t.Context())
	must(t, err)
	return tk
}

func taskStatus(t *testing.T, a *App, id string) string {
	t.Helper()
	tk, err := a.Store.Task(id)
	must(t, err)
	return tk.Status
}

// pushMain puts a commit on origin's main from outside saddle.
func pushMain(t *testing.T, a *App, f *qForge, file string) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "clone", "-q", f.origin, ".")
	write(t, dir, file, "news\n")
	commitAll(t, dir, "news "+file)
	git(t, dir, "push", "-q", "origin", "HEAD:main")
}

// #935: three independent green PRs merge in three ticks, one per tick, in
// age order, with no force-push: nothing moved main but the queue's own
// merges, and the repo doesn't require PRs up to date.
func TestQueueMergesThreeGreenPRsInThreeTicks(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		prOpenTask(t, a, f, id, map[string]string{id + ".txt": id + "\n"})
	}
	heads := map[string]string{}
	for _, p := range f.prs {
		heads[p.head] = f.rev(p.head)
	}
	for i, id := range []string{"t1", "t2", "t3"} {
		tk := tick(t, q)
		if tk.Applied == nil || tk.Applied.Kind != prgraph.ActMerge || tk.Applied.Change != id || tk.Result != "ok" {
			t.Fatalf("tick %d applied %v (%s), want merge(%s)", i+1, tk.Applied, tk.Result, id)
		}
		if s := taskStatus(t, a, id); s != store.Merged {
			t.Fatalf("%s is %s after its merge, want merged", id, s)
		}
	}
	if len(f.merges) != 3 || f.merges[0] != "#1 squash" {
		t.Fatalf("merges = %v", f.merges)
	}
	for _, p := range f.prs {
		if p.mergedHeadAt != heads[p.head] {
			t.Errorf("%s merged at %s, pushed at %s: the queue force-pushed it", p.head, short(p.mergedHeadAt), short(heads[p.head]))
		}
	}
	if hasEvent(t, a, EventQueueRebased, "") {
		t.Error("the queue rebased a PR though main never moved under it")
	}
	if tk := tick(t, q); tk.Applied != nil {
		t.Errorf("a fourth tick applied %v", tk.Applied)
	}
}

// #935: when main moved and the repo requires PRs up to date, the queue
// rebases the PR in its scratch worktree, force-pushes it, waits for CI,
// then merges. The task's live worktree is never touched.
func TestQueueRebasesBehindPRThenMergesWhenGreen(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	f.strict = true
	tk := prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	was := f.rev(tk.Branch)
	f.prs[0].checksAt = was
	pushMain(t, a, f, "news.txt")
	live := git(t, tk.Worktree, "rev-parse", "HEAD")

	got := tick(t, q)
	if got.Applied == nil || got.Applied.Kind != prgraph.ActRebase || got.Result != "ok" {
		t.Fatalf("tick applied %v (%s), want rebase(t1)", got.Applied, got.Result)
	}
	now := f.rev(tk.Branch)
	if now == was {
		t.Fatal("the branch wasn't force-pushed")
	}
	if _, err := gitx.Run(f.origin, "merge-base", "--is-ancestor", "main", now); err != nil {
		t.Fatal("the pushed head isn't on main's tip")
	}
	if out := git(t, f.origin, "log", "--format=%s", "main.."+tk.Branch); out != "t1 work" {
		t.Fatalf("main..%s = %q, want only its own commit", tk.Branch, out)
	}
	if h := git(t, tk.Worktree, "rev-parse", "HEAD"); h != live {
		t.Fatal("the queue moved the task's live worktree")
	}
	if _, err := os.Stat(a.queueScratch()); err == nil {
		t.Error("the scratch worktree was left behind")
	}
	// CI runs again at the new head: the queue waits.
	if got := tick(t, q); got.Applied != nil {
		t.Fatalf("tick applied %v while CI is pending", got.Applied)
	}
	f.prs[0].checksAt = now
	if got := tick(t, q); got.Applied == nil || got.Applied.Kind != prgraph.ActMerge || got.Result != "ok" {
		t.Fatalf("tick applied %v (%s), want the merge once green", got.Applied, got.Result)
	}
	if s := taskStatus(t, a, "t1"); s != store.Merged {
		t.Fatalf("t1 is %s, want merged", s)
	}
}

// #935: a red PR doesn't block a green one behind it.
func TestQueueRedPRDoesNotBlockGreen(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	prOpenTask(t, a, f, "t2", map[string]string{"t2.txt": "t2\n"})
	f.prs[0].checks = "fail"
	before, _ := a.Store.Tasks()
	got := tick(t, q)
	if got.Applied == nil || got.Applied.Kind != prgraph.ActMerge || got.Applied.Change != "t2" {
		t.Fatalf("tick applied %v, want merge(t2) past red t1", got.Applied)
	}
	if s := taskStatus(t, a, "t1"); s != store.PROpen {
		t.Fatalf("t1 is %s, want still pr_open", s)
	}
	if got := tick(t, q); got.Applied != nil {
		t.Fatalf("the queue acted on red t1: %v", got.Applied)
	}
	if after, _ := a.Store.Tasks(); len(after) != len(before) {
		t.Errorf("the queue spawned tasks for red CI: %d → %d", len(before), len(after))
	}
}

// #935: a PR stacked on its declared after is rebased --onto main from the
// old base tip once that after merges, force-pushed, retargeted to main,
// and then holds only its own commits.
func TestQueueRetargetsStackedPRAfterItsBaseMerges(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	t1 := prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	t2 := prOpenTask(t, a, f, "t2", map[string]string{"t2.txt": "t2\n"}, "t1")
	if t2.Base != t1.Branch || f.prs[1].base != t1.Branch {
		t.Fatalf("t2's base = %q, PR base %q; want %s", t2.Base, f.prs[1].base, t1.Branch)
	}
	// t2 waits on t1: the first tick merges t1 only.
	if got := tick(t, q); got.Applied == nil || got.Applied.Kind != prgraph.ActMerge || got.Applied.Change != "t1" {
		t.Fatalf("tick 1 applied %v, want merge(t1)", got.Applied)
	}
	got := tick(t, q)
	if got.Applied == nil || got.Applied.Kind != prgraph.ActMove || got.Applied.Change != "t2" || got.Result != "ok" {
		t.Fatalf("tick 2 applied %v (%s), want move(t2)", got.Applied, got.Result)
	}
	if f.prs[1].base != "main" || len(f.baseEdits) != 1 {
		t.Fatalf("t2's PR base = %s, edits %v; want one retarget to main", f.prs[1].base, f.baseEdits)
	}
	if out := git(t, f.origin, "log", "--format=%s", "main.."+t2.Branch); out != "t2 work" {
		t.Fatalf("main..%s = %q, want only t2's commit", t2.Branch, out)
	}
	if files := git(t, f.origin, "diff", "--name-only", "main..."+t2.Branch); files != "t2.txt" {
		t.Fatalf("t2's PR changes %q, want t2.txt only", files)
	}
	if tk, _ := a.Store.Task("t2"); tk.Base != "main" {
		t.Errorf("t2's stored base = %q, want main", tk.Base)
	}
	if got := tick(t, q); got.Applied == nil || got.Applied.Kind != prgraph.ActMerge || got.Applied.Change != "t2" {
		t.Fatalf("tick 3 applied %v, want merge(t2)", got.Applied)
	}
}

// #935: a conflicting rebase leaves the PR untouched, tells the owner once
// and the queue skips the task until its branch has a new head. Nothing is
// spawned.
func TestQueueConflictingRebaseLeavesPRUntouched(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	tk := prOpenTask(t, a, f, "t1", map[string]string{"README.md": "mine\n"})
	pushMain(t, a, f, "README.md") // main now edits the same line
	was := f.rev(tk.Branch)
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	tasks, _ := a.Store.Tasks()

	got := tick(t, q)
	if got.Applied == nil || got.Applied.Kind != prgraph.ActRebase || got.Result != "conflict" {
		t.Fatalf("tick applied %v (%s), want a conflicting rebase", got.Applied, got.Result)
	}
	if now := f.rev(tk.Branch); now != was {
		t.Fatal("the PR's branch moved on a conflict")
	}
	if len(f.baseEdits) != 0 || len(f.merges) != 0 {
		t.Fatalf("PR touched: edits %v merges %v", f.baseEdits, f.merges)
	}
	if !hasEvent(t, a, EventQueueFix, "head="+was) {
		t.Fatal("no fix-in-place mark on the head")
	}
	ns, _ := a.Store.PeekNotices(OrchestratorID, false)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "README.md") {
		t.Fatalf("notices = %+v, want one naming the conflict", ns)
	}
	if got := tick(t, q); got.Applied != nil {
		t.Fatalf("the queue retried the same head: %v", got.Applied)
	}
	if ns, _ := a.Store.PeekNotices(OrchestratorID, false); len(ns) != 1 {
		t.Fatalf("told the owner %d times", len(ns))
	}
	if after, _ := a.Store.Tasks(); len(after) != len(tasks) {
		t.Fatalf("the queue spawned tasks: %d → %d", len(tasks), len(after))
	}
	if _, err := os.Stat(filepath.Join(a.queueScratch(), ".git")); err == nil {
		t.Error("the scratch worktree was left mid-rebase")
	}
}

// #935: main red on its own checks pauses the queue with one notice; green
// again, it resumes and merges.
func TestQueuePausesWhileMainRedThenResumes(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	f.baseChecks = []forge.CheckRun{{Kind: forge.KindJob, Name: "ci", Status: "COMPLETED", Conclusion: "FAILURE"}}
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	if got := tick(t, q); got.Applied == nil || got.Applied.Kind != prgraph.ActEscalate {
		t.Fatalf("tick applied %v, want the pause notice", got.Applied)
	}
	for range 3 {
		if got := tick(t, q); got.Applied != nil {
			t.Fatalf("paused queue applied %v", got.Applied)
		}
	}
	ns, _ := a.Store.PeekNotices(OrchestratorID, false)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "paused") {
		t.Fatalf("notices = %+v, want one pause notice", ns)
	}
	if st, _ := automerge.Load(a.automergePath()); st.QueuePaused == "" {
		t.Fatal("the pause isn't recorded")
	}
	f.baseChecks[0].Conclusion = "SUCCESS"
	if got := tick(t, q); got.Applied == nil || got.Applied.Kind != prgraph.ActMerge {
		t.Fatalf("tick applied %v, want the merge once main is green", got.Applied)
	}
	if !hasEvent(t, a, EventQueueResumed, "") {
		t.Error("no resume event")
	}
	if st, _ := automerge.Load(a.automergePath()); st.QueuePaused != "" {
		t.Errorf("still paused: %q", st.QueuePaused)
	}
}

// #935: a failed merge goes through internal/retry: it backs off, the queue
// moves to the next PR meanwhile, and nothing latches.
func TestQueueMergeFailureBacksOff(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	prOpenTask(t, a, f, "t2", map[string]string{"t2.txt": "t2\n"})
	bad := &failMergeForge{qForge: f, fail: f.prs[0].url}
	q.F = bad
	if got := tick(t, q); got.Applied == nil || got.Applied.Change != "t1" || got.Result == "ok" {
		t.Fatalf("tick applied %v (%s), want t1's merge to fail", got.Applied, got.Result)
	}
	at, ok, err := a.Store.RetryAttempt("queue:merge:t1")
	if err != nil || !ok || !at.NextAt.After(time.Now()) {
		t.Fatalf("retry attempt = %+v, %v, %v; want a backoff", at, ok, err)
	}
	if got := tick(t, q); got.Applied == nil || got.Applied.Change != "t2" || got.Result != "ok" {
		t.Fatalf("tick applied %v (%s), want t2 merged while t1 backs off", got.Applied, got.Result)
	}
	q.Now = func() time.Time { return at.NextAt.Add(time.Second) }
	bad.fail = ""
	if got := tick(t, q); got.Applied == nil || got.Applied.Change != "t1" || got.Result != "ok" {
		t.Fatalf("tick applied %v (%s), want t1 retried and merged", got.Applied, got.Result)
	}
}

type failMergeForge struct {
	*qForge
	fail string
}

func (f *failMergeForge) MergePR(ctx context.Context, ref, method, head string) error {
	if ref == f.fail {
		return fmt.Errorf("merge refused: required review missing")
	}
	return f.qForge.MergePR(ctx, ref, method, head)
}

// #904: a rebase a killed process left half-done in the scratch worktree is
// aborted and the worktree cleaned before the queue uses it.
func TestQueueCleansHalfDoneRebase(t *testing.T) {
	t.Parallel()
	a, f, q := queueSetup(t)
	f.strict = true
	tk := prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	pushMain(t, a, f, "news.txt")
	// A queue process died mid-rebase in its scratch worktree.
	dir := a.queueScratch()
	must(t, os.MkdirAll(filepath.Dir(dir), 0o755))
	git(t, a.Root, "worktree", "add", "--detach", dir, tk.Branch)
	write(t, dir, "t1.txt", "other\n")
	commitAll(t, dir, "clash")
	git(t, dir, "checkout", "-q", "--detach", tk.Branch)
	write(t, dir, "t1.txt", "mine\n")
	commitAll(t, dir, "mine")
	cmdErr := func() error { _, err := gitx.Run(dir, "rebase", "HEAD@{2}"); return err }()
	if cmdErr == nil || !gitx.RebaseInProgress(dir) {
		t.Fatalf("could not leave a rebase half-done: %v", cmdErr)
	}
	if got := tick(t, q); got.Applied == nil || got.Result != "ok" {
		t.Fatalf("tick applied %v (%s), want the rebase to run", got.Applied, got.Result)
	}
	if !hasEvent(t, a, EventQueueCleaned, "") {
		t.Error("the half-done rebase wasn't reported as cleaned")
	}
}
