package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

func stuckNotices(t *testing.T, a *App) []store.Notice {
	t.Helper()
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	var out []store.Notice
	for _, n := range ns {
		if strings.HasPrefix(n.Text, "Stuck stack:") {
			out = append(out, n)
		}
	}
	return out
}

// #223 item 6: a stack red for stuck_after with nobody fixing it interrupts
// the orchestrator once per stack and reason, naming the stack, the layer
// and why; it may alert again once that clears and recurs.
func TestCheckStuckAlarmsOncePerStackAndReason(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, _ = originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	now := time.Now().UTC()

	// Red for 10 minutes: not yet.
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t2", PR: "https://github.com/o/r/pull/2", Head: "abc", Checks: []string{"CI / test"}, Since: now.Add(-10 * time.Minute)}}}))
	if al, err := a.CheckStuck(now); err != nil || len(al) != 0 {
		t.Fatalf("10 minutes red: %+v, %v", al, err)
	}
	// 31 minutes in, nobody fixing: one interrupt.
	al, err := a.CheckStuck(now.Add(21 * time.Minute))
	must(t, err)
	if len(al) != 1 || al[0].Stack != "t1" || al[0].Layer != "t2" || al[0].Reason != StuckCIRed {
		t.Fatalf("alarms = %+v, want stack t1 layer t2 ci-red", al)
	}
	ns := stuckNotices(t, a)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "layer t2") || !strings.Contains(ns[0].Text, "CI / test") || !strings.Contains(ns[0].Text, "from t1") {
		t.Fatalf("notices = %+v", ns)
	}
	if class, _ := ClassifyNotice(ns[0].Kind, ns[0].Text, false); class != NoticeInterrupt {
		t.Fatalf("stuck stack is %s, want interrupt", class)
	}
	// Deduped: the same stack and reason, even on another layer, stays quiet.
	s, _ := a.CIRed()
	s.Red = append(s.Red, CIRedLayer{Task: "t3", Head: "def", Checks: []string{"CI / lint"}, Since: now.Add(-time.Hour)})
	must(t, a.SetCIRed(s))
	if al, _ := a.CheckStuck(now.Add(40 * time.Minute)); len(al) != 0 || len(stuckNotices(t, a)) != 0 {
		t.Fatalf("repeat alarm: %+v", al)
	}
	// Another reason on the same stack is news.
	must(t, a.recordGate([]GateRed{{Task: "t2", Head: "abc", Check: GateCheck{"prepublish", "make check/spelling"}}}, nil, GateSourcePRs))
	g, _ := a.Gate()
	g.Red[0].Since = now.Add(-time.Hour)
	must(t, a.setGate(g))
	if al, _ := a.CheckStuck(now.Add(41 * time.Minute)); len(al) != 1 || al[0].Reason != StuckPrepublish {
		t.Fatalf("prepublish alarm = %+v", al)
	}
	_ = stuckNotices(t, a)

	// Clears, then goes red again: a new alarm.
	must(t, a.SetCIRed(CIRedState{}))
	if al, _ := a.CheckStuck(now.Add(42 * time.Minute)); len(al) != 0 {
		t.Fatalf("cleared: %+v", al)
	}
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t2", Head: "ghi", Checks: []string{"CI / test"}, Since: now}}}))
	if al, _ := a.CheckStuck(now.Add(80 * time.Minute)); len(al) != 1 || al[0].Reason != StuckCIRed {
		t.Fatalf("red again: %+v", al)
	}
}

// A live repair task fixing the layer keeps the alarm quiet; an at-risk
// flag, with no time of its own, starts its clock when first seen.
func TestCheckStuckQuietWhileFixingAndClocksFlag(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.StuckAfter = 5 * time.Minute
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	fixer, err := a.Spawn(SpawnReq{ID: "t9", Title: "fix t1"})
	must(t, err)
	now := time.Now().UTC()
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t1", Head: "abc", Checks: []string{"CI / test"}, Since: now.Add(-time.Hour), Repair: fixer.ID}}}))
	if al, err := a.CheckStuck(now); err != nil || len(al) != 0 {
		t.Fatalf("a live repair is fixing it: %+v, %v", al, err)
	}
	must(t, a.SetCIRed(CIRedState{}))

	must(t, a.SetFlag(StackFlag{Task: "t1", Cause: "t1's commit conflicts with main in one.txt"}))
	if al, _ := a.CheckStuck(now); len(al) != 0 {
		t.Fatalf("flag just seen: %+v", al)
	}
	al, err := a.CheckStuck(now.Add(6 * time.Minute))
	must(t, err)
	if len(al) != 1 || al[0].Reason != StuckAtRisk || !strings.Contains(al[0].Detail, "conflicts") {
		t.Fatalf("flag alarm = %+v", al)
	}
}
