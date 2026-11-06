package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// gateFixes lists the gate repair tasks spawned for task.
func gateFixes(t *testing.T, a *App, task string) []store.Task {
	t.Helper()
	ts, err := a.Store.Tasks()
	must(t, err)
	var out []store.Task
	for _, x := range ts {
		if strings.HasPrefix(x.Title, "Fix pre-publish gate for "+task+" ") {
			out = append(out, x)
		}
	}
	return out
}

// gateEventCount counts task's events of kind.
func gateEventCount(t *testing.T, a *App, task, kind string) int {
	t.Helper()
	es, err := a.Store.Events(-1)
	must(t, err)
	n := 0
	for _, e := range es {
		if e.Task == task && e.Kind == kind {
			n++
		}
	}
	return n
}

// redAtGate lands gateStack's four layers and runs prs, so t3 is red at
// the gate with its agent gone.
func redAtGate(t *testing.T) *App {
	t.Helper()
	a, _, _ := gateStack(t)
	if _, err := a.PRs(); err == nil {
		t.Fatal("prs published t3, red at its own tip")
	}
	s, err := a.Gate()
	must(t, err)
	if len(s.Red) != 1 || s.Red[0].Task != "t3" {
		t.Fatalf("gate red = %+v, want t3", s.Red)
	}
	return a
}

// #773: a layer red at the pre-publish gate with no live agent gets one
// repair, seeded with the gate's output; a spawn that fails at the cap is
// pending and retried, not forgotten.
func TestGateRedSpawnsRepairAndRetries(t *testing.T) {
	t.Parallel()
	a := redAtGate(t)
	busy := fillCap(t, a)

	if got := a.RetryRepairs(); len(got) != 0 {
		t.Fatalf("spawned %v at the cap", got)
	}
	if n := len(gateFixes(t, a, "t3")); n != 0 {
		t.Fatalf("%d repairs spawned at the cap", n)
	}
	ps := a.PendingRepairs()
	if !slices.ContainsFunc(ps, func(p PendingRepair) bool { return p.Kind == "gate" && p.Task == "t3" && p.AtCap() }) {
		t.Fatalf("pending = %+v, want t3's gate repair waiting on the cap", ps)
	}
	if err := a.repairFirst(); err == nil {
		t.Fatal("a fresh ticket may take the slot ahead of the pending gate repair")
	}

	must(t, a.Kill(busy.ID, false))
	_, _ = a.StallTick(time.Now())
	fx := gateFixes(t, a, "t3")
	if len(fx) != 1 {
		t.Fatalf("repairs after a slot freed = %d, want 1", len(fx))
	}
	for _, want := range []string{"Unknown word (uploaders)", "prepublish", "t3"} {
		if !strings.Contains(fx[0].Prompt, want) {
			t.Errorf("repair prompt lacks %q:\n%s", want, fx[0].Prompt)
		}
	}
	if slices.ContainsFunc(a.PendingRepairs(), func(p PendingRepair) bool { return p.Kind == "gate" }) {
		t.Fatal("gate repair still pending after it spawned")
	}
	a.RetryRepairs()
	if n := len(gateFixes(t, a, "t3")); n != 1 {
		t.Fatalf("repairs after another retry = %d, want still 1", n)
	}
	// Its commits fold into t3's layer like a CI repair's.
	s, err := a.CIRed()
	must(t, err)
	if !slices.ContainsFunc(s.Repairs, func(r CIRepairRecord) bool {
		return r.Task == "t3" && r.Repair == fx[0].ID && r.State == ciRepairSpawned
	}) {
		t.Fatalf("ci-red repairs = %+v, want %s recorded to fold into t3", s.Repairs, fx[0].ID)
	}
}

// #773: when base is red at the same check too, the layer isn't to blame:
// no repair, and one base-red event however often it is retried.
func TestGateRedWithBaseRedSpawnsNoRepair(t *testing.T) {
	t.Parallel()
	a := redAtGate(t)
	s, err := a.Gate()
	must(t, err)
	s.Red[0].Check.Cmd = "echo 'base is broken too'; exit 1"
	must(t, a.setGate(s))
	_ = orchNotices(t, a, "")

	for range 3 {
		a.RetryRepairs()
	}
	if n := len(gateFixes(t, a, "t3")); n != 0 {
		t.Fatalf("%d repairs spawned while base is red too", n)
	}
	if n := gateEventCount(t, a, "t3", eventGateBaseRed); n != 1 {
		t.Fatalf("base-red events = %d, want 1", n)
	}
	if got := orchNotices(t, a, "red on base"); len(got) != 1 {
		t.Fatalf("base-red notices = %q, want one", got)
	}
	if ps := a.PendingRepairs(); slices.ContainsFunc(ps, func(p PendingRepair) bool { return p.Kind == "gate" }) {
		t.Fatalf("pending = %+v, want no gate repair while base is red", ps)
	}
}

// #773: a live agent fixes its own gate red; saddle spawns nothing.
func TestGateRedWithLiveAgentSpawnsNoRepair(t *testing.T) {
	t.Parallel()
	a := redAtGate(t)
	must(t, a.Store.Revive("t3", store.Running))
	a.RetryRepairs()
	if n := len(gateFixes(t, a, "t3")); n != 0 {
		t.Fatalf("%d repairs spawned for a layer whose agent is live", n)
	}
}
