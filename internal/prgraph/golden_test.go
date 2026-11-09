package prgraph

import (
	"fmt"
	"testing"
	"time"
)

func indexOf(p Plan, match func(Action) bool) int {
	for i, a := range p.Actions {
		if match(a) {
			return i
		}
	}
	return -1
}

// golden1105 is 2026-11-05: t395-t400 form one stack, t395 <- t396 <- t397
// <- t398 <- t399, plus t400 alone on main. t397 (#814) uses logErr from
// t396 (a verified edge from the gate), yet its PR was laid on main because
// its commits applied there. t398 and t399 sit on it and went red.
func golden1105() *world {
	w := &world{
		now: t0, bases: map[string]string{"main": "m1"}, heads: map[string]string{},
		inBase: map[string]bool{}, native: true,
		required: []RequiredCheck{{Base: "main", Name: "ci"}},
	}
	w.st.Policy = DefaultPolicy()
	w.st.Policy.HostChecks = []string{"host/order"}
	pr := map[string]int{"t395": 811, "t396": 812, "t397": 814, "t398": 815, "t399": 816, "t400": 817}
	base := map[string]string{"t395": "main", "t396": "b/t395", "t397": "main", "t398": "b/t397", "t399": "b/t398", "t400": "main"}
	for i, id := range []string{"t395", "t396", "t397", "t398", "t399", "t400"} {
		st := Green
		if id == "t398" || id == "t399" {
			st = Red
		}
		c := Change{ID: id, Branch: "b/" + id, Base: "main", Head: "h" + id, Seq: int64(395 + i), PR: pr[id],
			State: st, Automerge: true, Gen: 1, Deadline: t0.Add(30 * 60e9)}
		if st == Red {
			c.Reason = CheckCause("ci")
		}
		w.st.Changes = append(w.st.Changes, c)
		w.heads[c.Branch] = c.Head
		w.prs = append(w.prs, PR{Number: pr[id], Branch: c.Branch, Head: c.Head, Base: base[id], State: PROpen, Mergeable: MergeClean})
		status := CheckSuccess
		if st == Red {
			status = CheckFailure
		}
		w.checks = append(w.checks, Check{Head: c.Head, Name: "ci", Status: status, Required: true},
			Check{Head: c.Head, Name: "host/order", Status: CheckFailure, Required: true, HostOwned: true})
	}
	w.relink()
	w.st.Edges = []Edge{
		{Change: "t396", Dep: "t395", Kind: Declared, Source: "after"},
		{Change: "t397", Dep: "t396", Kind: Verified, Source: "gate"},
		{Change: "t398", Dep: "t397", Kind: Declared, Source: "after"},
		{Change: "t399", Dep: "t398", Kind: Declared, Source: "after"},
	}
	w.checks = append(w.checks, Check{Head: "m1", Name: "ci", Status: CheckSuccess, Required: true})
	return w
}

func TestGolden20261105MovesBeforeMerge(t *testing.T) {
	w := golden1105()
	p := Reconcile(w.input())
	t.Log(p.Actions)
	mv := indexOf(p, func(a Action) bool {
		return a.Kind == ActMove && a.PR == 814 && a.Base == "b/t396" && a.Stack == "t395"
	})
	if mv < 0 {
		t.Fatalf("want move(#814, b/t396, stack t395); plan %v", p.Actions)
	}
	if m := indexOf(p, func(a Action) bool { return a.Kind == ActMerge }); m >= 0 && m < mv {
		t.Errorf("merge %v comes before the move", p.Actions[m])
	}
	for _, a := range p.Actions {
		if a.Kind == ActNeedsFix {
			t.Errorf("%v: t398/t399 are red because #814 is on the wrong base; that is a move, not a fix", a)
		}
		if a.Kind == ActMerge && a.Change != "t395" && a.Change != "t400" {
			t.Errorf("%v: only t395 and t400 have no unmerged deps", a)
		}
	}
	// And the world heals: apply plans until nothing is left to do.
	w.ci = ciGreen
	for range 20 {
		w.tick()
		w.now = w.now.Add(10 * time.Minute)
	}
	for _, c := range w.st.Changes {
		if c.State != Merged {
			t.Errorf("%s ended %s(%s)", c.ID, c.State, c.Reason)
		}
	}
}

// #726: a required check red on base gives one base repair, not one per
// change.
func TestGoldenBaseRedOneRepair(t *testing.T) {
	w := golden1105()
	for i := range w.checks {
		if w.checks[i].Head == "m1" {
			w.checks[i].Status = CheckFailure
		}
		if w.checks[i].Name == "ci" && w.checks[i].Head != "m1" {
			w.checks[i].Status = CheckFailure
		}
	}
	p := Reconcile(w.input())
	n := 0
	for _, a := range p.Actions {
		if a.Kind == ActNeedsFix {
			if a.Change != "" {
				t.Errorf("%v: base is red, not this change", a)
			}
			n++
		}
	}
	if n != 1 {
		t.Errorf("want one base repair, got %d: %v", n, p.Actions)
	}
}

// #651, #662: a repair is a new head on the same change, never a new
// change stacked on the red one. The NeedsFix count survives the new head,
// so repairs of repairs are capped.
func TestGoldenRepairIsANewHead(t *testing.T) {
	w := golden1105()
	c := w.change("t400")
	c.State, c.Reason = Red, CheckCause("ci")
	w.setCheck(Check{Head: "ht400", Name: "ci", Status: CheckFailure, Required: true})
	p := Reconcile(w.input())
	if indexOf(p, func(a Action) bool { return a.Kind == ActNeedsFix && a.Change == "t400" }) < 0 {
		t.Fatalf("want needs_fix(t400); plan %v", p.Actions)
	}
	w.tick()
	w.heads["b/t400"] = "ht400-fix"
	p = Reconcile(w.input())
	i := indexOf(p, func(a Action) bool {
		return a.Kind == ActTransition && a.Change == "t400" && a.To == Queued && a.Head == "ht400-fix"
	})
	if i < 0 {
		t.Fatalf("want transition(t400, queued) with the new head; plan %v", p.Actions)
	}
	// The cap: after RepairAttempts fixes the change gives up.
	w2 := golden1105()
	c = w2.change("t400")
	c.State, c.Reason = Red, CheckCause("ci")
	w2.setCheck(Check{Head: "ht400", Name: "ci", Status: CheckFailure, Required: true})
	w2.st.Attempts = []Attempt{{Key: "needs_fix/t400", Kind: "needs_fix", N: 3, Last: OutcomeOK}}
	p = Reconcile(w2.input())
	if indexOf(p, func(a Action) bool {
		return a.Kind == ActTransition && a.Change == "t400" && a.To == Superseded && a.Reason == SupersededGaveUp
	}) < 0 {
		t.Errorf("want superseded(gave_up) at the cap; plan %v", p.Actions)
	}
	if indexOf(p, func(a Action) bool { return a.Kind == ActEscalate && a.Change == "t400" }) < 0 {
		t.Errorf("want one escalation at the cap; plan %v", p.Actions)
	}
}

// #751: changes with no dependencies are stacks of one on main, never a
// deep chain in submission order.
func TestGoldenNoChainBySubmissionOrder(t *testing.T) {
	w := &world{now: t0, bases: map[string]string{"main": "m1"}, heads: map[string]string{}, inBase: map[string]bool{}}
	w.st.Policy = DefaultPolicy()
	for i := range 17 {
		id := fmt.Sprintf("c%02d", i)
		w.st.Changes = append(w.st.Changes, Change{ID: id, Branch: "b/" + id, Base: "main", Head: "h" + id, Seq: int64(i), State: Landed, Gen: 1, Automerge: true})
		w.heads["b/"+id] = "h" + id
		w.gates = append(w.gates, GateResult{Head: "h" + id, Onto: Onto{Base: "main", BaseTip: "m1"}, Status: GatePass})
	}
	for _, a := range Reconcile(w.input()).Actions {
		if a.Kind == ActPublish && a.Base != "main" {
			t.Errorf("%v: no deps, so its PR goes on main", a)
		}
	}
}
