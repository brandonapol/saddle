package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// landPairLosingNote lands t1 and t2 (two commits) in one land call, then
// leaves t2's train note without a range, as in #392: its status reads
// landed, but only its landing event says which commits are its.
func landPairLosingNote(t *testing.T, a *App) {
	t.Helper()
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"}, map[string]string{"two.txt": "two\nmore\n"})
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 2 {
		t.Fatalf("land = %+v, want both tasks", rs)
	}
	_, to, _ := strings.Cut(trainNote(t, a, "t2"), "..")
	must(t, a.Store.SetTrain("t2", store.TrainOK, to, false))
}

// #392: restack adopts the commits after t1's landed commit because their
// patch-ids match t2's landing, instead of refusing them as unowned.
func TestRestackAdoptsCommitsOfMultiTaskLand(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landPairLosingNote(t, a)
	moveMain(t, origin, "news.txt")

	if _, err := a.Restack(); err != nil {
		t.Fatalf("restack refused t2's own commits: %v", err)
	}
	for _, f := range []string{"one.txt", "two.txt"} {
		if n := git(t, a.Root, "rev-list", "--count", "origin/main.."+a.Cfg.Integration, "--", f); n == "0" {
			t.Errorf("%s is not on integration after restack", f)
		}
	}
	if !taskEvent(t, a, "t2", restackAdoptedEvent) {
		t.Error("restack recorded no adoption for t2")
	}
}

// #392: unstacking the landed task detaches its commits from the ownership
// check; restack then drops them instead of refusing.
func TestRestackAfterUnstackOfLandedTask(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landPairLosingNote(t, a)
	_, err := a.Unstack("t2")
	must(t, err)
	moveMain(t, origin, "news.txt")

	res, err := a.Restack()
	if err != nil {
		t.Fatalf("restack refused after unstack t2: %v", err)
	}
	if n := git(t, a.Root, "rev-list", "--count", "origin/main.."+a.Cfg.Integration, "--", "two.txt"); n != "0" {
		t.Errorf("unstacked t2's work is still on integration (%s commits)", n)
	}
	if !strings.Contains(strings.Join(res.Superseded, ","), "t2") {
		t.Errorf("restack superseded = %v, want t2", res.Superseded)
	}
}

func taskEvent(t *testing.T, a *App, task, kind string) bool {
	t.Helper()
	evs, err := a.Store.Events(-1)
	must(t, err)
	for _, e := range evs {
		if e.Task == task && e.Kind == kind {
			return true
		}
	}
	return false
}
