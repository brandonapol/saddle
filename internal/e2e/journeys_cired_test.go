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

	w.Spawn("t3", "Gamma work", []string{"gamma/**"}, finished("gamma", "gamma\n")...)
	w.Spawn("t4", "More alpha", []string{"alpha/**", "delta/**"},
		fa.Write("alpha/work.txt", "alpha two\n"), fa.Commit("more alpha"), fa.Done("Changes alpha/work.txt."))
	for _, id := range []string{"t3", "t4"} {
		w.WaitTask(id, "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	}
	r := w.MustSaddle("land")
	if v := w.Task("t3"); v.Status != "landed" {
		t.Fatalf("t3 changes nothing red CI touched, but didn't land: %s", r)
	}
	if v := w.Task("t4"); v.Status == "landed" || !strings.Contains(r.Stdout, "red CI") {
		t.Fatalf("t4 would stack on red t1 but wasn't held: %+v\n%s", v, r)
	}
	r = w.Saddle("prs")
	if r.Code == 0 || !strings.Contains(r.Stderr, "CI is red on t1") {
		t.Fatalf("prs over a red layer = %s, want it to say t1's CI holds it", r)
	}
	if v := w.Task("t3"); v.PR != "" {
		t.Fatalf("prs opened %s for t3, above red t1", v.PR)
	}
	w.CIRedTick() // what is held is recomputed every cycle
	holds, err := a.CIRedHolds()
	must(t, err)
	if len(holds) != 1 || !slices.Contains(holds[0].Held, "t3") || !slices.Equal(holds[0].Queued, []string{"t4"}) {
		t.Fatalf("holds = %+v, want t1 holding t3 and queued t4", holds)
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
	if v := w.Task("t4"); v.Status != "landed" {
		t.Fatalf("t4 still held after t1 went green: %+v", v)
	}
	w.MustSaddle("prs")
	for _, id := range []string{"t3", "t4"} {
		if w.Task(id).PR == "" {
			t.Fatalf("%s has no PR once the hold lifted", id)
		}
	}
}
