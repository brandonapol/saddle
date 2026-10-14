package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setIssueState makes the fake gh report issue n in state (OPEN or CLOSED).
func setIssueState(t *testing.T, n int, state string) {
	t.Helper()
	b := fmt.Sprintf(`{"title":"issue %d","body":"","state":%q}`, n, state)
	must(t, os.WriteFile(filepath.Join(fakeGHDir(t), fmt.Sprintf("issue-%d", n)), []byte(b), 0o644))
}

// issueCloses are the gh issue close calls in log.
func issueCloses(log []string) []string {
	var out []string
	for _, c := range log {
		if strings.HasPrefix(c, "issue close ") {
			out = append(out, c)
		}
	}
	return out
}

// #572: t1 worked on #3200 and its PR merged, but the PR body didn't close
// the issue (edited, or a template stripped the line). Reconcile closes it
// with a comment naming the PR.
func TestMergedTaskClosesItsOpenIssue(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	landSpec(t, a, SpawnReq{ID: "t1", Title: "fix login", Issue: 3200}, map[string]string{"one.txt": "one\n"})
	_, err := a.PRs()
	must(t, err)
	t1, err := a.Store.Task("t1")
	must(t, err)
	setPRBody(t, t1.PR, "MERGED", "main", "Fixes the login page.")
	setIssueState(t, 3200, "OPEN")
	before := len(ghLog())

	_, err = a.ReconcileStack(a.ghLookup)
	must(t, err)
	closes := issueCloses(ghLog()[before:])
	if len(closes) != 1 || !strings.HasPrefix(closes[0], "issue close 3200 ") || !strings.Contains(closes[0], t1.PR) {
		t.Fatalf("issue closes = %q, want #3200 closed with a comment naming %s", closes, t1.PR)
	}
	if !eventWith(t, a, "t1", EventIssueClosed, "#3200") {
		t.Fatal("no issue_closed event for t1")
	}
}

// The backstop leaves alone an issue already closed, a task with no issue,
// and a task whose PR was closed rather than merged.
func TestMergedTaskLeavesOtherIssuesAlone(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, ghLog := originWithGh(t, a)
	landSpec(t, a, SpawnReq{ID: "t1", Title: "one", Issue: 3201}, map[string]string{"one.txt": "one\n"})
	landSpec(t, a, SpawnReq{ID: "t2", Title: "two"}, map[string]string{"two.txt": "two\n"})
	landSpec(t, a, SpawnReq{ID: "t3", Title: "three", Issue: 3203}, map[string]string{"three.txt": "three\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	t3, _ := a.Store.Task("t3")
	setPR(t, t1.PR, "MERGED", "main")
	setPR(t, t2.PR, "MERGED", "main")
	setPR(t, t3.PR, "CLOSED", "main")
	setIssueState(t, 3201, "CLOSED")
	setIssueState(t, 3203, "OPEN")
	before := len(ghLog())

	_, err = a.ReconcileStack(a.ghLookup)
	must(t, err)
	if got := trainState(t, a, "t1"); got != TrainMerged {
		t.Fatalf("t1's train row = %q, want %s", got, TrainMerged)
	}
	if closes := issueCloses(ghLog()[before:]); len(closes) != 0 {
		t.Fatalf("issue closes = %q, want none", closes)
	}
}
