package sentinel

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/ciwatch"
	"github.com/brandonapol/saddle/internal/forge"
)

func TestRollupVerdict(t *testing.T) {
	for _, c := range []struct {
		r    rollup
		want string
	}{
		{rollup{Kind: forge.KindJob, Status: "COMPLETED", Conclusion: "SUCCESS"}, "pass"},
		{rollup{Kind: forge.KindJob, Status: "COMPLETED", Conclusion: "FAILURE"}, "fail"},
		{rollup{Kind: forge.KindJob, Status: "COMPLETED", Conclusion: "TIMED_OUT"}, "fail"},
		{rollup{Kind: forge.KindJob, Status: "COMPLETED", Conclusion: "SKIPPED"}, "pass"},
		{rollup{Kind: forge.KindJob, Status: "IN_PROGRESS"}, "pending"},
		{rollup{Kind: forge.KindStatus, State: "ERROR"}, "fail"},
		{rollup{Kind: forge.KindStatus, State: "PENDING"}, "pending"},
		{rollup{Kind: forge.KindStatus, State: "SUCCESS"}, "pass"},
	} {
		if got := c.r.verdict(); got != c.want {
			t.Errorf("%+v: verdict = %s, want %s", c.r, got, c.want)
		}
	}
	if l := (rollup{Kind: forge.KindJob, Name: "lint", Workflow: "CI"}).label(); l != "CI / lint" {
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

// ciFake answers the ci-red watcher's PR reads from a canned PR and records
// every other call as "rerun RUN", "label NAME", "add-label PR" or
// "remove-label PR".
type ciFake struct {
	view  forge.PR
	calls []string
}

func (f *ciFake) PR(context.Context, string, forge.PRFields) (forge.PR, error) { return f.view, nil }
func (f *ciFake) Checks(context.Context, string) ([]forge.CICheck, error)      { return nil, nil }
func (f *ciFake) FailedLog(context.Context, string, int) (forge.JobLog, error) {
	return forge.JobLog{}, nil
}
func (f *ciFake) RerunFailed(_ context.Context, id string) error { return f.call("rerun " + id) }
func (f *ciFake) EnsureLabel(_ context.Context, l forge.Label) error {
	return f.call("label " + l.Name)
}
func (f *ciFake) AddLabel(_ context.Context, ref, _ string) error { return f.call("add-label " + ref) }
func (f *ciFake) RemoveLabel(_ context.Context, ref, _ string) error {
	return f.call("remove-label " + ref)
}

func (f *ciFake) call(c string) error {
	f.calls = append(f.calls, c)
	return nil
}

func (f *ciFake) called(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func ciView(head string, checks ...forge.CheckRun) forge.PR {
	return forge.PR{State: forge.StateOpen, HeadSHA: head, Checks: checks}
}

func ciRun(name, status, conclusion, started string) forge.CheckRun {
	return forge.CheckRun{Kind: forge.KindJob, Name: name, Workflow: "CI", Status: status, Conclusion: conclusion, StartedAt: started}
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
	c := &CIRed{App: a, Forge: f, Logs: func(context.Context, string) []ciwatch.Failed {
		return []ciwatch.Failed{{Check: ciwatch.Check{Name: "all", Workflow: "CI"},
			RunURL: "https://github.com/o/r/actions/runs/42", LogTail: log}}
	}}
	rep, err := c.Check(context.Background())
	must(t, err)
	if len(rep.Red) != 0 || tasks() != before || f.called("add-label") {
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
	if rep, err = c.Check(context.Background()); err != nil || len(rep.Red) != 0 || !f.called("rerun 42") {
		t.Fatalf("infra failure: %+v, %v, calls %v; want a rerun and no red", rep, err, f.calls)
	}
	if rep, _ = c.Check(context.Background()); len(rep.Red) != 1 || rep.Red[0].Task != tk.ID {
		t.Fatalf("infra failure after its rerun: %+v, want %s red", rep, tk.ID)
	}
}

// #647: on a forge without PR reads or labels ([forge] cli = "none") the
// ci-red watcher and the stack sentinel's GitHub do nothing and report no
// errors.
func TestUnsupportedForgeIsQuiet(t *testing.T) {
	a, _ := setup(t)
	newFakeGH(t)
	landTask(t, a, "t1", "f1")
	_, err := a.PRs()
	must(t, err)
	none := forge.Unsupported{Reason: "local-only"}
	rep, err := (&CIRed{App: a, Forge: none}).Check(context.Background())
	if err != nil || len(rep.Red) != 0 {
		t.Fatalf("ci-red on no forge = %+v, %v; want nothing and no error", rep, err)
	}
	g := &GH{Forge: none}
	must(t, g.AddLabel("u", Label))
	must(t, g.RemoveLabel("u", Label))
	must(t, g.AddComment("u", "x"))
	must(t, g.EditComment("IC_1", "x"))
	if cs, err := g.Comments("u"); err != nil || cs != nil {
		t.Fatalf("comments = %v, %v", cs, err)
	}
	if _, err := g.PR("u"); err == nil {
		t.Fatal("PR on no forge: want an error, which the stack tolerates")
	}
}
