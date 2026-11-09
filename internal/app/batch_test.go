package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/automerge"
)

// batchOfThree lands t1..t3 under output = "batch" and publishes them. t3
// edits the file t1 does, which under "stack" or "per-task" would replay or
// re-stack it; in a batch every layer is the commit the train landed.
func batchOfThree(t *testing.T) (*App, *fakeHub, func() []string) {
	t.Helper()
	a := trainSetup(t)
	a.Cfg.Train.Output = OutputBatch
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "one again", map[string]string{"one.txt": "one\nagain\n"})
	_, err := a.PRs()
	must(t, err)
	h := newFakeHub(t, a, origin)
	t1, t2, t3 := mustTask(t, a, "t1"), mustTask(t, a, "t2"), mustTask(t, a, "t3")
	h.base[t1.PR], h.base[t2.PR], h.base[t3.PR] = "main", t1.Branch, t2.Branch
	return a, h, ghLog
}

// #913: a batch is one linear stack in train order, each layer published at
// the very commit the train landed and tested: no replay, no re-cut, no
// stacking by issue, task text or conflicts.
func TestBatchPublishesLandedCommitsInTrainOrder(t *testing.T) {
	t.Parallel()
	a, h, ghLog := batchOfThree(t)
	stack, err := a.landedStack()
	must(t, err)
	if len(stack) != 3 {
		t.Fatalf("stack = %d layers, want 3", len(stack))
	}
	for _, l := range stack {
		if got := remoteRev(t, h.origin, l.Branch); got != l.To {
			t.Errorf("%s published at %s, want the landed commit %s", l.ID, got, l.To)
		}
	}
	calls := ghLog()
	t1, t2 := mustTask(t, a, "t1"), mustTask(t, a, "t2")
	for _, want := range []string{"--base main --head " + t1.Branch, "--base " + t1.Branch + " --head " + t2.Branch} {
		if !slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "pr create") && strings.Contains(c, want) }) {
			t.Errorf("no pr create with %q:\n%s", want, strings.Join(calls, "\n"))
		}
	}
}

// #913: in a batch a stacked layer's saddle/base is green. It merges by the
// batch's fast-forward, not by its own merge button, so a red X there only
// hid real failures and left GitHub calling every PR UNSTABLE.
func TestBatchBaseStatusIsGreen(t *testing.T) {
	t.Parallel()
	a, h, ghLog := batchOfThree(t)
	sha := remoteRev(t, h.origin, mustTask(t, a, "t2").Branch)
	for _, c := range ghLog() {
		if strings.Contains(c, "statuses/"+sha) && strings.Contains(c, "context=saddle/base") && !strings.Contains(c, "state=success") {
			t.Fatalf("t2's saddle/base isn't green: %s", c)
		}
	}
}

// #913: merge once. With t1 red, t2 green and t3 pending, the batch merge
// fast-forwards main to t2's commit itself: no merge button, nothing
// rewritten, so GitHub sees t1's and t2's PRs merged. Both go to main first
// so GitHub can tell, both tasks are merged, and t3 now targets main.
func TestBatchMergeFastForwardsMainToHighestGreen(t *testing.T) {
	t.Parallel()
	a, h, ghLog := batchOfThree(t)
	t1, t2, t3 := mustTask(t, a, "t1"), mustTask(t, a, "t2"), mustTask(t, a, "t3")
	h.checks[t1.PR] = []string{automerge.ChecksFail}
	h.checks[t2.PR] = []string{automerge.ChecksPass}
	h.checks[t3.PR] = []string{automerge.ChecksPending}
	want := remoteRev(t, h.origin, t2.Branch)

	res, err := a.BatchMerge(h)
	must(t, err)
	if res.PR != t2.PR || !slices.Equal(res.Covered, []string{"t1", "t2"}) || res.Head != want {
		t.Fatalf("batch merge = %+v, want t2's PR at %s covering t1, t2", res, want)
	}
	if got := remoteRev(t, h.origin, "main"); got != want {
		t.Fatalf("origin/main = %s, want t2's commit %s (a fast-forward)", got, want)
	}
	if len(h.merged) != 0 {
		t.Fatalf("used the merge button on %v; a batch fast-forwards", h.merged)
	}
	calls := ghLog()
	if !contains(calls, "pr edit "+t2.PR+" --base main") {
		t.Errorf("t2's PR wasn't retargeted to main before the push:\n%s", strings.Join(calls, "\n"))
	}
	if !contains(calls, "pr edit "+t3.PR+" --base main") {
		t.Errorf("t3's PR wasn't retargeted to main after the merge:\n%s", strings.Join(calls, "\n"))
	}
	for _, id := range []string{"t1", "t2"} {
		if got := trainState(t, a, id); got != TrainMerged {
			t.Errorf("%s = %s, want merged", id, got)
		}
	}
	if got := trainState(t, a, "t3"); got == TrainMerged {
		t.Errorf("t3 = merged; its CI is still pending")
	}
	if remoteRev(t, h.origin, t3.Branch) == "" {
		t.Error("t3's branch is gone")
	}
}

// Nothing green, nothing merges, and the result says why.
func TestBatchMergeWaitsForAGreenLayer(t *testing.T) {
	t.Parallel()
	a, h, _ := batchOfThree(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		h.checks[mustTask(t, a, id).PR] = []string{automerge.ChecksPending}
	}
	before := remoteRev(t, h.origin, "main")
	res, err := a.BatchMerge(h)
	must(t, err)
	if res.PR != "" || res.Why == "" {
		t.Fatalf("batch merge = %+v, want nothing merged and a reason", res)
	}
	if got := remoteRev(t, h.origin, "main"); got != before {
		t.Fatalf("main moved to %s with nothing green", got)
	}
}

// A layer labeled needs-human stops the batch below it: the merge carries
// only what is under the first such layer.
func TestBatchMergeStopsBelowNeedsHuman(t *testing.T) {
	t.Parallel()
	a, h, _ := batchOfThree(t)
	t1, t2, t3 := mustTask(t, a, "t1"), mustTask(t, a, "t2"), mustTask(t, a, "t3")
	h.checks[t1.PR] = []string{automerge.ChecksPass}
	h.checks[t2.PR] = []string{automerge.ChecksPass}
	h.checks[t3.PR] = []string{automerge.ChecksPass}
	h.labels[t2.PR] = []string{automerge.NeedsHuman}
	res, err := a.BatchMerge(h)
	must(t, err)
	if res.PR != t1.PR || !slices.Equal(res.Covered, []string{"t1"}) {
		t.Fatalf("batch merge = %+v, want only t1", res)
	}
}

// #913/#912: in a batch, red CI on a layer holds nothing above it and
// freezes no files. A later layer is how it goes green, and the merge
// carries it past.
func TestBatchCIRedHoldsNothing(t *testing.T) {
	t.Parallel()
	a, _, _ := batchOfThree(t)
	stack, err := a.landedStack()
	must(t, err)
	red := []CIRedLayer{{Task: "t1", PR: mustTask(t, a, "t1").PR, Head: stack[0].To, Checks: []string{"CI / test"}}}
	must(t, a.ciRedHolds(stack, red))
	if len(red[0].Held) != 0 {
		t.Fatalf("red t1 holds %v in a batch", red[0].Held)
	}
	must(t, a.SetCIRed(CIRedState{Red: red}))
	if frozen, _ := a.ciRedFrozen(); len(frozen) != 0 {
		t.Fatalf("frozen = %v in a batch", frozen)
	}
	if why := a.CIRedCovers("t2"); why != "" {
		t.Fatalf("t2 covered by red below it in a batch: %s", why)
	}
}

// #912: a red layer with a green layer above it in the batch needs no
// repair: the batch merge carries it. Seven repairs were spent this way.
func TestBatchRepairSkippedWhenALayerAboveIsGreen(t *testing.T) {
	t.Parallel()
	a, _, _ := batchOfThree(t)
	stack, err := a.landedStack()
	must(t, err)
	t1 := mustTask(t, a, "t1")
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t1", PR: t1.PR, Head: stack[0].To, Checks: []string{"CI / test"}}}}))
	last := automerge.Status{Stacks: []automerge.Stack{{ID: "t1", Nodes: []automerge.Node{
		{Task: "t1", PR: t1.PR, Checks: automerge.ChecksFail},
		{Task: "t2", PR: mustTask(t, a, "t2").PR, Checks: automerge.ChecksPass},
		{Task: "t3", PR: mustTask(t, a, "t3").PR, Checks: automerge.ChecksFail},
	}}}}
	must(t, automerge.State{Last: last}.Save(a.automergePath()))

	note, err := a.CIRedRepair("t1", []CIRedFailure{{Check: "CI / test"}})
	must(t, err)
	if !strings.Contains(note, "t2") || !strings.Contains(note, "carries") {
		t.Fatalf("note = %q, want it to say t2 carries t1", note)
	}
	ts, err := a.Store.Tasks()
	must(t, err)
	if len(ts) != 3 {
		t.Fatalf("spawned a repair: %d tasks", len(ts))
	}

	// The top layer is red with nothing above it: that one is repaired.
	if c, _ := a.batchCarrier("t3"); c != "" {
		t.Fatalf("t3 carried by %q; nothing is above it", c)
	}
}
