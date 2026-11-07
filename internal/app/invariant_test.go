package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/invariants"
	"github.com/brandonapol/saddle/internal/store"
)

// violationCounts counts the invariant_violated events of kind and the
// orchestrator's undelivered invariant notices.
func violationCounts(t *testing.T, a *App, kind invariants.Kind) (events, allEvents, notices int) {
	t.Helper()
	evs, err := a.Store.Events(-1)
	must(t, err)
	for _, e := range evs {
		if e.Kind == EventInvariantViolated {
			allEvents++
			if strings.HasPrefix(e.Data, string(kind)+" ") {
				events++
			}
		}
	}
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	for _, n := range ns {
		if strings.HasPrefix(n.Text, "Invariant violated") {
			notices++
		}
	}
	return events, allEvents, notices
}

// reportsOnce runs the monitor twice at now and asserts the fixture
// violates exactly kind on subject: one event and one notice, and the
// second tick repeats neither.
func reportsOnce(t *testing.T, a *App, now time.Time, kind invariants.Kind, subject string) {
	t.Helper()
	for tick := 1; tick <= 2; tick++ {
		fresh, err := a.CheckInvariants(now.Add(time.Duration(tick) * time.Minute))
		must(t, err)
		if tick == 1 && (len(fresh) != 1 || fresh[0].Kind != kind || fresh[0].Subject != subject) {
			t.Fatalf("tick 1 reported %+v, want one %s on %s", fresh, kind, subject)
		}
		if tick == 2 && len(fresh) != 0 {
			t.Fatalf("tick 2 reported again: %+v", fresh)
		}
		ev, all, ns := violationCounts(t, a, kind)
		if ev != 1 || all != 1 || ns != 1 {
			t.Fatalf("tick %d: %d %s events, %d in all, %d notices; want 1, 1, 1", tick, ev, kind, all, ns)
		}
	}
	open, err := a.OpenViolations()
	must(t, err)
	if len(open) != 1 || open[0].Kind != kind {
		t.Fatalf("open = %+v", open)
	}
	if ls := a.ViolationLines(); len(ls) != 1 || !strings.Contains(ls[0], string(kind)) {
		t.Fatalf("status lines = %q", ls)
	}
}

// I1, #819: a task that landed with no PR more than ten minutes ago.
func TestInvariantMonitorDoneNotOnMain(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	now := time.Now().UTC()
	if fresh, err := a.CheckInvariants(now); err != nil || len(fresh) != 0 {
		t.Fatalf("just landed: %+v, %v", fresh, err)
	}
	reportsOnce(t, a, now.Add(11*time.Minute), invariants.DoneNotOnMain, "t1")
}

// I5, #784: a killed task that something set back to conflict.
func TestInvariantMonitorKilledTaskBackInConflict(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.Store.CreateTask(store.Task{ID: "t5", Title: "x", Role: store.RoleWorker, Status: store.Running}))
	a.Store.Event("t5", "kill", "")
	must(t, a.Store.Revive("t5", store.Conflict))
	reportsOnce(t, a, time.Now().UTC().Add(time.Second), invariants.StateWithoutExit, "task t5")
}

// I4: a ci-red layer someone acked: nothing retries it.
func TestInvariantMonitorAckedCIRedIsLatched(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t2", PR: "https://github.com/o/r/pull/2", Checks: []string{"test"}, Acked: true}}}))
	reportsOnce(t, a, time.Now().UTC(), invariants.Latched, "ci-red t2")
}

// I7, #820: a repair spawned for saddle/base, which saddle sets itself.
func TestInvariantMonitorRepairOfSaddleBase(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.Store.CreateTask(store.Task{ID: "t7", Title: "fix", Role: store.RoleWorker, Status: store.Running}))
	must(t, a.SetCIRed(CIRedState{Repairs: []CIRepairRecord{{Task: "t2", Head: "abc", Repair: "t7", State: ciRepairSpawned,
		Fails: []CIRedFailure{{Check: "saddle/base"}}}}}))
	reportsOnce(t, a, time.Now().UTC(), invariants.SelfRepair, "t7")
}

// I8: t402 was spawned on grok while [adapters] order left grok out.
func TestInvariantMonitorSpawnOnExcludedAdapter(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.AdapterOrder, a.Cfg.Harness = []string{"claude", "codex"}, "claude"
	must(t, a.Store.CreateTask(store.Task{ID: "t402", Title: "repair", Role: store.RoleWorker, Status: store.Running}))
	a.Store.Event("t402", "spawn", "parent=t0 adapter=grok (repair pick) model=grok-4 claims=internal/app/x.go")
	reportsOnce(t, a, time.Now().UTC(), invariants.PolicyBypass, "t402")
}

// I3, #814: t2 used code from t1, whose PR hadn't merged, but its PR was
// opened on main. Not parallel: it stubs the forge read.
func TestInvariantMonitorPROnMainAboveUnmergedDependency(t *testing.T) {
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	git(t, a.Root, "remote", "add", "origin", t.TempDir())
	must(t, a.Store.SetField("t1", "pr", "https://github.com/o/r/pull/1"))
	must(t, a.Store.SetField("t2", "pr", "https://github.com/o/r/pull/2"))
	must(t, a.saveBase("t1", "main"))
	must(t, a.saveBase("t2", "main"))
	write(t, a.Root, ".saddle/deps/t2", "t1\n")
	old := invariantOpenPRs
	t.Cleanup(func() { invariantOpenPRs = old })
	invariantOpenPRs = func(*App, context.Context) ([]forge.PR, error) {
		return []forge.PR{
			{URL: "https://github.com/o/r/pull/1", Head: "saddle/t1", Base: "main", State: forge.StateOpen},
			{URL: "https://github.com/o/r/pull/2", Head: "saddle/t2", Base: "main", State: forge.StateOpen},
		}, nil
	}
	reportsOnce(t, a, time.Now().UTC(), invariants.PRBaseWrong, "t2")

	// Fixed: it clears once, and the store's saved base drifting from the
	// live one is a new violation.
	invariantOpenPRs = func(*App, context.Context) ([]forge.PR, error) {
		return []forge.PR{
			{URL: "https://github.com/o/r/pull/1", Head: "saddle/t1", Base: "main", State: forge.StateOpen},
			{URL: "https://github.com/o/r/pull/2", Head: "saddle/t2", Base: a.mustBranch(t, "t1"), State: forge.StateOpen},
		}, nil
	}
	fresh, err := a.CheckInvariants(time.Now().UTC().Add(time.Hour))
	must(t, err)
	if len(fresh) != 1 || fresh[0].Kind != invariants.PRDrift || fresh[0].Subject != "t2" {
		t.Fatalf("after the move: %+v", fresh)
	}
	evs, err := a.Store.Events(-1)
	must(t, err)
	cleared := 0
	for _, e := range evs {
		if e.Kind == EventInvariantCleared && strings.HasPrefix(e.Data, string(invariants.PRBaseWrong)+" t2") {
			cleared++
		}
	}
	if cleared != 1 {
		t.Fatalf("%d cleared events, want 1", cleared)
	}
}

func (a *App) mustBranch(t *testing.T, id string) string {
	t.Helper()
	tk, err := a.Store.Task(id)
	must(t, err)
	return tk.Branch
}

// A violation that clears and comes back is reported again.
func TestInvariantMonitorReportsARecurrence(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now().UTC()
	acked := CIRedState{Red: []CIRedLayer{{Task: "t2", PR: "#2", Acked: true}}}
	must(t, a.SetCIRed(acked))
	_, err := a.CheckInvariants(now)
	must(t, err)
	_, err = a.Store.TakeNotices(OrchestratorID, false) // the orchestrator read it
	must(t, err)
	must(t, a.SetCIRed(CIRedState{}))
	_, err = a.CheckInvariants(now)
	must(t, err)
	if open, _ := a.OpenViolations(); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	must(t, a.SetCIRed(acked))
	fresh, err := a.CheckInvariants(now)
	must(t, err)
	if len(fresh) != 1 {
		t.Fatalf("recurrence: %+v", fresh)
	}
	if _, all, ns := violationCounts(t, a, invariants.Latched); all != 2 || ns != 1 {
		t.Fatalf("%d events, %d new notices; want 2, 1", all, ns)
	}
}

func TestParseSpawnEvent(t *testing.T) {
	sp := parseSpawnEvent("parent=t0 adapter=grok (asked for by spawn) model=grok-4 claims=a.go,b/**")
	if sp.Adapter != "grok" || sp.Model != "grok-4" || len(sp.Claims) != 2 || sp.Claims[1] != "b/**" {
		t.Fatalf("%+v", sp)
	}
	if sp := parseSpawnEvent("parent= adapter=claude (default) model= claims="); sp.Adapter != "claude" || sp.Model != "" || sp.Claims != nil {
		t.Fatalf("%+v", sp)
	}
}
