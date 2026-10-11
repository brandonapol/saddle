package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// needsOne is a check that passes only where two.txt has one.txt beside it:
// t2's tests need t1's work though their files don't overlap.
const needsOne = "test ! -f two.txt || test -f one.txt || { echo 'two needs one'; exit 1; }"

// landSpec spawns a task from req, commits files on its branch and lands it.
func landSpec(t *testing.T, a *App, req SpawnReq, files map[string]string) store.Task {
	t.Helper()
	tk, err := a.Spawn(req)
	must(t, err)
	for rel, body := range files {
		write(t, tk.Worktree, rel, body)
	}
	commitAll(t, tk.Worktree, req.Title)
	must(t, a.Done(tk.ID, req.Title+" summary"))
	rs, err := a.Land()
	must(t, err)
	for _, r := range rs {
		if r.State != store.TrainOK {
			t.Fatalf("land %s: %s %s", r.Task, r.State, r.Note)
		}
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	return got
}

// baseEdits maps each PR the fake gh retargeted since before to its last
// --base.
func baseEdits(log []string) map[string]string {
	edits := map[string]string{}
	for _, c := range log {
		if f := strings.Fields(c); len(f) == 5 && f[0] == "pr" && f[1] == "edit" && f[3] == "--base" {
			edits[f[2]] = f[4]
		}
	}
	return edits
}

func eventWith(t *testing.T, a *App, task, kind, text string) bool {
	t.Helper()
	ev, err := a.Store.Events(500)
	must(t, err)
	return slices.ContainsFunc(ev, func(e store.Event) bool {
		return e.Task == task && e.Kind == kind && strings.Contains(e.Data, text)
	})
}

// #358: t2's issue is the follow-up to t1's and says so. Their files are
// disjoint and t2's tests fail without t1, yet nothing declared the edge.
// prs reads it from the issue text, records it, and stacks t2 on t1.
func TestPRsStacksTaskWhoseIssueNamesAnother(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Test.Cmd = needsOne
	_, ghLog := originWithGh(t, a)
	setIssue(t, 3094, "Drop reconnect listeners", "Follow-up to #3093: once the client resyncs on reconnect, the listeners are redundant.")
	t1 := landSpec(t, a, SpawnReq{ID: "t1", Title: "resync on reconnect", Issue: 3093}, map[string]string{"one.txt": "one\n"})
	t2 := landSpec(t, a, SpawnReq{ID: "t2", Title: "drop reconnect listeners", Issue: 3094}, map[string]string{"two.txt": "two\n"})

	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	bases := prBases(ghLog())
	if bases[t1.Branch] != "main" || bases[t2.Branch] != t1.Branch {
		t.Fatalf("PR bases = %v, want t1 on main and t2 on %s", bases, t1.Branch)
	}
	if !slices.Contains(a.TaskAfter("t2"), "t1") {
		t.Fatalf("t2's after edges = %v, want t1 recorded", a.TaskAfter("t2"))
	}
	if !eventWith(t, a, "t2", EventDependsOn, "#3093") {
		t.Fatal("no depends_on event naming how the edge was found")
	}
}

// #358 item 2: with no edge to find, the pre-publish gate checks t2 at its
// PR head, on main without t1, not at the integration tip that has t1
// underneath: it fails there, so t2 isn't published, and t1 is.
func TestPRsGateFailsDependentAgainstItsBase(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Test.Cmd = needsOne
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})

	_, err := a.PRs()
	if err == nil || !strings.Contains(err.Error(), "layer t2") || !strings.Contains(err.Error(), "two needs one") {
		t.Fatalf("prs of a layer red on its own base: err = %v", err)
	}
	if remoteRev(t, origin, t1.Branch) == "" || remoteRev(t, origin, t2.Branch) != "" {
		t.Fatal("want t1 published and t2 held")
	}
}

// #358 item 2 through restack: two published layers on main, and main
// moves. Restack re-cuts t2 onto the new main, where it lacks t1; the gate
// must check that PR head, not the integration tip, so t2 is flagged and
// not pushed while t1 is.
func TestRestackGatesPRHeadNotIntegrationTip(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	was1, was2 := remoteRev(t, origin, t1.Branch), remoteRev(t, origin, t2.Branch)
	a.Cfg.Train.Prepublish.Cmd = needsOne
	moveMain(t, origin, "main.txt")

	res, err := a.Restack()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.GateRed, []string{"t2"}) {
		t.Fatalf("restack gate red = %v, want [t2]", res.GateRed)
	}
	if remoteRev(t, origin, t1.Branch) == was1 {
		t.Fatal("t1 wasn't pushed onto the new main")
	}
	if remoteRev(t, origin, t2.Branch) != was2 {
		t.Fatal("restack pushed t2's red PR head")
	}
}

// #359: two independent layers go up on main; the owner retargets t2 onto
// t1's branch with gh pr edit --base. Restack keeps that base, records the
// edge, and t2's PR head carries t1's work.
func TestRestackKeepsManualRetarget(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t2, _ = a.Store.Task(t2.ID)
	setPR(t, t2.PR, "OPEN", t1.Branch) // gh pr edit --base, by hand
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got := baseEdits(ghLog()[before:])[t2.PR]; got != t1.Branch {
		t.Fatalf("restack set t2's base to %q, want the hand-set %s", got, t1.Branch)
	}
	if !slices.Contains(a.TaskAfter("t2"), "t1") || !eventWith(t, a, "t2", EventDependsOn, "manual retarget") {
		t.Fatalf("t2's after edges = %v, want t1 recorded from the retarget", a.TaskAfter("t2"))
	}
	head := remoteRev(t, origin, t2.Branch)
	if files := git(t, origin, "ls-tree", "-r", "--name-only", head); !strings.Contains(files, "one.txt") {
		t.Fatalf("t2's PR head doesn't sit on t1's work:\n%s", files)
	}

	// prs keeps it too.
	before = len(ghLog())
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if got := baseEdits(ghLog()[before:])[t2.PR]; got != t1.Branch {
		t.Fatalf("prs set t2's base to %q, want %s", got, t1.Branch)
	}
}

// #359: a retarget saddle can't keep (onto a layer that landed later) is
// changed back out loud: a pr_base event names the old and the new base,
// and the orchestrator hears it.
func TestRestackNamesBaseItChangesBack(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	setPR(t, t1.PR, "OPEN", t2.Branch)
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got := baseEdits(ghLog()[before:])[t1.PR]; got != "main" {
		t.Fatalf("t1's base = %q, want main", got)
	}
	if !eventWith(t, a, "t1", EventPRBase, t2.Branch+" → main") {
		t.Fatal("no pr_base event naming the old and new base")
	}
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	if !slices.ContainsFunc(ns, func(n store.Notice) bool {
		return strings.Contains(n.Text, "base from "+t2.Branch+" back to main")
	}) {
		t.Fatalf("orchestrator wasn't told about the change back: %+v", ns)
	}
}

// #358 item 3: t2 depends on t1, whose PR is still open (in the merge
// queue, say) while t1 is out of the stack. Restack doesn't move t2 onto
// main without t1; once t1's PR merges, the next restack does.
func TestRestackHoldsLayerUntilDependencyMerges(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landSpec(t, a, SpawnReq{ID: "t1", Title: "one"}, map[string]string{"one.txt": "one\n"})
	t2 := landSpec(t, a, SpawnReq{ID: "t2", Title: "two", After: []string{"t1"}}, map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	t2, _ = a.Store.Task(t2.ID)
	must(t, a.Store.SetTrain(t1.ID, TrainRepairing, "", false))
	setPR(t, t2.PR, "OPEN", t1.Branch)
	was := remoteRev(t, origin, t2.Branch)
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got, ok := baseEdits(ghLog()[before:])[t2.PR]; ok {
		t.Fatalf("restack retargeted t2 onto %s while t1's PR is open", got)
	}
	if remoteRev(t, origin, t2.Branch) != was {
		t.Fatal("restack pushed t2 without t1 under it")
	}
	if !eventWith(t, a, "t2", EventDependencyHold, t1.PR) {
		t.Fatal("no dependency_hold event")
	}

	setPR(t, t1.PR, "MERGED", "main")
	before = len(ghLog())
	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got := baseEdits(ghLog()[before:])[t2.PR]; got != "main" {
		t.Fatalf("after t1 merged, t2's base = %q, want main", got)
	}
}

func TestNamesNeedsAWholeReference(t *testing.T) {
	t.Parallel()
	dep := store.Task{ID: "t1", Issue: 31, PR: "https://github.com/o/r/pull/40", Branch: "saddle/t1-resync"}
	for text, want := range map[string]bool{
		"Builds on t1's resync.":               true,
		"follow-up to #31":                     true,
		"see https://github.com/o/r/pull/40":   true,
		"needs #40 first":                      true,
		"on top of saddle/t1-resync":           true,
		"t12 and t10 only":                     false,
		"closes #312":                          false,
		"other/repo#31 is unrelated, as is #4": false,
		"st1 is a word, so is t1x":             false,
		"nothing here":                         false,
	} {
		if _, got := names(text, dep); got != want {
			t.Errorf("names(%q) = %v, want %v", text, got, want)
		}
	}
}
