package app

import (
	"strings"
	"testing"
)

// flagTwoStacks lands stack A (t1, t2 both change a.txt) and stack B (t3,
// alone on b.txt, later in train order), and flags A's bottom layer.
func flagTwoStacks(t *testing.T) *App {
	t.Helper()
	a := trainSetup(t)
	landTask(t, a, "t1", "a one", map[string]string{"a.txt": "one\n"})
	landTask(t, a, "t2", "a two", map[string]string{"a.txt": "one\ntwo\n"})
	landTask(t, a, "t3", "b", map[string]string{"b.txt": "b\n"})
	must(t, a.SetFlag(StackFlag{Task: "t1", Cause: "does not descend from main"}))
	return a
}

// #771: the at-risk flag covers its own PR stack, not an independent stack
// later in train order.
func TestFlagCoversOnlyItsOwnStack(t *testing.T) {
	t.Parallel()
	a := flagTwoStacks(t)
	if why := a.flagCovers("t3"); why != "" {
		t.Fatalf("t3 is in another stack, flag covers it: %q", why)
	}
	for _, id := range []string{"t1", "t2"} {
		if why := a.flagCovers(id); !strings.Contains(why, "does not descend") {
			t.Fatalf("%s is in the flagged stack: %q", id, why)
		}
	}
}

// #771: a flag whose task has left the stack covers nothing.
func TestFlagOnTaskOutOfStackCoversNothing(t *testing.T) {
	t.Parallel()
	a := flagTwoStacks(t)
	must(t, a.SetFlag(StackFlag{Task: "t9", Cause: "gone"}))
	for _, id := range []string{"t1", "t2", "t3"} {
		if why := a.flagCovers(id); why != "" {
			t.Fatalf("%s: flag on a task out of the stack covers it: %q", id, why)
		}
	}
}

// #771: land freezes only files the flagged stack changed, not those of an
// independent stack later in train order.
func TestFrozenFilesOnlyFlaggedStack(t *testing.T) {
	t.Parallel()
	a := flagTwoStacks(t)
	frozen, err := a.frozenFiles()
	must(t, err)
	if frozen["a.txt"] == "" {
		t.Fatalf("a.txt isn't frozen: %v", frozen)
	}
	if l, ok := frozen["b.txt"]; ok {
		t.Fatalf("b.txt (independent stack, %s) is frozen: %v", l, frozen)
	}
}

// #771: prs still publishes an independent stack while another is flagged,
// and holds the flagged stack back.
func TestPRsPublishesIndependentStackWhileFlagged(t *testing.T) {
	t.Parallel()
	a := flagTwoStacks(t)
	originWithGh(t, a)
	_, err := a.PRs()
	if err == nil || !strings.Contains(err.Error(), "flagged at risk from t1") {
		t.Fatalf("prs err = %v, want the flag", err)
	}
	if pr := mustTask(t, a, "t3").PR; pr == "" {
		t.Fatal("t3 (independent stack) got no PR")
	}
	for _, id := range []string{"t1", "t2"} {
		if pr := mustTask(t, a, id).PR; pr != "" {
			t.Fatalf("%s (flagged stack) got PR %s", id, pr)
		}
	}
}
