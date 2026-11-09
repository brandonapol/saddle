package app

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/store"
)

// Fix in place (ADR 0012 rule 6, #936): a conflict or red CI on a pr_open
// task's PR reopens that same task on its own branch. Against the queue's
// in-memory forge over a real bare origin.

func (f *qForge) AddLabel(_ context.Context, ref, label string) error {
	p := f.find(ref)
	if p == nil {
		return fmt.Errorf("no PR %s", ref)
	}
	if !slices.Contains(p.labels, label) {
		p.labels = append(p.labels, label)
	}
	return nil
}

func (f *qForge) RemoveLabel(_ context.Context, ref, label string) error {
	p := f.find(ref)
	if p == nil {
		return fmt.Errorf("no PR %s", ref)
	}
	p.labels = slices.DeleteFunc(p.labels, func(l string) bool { return l == label })
	return nil
}

func taskRow(t *testing.T, a *App, id string) store.Task {
	t.Helper()
	tk, err := a.Store.Task(id)
	must(t, err)
	return tk
}

// agentGone closes id's window: its agent exited.
func agentGone(t *testing.T, a *App, ft *fakeTmux, id string) {
	t.Helper()
	must(t, ft.KillWindow(taskRow(t, a, id).Window))
}

// fixAndPublish stands in for the fix agent's done: a commit on the task's
// branch, pushed to the same PR, and the task back to pr_open.
func fixAndPublish(t *testing.T, a *App, id, file string) {
	t.Helper()
	tk := taskRow(t, a, id)
	commitFiles(t, tk.Worktree, "fix "+file, map[string]string{file: "fix " + time.Now().String() + "\n"})
	gitEnv(t, a.Root, []string{"SADDLE_TRAIN=1"}, "push", "-q", "-f", "origin", "refs/heads/"+tk.Branch+":refs/heads/"+tk.Branch)
	must(t, a.Store.SetStatus(id, store.PROpen))
}

func fixQueueSetup(t *testing.T) (*App, *fakeTmux, *qForge, *MergeQueue) {
	t.Helper()
	a, f, q := queueSetup(t)
	return a, a.Tmux.(*fakeTmux), f, q
}

// #936: a red PR whose agent is gone restarts the same task id on the same
// branch, in its worktree; the PR number doesn't change and no task is
// created.
func TestFixRedPRAgentGoneRestartsSameTask(t *testing.T) {
	t.Parallel()
	a, ft, f, q := fixQueueSetup(t)
	tk := prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	f.prs[0].checks = "fail"
	agentGone(t, a, ft, "t1")
	before, _ := a.Store.Tasks()

	tick(t, q)
	got := taskRow(t, a, "t1")
	if got.Status != store.Fixing {
		t.Fatalf("t1 is %s, want fixing", got.Status)
	}
	if !strings.HasPrefix(got.FixReason, "ci: ci failed") || !strings.Contains(got.FixReason, f.prs[0].url+"/checks") {
		t.Fatalf("fix reason = %q, want the failed check and its link", got.FixReason)
	}
	if got.FixAttempts != 1 {
		t.Fatalf("fix attempts = %d, want 1", got.FixAttempts)
	}
	if got.Branch != tk.Branch || got.Worktree != tk.Worktree || got.PR != tk.PR {
		t.Fatalf("t1 moved: branch %s worktree %s PR %s", got.Branch, got.Worktree, got.PR)
	}
	if got.Window == tk.Window || !ft.Alive(got.Window) || !strings.HasPrefix(ft.names[got.Window], "t1-") {
		t.Fatalf("no new window for t1: %q (was %q)", got.Window, tk.Window)
	}
	if !hasEvent(t, a, EventFixStarted, "") {
		t.Fatal("no fix_started event")
	}
	if after, _ := a.Store.Tasks(); len(after) != len(before) {
		t.Fatalf("tasks %d → %d: a fix must never create a task", len(before), len(after))
	}
	for _, x := range func() []store.Task { ts, _ := a.Store.Tasks(); return ts }() {
		if strings.HasPrefix(x.Title, "Repair ") {
			t.Fatalf("a repair task exists: %s %q", x.ID, x.Title)
		}
	}
	if len(f.prs) != 1 || f.prs[0].n != 1 || f.prs[0].head != tk.Branch {
		t.Fatalf("PRs = %+v, want the one PR #1 on %s", f.prs, tk.Branch)
	}
	// A fixing task is out of the queue: nothing more happens until done.
	if tk := tick(t, q); tk.Applied != nil {
		t.Fatalf("the queue acted on a fixing task: %v", tk.Applied)
	}
	if n := taskRow(t, a, "t1").FixAttempts; n != 1 {
		t.Fatalf("a live fix agent was restarted: attempts %d", n)
	}
}

// #936: a conflict on the queue's rebase makes the task fixing with a
// reason naming the files, and its live agent is told.
func TestFixQueueConflictTellsLiveAgent(t *testing.T) {
	t.Parallel()
	a, ft, f, q := fixQueueSetup(t)
	tk := prOpenTask(t, a, f, "t1", map[string]string{"README.md": "mine\n"})
	pushMain(t, a, f, "README.md")
	_, _ = a.Store.TakeNotices("t1", false)
	windows := len(ft.windows)

	got := tick(t, q)
	if got.Result != "conflict" {
		t.Fatalf("tick = %v (%s), want a conflicting rebase", got.Applied, got.Result)
	}
	row := taskRow(t, a, "t1")
	if row.Status != store.Fixing || row.FixReason != "conflict: README.md" {
		t.Fatalf("t1 is %s (%q), want fixing with conflict: README.md", row.Status, row.FixReason)
	}
	ns, _ := a.Store.PeekNotices("t1", true)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "README.md") || !strings.Contains(ns[0].Text, tk.PR) || !strings.Contains(ns[0].Text, "saddle sync") {
		t.Fatalf("t1's notices = %+v, want the conflict, its PR and saddle sync", ns)
	}
	if !hasEvent(t, a, EventFixTold, "") || hasEvent(t, a, EventFixStarted, "") {
		t.Fatal("the live agent should be told, not replaced")
	}
	if len(ft.windows) != windows || row.Window != tk.Window {
		t.Fatalf("a window was opened for a live agent")
	}
	// The orchestrator hears in its digest: nothing for it to do.
	if !hasEvent(t, a, EventNoticeDigest, "README.md") {
		t.Fatal("the orchestrator's digest doesn't name the conflict")
	}
}

// #936: after [merge] fix_attempts fixes that stayed red the PR is labeled
// needs-human, the owner is told once and the task is gave_up. No task is
// created; removing the label sends it back to pr_open with the count reset.
func TestFixGivesUpAfterAttemptsThenLabelRemovalRetries(t *testing.T) {
	t.Parallel()
	a, _, f, q := fixQueueSetup(t)
	var owner []string
	a.OwnerNotify = func(s string) { owner = append(owner, s) }
	prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	f.prs[0].checks = "fail" // red at every head
	before, _ := a.Store.Tasks()

	for i := 1; i <= 2; i++ {
		tick(t, q)
		row := taskRow(t, a, "t1")
		if row.Status != store.Fixing || row.FixAttempts != i {
			t.Fatalf("round %d: t1 is %s with %d attempts", i, row.Status, row.FixAttempts)
		}
		fixAndPublish(t, a, "t1", fmt.Sprintf("fix%d.txt", i))
	}
	tick(t, q)
	row := taskRow(t, a, "t1")
	if row.Status != store.GaveUp {
		t.Fatalf("t1 is %s after 2 failed fixes, want gave_up", row.Status)
	}
	if !slices.Contains(f.prs[0].labels, automerge.NeedsHuman) {
		t.Fatalf("PR labels = %v, want needs-human", f.prs[0].labels)
	}
	if len(owner) != 1 || !strings.Contains(owner[0], automerge.NeedsHuman) {
		t.Fatalf("owner told %d times: %q", len(owner), owner)
	}
	if after, _ := a.Store.Tasks(); len(after) != len(before) {
		t.Fatalf("tasks %d → %d after giving up", len(before), len(after))
	}
	// Terminal until retry: more ticks change nothing and say nothing.
	tick(t, q)
	if row := taskRow(t, a, "t1"); row.Status != store.GaveUp || len(owner) != 1 {
		t.Fatalf("t1 is %s, owner told %d times", row.Status, len(owner))
	}

	// Anyone removes the label: back to pr_open, count reset.
	f.prs[0].labels = nil
	tick(t, q)
	row = taskRow(t, a, "t1")
	if row.Status == store.GaveUp || row.FixAttempts > 1 {
		t.Fatalf("after the label went: t1 is %s with %d attempts", row.Status, row.FixAttempts)
	}
	if !hasEvent(t, a, EventFixRetry, "label was removed") {
		t.Fatal("no fix_retry event for the removed label")
	}
}

// #936: saddle retry on a gave_up task removes needs-human, resets its fix
// count and puts it back to pr_open.
func TestRetryResetsGaveUpTask(t *testing.T) {
	t.Parallel()
	a, _, f, q := fixQueueSetup(t)
	a.Cfg.Merge.FixAttempts = 1
	a.OwnerNotify = func(string) {}
	prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	f.prs[0].checks = "fail"
	tick(t, q)
	fixAndPublish(t, a, "t1", "fix.txt")
	tick(t, q)
	if row := taskRow(t, a, "t1"); row.Status != store.GaveUp {
		t.Fatalf("t1 is %s, want gave_up", row.Status)
	}
	must(t, a.retry("t1", f))
	row := taskRow(t, a, "t1")
	if row.Status != store.PROpen || row.FixAttempts != 0 {
		t.Fatalf("after retry t1 is %s with %d attempts, want pr_open with 0", row.Status, row.FixAttempts)
	}
	if slices.Contains(f.prs[0].labels, automerge.NeedsHuman) {
		t.Fatal("retry left the needs-human label")
	}
	if err := a.retry("t9", f); err == nil {
		t.Fatal("retry of a missing task succeeded")
	}
}

// #936: a PR closed without merging kills its task with killed_by
// pr_closed; its worktree and branch stay.
func TestFixClosedPRKillsTask(t *testing.T) {
	t.Parallel()
	a, _, f, q := fixQueueSetup(t)
	tk := prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	f.prs[0].state = forge.StateClosed
	tick(t, q)
	row := taskRow(t, a, "t1")
	if row.Status != store.Killed || row.KilledBy != store.KilledByPRClosed {
		t.Fatalf("t1 is %s killed_by %q, want killed by pr_closed", row.Status, row.KilledBy)
	}
	if _, err := os.Stat(tk.Worktree); err != nil {
		t.Fatalf("worktree removed: %v", err)
	}
	if out := git(t, a.Root, "branch", "--list", tk.Branch); out == "" {
		t.Fatal("branch removed")
	}
}

// #936: before the fix agent starts, a local branch the queue's rebase left
// stale adopts the PR's remote head, so a later done can't push the old
// head back.
func TestFixAdoptsRemoteHeadBeforeAgentStarts(t *testing.T) {
	t.Parallel()
	a, ft, f, q := fixQueueSetup(t)
	f.strict = true
	tk := prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	pushMain(t, a, f, "news.txt")
	if got := tick(t, q); got.Result != "ok" {
		t.Fatalf("queue rebase: %v (%s)", got.Applied, got.Result)
	}
	remote := f.rev(tk.Branch)
	if local := git(t, tk.Worktree, "rev-parse", "HEAD"); local == remote {
		t.Fatal("setup: the local branch should be stale after the queue's rebase")
	}
	f.prs[0].checks, f.prs[0].checksAt = "fail", ""
	agentGone(t, a, ft, "t1")

	tick(t, q)
	if row := taskRow(t, a, "t1"); row.Status != store.Fixing {
		t.Fatalf("t1 is %s, want fixing", row.Status)
	}
	if local := git(t, tk.Worktree, "rev-parse", "HEAD"); local != remote {
		t.Fatalf("local head %s, want the PR's head %s", short(local), short(remote))
	}
	evs, _ := a.Store.EventsOfKinds(EventFixAdopted, EventFixStarted)
	if len(evs) != 2 || evs[0].Kind != EventFixAdopted || evs[1].Kind != EventFixStarted {
		t.Fatalf("events = %+v, want the adoption before the agent started", evs)
	}
}

// #936: the fix count resets once the PR goes green.
func TestFixCountResetsWhenGreen(t *testing.T) {
	t.Parallel()
	a, _, f, q := fixQueueSetup(t)
	a.Cfg.Train.AutoMerge = false // keep it open while green
	prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	f.prs[0].checks = "fail"
	tick(t, q)
	fixAndPublish(t, a, "t1", "fix.txt")
	f.prs[0].checks = "pass"
	tick(t, q)
	if row := taskRow(t, a, "t1"); row.Status != store.PROpen || row.FixAttempts != 0 {
		t.Fatalf("t1 is %s with %d attempts, want pr_open with 0", row.Status, row.FixAttempts)
	}
}

// #936: while main is red no fix starts; the queue pauses instead.
func TestFixNoneWhileMainRed(t *testing.T) {
	t.Parallel()
	a, _, f, q := fixQueueSetup(t)
	prOpenTask(t, a, f, "t1", map[string]string{"t1.txt": "t1\n"})
	f.prs[0].checks = "fail"
	f.baseChecks = []forge.CheckRun{{Kind: forge.KindJob, Name: "ci", Status: "COMPLETED", Conclusion: "FAILURE"}}
	tick(t, q)
	tick(t, q)
	if row := taskRow(t, a, "t1"); row.Status != store.PROpen || row.FixAttempts != 0 {
		t.Fatalf("t1 is %s with %d attempts while main is red", row.Status, row.FixAttempts)
	}
}

// #936: with merging off the queue never rebases, so a PR the forge says
// conflicts is checked with git and reopened with the files.
func TestFixForgeConflictWithMergingOff(t *testing.T) {
	t.Parallel()
	a, _, f, q := fixQueueSetup(t)
	a.Cfg.Train.AutoMerge = false
	prOpenTask(t, a, f, "t1", map[string]string{"README.md": "mine\n"})
	pushMain(t, a, f, "README.md")
	tick(t, q)
	if row := taskRow(t, a, "t1"); row.Status != store.Fixing || row.FixReason != "conflict: README.md" {
		t.Fatalf("t1 is %s (%q), want fixing with conflict: README.md", row.Status, row.FixReason)
	}
}

// #936: with legacy_train off nothing spawns a repair: a repair spawn is
// refused, none is pending or reserves files, and the CI-red path refuses.
func TestNoRepairsWithLegacyTrainOff(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	orig, err := a.Spawn(SpawnReq{ID: "t1", Title: "work"})
	must(t, err)
	if _, err := a.Spawn(SpawnReq{Title: repairTitlePrefix("t1") + "work", For: "t1"}); err == nil || !strings.Contains(err.Error(), "fix in place") {
		t.Fatalf("repair spawn = %v, want refused", err)
	}
	if _, err := a.Spawn(SpawnReq{Title: ciFixTitle(orig)}); err == nil {
		t.Fatal("a CI fix task spawned")
	}
	if _, _, err := a.ensureRepair(orig, repairSeed{}, true); err == nil {
		t.Fatal("ensureRepair spawned with legacy_train off")
	}
	if _, err := a.CIRedRepair("t1", nil); err == nil {
		t.Fatal("CIRedRepair ran with legacy_train off")
	}
	if got := a.spawnRepairs(); got != nil {
		t.Fatalf("spawnRepairs = %v", got)
	}
	if ps := a.PendingRepairs(); len(ps) != 0 {
		t.Fatalf("pending repairs = %+v", ps)
	}
	ts, _ := a.Store.Tasks()
	for _, x := range ts {
		if strings.HasPrefix(x.Title, "Repair ") {
			t.Fatalf("task %s %q is a repair", x.ID, x.Title)
		}
	}
}
