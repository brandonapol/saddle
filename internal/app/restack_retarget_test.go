package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// #359, as reported: two independent layers go up on main, the owner moves
// t2's PR onto t1's branch with gh pr edit --base, then main moves. Restack
// re-cuts both and force-pushes them, and t2's base must stay on t1's
// branch, with t2's new head on t1's new one.
func TestRestackKeepsManualRetargetWhenBaseMoves(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, t2 = mustTask(t, a, "t1"), mustTask(t, a, "t2")
	setPR(t, t1.PR, "OPEN", "main")
	setPR(t, t2.PR, "OPEN", t1.Branch) // gh pr edit --base, by hand
	was2 := remoteRev(t, origin, t2.Branch)
	moveMain(t, origin, "news.txt")
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got, ok := baseEdits(ghLog()[before:])[t2.PR]; ok && got != t1.Branch {
		t.Fatalf("restack set t2's base to %q, want the hand-set %s", got, t1.Branch)
	}
	new1, new2 := remoteRev(t, origin, t1.Branch), remoteRev(t, origin, t2.Branch)
	if new2 == was2 {
		t.Fatal("t2 wasn't re-cut onto the moved base")
	}
	if parent := git(t, origin, "rev-parse", new2+"^"); parent != new1 {
		t.Fatalf("t2's pushed head sits on %s, want t1's head %s", short(parent), short(new1))
	}
	if !slices.Contains(a.TaskAfter("t2"), "t1") {
		t.Fatalf("t2's after edges = %v, want t1 recorded from the retarget", a.TaskAfter("t2"))
	}
}

// #359: when GitHub can't say where t2's PR points, restack doesn't know
// whether someone moved it, so it doesn't edit a base saddle already set to
// what the layout wants: that edit could only undo a hand retarget, silently.
func TestRestackLeavesBaseItCantSee(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, t2 := mustTask(t, a, "t1"), mustTask(t, a, "t2")
	setPR(t, t1.PR, "OPEN", "main")
	setPR(t, t2.PR, "OPEN", t1.Branch) // the owner moves it by hand
	failPRState(t, a, t2.PR)           // and GitHub won't say so
	moveMain(t, origin, "news.txt")
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got, ok := baseEdits(ghLog()[before:])[t2.PR]; ok {
		t.Fatalf("restack set t2's base to %q without knowing where it pointed", got)
	}
}

// #359: the owner moves t2's PR onto the branch of t1, a saddle task whose
// PR is open but out of the stack (in the merge queue, say). Saddle records
// the edge and leaves t2's PR where it was until t1 merges, rather than
// moving it back to main.
func TestRestackKeepsRetargetOntoTaskOutsideStack(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, t2 := mustTask(t, a, "t1"), mustTask(t, a, "t2")
	must(t, a.Store.SetTrain(t1.ID, TrainRepairing, "", false))
	setPR(t, t1.PR, "OPEN", "main")
	setPR(t, t2.PR, "OPEN", t1.Branch) // gh pr edit --base, by hand
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got, ok := baseEdits(ghLog()[before:])[t2.PR]; ok {
		t.Fatalf("restack moved t2's hand-set base to %q while t1's PR is open", got)
	}
	if !slices.Contains(a.TaskAfter("t2"), "t1") || !eventWith(t, a, "t2", EventDependsOn, "manual retarget") {
		t.Fatalf("t2's after edges = %v, want t1 recorded from the retarget", a.TaskAfter("t2"))
	}
	if !eventWith(t, a, "t2", EventDependencyHold, t1.PR) {
		t.Fatal("no dependency_hold event")
	}
}

// failPRState makes the fake gh fail to report url's state and base, as
// when GitHub rate-limits or times out, while other calls still work.
func failPRState(t *testing.T, a *App, url string) {
	t.Helper()
	bin := filepath.Join(a.Root, ".git", FakeGHSubdir)
	must(t, os.Rename(filepath.Join(bin, "gh"), filepath.Join(bin, "gh.answers")))
	script := `#!/bin/sh
if [ "$1 $2 $3" = "pr view ` + url + `" ] && [ "$5" = "state,mergeable,baseRefName" ]; then
	echo "HTTP 502: Bad Gateway" >&2
	exit 1
fi
exec "` + filepath.Join(bin, "gh.answers") + `" "$@"
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
}
