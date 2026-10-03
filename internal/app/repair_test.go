package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// repairTasks lists the repair tasks spawned for orig.
func repairTasks(t *testing.T, a *App, orig string) []store.Task {
	t.Helper()
	ts, err := a.Store.Tasks()
	must(t, err)
	var out []store.Task
	for _, x := range ts {
		if strings.HasPrefix(x.Title, repairTitlePrefix(orig)) {
			out = append(out, x)
		}
	}
	return out
}

// orchNotices takes the orchestrator's notices that contain s.
func orchNotices(t *testing.T, a *App, s string) []string {
	t.Helper()
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	var out []string
	for _, n := range ns {
		if strings.Contains(n.Text, s) {
			out = append(out, n.Text)
		}
	}
	return out
}

// #172: t1 landed and its window closed (close_on_land), then main moved
// under its landed commit. Restack can't hand the conflict to t1, so it
// takes t1 out of the replay, finishes the rest of the restack, and spawns
// exactly one repair task seeded with t1's branch, commits, the conflicting
// files, the base and t1's prompt and summary. A second restack spawns no
// second repair.
func TestRestackOrphanConflictSpawnsOneRepair(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1, err := a.Spawn(SpawnReq{ID: "t1", Title: "one", Prompt: "Make README say one.", Claims: []string{"docs/**"}})
	must(t, err)
	write(t, t1.Worktree, "README.md", "one\n")
	commitAll(t, t1.Worktree, "one")
	must(t, a.Done(t1.ID, "README now says one."))
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	if a.ownWindow(t1) {
		t.Fatal("t1's window should be gone after landing")
	}
	base := moveMain(t, origin, "README.md")
	_ = orchNotices(t, a, "")

	res, err := a.Restack()
	if err != nil {
		t.Fatalf("restack with an orphaned conflict: %v", err)
	}
	if got := trainState(t, a, t1.ID); got != TrainRepairing {
		t.Fatalf("t1 train state = %s, want %s", got, TrainRepairing)
	}
	if !slices.Contains(res.Repairing, t1.ID) {
		t.Fatalf("restack result = %+v, want t1 repairing", res)
	}
	integ := a.Cfg.Integration
	if mb := git(t, a.Root, "merge-base", "origin/main", integ); mb != base {
		t.Fatal("integration isn't on the new main")
	}
	if got := git(t, a.Root, "show", integ+":README.md"); got != "main" {
		t.Fatalf("integration README = %q, want main's (t1's commit dropped until its repair lands)", got)
	}
	if got := git(t, a.Root, "show", t2.Branch+":two.txt"); got != "two" {
		t.Fatal("t2 wasn't restacked")
	}
	rs := repairTasks(t, a, t1.ID)
	if len(rs) != 1 {
		t.Fatalf("repair tasks = %+v, want one", rs)
	}
	r := rs[0]
	for _, want := range []string{t1.Branch, "README.md", short(base), "Make README say one.", "README now says one.", "cherry-pick"} {
		if !strings.Contains(r.Prompt, want) {
			t.Fatalf("repair prompt lacks %q:\n%s", want, r.Prompt)
		}
	}
	cl, _ := a.Store.Claims()
	if !slices.Contains(cl[r.ID], "README.md") || !slices.Contains(cl[r.ID], "docs/**") {
		t.Fatalf("repair claims = %v, want the conflicting file and t1's claims", cl[r.ID])
	}
	if ns, _ := a.Store.TakeNotices(t1.ID, false); len(ns) != 0 {
		t.Fatalf("the dead t1 was handed the conflict: %+v", ns)
	}
	if got := orchNotices(t, a, r.ID); len(got) != 1 {
		t.Fatalf("orchestrator notices about the repair = %q, want one", got)
	}

	if _, err := a.Restack(); err != nil {
		t.Fatalf("second restack: %v", err)
	}
	if rs := repairTasks(t, a, t1.ID); len(rs) != 1 {
		t.Fatalf("second restack spawned another repair: %+v", rs)
	}
	if got := orchNotices(t, a, "Repair"); len(got) != 0 {
		t.Fatalf("second restack notified again: %q", got)
	}
}

// When the repair lands, the original leaves the stack as superseded and
// its PR is closed with a comment naming the repair.
func TestRepairLandSupersedesOriginal(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"README.md": "one\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	moveMain(t, origin, "README.md")
	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	rs := repairTasks(t, a, t1.ID)
	if len(rs) != 1 {
		t.Fatalf("repair tasks = %+v", rs)
	}
	r := rs[0]
	write(t, r.Worktree, "README.md", "main\none\n")
	commitAll(t, r.Worktree, "one, on the new main")
	must(t, a.Done(r.ID, "Re-lands t1 on main."))
	out, err := a.Land()
	if err != nil || len(out) != 1 || out[0].State != store.TrainOK {
		t.Fatalf("repair land = %+v, %v", out, err)
	}
	if got := trainState(t, a, t1.ID); got != TrainSuperseded {
		t.Fatalf("t1 after its repair landed = %s, want superseded", got)
	}
	calls := prCalls(ghLog(), t1.PR)
	if !slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "pr close") }) ||
		!slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "pr comment") && strings.Contains(c, r.ID) }) {
		t.Fatalf("t1's PR wasn't closed with a comment naming %s: %v", r.ID, calls)
	}
	if _, err := a.Restack(); err != nil {
		t.Fatalf("restack after the repair: %v", err)
	}
	if got := git(t, a.Root, "show", a.Cfg.Integration+":README.md"); got != "main\none" {
		t.Fatalf("integration README = %q", got)
	}
}

// A task whose agent is still alive gets the restack conflict, as before:
// no repair task.
func TestRestackConflictLiveOwnerGetsNoRepair(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.CloseOnLand = false
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"README.md": "one\n"})
	moveMain(t, origin, "README.md")
	if _, err := a.Restack(); err == nil {
		t.Fatal("restack succeeded over a live owner's conflict")
	}
	if rs := repairTasks(t, a, t1.ID); len(rs) != 0 {
		t.Fatalf("live owner got a repair task: %+v", rs)
	}
	if got := trainState(t, a, t1.ID); got != store.TrainOK {
		t.Fatalf("t1 = %s, want still landed", got)
	}
	if ns, _ := a.Store.TakeNotices(t1.ID, false); len(ns) == 0 {
		t.Fatal("live owner wasn't told about the conflict")
	}
}

// A queued branch whose agent is gone (here: requeued after landing, or its
// window closed) conflicts in land: the train spawns one repair instead of
// returning the conflict to nobody, and doesn't escalate.
func TestLandConflictOrphanSpawnsRepair(t *testing.T) {
	a, ft := setup(t)
	a.Cfg.Test.Cmd = "none"
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	t2, _ := a.Spawn(SpawnReq{Title: "two", Claims: []string{"two/**"}})
	write(t, t1.Worktree, "README.md", "one\n")
	commitAll(t, t1.Worktree, "one")
	write(t, t2.Worktree, "README.md", "two\n")
	commitAll(t, t2.Worktree, "two")
	must(t, a.Done(t1.ID, "one"))
	must(t, a.Done(t2.ID, "two"))
	must(t, ft.KillWindow(t2.Window))
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[1].State != store.TrainError {
		t.Fatalf("results = %+v", rs)
	}
	reps := repairTasks(t, a, t2.ID)
	if len(reps) != 1 {
		t.Fatalf("repair tasks = %+v, want one", reps)
	}
	r := reps[0]
	if !strings.Contains(r.Prompt, "README.md") || !strings.Contains(r.Prompt, t2.Branch) {
		t.Fatalf("repair prompt:\n%s", r.Prompt)
	}
	cl, _ := a.Store.Claims()
	if len(cl[t2.ID]) != 0 || !slices.Contains(cl[r.ID], "two/**") || !slices.Contains(cl[r.ID], "README.md") {
		t.Fatalf("claims = %v, want t2's moved to the repair %s", cl, r.ID)
	}
	if got := trainState(t, a, t2.ID); got == TrainEscalated {
		t.Fatal("orphan conflict escalated")
	}

	// The repair resolves on a fresh branch and lands normally.
	git(t, r.Worktree, "cherry-pick", "-X", "theirs", t2.Branch)
	must(t, a.Done(r.ID, "two, merged with one"))
	out, err := a.Land()
	if err != nil || len(out) != 1 || out[0].State != store.TrainOK {
		t.Fatalf("repair land = %+v, %v", out, err)
	}
	if got := trainState(t, a, t2.ID); got != TrainSuperseded {
		t.Fatalf("t2 after its repair landed = %s", got)
	}
	if got, _ := a.Store.Task(t2.ID); got.Active() {
		t.Fatalf("t2 still active after its repair landed: %s", got.Status)
	}
}

// `saddle repair <task>` triggers a repair by hand, at most once.
func TestRepairByHand(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.CloseOnLand = false // a live but stuck owner
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"README.md": "one\n"})
	moveMain(t, origin, "README.md")
	if _, err := a.Restack(); err == nil {
		t.Fatal("want the conflict to go to the live owner first")
	}
	res, err := a.Repair(t1.ID, false)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !res.Created || res.Repair == "" {
		t.Fatalf("repair result = %+v", res)
	}
	r, _ := a.Store.Task(res.Repair)
	if !strings.Contains(r.Prompt, "README.md") {
		t.Fatalf("hand repair prompt lacks the conflicting file:\n%s", r.Prompt)
	}
	if got := trainState(t, a, t1.ID); got != TrainRepairing {
		t.Fatalf("t1 = %s", got)
	}
	again, err := a.Repair(t1.ID, false)
	if err != nil || again.Created || again.Repair != res.Repair {
		t.Fatalf("second repair = %+v, %v; want the same task, not a new one", again, err)
	}
	if _, err := a.Repair("t9", false); err == nil {
		t.Fatal("repair of an unknown task succeeded")
	}
}
