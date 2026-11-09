package prgraph

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"
)

func qc(id string, seq int64) QueueChange {
	return QueueChange{ID: id, Seq: seq, Branch: "b/" + id, Head: "h" + id, PR: int(seq), PRState: PROpen,
		PRBase: "main", Mergeable: MergeClean, Checks: CheckSuccess}
}

func qin(cs ...QueueChange) QueueInput {
	return QueueInput{Now: t0, Base: "main", BaseTip: "m1", Merging: true, Method: "squash", Settle: time.Minute, Changes: cs}
}

func effectful(p Plan) []Action { return p.Effectful() }

// #935: candidates go in owner order, then oldest first; the first is a
// merge, and nothing is rebased when nothing is behind.
func TestQueueOrderAndMerge(t *testing.T) {
	a, b, c := qc("t1", 1), qc("t2", 2), qc("t3", 3)
	c.Order = 1
	got := effectful(Queue(qin(a, b, c)))
	if len(got) != 3 {
		t.Fatalf("plan %v", got)
	}
	for i, want := range []string{"t3", "t1", "t2"} {
		if got[i].Kind != ActMerge || got[i].Change != want || got[i].Method != "squash" {
			t.Errorf("action %d = %v, want merge(%s)", i, got[i], want)
		}
	}
}

// #935: red CI skips that PR and the green one behind it is next.
func TestQueueRedDoesNotBlockGreen(t *testing.T) {
	a, b := qc("t1", 1), qc("t2", 2)
	a.Checks = CheckFailure
	got := effectful(Queue(qin(a, b)))
	if len(got) != 1 || got[0].Kind != ActMerge || got[0].Change != "t2" {
		t.Fatalf("plan %v, want merge(t2) only", got)
	}
}

// #935: behind or conflicting is a rebase onto base, not a merge; pending,
// unknown mergeability and settling wait.
func TestQueueBehindRebasesPendingWaits(t *testing.T) {
	a, b, c, d := qc("t1", 1), qc("t2", 2), qc("t3", 3), qc("t4", 4)
	a.Behind = true
	b.Mergeable = MergeConflicting
	c.Checks = CheckPending
	d.Checks, d.PushedAt = "", t0.Add(-10*time.Second)
	p := Queue(qin(a, b, c, d))
	got := effectful(p)
	if len(got) != 2 || got[0].Kind != ActRebase || got[0].Change != "t1" || got[1].Kind != ActRebase || got[1].Change != "t2" {
		t.Fatalf("plan %v, want rebase(t1), rebase(t2)", got)
	}
	if got[0].Onto == nil || got[0].Onto.BaseTip != "m1" || got[0].Guard.Head != "ht1" {
		t.Errorf("rebase %+v: want onto main@m1 guarded on its head", got[0])
	}
	in := qin(d)
	in.Now = t0.Add(2 * time.Minute)
	if got := effectful(Queue(in)); len(got) != 1 || got[0].Kind != ActMerge {
		t.Errorf("no checks once settled: plan %v, want a merge", got)
	}
}

// #935: drafts, needs-human, held, fix-in-place and merging off are never
// merged.
func TestQueueSkipsWhatItMayNotMerge(t *testing.T) {
	a, b, c, d := qc("t1", 1), qc("t2", 2), qc("t3", 3), qc("t4", 4)
	a.Draft, b.NeedsHuman, c.Held, d.FixInPlace = true, true, true, "conflict"
	if got := effectful(Queue(qin(a, b, c, d))); len(got) != 0 {
		t.Fatalf("plan %v, want nothing", got)
	}
	in := qin(qc("t5", 5))
	in.Merging = false
	if got := effectful(Queue(in)); len(got) != 0 {
		t.Fatalf("merging off: plan %v", got)
	}
}

// #935: base red pauses the queue with one escalation; once told, it only
// waits; green resumes.
func TestQueueBaseRedPauses(t *testing.T) {
	in := qin(qc("t1", 1))
	in.BaseRed = []string{"ci"}
	got := effectful(Queue(in))
	if len(got) != 1 || got[0].Kind != ActEscalate || got[0].Key != QueueBaseRedKey("main") {
		t.Fatalf("plan %v, want one escalation", got)
	}
	in.Paused = true
	if got := effectful(Queue(in)); len(got) != 0 {
		t.Fatalf("paused: plan %v, want nothing", got)
	}
	in.BaseRed = nil
	if got := effectful(Queue(in)); len(got) != 1 || got[0].Kind != ActMerge {
		t.Fatalf("green again: plan %v, want a merge", got)
	}
}

// #935: a stacked PR waits while its after is unmerged, then is moved onto
// base from its old base tip; one stacked on anything but its declared
// after is left alone.
func TestQueueMovesStackedAfterItsBaseMerges(t *testing.T) {
	s := qc("t2", 2)
	s.PRBase, s.After, s.AfterBranch, s.From = "b/t1", "t1", "b/t1", "ht1"
	if got := effectful(Queue(qin(s))); len(got) != 0 {
		t.Fatalf("after unmerged: plan %v", got)
	}
	s.AfterMerged = true
	got := effectful(Queue(qin(s)))
	if len(got) != 1 || got[0].Kind != ActMove || got[0].Base != "main" || got[0].From != "ht1" || got[0].Guard.PRBase != "b/t1" {
		t.Fatalf("plan %v, want move(t2 onto main from ht1)", got)
	}
	s.PRBase = "b/other"
	if got := effectful(Queue(qin(s))); len(got) != 0 {
		t.Fatalf("not on its after: plan %v", got)
	}
}

// #935: a PR the forge reports merged is a transition to merged.
func TestQueueMergedOnForgeTransitions(t *testing.T) {
	a := qc("t1", 1)
	a.PRState = PRMerged
	got := effectful(Queue(qin(a)))
	if len(got) != 1 || got[0].Kind != ActTransition || got[0].To != Merged {
		t.Fatalf("plan %v", got)
	}
}

// Queue is deterministic: shuffling its input changes nothing.
func TestQueueDeterministic(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for range 200 {
		var cs []QueueChange
		for i := range 1 + r.Intn(8) {
			c := qc(string(rune('a'+i)), int64(r.Intn(5)))
			c.Order = r.Intn(3)
			c.Checks = []CheckStatus{CheckSuccess, CheckPending, CheckFailure, ""}[r.Intn(4)]
			c.Behind = r.Intn(4) == 0
			cs = append(cs, c)
		}
		want, _ := json.Marshal(Queue(qin(cs...)))
		r.Shuffle(len(cs), func(i, j int) { cs[i], cs[j] = cs[j], cs[i] })
		if got, _ := json.Marshal(Queue(qin(cs...))); string(got) != string(want) {
			t.Fatalf("shuffled input changed the plan:\n%s\n%s", want, got)
		}
	}
}
