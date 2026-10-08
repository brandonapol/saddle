//go:build e2e

package e2e

import (
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

// TestJourneyInfiniteModeRunsToTheLimit (#285): `saddle autopilot infinite
// on` starts a run that drains the ready queue, lands what finishes, and
// when the queue is empty asks the orchestrator for more work instead of
// stopping. At the plan limit it parks the running task; a restarted driver
// still knows what it parked and resumes it after the reset; the task then
// finishes and lands, and `infinite off` ends the run with a summary.
func TestJourneyInfiniteModeRunsToTheLimit(t *testing.T) {
	w := world(t, Options{Top: "concurrency = 2\n"})
	issues := []autopilot.Issue{
		{Number: 1, Title: "Area one", Body: "Work in area1.\nclaims: area1/**"},
		{Number: 2, Title: "Area two", Body: "Work in area2.\nclaims: area2/**"},
	}
	must(t, fa.Script{Steps: []fa.Step{fa.Wait("never-comes")}}.Save(w.Scripts, "t1"))
	must(t, fa.Script{Steps: finished("area2", "area2\n")}.Save(w.Scripts, "t2"))

	if out := w.MustSaddle("autopilot", "infinite", "on"); !strings.Contains(out.Stdout, "infinite mode is on") {
		t.Fatalf("infinite on: %s", out)
	}

	a := w.App()
	a.Bin = w.Bins.Saddle
	var nudges []string
	a.SetCompactTarget(app.FuncTarget{SendFn: func(s string) error { nudges = append(nudges, s); return nil }})
	t.Cleanup(func() { a.SetCompactTarget(nil) })
	gate := &usageGate{}
	now := time.Date(2026, 10, 8, 22, 0, 0, 0, time.Local)
	driver := func() *autopilot.Driver { // what saddle up builds each start
		d := a.NewAutopilot(func(string) ([]autopilot.Issue, error) { return issues, nil })
		gate.Env = d.Env
		d.Env, d.Now = gate, func() time.Time { return now }
		return d
	}
	d := driver()
	tick := func() autopilot.Report {
		t.Helper()
		rep, err := d.Tick()
		must(t, err)
		now = now.Add(autopilot.DefaultInterval)
		return rep
	}

	if rep := tick(); !slices.Equal(spawnedIssues(rep), []int{1, 2}) {
		t.Fatalf("first tick spawned %v, want #1 and #2", spawnedIssues(rep))
	}
	w.WaitStatus("t1", "idle")
	w.WaitTask("t2", "done", func(v mcpserver.TaskView) bool { return v.Status == "done" })
	if rep := tick(); rep.Landed != 1 || rep.Stopped != "" {
		t.Fatalf("second tick: %+v, want t2 landed and the run going on", rep)
	}

	// Nothing ready: it asks the orchestrator for work rather than stopping.
	now = now.Add(autopilot.DefaultStallAfter)
	if rep := tick(); rep.Stopped != "" || !strings.Contains(rep.Nudged, "find more work") {
		t.Fatalf("empty queue: %+v (nudges %q)", rep, nudges)
	}

	// The weekly window fills: t1 is parked, not killed.
	gate.pause, gate.resets = true, now.Add(3*time.Hour)
	if rep := tick(); !rep.Sleeping {
		t.Fatalf("at the limit: %+v", rep)
	}
	if v := w.Task("t1"); v.Status != app.StatusPaused {
		t.Fatalf("t1 is %s, want paused", v.Status)
	}
	sent := len(nudges)
	for range 5 {
		tick()
	}
	if len(nudges) != sent {
		t.Errorf("nudged while parked: %q", nudges[sent:])
	}

	// saddle up restarts; after the reset the new driver resumes t1, which
	// now finishes and lands.
	d = driver()
	rescript(w, "t1", finished("area1", "area1\n")...)
	gate.pause, now = false, gate.resets
	tick()
	w.WaitTask("t1", "done", func(v mcpserver.TaskView) bool { return v.Status == "done" })
	if rep := tick(); rep.Landed != 1 {
		t.Fatalf("after the reset: %+v, want t1 landed", rep)
	}
	for _, id := range []string{"t1", "t2"} {
		if v := w.Task(id); v.Status != store.Landed {
			t.Errorf("%s is %s, want landed", id, v.Status)
		}
	}
	for _, kind := range []string{autopilot.EventPark, autopilot.EventUnpark, autopilot.EventNudge} {
		if len(events(t, a, kind)) == 0 {
			t.Errorf("no %s event", kind)
		}
	}

	out := w.MustSaddle("autopilot", "infinite", "off")
	if !strings.Contains(out.Stdout, "autopilot stopped: turned off") {
		t.Fatalf("infinite off: %s", out)
	}
	if st := w.MustSaddle("autopilot", "status"); !strings.Contains(st.Stdout, "autopilot: off") {
		t.Fatalf("status after off: %s", st)
	}
}
