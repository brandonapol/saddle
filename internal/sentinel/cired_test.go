package sentinel

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/ciwatch"
)

func TestRollupVerdict(t *testing.T) {
	for _, c := range []struct {
		r    rollup
		want string
	}{
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SUCCESS"}, "pass"},
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "FAILURE"}, "fail"},
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "TIMED_OUT"}, "fail"},
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SKIPPED"}, "pass"},
		{rollup{Typename: "CheckRun", Status: "IN_PROGRESS"}, "pending"},
		{rollup{Typename: "StatusContext", State: "ERROR"}, "fail"},
		{rollup{Typename: "StatusContext", State: "PENDING"}, "pending"},
		{rollup{Typename: "StatusContext", State: "SUCCESS"}, "pass"},
	} {
		if got := c.r.verdict(); got != c.want {
			t.Errorf("%+v: verdict = %s, want %s", c.r, got, c.want)
		}
	}
	if l := (rollup{Typename: "CheckRun", Name: "lint", Workflow: "CI"}).label(); l != "CI / lint" {
		t.Errorf("label = %q", l)
	}
}

// Quiet cycles back off up to the cap; anything red or changing resets it.
func TestCIRedBackoff(t *testing.T) {
	iv, maxIv := time.Minute, 5*time.Minute
	w := iv
	var got []time.Duration
	for range 4 {
		w = nextWait(w, iv, maxIv, true)
		got = append(got, w)
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	}
	if w = nextWait(w, iv, maxIv, false); w != iv {
		t.Fatalf("a red cycle waited %v, want %v", w, iv)
	}
}

// ciFake answers the ci-red watcher's gh calls from a canned PR view and
// records every call.
type ciFake struct {
	view  string
	calls []string
}

func (f *ciFake) gh(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) > 1 && args[0] == "pr" && args[1] == "view" {
		return f.view, nil
	}
	return "", nil
}

func (f *ciFake) called(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func ciView(head string, checks ...string) string {
	return `{"state":"OPEN","headRefOid":"` + head + `","statusCheckRollup":[` + strings.Join(checks, ",") + `]}`
}

func ciRun(name, status, conclusion, started string) string {
	return fmt.Sprintf(`{"__typename":"CheckRun","name":%q,"workflowName":"CI","status":%q,"conclusion":%q,"startedAt":%q}`,
		name, status, conclusion, started)
}

// #234: a canceled run reads as a failed aggregate check ("needs.*.result
// == cancelled"). It is not red: no hold, no label, no repair. A run that a
// newer run of the same check superseded is not red either, and an infra
// failure is rerun once with `gh run rerun --failed` before it counts.
func TestCIRedIgnoresCanceledAndSupersededRuns(t *testing.T) {
	a, _ := setup(t)
	newFakeGH(t)
	tk := landTask(t, a, "t1", "f1")
	_, err := a.PRs()
	must(t, err)
	tasks := func() int { ts, _ := a.Store.Tasks(); return len(ts) }
	before := tasks()

	f := &ciFake{view: ciView("aaa", ciRun("all", "COMPLETED", "FAILURE", "2026-10-06T10:00:00Z"))}
	log := "needs.test.result == cancelled\nProcess completed with exit code 1."
	c := &CIRed{App: a, GH: f.gh, Logs: func(context.Context, string) []ciwatch.Failed {
		return []ciwatch.Failed{{Check: ciwatch.Check{Name: "all", Workflow: "CI"},
			RunURL: "https://github.com/o/r/actions/runs/42", LogTail: log}}
	}}
	rep, err := c.Check(context.Background())
	must(t, err)
	if len(rep.Red) != 0 || tasks() != before || f.called("pr edit") {
		t.Fatalf("canceled run went red: %+v, tasks %d→%d, calls %v", rep, before, tasks(), f.calls)
	}

	// Superseded: the failed run has a newer, pending sibling on the same head.
	f.view = ciView("bbb",
		ciRun("ci", "COMPLETED", "FAILURE", "2026-10-06T10:00:00Z"),
		ciRun("ci", "IN_PROGRESS", "", "2026-10-06T10:05:00Z"))
	if rep, err = c.Check(context.Background()); err != nil || len(rep.Red) != 0 {
		t.Fatalf("superseded run went red: %+v, %v", rep, err)
	}

	// Infra: rerun once, then it counts.
	f.view = ciView("ccc", ciRun("all", "COMPLETED", "FAILURE", "2026-10-06T11:00:00Z"))
	log = "Error: Unable to resolve action `actions/setup-go@v9`, unable to find version"
	if rep, err = c.Check(context.Background()); err != nil || len(rep.Red) != 0 || !f.called("run rerun 42 --failed") {
		t.Fatalf("infra failure: %+v, %v, calls %v; want a rerun and no red", rep, err, f.calls)
	}
	if rep, _ = c.Check(context.Background()); len(rep.Red) != 1 || rep.Red[0].Task != tk.ID {
		t.Fatalf("infra failure after its rerun: %+v, want %s red", rep, tk.ID)
	}
}
