package app

import (
	"slices"
	"strings"
	"testing"
)

func TestAboveFollowsTheStackChain(t *testing.T) {
	// 0 ← 1 ← 2 is one stack; 3 targets base.
	layout := []prLayer{{Below: -1}, {Below: 0}, {Below: 1}, {Below: -1}}
	for _, c := range []struct {
		j, at int
		want  bool
	}{{1, 0, true}, {2, 0, true}, {2, 1, true}, {3, 0, false}, {0, 1, false}, {0, 0, false}} {
		if got := above(layout, c.j, c.at); got != c.want {
			t.Errorf("above(%d, %d) = %v, want %v", c.j, c.at, got, c.want)
		}
	}
}

func TestCIRedErrNamesRedLayerAndHeld(t *testing.T) {
	if ciRedErr(nil, nil) != nil {
		t.Fatal("nothing held, but prs failed")
	}
	landed := []landedTask{{}, {}, {}}
	landed[1].ID, landed[2].ID = "t2", "t3"
	err := ciRedErr(map[int]string{1: "t1", 2: "t1"}, landed)
	if err == nil || !strings.Contains(err.Error(), "t1 (held: t2, t3)") || !strings.Contains(err.Error(), "sentinel ack") {
		t.Fatalf("err = %v", err)
	}
}

// The escape hatch: an acked red layer holds nothing back until it goes red
// on a new head; auto-merge still never merges it.
func TestAckCIRedStopsHoldingUntilNewHead(t *testing.T) {
	a, _ := setup(t)
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t1", Head: "aaa", Checks: []string{"CI / ci"}, Held: []string{"t2"}}}}))
	acked, err := a.AckCIRed()
	must(t, err)
	if len(acked) != 1 || acked[0].Task != "t1" {
		t.Fatalf("acked = %+v", acked)
	}
	s, err := a.CIRed()
	must(t, err)
	if len(s.holding()) != 0 {
		t.Fatalf("acked layer still holds: %+v", s.holding())
	}
	if a.CIRedCovers("t2") == "" || a.CIRedCovers("t1") == "" {
		t.Fatal("ack let auto-merge at a red PR or one above it")
	}
	if a.CIRedCovers("t9") != "" {
		t.Fatal("auto-merge kept off a task red CI doesn't hold")
	}
	if again, _ := a.AckCIRed(); len(again) != 0 {
		t.Fatalf("second ack = %+v, want nothing new", again)
	}
	held := slices.Clone(s.Red[0].Held)
	if !slices.Equal(held, []string{"t2"}) {
		t.Fatalf("held = %v", held)
	}
}

// #193: t64's CI ran `make test/e2e`, a target only t61 (unmerged) adds.
// That failure is explained by the sibling, so it is no repair case.
func TestSiblingExplainsMissingTarget(t *testing.T) {
	fails := []CIRedFailure{{Check: "CI / test", LogTail: "make: *** No rule to make target 'test/e2e'.  Stop.\nError: Process completed with exit code 2."}}
	toks := failureTokens(fails)
	if !slices.Contains(toks, "test/e2e") {
		t.Fatalf("tokens = %v, want test/e2e", toks)
	}
	sibling := "test/e2e: ## Run the end-to-end journeys\n\tgo test -tags e2e ./internal/e2e/...\n"
	baseHas := func(tok string) bool { return tok == "Process" }
	if tok, ok := explainedBy(toks, sibling, baseHas); !ok || tok != "test/e2e" {
		t.Fatalf("explainedBy = %q, %v; want test/e2e", tok, ok)
	}
	// The base already has it: the red layer broke it itself.
	if _, ok := explainedBy(toks, sibling, func(string) bool { return true }); ok {
		t.Fatal("explained by a sibling though base has the name")
	}
	if _, ok := explainedBy(toks, "unrelated.go\n", baseHas); ok {
		t.Fatal("explained by a sibling that doesn't add the name")
	}
}

func TestIsLintFailure(t *testing.T) {
	for _, c := range []struct {
		f    CIRedFailure
		want bool
	}{
		{CIRedFailure{Check: "CI / lint"}, true},
		{CIRedFailure{Check: "CI / check", Step: "check/format"}, true},
		{CIRedFailure{Check: "CI / test", LogTail: "dart format: 2 files would reformat"}, true},
		{CIRedFailure{Check: "CI / test", LogTail: "--- FAIL: TestThing"}, false},
	} {
		if got := isLintFailure([]CIRedFailure{c.f}); got != c.want {
			t.Errorf("%+v: lint = %v, want %v", c.f, got, c.want)
		}
	}
}

func TestCIRedOwnsOnlyStackedLandedTasks(t *testing.T) {
	a, _ := setup(t)
	if a.CIRedOwns("t1") || a.CIRedOwns("nope") {
		t.Fatal("ci-red owns a task that isn't stacked")
	}
}

// #234: only real failures are red. Canceled and superseded runs are
// ignored, infra failures and known flaky checks are rerun once.
func TestClassifyCIFailure(t *testing.T) {
	flaky := []string{"CI / e2e", "race"}
	for _, c := range []struct {
		f    CIRedFailure
		want string
	}{
		{CIRedFailure{Check: "CI / ci", LogTail: "--- FAIL: TestThing\nFAIL\tdemo/alpha"}, CIReal},
		{CIRedFailure{Check: "CI / Check every suite passed", LogTail: "needs.test.result == cancelled\nProcess completed with exit code 1."}, CIIgnore},
		{CIRedFailure{Check: "CI / ci", LogTail: "Canceling since a higher priority waiting request for 'CI-refs/heads/x' exists"}, CIIgnore},
		{CIRedFailure{Check: "CI / ci", LogTail: "##[error]The operation was canceled."}, CIIgnore},
		{CIRedFailure{Check: "CI / ci", LogTail: "Error: Unable to resolve action `actions/setup-go@v9`"}, CIRerun},
		{CIRedFailure{Check: "CI / ci", LogTail: "The runner has received a shutdown signal."}, CIRerun},
		{CIRedFailure{Check: "CI / ci", LogTail: "fetch: 502 Bad Gateway"}, CIRerun},
		{CIRedFailure{Check: "CI / e2e", LogTail: "--- FAIL: TestJourney"}, CIRerun},
		{CIRedFailure{Check: "CI / race", LogTail: "--- FAIL: TestRace"}, CIRerun},
		// A real test failure that mentions cancellation is still real.
		{CIRedFailure{Check: "CI / ci", LogTail: "--- FAIL: TestCancel\n    context canceled"}, CIReal},
	} {
		if got, why := ClassifyCIFailure(c.f, flaky); got != c.want {
			t.Errorf("%s %q: class = %s (%s), want %s", c.f.Check, c.f.LogTail, got, why, c.want)
		}
	}
}

func ciEvents(t *testing.T, a *App, kind string) []string {
	t.Helper()
	es, err := a.Store.Events(500)
	must(t, err)
	var out []string
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e.Task+" "+e.Data)
		}
	}
	return out
}

// #234: a canceled run is not red and spawns nothing; an infra failure is
// rerun once (gh run rerun --failed) and only counts when it fails again.
func TestScreenCIPollIgnoresCanceledAndRerunsInfraOnce(t *testing.T) {
	a, _ := setup(t)
	var reruns []string
	rerun := func(run string) error { reruns = append(reruns, run); return nil }
	runURL := "https://github.com/o/r/actions/runs/42"

	p := CIPoll{Task: "t1", PR: "1", Head: "aaa", Failed: []string{"CI / all"}}
	fails := []CIRedFailure{{Check: "CI / all", RunURL: runURL, LogTail: "needs.test.result == cancelled"}}
	got, err := a.ScreenCIPoll(p, fails, rerun)
	must(t, err)
	if len(got.Failed) != 0 || got.Green || len(reruns) != 0 {
		t.Fatalf("canceled run: poll = %+v, reruns %v; want nothing failed, not green, no rerun", got, reruns)
	}
	// Seen again, it is not logged twice.
	_, _ = a.ScreenCIPoll(p, fails, rerun)
	if ev := ciEvents(t, a, EventCIIgnored); len(ev) != 1 || !strings.Contains(ev[0], "CI / all") {
		t.Fatalf("ci_ignored events = %v, want one naming the check", ev)
	}

	p = CIPoll{Task: "t1", PR: "1", Head: "bbb", Failed: []string{"CI / ci"}}
	fails = []CIRedFailure{{Check: "CI / ci", RunURL: runURL, LogTail: "Unable to resolve action actions/checkout@v9"}}
	got, err = a.ScreenCIPoll(p, fails, rerun)
	must(t, err)
	if len(got.Failed) != 0 || got.Green || !slices.Equal(reruns, []string{"42"}) {
		t.Fatalf("infra failure: poll = %+v, reruns %v; want it rerun once, not red", got, reruns)
	}
	if ev := ciEvents(t, a, EventCIRerun); len(ev) != 1 {
		t.Fatalf("ci_rerun events = %v, want one", ev)
	}
	// It fails the same way after the rerun: now it counts.
	got, err = a.ScreenCIPoll(p, fails, rerun)
	must(t, err)
	if !slices.Equal(got.Failed, []string{"CI / ci"}) || len(reruns) != 1 {
		t.Fatalf("after one rerun: poll = %+v, reruns %v; want it red, no second rerun", got, reruns)
	}

	// A real failure is red straight away.
	p = CIPoll{Task: "t2", PR: "2", Head: "ccc", Failed: []string{"CI / ci"}}
	fails = []CIRedFailure{{Check: "CI / ci", RunURL: runURL, LogTail: "--- FAIL: TestThing"}}
	if got, _ = a.ScreenCIPoll(p, fails, rerun); !slices.Equal(got.Failed, []string{"CI / ci"}) {
		t.Fatalf("real failure screened out: %+v", got)
	}
}
