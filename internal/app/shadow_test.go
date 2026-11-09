package app

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

var shadowNow = time.Date(2026, 11, 5, 18, 30, 0, 0, time.UTC)

func shadowTask(id, status, pr string) store.Task {
	return store.Task{ID: id, Title: id, Role: store.RoleWorker, Branch: "saddle/" + id + "-work", Status: status, PR: pr}
}

func changeByID(t *testing.T, st prgraph.State, id string) (prgraph.Change, bool) {
	t.Helper()
	for _, c := range st.Changes {
		if c.ID == id {
			return c, true
		}
	}
	return prgraph.Change{}, false
}

// The adapter maps today's stores (the train table, tasks, deps, owner
// stacks, ci-red.json, automerge holds) onto prgraph's desired state.
func TestShadowInputMapsLegacyStores(t *testing.T) {
	t.Parallel()
	s := ShadowStores{
		Now:  shadowNow,
		Base: "main",
		Tasks: []store.Task{
			shadowTask("t1", store.Landed, "https://github.com/o/r/pull/11"),
			shadowTask("t2", store.Landed, "https://github.com/o/r/pull/12"),
			shadowTask("t3", store.Done, ""),
			shadowTask("t4", store.Done, ""),
			shadowTask("t5", store.Landed, "https://github.com/o/r/pull/15"),
			shadowTask("t6", store.Killed, "https://github.com/o/r/pull/16"),
			shadowTask("t7", store.Landed, ""),
			shadowTask("t8", store.Landed, "https://github.com/o/r/pull/18"),
			shadowTask("t9", store.Conflict, ""),
			shadowTask("t10", store.Killed, ""),
			{ID: "t0", Role: store.RoleOrchestrator},
		},
		Train: []store.TrainEntry{
			{Task: "t1", Seq: 1, State: store.TrainOK, Note: "a..h1"},
			{Task: "t2", Seq: 2, State: store.TrainOK, Note: "h1..h2"},
			{Task: "t3", Seq: 3, State: store.Queued},
			{Task: "t4", Seq: 4, State: store.OnHold},
			{Task: "t5", Seq: 5, State: TrainMerged, Note: "x..h5"},
			{Task: "t6", Seq: 6, State: store.TrainOK, Note: "x..h6"},
			{Task: "t7", Seq: 7, State: TrainFolded, Note: "x..h7"},
			{Task: "t8", Seq: 8, State: TrainMerged, Note: "x..h8"},
			{Task: "t9", Seq: 9, State: store.TrainError},
			{Task: "t10", Seq: 10, State: TrainEscalated, Note: "tests failed (test_failed)"},
		},
		Edges:          []prgraph.Edge{{Change: "t2", Dep: "t1", Kind: prgraph.Declared, Source: "after"}, {Change: "t3", Dep: "t8", Kind: prgraph.Declared, Source: "after"}},
		Stacks:         []CustomStack{{Name: "s", Tasks: []string{"t1", "t3"}}},
		Automerge:      true,
		AutomergeHolds: []string{"t2"},
		CIRed: CIRedState{
			Red:     []CIRedLayer{{Task: "t1", Head: "h1", Checks: []string{"ci / test"}}},
			Repairs: []CIRepairRecord{{Task: "t1", Head: "h1", Repair: "t20", State: ciRepairSpawned}},
		},
		Since: map[string]time.Time{"t1": shadowNow.Add(-time.Hour)},
	}
	git := prgraph.GitFacts{Heads: []prgraph.BranchHead{{Branch: "saddle/t1-work", SHA: "h1"}, {Branch: "saddle/t2-work", SHA: "h2b"}}}
	in := ShadowInput(s, prgraph.ForgeSnapshot{}, git)
	st := in.State
	want := map[string]prgraph.ChangeState{
		"t1": prgraph.Red, "t2": prgraph.Published, "t3": prgraph.Queued, "t4": prgraph.Queued,
		"t6": prgraph.Superseded, "t8": prgraph.Merged, "t9": prgraph.Red,
	}
	for id, w := range want {
		c, ok := changeByID(t, st, id)
		if !ok {
			t.Errorf("%s: no change", id)
			continue
		}
		if c.State != w {
			t.Errorf("%s: state %s, want %s", id, c.State, w)
		}
	}
	// A killed task's change is superseded whatever its train row says
	// (#705: legacy leaves killed tasks escalated or queued).
	t10, _ := changeByID(t, st, "t10")
	if t10.State != prgraph.Superseded || t10.Reason != prgraph.SupersededKilled {
		t.Errorf("killed t10 = %+v, want superseded(killed)", t10)
	}
	for _, id := range []string{"t0", "t5", "t7"} {
		if _, ok := changeByID(t, st, id); ok {
			t.Errorf("%s is a change; want it left out (orchestrator, irrelevant merged, folded)", id)
		}
	}
	t1, _ := changeByID(t, st, "t1")
	if t1.Reason != prgraph.CheckCause("ci / test") || t1.PR != 11 || t1.Head != "h1" || !t1.Automerge {
		t.Errorf("t1 = %+v", t1)
	}
	t2, _ := changeByID(t, st, "t2")
	if t2.Head != "h2b" || t2.Automerge {
		t.Errorf("t2 = %+v; want the branch tip as head and automerge held", t2)
	}
	t4, _ := changeByID(t, st, "t4")
	if !t4.HoldUntil.After(shadowNow) {
		t.Errorf("t4 on hold has no hold: %+v", t4)
	}
	t6, _ := changeByID(t, st, "t6")
	if t6.Reason != prgraph.SupersededKilled {
		t.Errorf("t6 reason = %q", t6.Reason)
	}
	t9, _ := changeByID(t, st, "t9")
	if t9.Reason != prgraph.CauseConflict {
		t.Errorf("t9 reason = %q", t9.Reason)
	}
	hasEdge := func(c, d, src string) bool {
		for _, e := range st.Edges {
			if e.Change == c && e.Dep == d && e.Source == src {
				return true
			}
		}
		return false
	}
	if !hasEdge("t2", "t1", "after") || !hasEdge("t3", "t1", "owner_stack") || !hasEdge("t3", "t8", "after") {
		t.Errorf("edges = %+v", st.Edges)
	}
	var fix prgraph.Attempt
	for _, a := range st.Attempts {
		if a.Key == "needs_fix/t1" {
			fix = a
		}
	}
	if fix.N != 1 {
		t.Errorf("t1's repair isn't an attempt: %+v", st.Attempts)
	}
}

func shadowPublished(id string, pr int, base, head string, checks ...prgraph.Check) (ShadowStores, prgraph.ForgeSnapshot, prgraph.GitFacts) {
	s := ShadowStores{Now: shadowNow, Base: "main", Automerge: true,
		Tasks: []store.Task{shadowTask(id, store.Landed, "https://github.com/o/r/pull/"+strconv.Itoa(pr))},
		Train: []store.TrainEntry{{Task: id, Seq: 1, State: store.TrainOK, Note: "x.." + head}}}
	f := prgraph.ForgeSnapshot{PRs: []prgraph.PR{{Number: pr, Branch: "saddle/" + id + "-work", Head: head, Base: base, State: prgraph.PROpen, Mergeable: prgraph.MergeClean}}, Checks: checks}
	g := prgraph.GitFacts{Bases: []prgraph.BranchHead{{Branch: "main", SHA: "m1"}}, Heads: []prgraph.BranchHead{{Branch: "saddle/" + id + "-work", SHA: head}}}
	return s, f, g
}

func effectful(p prgraph.Plan, kind prgraph.ActionKind) []prgraph.Action {
	var out []prgraph.Action
	for _, a := range p.Actions {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// 2026-11-05: t396 went red only on a check the host posts itself
// (host/order here), and legacy spawned t402 and t403 to repair it. prgraph settles
// the legacy state and asks for no fix; a real red check gets one.
func TestShadowSettleHostCheckNeverNeedsFix(t *testing.T) {
	t.Parallel()
	host := prgraph.Check{Head: "h", Name: "host/order", Status: prgraph.CheckFailure, Required: true, HostOwned: true}
	s, f, g := shadowPublished("t396", 812, "main", "h", host,
		prgraph.Check{Head: "h", Name: "ci / test", Status: prgraph.CheckSuccess, Required: true})
	_, p := SettlePlan(ShadowInput(s, f, g))
	if fx := effectful(p, prgraph.ActNeedsFix); len(fx) != 0 {
		t.Fatalf("needs_fix for a host-owned red: %v", fx)
	}
	if m := effectful(p, prgraph.ActMerge); len(m) != 1 || m[0].PR != 812 {
		t.Errorf("want merge(#812) once the real checks are green; plan %v", p.Actions)
	}
	s, f, g = shadowPublished("t397", 814, "main", "h",
		prgraph.Check{Head: "h", Name: "ci / lint", Status: prgraph.CheckFailure, Required: true})
	_, p = SettlePlan(ShadowInput(s, f, g))
	if fx := effectful(p, prgraph.ActNeedsFix); len(fx) != 1 || fx[0].Change != "t397" {
		t.Errorf("want needs_fix(t397) for a real red; plan %v", p.Actions)
	}
}

// The diff reports a legacy action prgraph's plan doesn't have at once, and
// a prgraph action legacy hasn't matched once the window passes. A matched
// pair reports nothing.
func TestShadowDiff(t *testing.T) {
	t.Parallel()
	d := newShadowDiff(10 * time.Minute)
	move := prgraph.Action{Kind: prgraph.ActMove, Change: "t397", PR: 814, Base: "saddle/t396-work"}
	merge := prgraph.Action{Kind: prgraph.ActMerge, Change: "t1", PR: 11}
	plan := prgraph.Plan{Actions: []prgraph.Action{move, merge, {Kind: prgraph.ActWait, Change: "t2"}}}
	fix := LegacyAction{Action: prgraph.Action{Kind: prgraph.ActNeedsFix, Change: "t396"}, At: shadowNow, Detail: "ci_repair_spawned t402"}
	out := d.Observe(shadowNow, plan, []LegacyAction{fix})
	if len(out) != 1 || out[0].Side != ShadowLegacyOnly || out[0].Change != "t396" || !strings.Contains(out[0].Detail, "t402") {
		t.Fatalf("first tick = %+v; want legacy_only needs_fix(t396)", out)
	}
	merged := LegacyAction{Action: prgraph.Action{Kind: prgraph.ActMerge, Change: "t1", PR: 11}, At: shadowNow.Add(5 * time.Minute)}
	if out := d.Observe(shadowNow.Add(5*time.Minute), plan, []LegacyAction{merged}); len(out) != 0 {
		t.Fatalf("matched merge reported: %+v", out)
	}
	out = d.Observe(shadowNow.Add(11*time.Minute), plan, nil)
	if len(out) != 1 || out[0].Side != ShadowPrgraphOnly || out[0].Change != "t397" || out[0].Kind != prgraph.ActMove {
		t.Fatalf("after the window = %+v; want prgraph_only move(t397)", out)
	}
	if out := d.Observe(shadowNow.Add(30*time.Minute), plan, nil); len(out) != 0 {
		t.Fatalf("a still-planned action is reported again: %+v", out)
	}
}

// Legacy actions are read from the events table.
func TestLegacyActionFromEvents(t *testing.T) {
	t.Parallel()
	pr := map[string]int{"t397": 814}
	cases := []struct {
		ev   store.Event
		want prgraph.Action
	}{
		{store.Event{Task: "t396", Kind: "ci_repair_spawned", Data: "t402"}, prgraph.Action{Kind: prgraph.ActNeedsFix, Change: "t396"}},
		{store.Event{Task: "t397", Kind: "restack_retarget", Data: "https://github.com/o/r/pull/814 main → saddle/t398-x"}, prgraph.Action{Kind: prgraph.ActMove, Change: "t397", PR: 814, Base: "saddle/t398-x"}},
		{store.Event{Task: "t409", Kind: "automerge_merged", Data: "https://github.com/o/r/pull/846 by squash: checks pass"}, prgraph.Action{Kind: prgraph.ActMerge, Change: "t409", PR: 846}},
		{store.Event{Task: "t395", Kind: "pr_layout", Data: "71532e456f14 published as a2e65ad3b597 on main"}, prgraph.Action{Kind: prgraph.ActPublish, Change: "t395", Base: "main"}},
		{store.Event{Task: "t396", Kind: "pr_layout", Data: "8dcb9986174a published as b63678e8f55e on saddle/t395-x (t396 runs after t395)"}, prgraph.Action{Kind: prgraph.ActPublish, Change: "t396", Base: "saddle/t395-x"}},
		{store.Event{Task: "t395", Kind: "gh_stack_link", Data: "t395 → t396 → t398"}, prgraph.Action{Kind: prgraph.ActLink, Change: "t395", Stack: "t395"}},
		{store.Event{Task: "t395", Kind: "restack", Data: "refs/heads/saddle/t395-x 71532e456f14 → bd13584093af (abort=false)"}, prgraph.Action{Kind: prgraph.ActRebase, Change: "t395"}},
		{store.Event{Task: "t392", Kind: "train_test_failed", Data: "tests failed"}, prgraph.Action{Kind: prgraph.ActNeedsFix, Change: "t392", Cause: prgraph.CauseGate}},
		{store.Event{Task: "t3", Kind: "done_gate_started", Data: "t3-x make check"}, prgraph.Action{Kind: prgraph.ActGate, Change: "t3"}},
	}
	for _, c := range cases {
		got, ok := legacyAction(c.ev, pr)
		if !ok {
			t.Errorf("%s %q: not an action", c.ev.Kind, c.ev.Data)
			continue
		}
		if got.Kind != c.want.Kind || got.Change != c.want.Change || got.PR != c.want.PR || got.Base != c.want.Base || got.Stack != c.want.Stack {
			t.Errorf("%s %q = %+v, want %+v", c.ev.Kind, c.ev.Data, got.Action, c.want)
		}
	}
	// A retarget that names no old base is saddle re-setting a base, not a move.
	if _, ok := legacyAction(store.Event{Task: "t395", Kind: "restack_retarget", Data: "https://github.com/o/r/pull/811 → main"}, pr); ok {
		t.Error("a retarget with no old base counted as a move")
	}
	if _, ok := legacyAction(store.Event{Task: "t1", Kind: "tool", Data: "x"}, pr); ok {
		t.Error("a tool event counted as an action")
	}
}
