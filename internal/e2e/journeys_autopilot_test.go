//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/autopilot"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// usageGate stands in for plan-limit pressure the test turns on and off.
type usageGate struct {
	autopilot.Env
	pause  bool
	resets time.Time
}

func (g *usageGate) Usage(now time.Time) (autopilot.Usage, error) {
	if g.pause {
		return autopilot.Usage{Percent: 1, Pause: true, ResetsAt: g.resets}, nil
	}
	return g.Env.Usage(now)
}

// TestJourneyAutopilotDrainsReadyQueue (#256): with nobody at the wheel,
// autopilot drains a six-issue ready queue with disjoint claims two at a
// time (the cap), resumes a task whose window was lost, sleeps through a
// plan-limit pause until the reset, lands everything and stops with a
// summary that interrupts the orchestrator. The clock is the test's.
func TestJourneyAutopilotDrainsReadyQueue(t *testing.T) {
	w := world(t, Options{Top: "concurrency = 2\n"})
	old := app.OrphanAfter
	app.OrphanAfter = 0
	t.Cleanup(func() { app.OrphanAfter = old })

	var issues []autopilot.Issue
	for n := 1; n <= 6; n++ {
		dir := fmt.Sprintf("area%d", n)
		issues = append(issues, autopilot.Issue{Number: n, Title: "Area " + dir, Body: "Work in " + dir + ".\nclaims: " + dir + "/**"})
		// Autopilot spawns issues oldest first, so #n becomes tn.
		steps := finished(dir, dir+"\n")
		if n == 1 {
			steps = []fa.Step{fa.Wait("never-comes")}
		}
		must(t, fa.Script{Steps: steps}.Save(w.Scripts, fmt.Sprintf("t%d", n)))
	}
	a := w.App()
	a.Bin = w.Bins.Saddle // agents' hooks run saddle, not this test binary
	d := a.NewAutopilot(func(label string) ([]autopilot.Issue, error) {
		if label != autopilot.DefaultReadyLabel {
			return nil, fmt.Errorf("label %q", label)
		}
		return issues, nil
	})
	gate := &usageGate{Env: d.Env}
	d.Env = gate
	now := time.Date(2026, 10, 8, 22, 0, 0, 0, time.Local)
	d.Now = func() time.Time { return now }
	_, err := d.Enable(autopilot.Options{})
	must(t, err)

	live := func() []string {
		ts, err := a.Store.Tasks()
		must(t, err)
		var out []string
		for _, tk := range ts {
			if tk.Role == store.RoleWorker && (tk.Status == store.Running || tk.Status == store.Idle || tk.Status == store.NeedsYou) {
				out = append(out, tk.ID)
			}
		}
		return out
	}
	tick := func() autopilot.Report {
		t.Helper()
		rep, err := d.Tick()
		must(t, err)
		if l := live(); len(l) > 2 {
			t.Fatalf("over the cap after a tick: %v live (%s)", l, rep.Decision)
		}
		now = now.Add(autopilot.DefaultInterval)
		return rep
	}
	// settle waits until no worker is left running: each is done or landed.
	settle := func() {
		t.Helper()
		Eventually(t, "every task to finish", func() error {
			if l := live(); len(l) > 0 {
				return errorf("still running: %v", l)
			}
			return nil
		})
	}
	var all []int
	collect := func(rep autopilot.Report) []int {
		got := spawnedIssues(rep)
		all = append(all, got...)
		return got
	}

	rep := tick()
	if got := collect(rep); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("first tick spawned %v, want #1 and #2 (cap 2)", got)
	}
	w.WaitStatus("t1", "idle")
	w.WaitTask("t2", "done", func(v mcpserver.TaskView) bool { return v.Status == "done" })

	// t1 loses its window; the next tick resumes it with its brief, lands
	// t2 and fills its slot. The cap is checked after every tick.
	rescript(w, "t1", finished("area1", "area1\n")...)
	t1, err := a.Store.Task("t1")
	must(t, err)
	must(t, a.Tmux.KillWindow(t1.Window))
	rep = tick()
	if !slices.Contains(rep.Resumed, "t1") || rep.Landed != 1 {
		t.Fatalf("second tick: resumed %v, landed %d; want t1 resumed and t2 landed", rep.Resumed, rep.Landed)
	}
	if got := collect(rep); len(got) == 0 || got[0] != 3 {
		t.Fatalf("second tick spawned %v, want #3 next", got)
	}
	w.WaitAgentLog("t1", "may already exist in the worktree")
	settle()

	// Plan-limit pressure: no spawns until the reset, then it tops up again.
	gate.pause, gate.resets = true, now.Add(time.Hour)
	rep = tick()
	if !rep.Sleeping || len(rep.Spawned) != 0 {
		t.Fatalf("under a usage pause: %+v", rep)
	}
	gate.pause = false
	if rep = tick(); len(rep.Spawned) != 0 {
		t.Fatalf("woke before the reset: %+v", rep)
	}
	now = gate.resets
	for range 10 {
		settle()
		if rep = tick(); rep.Stopped != "" {
			break
		}
		collect(rep)
	}
	if rep.Stopped != "queue empty" {
		t.Fatalf("last tick: %+v", rep)
	}
	if !slices.Equal(all, []int{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("spawned %v, want each issue once, oldest first", all)
	}

	for n := 1; n <= 6; n++ {
		if v := w.Task(fmt.Sprintf("t%d", n)); v.Status != "landed" {
			t.Errorf("t%d is %s, want landed", n, v.Status)
		}
	}
	st, err := d.Status()
	must(t, err)
	if st.On || !strings.Contains(st.Summary, "spawned 6 tasks") {
		t.Fatalf("final state = %+v", st)
	}
	ns, err := a.Store.PeekNotices(app.OrchestratorID, false)
	must(t, err)
	if !slices.ContainsFunc(ns, func(n store.Notice) bool {
		return n.Kind == store.NoticeAction && strings.HasPrefix(n.Text, "autopilot stopped: queue empty")
	}) {
		t.Fatalf("no stop interrupt for the orchestrator: %+v", ns)
	}
	for _, kind := range []string{autopilot.EventSleep, autopilot.EventWake, autopilot.EventStopped} {
		if len(events(t, a, kind)) == 0 {
			t.Errorf("no %s event", kind)
		}
	}
}

func spawnedIssues(rep autopilot.Report) []int {
	var out []int
	for _, s := range rep.Spawned {
		out = append(out, s.Issue)
	}
	return out
}
