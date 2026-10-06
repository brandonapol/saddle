//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyInitDoctorFreshRepo: a fresh repo fails doctor only on what
// init fixes, and passes after saddle init with no hand edits.
func TestJourneyInitDoctorFreshRepo(t *testing.T) {
	w := world(t, Options{NoInit: true})

	r := w.Saddle("doctor", "--json")
	if r.Code == 0 {
		t.Fatalf("doctor passed before init: %s", r)
	}
	checks := doctorChecks(t, r.Stdout)
	for _, name := range []string{"ref guard hooks", ".saddle ignored", "state.db"} {
		if c := checks[name]; c.Status != "fail" || !strings.Contains(c.Fix, "saddle init") {
			t.Errorf("before init, %s = %+v, want a fail fixed by saddle init", name, c)
		}
	}
	for _, name := range []string{"git remote", "default branch", "gh auth", "merge settings", "tmux", "claude"} {
		if c := checks[name]; c.Status != "ok" {
			t.Errorf("before init, %s = %+v, want ok", name, c)
		}
	}

	r = w.MustSaddle("init", "--trust")
	if !strings.Contains(r.Stdout, "initialized "+w.Repo+"/.saddle") {
		t.Fatalf("init output: %s", r)
	}
	r = w.Saddle("doctor", "--json")
	if r.Code != 0 {
		t.Fatalf("doctor fails after init: %s", r)
	}
	for name, c := range doctorChecks(t, r.Stdout) {
		if c.Status == "fail" {
			t.Errorf("after init, %s failed: %+v", name, c)
		}
	}
	// Running init again is harmless.
	w.MustSaddle("init", "-q", "--trust")
	if r := w.Saddle("doctor"); r.Code != 0 {
		t.Fatalf("doctor after a second init: %s", r)
	}
}

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

func doctorChecks(t *testing.T, out string) map[string]doctorCheck {
	t.Helper()
	var v struct {
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	m := map[string]doctorCheck{}
	for _, c := range v.Checks {
		m[c.Name] = c
	}
	return m
}

// finished is a fake agent that writes one file under dir, commits and
// calls done.
func finished(dir, body string) []fa.Step {
	return []fa.Step{fa.Write(dir+"/work.txt", body), fa.Commit("work in " + dir), fa.Done("Adds " + dir + "/work.txt.")}
}

// landTwo spawns two agents on disjoint claims, waits for both to call
// done, lands them and opens their PRs. It returns the PR URLs by task.
func landTwo(t *testing.T, w *World) map[string]string {
	t.Helper()
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	for _, id := range []string{"t1", "t2"} {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	}
	r := w.MustSaddle("land")
	for _, id := range []string{"t1", "t2"} {
		if !strings.Contains(r.Stdout, id) || w.Task(id).Status != "landed" {
			t.Fatalf("%s didn't land: %s", id, r)
		}
	}
	integ := w.Git(w.Repo, "ls-tree", "-r", "--name-only", "saddle/integration")
	if !strings.Contains(integ, "alpha/work.txt") || !strings.Contains(integ, "beta/work.txt") {
		t.Fatalf("integration lacks the work:\n%s", integ)
	}
	r = w.MustSaddle("prs")
	urls := map[string]string{}
	for _, id := range []string{"t1", "t2"} {
		urls[id] = w.Task(id).PR
		if urls[id] == "" || !strings.Contains(r.Stdout, urls[id]) {
			t.Fatalf("%s has no PR: %s", id, r)
		}
	}
	return urls
}

func prNumber(t *testing.T, s *fakegh.State, url string) *fakegh.PR {
	t.Helper()
	for _, p := range s.PRs {
		if s.PRURL(p.Number) == url {
			return p
		}
	}
	t.Fatalf("no PR %s in the fake GitHub", url)
	return nil
}

// TestJourneyParallelLandPRsAutomerge: two agents work in parallel on
// disjoint claims; the train lands both, prs opens a two-layer stack, and
// auto-merge waits for CI, then merges it bottom-up into main, restacking
// and retargeting the top PR after the bottom one merges.
func TestJourneyParallelLandPRsAutomerge(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	must(t, w.GH.Update(func(s *fakegh.State) error {
		s.DefaultChecks = []fakegh.Check{{Name: "ci", Workflow: "CI", State: fakegh.Pending}}
		return nil
	}))
	urls := landTwo(t, w)

	s := w.GHState()
	low, high := prNumber(t, s, urls["t1"]), prNumber(t, s, urls["t2"])
	if low.Base != "main" || high.Base != low.Head {
		t.Fatalf("stack bases: %s on %s, %s on %s; want t2 stacked on t1 on main", low.Head, low.Base, high.Head, high.Base)
	}
	if !strings.Contains(high.Body, "Stack") {
		t.Fatalf("top PR body lacks the stack list:\n%s", high.Body)
	}

	// Off by default: a tick merges nothing.
	if st := w.AutomergeTick(); st.Enabled || st.Merged != "" {
		t.Fatalf("auto-merge acted while off: %+v", st)
	}
	w.MustSaddle("automerge", "on")
	st := w.AutomergeTick()
	if st.Merged != "" || len(st.Stacks) != 1 || !strings.Contains(st.Stacks[0].Why, "pending") {
		t.Fatalf("merged on pending CI: %+v", st)
	}
	must(t, w.GH.SetAllChecks(fakegh.Pass))

	st = w.AutomergeTick()
	if st.Merged != urls["t1"] {
		t.Fatalf("first tick merged %q, want t1's %s: %+v", st.Merged, urls["t1"], st)
	}
	s = w.GHState()
	if p := prNumber(t, s, urls["t2"]); p.Base != "main" {
		t.Fatalf("after t1 merged, t2's PR targets %s, want main (restack retargets it)", p.Base)
	}
	if p := prNumber(t, s, urls["t1"]); p.State != "MERGED" || p.MergedBy != "squash" {
		t.Fatalf("t1's PR = %+v", p)
	}
	// t2's branch was rebuilt on the new main: its PR has only its own change.
	if files := w.Git(w.Repo, "diff", "--name-only", "origin/main...origin/saddle/t2-beta-work"); files != "beta/work.txt" {
		t.Fatalf("t2's PR diff after restack = %q", files)
	}

	st = w.AutomergeTick()
	if st.Merged != urls["t2"] {
		t.Fatalf("second tick merged %q, want t2's %s: %+v", st.Merged, urls["t2"], st)
	}
	if w.OriginFile("main", "alpha/work.txt") != "alpha\n" || w.OriginFile("main", "beta/work.txt") != "beta\n" {
		t.Fatal("main lacks the merged work")
	}
	if n := w.Git(w.Repo, "rev-list", "--count", "origin/main"); n != "3" {
		t.Fatalf("main has %s commits, want 3 (initial + two squashes)", n)
	}
	if st := w.AutomergeTick(); st.Merged != "" || len(st.Stacks) != 0 {
		t.Fatalf("stack not empty after both merged: %+v", st)
	}
}

// conflictSetup makes t2's branch conflict with t1's landed work the way it
// happens in practice: t2 was cut before t1 landed, and only edits the
// shared file once t1's claim is gone, without syncing first. (With the
// default auto-rebase the train would move t2's clean worktree onto t1
// first, so these journeys turn it off.)
func conflictSetup(t *testing.T, w *World, t2 ...fa.Step) {
	t.Helper()
	w.Spawn("t1", "First edit", []string{"alpha/**"},
		fa.Write("shared.txt", "one\n"), fa.Commit("one"), fa.Done("One."))
	steps := append([]fa.Step{fa.Wait("proceed-now"),
		fa.Write("shared.txt", "two\n"), fa.Commit("two"), fa.Done("Two.")}, t2...)
	w.Spawn("t2", "Second edit", []string{"beta/**"}, steps...)
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t1").Status != "landed" {
		t.Fatalf("t1 didn't land: %s", r)
	}
	w.MustSaddle("message", "t2", "proceed-now")
	w.WaitTask("t2", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
}

// TestJourneyConflictReturnedToProducer: t2's branch conflicts with landed
// work. The train hands the conflict back to t2, not the human: its window
// is woken, its hook delivers the conflict, and it syncs, resolves, calls
// done again and lands.
func TestJourneyConflictReturnedToProducer(t *testing.T) {
	w := world(t, Options{Tables: "[train]\nno_auto_rebase = true\n"})
	conflictSetup(t, w,
		fa.Wait("could not land"),
		fa.Step{Run: "saddle sync", Optional: true}, // stops at the conflict, exits non-zero
		fa.Resolve("shared.txt", "one\ntwo\n"),
		fa.Done("Two, merged with one."))

	r := w.Saddle("land")
	if v := w.Task("t2"); v.Status != "conflict" || strings.HasPrefix(v.Train, "landed") {
		t.Fatalf("t2 after a conflicting land = %+v\n%s", v, r)
	}
	w.WaitAgentLog("t2", "input: [saddle] You have new notices")
	w.WaitAgentLog("t2", "could not land")
	w.WaitAgentLog("t2", "idle: script finished")
	w.WaitTask("t2", "queued again", func(v mcpserver.TaskView) bool { return v.Train == "queued" })

	r = w.MustSaddle("land")
	if w.Task("t2").Status != "landed" {
		t.Fatalf("t2 didn't land after resolving: %s", r)
	}
	if got := w.Git(w.Repo, "show", "saddle/integration:shared.txt"); got != "one\ntwo" {
		t.Fatalf("integration shared.txt = %q", got)
	}
}

// TestJourneyConflictEscalatesAfterMaxAttempts: a producer that keeps
// handing back the same conflict is escalated to the owner after
// train.max_attempts, and told to stop instead of being handed it again.
func TestJourneyConflictEscalatesAfterMaxAttempts(t *testing.T) {
	w := world(t, Options{Tables: "[train]\nno_auto_rebase = true\nmax_attempts = 2\n"})
	conflictSetup(t, w,
		fa.Wait("could not land"),
		fa.Done("Trying again without fixing it."),
		fa.Wait("Stop retrying"),
		fa.Wait("Message from the user: fix it like this"))

	w.Saddle("land")
	w.WaitAgentLog("t2", "done ok")
	w.WaitTask("t2", "queued again", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	r := w.Saddle("land")
	if !strings.Contains(r.Stdout, "escalated") {
		t.Fatalf("second failure wasn't escalated: %s", r)
	}
	v := w.Task("t2")
	if v.Status != "needs_you" || !strings.HasPrefix(v.Train, "escalated") {
		t.Fatalf("t2 = %+v, want needs_you and escalated", v)
	}
	if n, err := w.App().Store.PendingNotices("t0"); err != nil || n == 0 {
		t.Fatalf("the orchestrator has no notice of the escalation (%d, %v)", n, err)
	}
	// The producer was told to stop, without being woken for it.
	if strings.Contains(w.AgentLog("t2"), "Stop retrying") {
		t.Fatal("the escalation woke the producer")
	}
}

// TestJourneyMessageReachesEscalatedTask: after an escalation the owner is
// told to "tell t2 what to do"; that message must wake t2.
func TestJourneyMessageReachesEscalatedTask(t *testing.T) {
	w := world(t, Options{Tables: "[train]\nno_auto_rebase = true\nmax_attempts = 1\n"})
	conflictSetup(t, w, fa.Wait("Message from the user: fix it like this"))
	if r := w.Saddle("land"); !strings.Contains(r.Stdout, "escalated") {
		t.Fatalf("not escalated: %s", r)
	}
	w.MustSaddle("message", "t2", "fix it like this")
	w.WaitAgentLog("t2", "idle: script finished")
	// Escalation doesn't wake t2, so its notice arrives with the message, in
	// one delivery.
	for _, line := range strings.Split(w.AgentLog("t2"), "\n") {
		if strings.Contains(line, "Stop retrying") && strings.Contains(line, "Message from the user: fix it like this") {
			return
		}
	}
	t.Fatalf("no single delivery carried both the escalation and the message:\n%s", w.AgentLog("t2"))
}

// TestJourneyRestackAfterHumanSquashMerge: a person squash-merges the
// bottom PR of a stack in the GitHub UI. Restack sees it merged, rebuilds
// the rest of the stack on the new main and retargets the next PR to main,
// and prs never reopens the merged one.
func TestJourneyRestackAfterHumanSquashMerge(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	urls := landTwo(t, w)
	low := prNumber(t, w.GHState(), urls["t1"])
	must(t, w.GH.MergeByHand(low.Number, "squash"))

	out := w.MustMCP("t0", "restack", nil)
	if !strings.Contains(out, "retargeted") {
		t.Fatalf("restack: %s", out)
	}
	s := w.GHState()
	high := prNumber(t, s, urls["t2"])
	if high.Base != "main" {
		t.Fatalf("t2's PR targets %s after restack, want main", high.Base)
	}
	if files := w.Git(w.Repo, "diff", "--name-only", "origin/main...origin/"+high.Head); files != "beta/work.txt" {
		t.Fatalf("t2's PR diff = %q, want only its own file", files)
	}
	if mb, main := w.Git(w.Repo, "merge-base", "origin/main", "origin/"+high.Head), w.Git(w.Repo, "rev-parse", "origin/main"); mb != main {
		t.Fatal("t2's branch isn't on the new main")
	}
	w.MustSaddle("prs")
	if s := w.GHState(); len(s.PRs) != 2 || len(s.Open()) != 1 {
		t.Fatalf("prs after the merge: %d PRs, %d open; want the merged one left alone", len(s.PRs), len(s.Open()))
	}
	must(t, w.GH.MergeByHand(high.Number, "squash"))
	if w.OriginFile("main", "alpha/work.txt") == "" || w.OriginFile("main", "beta/work.txt") == "" {
		t.Fatal("main lacks the stack's work")
	}
}

// TestJourneyUnstackAndRequeue: the escape hatches. unstack takes a landed
// task out of the stack for good: restack drops its commits from
// integration and prs stops publishing it. requeue puts it back in the
// train, and it lands and gets a PR again.
func TestJourneyUnstackAndRequeue(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	urls := landTwo(t, w)

	r := w.MustSaddle("unstack", "t1")
	if !strings.Contains(r.Stdout, "t1 is out of the PR stack") {
		t.Fatalf("unstack: %s", r)
	}
	if v := w.Task("t1"); !strings.HasPrefix(v.Train, "superseded") {
		t.Fatalf("t1 after unstack = %+v", v)
	}
	w.MustMCP("t0", "restack", nil)
	if files := w.Git(w.Repo, "ls-tree", "-r", "--name-only", "saddle/integration"); strings.Contains(files, "alpha/") {
		t.Fatalf("integration still has t1's work after restack:\n%s", files)
	}
	// unstack's own sentinel check flagged t2 (it still held t1's commit);
	// restack doesn't re-check, so check now (#190, TestJourneyPRsRightAfterUnstackRestack).
	if r := w.MustSaddle("sentinel", "check"); !strings.Contains(r.Stdout, "checks clean") {
		t.Fatalf("stack not clean after restack: %s", r)
	}
	w.MustSaddle("prs")
	s := w.GHState()
	if p := prNumber(t, s, urls["t2"]); p.Base != "main" {
		t.Fatalf("t2's PR targets %s, want main once t1 left the stack", p.Base)
	}

	r = w.MustSaddle("requeue", "t1")
	if !strings.Contains(r.Stdout, "queued again") {
		t.Fatalf("requeue: %s", r)
	}
	w.MustSaddle("land")
	if v := w.Task("t1"); v.Status != "landed" || !strings.HasPrefix(v.Train, "landed") {
		t.Fatalf("t1 after requeue and land = %+v", v)
	}
	if files := w.Git(w.Repo, "ls-tree", "-r", "--name-only", "saddle/integration"); !strings.Contains(files, "alpha/work.txt") {
		t.Fatalf("integration lacks t1's work after requeue:\n%s", files)
	}
	r = w.MustSaddle("prs")
	if !strings.Contains(r.Stdout, w.Task("t1").PR) || w.Task("t1").PR == "" {
		t.Fatalf("t1 isn't published again: %s", r)
	}
	if !strings.Contains(w.OriginFile(w.Task("t1").Branch, "alpha/work.txt"), "alpha") {
		t.Fatal("t1's branch on origin lacks its work after requeue")
	}
}

// TestJourneyPRsRightAfterUnstackRestack: unstack says "run restack to drop
// their commits"; once restack has, prs must work without waiting for the
// next sentinel cycle.
func TestJourneyPRsRightAfterUnstackRestack(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	landTwo(t, w)
	w.MustSaddle("unstack", "t1")
	w.MustMCP("t0", "restack", nil)
	if r := w.Saddle("prs"); r.Code != 0 {
		t.Fatalf("prs after unstack and restack: %s", r)
	}
}
