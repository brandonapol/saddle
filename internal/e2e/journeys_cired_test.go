//go:build e2e

package e2e

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/sentinel"
	"github.com/brandonapol/saddle/internal/store"
)

// CIRedTick runs one cycle of the ci-red watcher, the loop `saddle up`
// runs on its backoff schedule, with no wait.
func (w *World) CIRedTick() sentinel.CIRedReport {
	w.T.Helper()
	a, err := app.Open(w.Repo)
	must(w.T, err)
	defer func() { _ = a.Close() }()
	rep, err := sentinel.NewCIRed(a).Check(context.Background())
	must(w.T, err)
	return rep
}

func ciCheck(state string) fakegh.Check {
	return fakegh.Check{Name: "ci", Workflow: "CI", State: state,
		Log: "CI\tgo test\t2026-10-04T10:00:00.0000000Z --- FAIL: TestThing\nCI\tgo test\t2026-10-04T10:00:01.0000000Z FAIL\tdemo/alpha"}
}

func hasLabel(p *fakegh.PR, l string) bool { return slices.Contains(p.Labels, l) }

// TestJourneyCIRedHoldsAboveAndClearsWhenGreen (#213): t1 and t2 form one
// stack and t1's CI goes red. Every layer above t1 is held: both PRs carry
// ci-red (never needs-human) and the orchestrator hears an info notice. t3
// lands above it but prs opens no PR for it; t4, which changes t1's files,
// is held by land; auto-merge merges nothing. Once t1 is green the hold
// lifts by itself: t4 lands and prs opens t3's and t4's PRs.
func TestJourneyCIRedHoldsAboveAndClearsWhenGreen(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	urls := landTwo(t, w)
	s := w.GHState()
	low, high := prNumber(t, s, urls["t1"]), prNumber(t, s, urls["t2"])
	must(t, w.GH.SetChecks(low.Number, ciCheck(fakegh.Fail)))
	must(t, w.GH.SetChecks(high.Number, ciCheck(fakegh.Pass)))
	a := w.App()
	_, _ = a.Store.TakeNotices("t0", false)

	rep := w.CIRedTick()
	if len(rep.Red) != 1 || rep.Red[0].Task != "t1" || !slices.Equal(rep.Red[0].Held, []string{"t2"}) {
		t.Fatalf("red layers = %+v, want t1 holding t2", rep.Red)
	}
	s = w.GHState()
	for _, n := range []int{low.Number, high.Number} {
		if p := s.PR(n); !hasLabel(p, app.LabelCIRed) || hasLabel(p, sentinel.Label) {
			t.Fatalf("PR #%d labels = %v, want ci-red and not needs-human", n, p.Labels)
		}
	}
	ns, err := a.Store.TakeNotices("t0", false)
	must(t, err)
	if len(ns) == 0 || !slices.ContainsFunc(ns, func(n store.Notice) bool { return strings.Contains(n.Text, "CI is red on t1") }) {
		t.Fatalf("orchestrator notices = %+v, want one about t1's red CI", ns)
	}
	for _, n := range ns {
		if n.Kind == store.NoticeAction && strings.Contains(n.Text, "CI is red") {
			t.Fatalf("a red layer is routine, but the notice needs action: %+v", n)
		}
	}
	if why := a.CIRedCovers("t2"); why == "" {
		t.Fatal("auto-merge isn't kept off t2, above red t1")
	}
	// A quiet second cycle changes nothing and notifies nobody.
	if rep := w.CIRedTick(); len(rep.New) != 0 || len(rep.Cleared) != 0 {
		t.Fatalf("second cycle = %+v, want no change", rep)
	}

	// t3 is t1's repair (see the journeys below); t10 and t11 are new work.
	w.Spawn("t10", "Gamma work", []string{"gamma/**"}, finished("gamma", "gamma\n")...)
	// t11 changes t2's file, so it would stack on t2, above red t1.
	w.Spawn("t11", "More beta", []string{"beta/**", "delta/**"},
		fa.Write("beta/work.txt", "beta two\n"), fa.Commit("more beta"), fa.Done("Changes beta/work.txt."))
	for _, id := range []string{"t10", "t11"} {
		w.WaitTask(id, "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	}
	r := w.MustSaddle("land")
	if v := w.Task("t10"); v.Status != "landed" {
		t.Fatalf("t10 changes nothing red CI touched, but didn't land: %s", r)
	}
	if v := w.Task("t11"); v.Status == "landed" || !strings.Contains(r.Stdout, "red CI") {
		t.Fatalf("t11 would stack on red t1 but wasn't held: %+v\n%s", v, r)
	}
	r = w.Saddle("prs")
	if r.Code == 0 || !strings.Contains(r.Stderr, "CI is red on t1") {
		t.Fatalf("prs over a red layer = %s, want it to say t1's CI holds it", r)
	}
	if v := w.Task("t10"); v.PR != "" {
		t.Fatalf("prs opened %s for t10, above red t1", v.PR)
	}
	w.CIRedTick() // what is held is recomputed every cycle
	holds, err := a.CIRedHolds()
	must(t, err)
	if len(holds) != 1 || !slices.Contains(holds[0].Held, "t10") || !slices.Equal(holds[0].Queued, []string{"t11"}) {
		t.Fatalf("holds = %+v, want t1 holding t10 and queued t11", holds)
	}
	if st := w.MustSaddle("status"); !strings.Contains(st.Stdout, "ci-red on t1") {
		t.Fatalf("status doesn't show the red layer:\n%s", st.Stdout)
	}
	w.MustSaddle("automerge", "on")
	w.AutomergeTick()
	if p := w.GHState().PR(low.Number); p.State != "OPEN" {
		t.Fatalf("auto-merge merged red t1: %s", p.State)
	}
	if p := w.GHState().PR(high.Number); p.State != "OPEN" {
		t.Fatalf("auto-merge merged t2 above red t1: %s", p.State)
	}
	w.MustSaddle("automerge", "off")

	must(t, w.GH.SetChecks(low.Number, ciCheck(fakegh.Pass)))
	if rep := w.CIRedTick(); len(rep.Red) != 0 || !slices.Equal(rep.Cleared, []string{"t1"}) {
		t.Fatalf("after green: %+v, want t1 cleared", rep)
	}
	s = w.GHState()
	for _, n := range []int{low.Number, high.Number} {
		if p := s.PR(n); hasLabel(p, app.LabelCIRed) {
			t.Fatalf("PR #%d still labeled ci-red: %v", n, p.Labels)
		}
	}
	w.MustSaddle("land")
	if v := w.Task("t11"); v.Status != "landed" {
		t.Fatalf("t11 still held after t1 went green: %+v", v)
	}
	w.MustSaddle("prs")
	for _, id := range []string{"t10", "t11"} {
		if w.Task(id).PR == "" {
			t.Fatalf("%s has no PR once the hold lifted", id)
		}
	}
}

// repairTasks lists the CI repair tasks of red task orig: they share the
// CI watcher's fix-task title, so neither spawns a second one.
func repairTasks(w *World, orig string) []mcpserver.TaskView {
	var out []mcpserver.TaskView
	for _, v := range w.Status().Tasks {
		if strings.HasPrefix(v.Title, "Fix CI for "+orig+" ") {
			out = append(out, v)
		}
	}
	return out
}

// TestJourneyCIRedLintRepairFoldsIntoRedLayer (#213, #196): t1's lint check
// goes red. Saddle runs the repo's own fixer (make fix) on t1's layer and
// spawns exactly one sonnet repair for that head, seeded with the failing
// log and the fixer's commit; a second cycle on the same head spawns none.
// The repair lands through the train and is folded into t1's layer: t1's
// PR gets the fix, the repair gets no PR of its own, and once the checks
// pass the hold lifts.
func TestJourneyCIRedLintRepairFoldsIntoRedLayer(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	w.WriteFile("Makefile", "fix:\n\tsed -i 's/ *$$//' alpha/work.txt\n")
	w.Git(w.Repo, "add", "Makefile")
	w.Git(w.Repo, "commit", "-q", "-m", "fixer")
	w.Git(w.Repo, "push", "-q", "origin", "main")
	w.Spawn("t1", "Alpha work", []string{"alpha/**"},
		fa.Write("alpha/work.txt", "alpha   \n"), fa.Commit("alpha, with trailing spaces"), fa.Done("Adds alpha."))
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	for _, id := range []string{"t1", "t2"} {
		w.WaitTask(id, "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	}
	w.MustSaddle("land")
	w.MustSaddle("prs")
	t1PR := prNumber(t, w.GHState(), w.Task("t1").PR)
	must(t, w.GH.SetChecks(t1PR.Number, fakegh.Check{Name: "lint", Workflow: "CI", State: fakegh.Fail,
		Log: "CI\tlint\t2026-10-04T10:00:00.0000000Z alpha/work.txt: trailing whitespace"}))
	must(t, fa.Script{Steps: []fa.Step{
		fa.Run("git cherry-pick refs/saddle/ci-fix/t1"), fa.Done("Strips alpha's trailing spaces.")}}.Save(w.Scripts, "t3"))

	rep := w.CIRedTick()
	if rs := repairTasks(w, "t1"); len(rs) != 1 || rs[0].ID != "t3" || rs[0].Model != "sonnet" {
		t.Fatalf("repairs = %+v (report %+v), want one sonnet repair t3", rs, rep)
	}
	a := w.App()
	t3, err := a.Store.Task("t3")
	must(t, err)
	for _, want := range []string{"trailing whitespace", "make fix", "refs/saddle/ci-fix/t1", "not as a new PR"} {
		if !strings.Contains(t3.Prompt, want) {
			t.Fatalf("repair prompt lacks %q:\n%s", want, t3.Prompt)
		}
	}
	if got := w.Git(w.Repo, "show", "refs/saddle/ci-fix/t1:alpha/work.txt"); got != "alpha" {
		t.Fatalf("make fix's commit has alpha/work.txt = %q", got)
	}
	if rep := w.CIRedTick(); len(repairTasks(w, "t1")) != 1 {
		t.Fatalf("a second cycle on the same head spawned another repair: %+v", rep)
	}

	w.WaitTask("t3", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t3").Status != "landed" {
		t.Fatalf("the repair was held instead of landing: %s", r)
	}
	must(t, w.GH.SetChecks(t1PR.Number, fakegh.Check{Name: "lint", Workflow: "CI", State: fakegh.Pass}))
	rep = w.CIRedTick()
	if len(rep.Red) != 0 || !slices.Equal(rep.Cleared, []string{"t1"}) {
		t.Fatalf("after the fold went green: %+v", rep)
	}
	if v := w.Task("t3"); !strings.HasPrefix(v.Train, app.TrainFolded) || v.PR != "" {
		t.Fatalf("t3 = %+v, want folded with no PR; events %v %v", v, events(t, a, "ci_repair_fold_failed"), events(t, a, sentinel.EventCIRedError))
	}
	if got := w.OriginFile(w.Task("t1").Branch, "alpha/work.txt"); got != "alpha\n" {
		t.Fatalf("t1's PR branch has alpha/work.txt = %q, want the fix", got)
	}
	if got := w.OriginFile(w.Task("t2").Branch, "alpha/work.txt"); got != "alpha\n" {
		t.Fatalf("t2's PR branch, above t1, has alpha/work.txt = %q, want the fix", got)
	}
	w.MustSaddle("prs")
	if v := w.Task("t3"); v.PR != "" {
		t.Fatalf("prs opened %s for the folded repair", v.PR)
	}
	if n := len(w.GHState().PRs); n != 2 {
		t.Fatalf("%d PRs, want t1's and t2's only", n)
	}
}

// TestJourneyCIRedEscalatesAfterRepairAttempts (#213): t1's CI stays red
// through every repair. Each red head gets one repair, folded into t1's
// layer; after [ci] repair_attempts (2) the orchestrator gets one action
// notice and no third repair is spawned.
func TestJourneyCIRedEscalatesAfterRepairAttempts(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	urls := landTwo(t, w)
	low := prNumber(t, w.GHState(), urls["t1"])
	must(t, w.GH.SetChecks(low.Number, ciCheck(fakegh.Fail)))
	for i, id := range []string{"t3", "t4", "t5"} {
		must(t, fa.Script{Steps: []fa.Step{
			fa.Append("alpha/work.txt", "try "+id+"\n"), fa.Commit("try " + id), fa.Done("Attempt " + string(rune('1'+i)) + ".")}}.Save(w.Scripts, id))
	}
	a := w.App()

	for n, id := range []string{"t3", "t4"} {
		w.CIRedTick()
		if rs := repairTasks(w, "t1"); len(rs) != n+1 || rs[n].ID != id {
			t.Fatalf("attempt %d: repairs = %+v, want %s", n+1, rs, id)
		}
		w.WaitTask(id, "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
		w.MustSaddle("land")
		_, _ = a.Store.TakeNotices("t0", false)
	}
	// The second repair folds, CI is red on the new head, and the attempts are spent.
	rep := w.CIRedTick()
	if rs := repairTasks(w, "t1"); len(rs) != 2 {
		t.Fatalf("repairs = %+v, want no third after 2 attempts (report %+v)", rs, rep)
	}
	ns, err := a.Store.TakeNotices("t0", true)
	must(t, err)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "after 2 repair attempts") {
		t.Fatalf("orchestrator action notices = %+v, want one escalation", ns)
	}
	if len(rep.Red) != 1 || !rep.Red[0].Escalated {
		t.Fatalf("red = %+v, want t1 escalated", rep.Red)
	}
	if got := w.OriginFile(w.Task("t1").Branch, "alpha/work.txt"); !strings.Contains(got, "try t3") || !strings.Contains(got, "try t4") {
		t.Fatalf("t1's PR branch lacks the folded repairs: %q", got)
	}
	// Still red, still escalated: nobody hears it again.
	w.CIRedTick()
	if ns, _ := a.Store.TakeNotices("t0", true); len(ns) != 0 {
		t.Fatalf("escalated twice: %+v", ns)
	}
	if st := w.MustSaddle("status"); !strings.Contains(st.Stdout, "2 repairs failed") {
		t.Fatalf("status doesn't show the escalation:\n%s", st.Stdout)
	}
}
