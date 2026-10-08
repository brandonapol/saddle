package autopilot

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Infinite mode (#285) persists in the state file, so a restarted saddle up
// (a new Driver on the same path) picks the run up where it was.
func TestInfinitePersistsAcrossRestart(t *testing.T) {
	r := newRig(t, 2, claimed(1, "a/**"))
	if _, err := r.d.SetInfinite(true); err != nil {
		t.Fatal(err)
	}
	restarted := &Driver{Path: r.d.Path, Env: r.env, Now: r.d.Now}
	st, err := restarted.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.On || !st.Infinite {
		t.Fatalf("after restart: on=%v infinite=%v, want both", st.On, st.Infinite)
	}
	if rep, err := restarted.Tick(); err != nil || len(rep.Spawned) != 1 {
		t.Fatalf("restarted driver should keep topping up: %+v, %v", rep, err)
	}
	if got := st.Goal(); !strings.Contains(got, "plan limit") {
		t.Errorf("infinite goal = %q, want it to name the plan limit", got)
	}
}

// Turning infinite on over a bounded run drops its stop condition; turning
// it off ends the run with a summary.
func TestInfiniteOnOverRunAndOff(t *testing.T) {
	r := newRig(t, 2, claimed(1, "a/**"))
	r.on(Options{Stop: Stop{MaxTasks: 1}})
	r.tick() // spawns #1 and drains: max tasks reached
	if st := r.state(); st.Draining == "" {
		t.Fatal("bounded run should drain after one task")
	}
	st, err := r.d.SetInfinite(true)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Infinite || st.Draining != "" || st.Stop != (Stop{}) || len(st.Spawned) != 1 {
		t.Fatalf("infinite over a run = %+v, want the run kept, its stop and drain dropped", st)
	}
	if st, err = r.d.SetInfinite(false); err != nil || st.On || st.Infinite || st.Summary == "" {
		t.Fatalf("off = %+v, %v; want the run ended with a summary", st, err)
	}
	if st, err = r.d.SetInfinite(false); err != nil || st.On {
		t.Fatalf("off twice = %+v, %v", st, err)
	}
}

// With nothing ready, infinite mode doesn't end the run: it asks the
// orchestrator to find more work, once per NudgeEvery.
func TestInfiniteEmptyQueueAsksForMoreWork(t *testing.T) {
	r := newRig(t, 2)
	if _, err := r.d.SetInfinite(true); err != nil {
		t.Fatal(err)
	}
	for range 10 { // 5 minutes
		r.tick()
	}
	st := r.state()
	if !st.On || st.Stopped != "" {
		t.Fatalf("an empty queue stopped infinite mode: %+v", st)
	}
	if len(r.env.nudges) != 1 {
		t.Fatalf("nudges = %d, want 1: %q", len(r.env.nudges), r.env.nudges)
	}
	n := r.env.nudges[0]
	for _, want := range []string{"ready queue is empty", "find more work", DefaultReadyLabel} {
		if !strings.Contains(n, want) {
			t.Errorf("nudge lacks %q: %s", want, n)
		}
	}
	r.now = r.now.Add(DefaultNudgeEvery)
	r.tick()
	if len(r.env.nudges) != 2 {
		t.Errorf("the empty queue should be nudged again after %s, got %d", DefaultNudgeEvery, len(r.env.nudges))
	}

	// New ready work is picked up at once, and the nudging stops.
	r.env.issues = []Issue{claimed(7, "x/**")}
	if rep := r.tick(); len(rep.Spawned) != 1 {
		t.Fatalf("new ready issue not spawned: %+v", rep)
	}
	if st := r.state(); st.StallSince != (time.Time{}) {
		t.Errorf("stall should clear once work starts, since %v", st.StallSince)
	}
}

// At the plan limit, infinite mode parks running tasks, stays quiet (no
// spawns, no nudges) until the reset, then resumes them and tops up again.
func TestInfiniteParksAtLimitAndResumesOnReset(t *testing.T) {
	r := newRig(t, 3, claimed(1, "a/**"), claimed(2, "b/**"), claimed(3, "c/**"))
	if _, err := r.d.SetInfinite(true); err != nil {
		t.Fatal(err)
	}
	r.env.limit = 2
	r.tick() // spawns #1, #2
	reset := r.now.Add(2 * time.Hour)
	r.env.usage = Usage{Percent: 1, ResetsAt: reset} // weekly window full; launches not opted into pausing
	r.env.limit = 3
	r.env.backlog = r.now.Add(-time.Hour) // unread notices would normally nudge

	rep := r.tick()
	st := r.state()
	if !rep.Sleeping || len(rep.Spawned) != 0 {
		t.Fatalf("at the limit: %+v, want sleeping with no spawns", rep)
	}
	if !slices.Equal(st.Parked, []string{"t1", "t2"}) {
		t.Fatalf("parked = %q, want t1 t2", st.Parked)
	}
	if !st.SleepUntil.Equal(reset) || !st.On {
		t.Fatalf("state = %+v, want on and sleeping until %v", st, reset)
	}
	for range 20 {
		r.tick()
	}
	if len(r.env.parks) != 1 {
		t.Errorf("parked %d times, want once: %q", len(r.env.parks), r.env.parks)
	}
	if len(r.env.nudges) != 0 || len(r.env.spawns) != 2 {
		t.Fatalf("while parked: nudges %q, spawns %v; want none new", r.env.nudges, r.env.spawned())
	}

	// A saddle up restart while parked keeps the parked list.
	r.d = &Driver{Path: r.d.Path, Env: r.env, Now: r.d.Now}
	r.now = reset.Add(time.Minute)
	r.env.usage = Usage{Percent: 0.1}
	rep = r.tick()
	if !slices.Equal(r.env.unparked, []string{"t1", "t2"}) {
		t.Fatalf("unparked = %q, want t1 t2 after the reset", r.env.unparked)
	}
	if st := r.state(); len(st.Parked) != 0 || !st.SleepUntil.IsZero() {
		t.Errorf("after the reset: parked %q, sleep until %v", st.Parked, st.SleepUntil)
	}
	if len(rep.Spawned) != 1 || rep.Spawned[0].Issue != 3 {
		t.Errorf("after the reset it should top up with #3: %+v", rep.Spawned)
	}
	if !r.env.hasEvent(EventPark) || !r.env.hasEvent(EventUnpark) {
		t.Errorf("events lack park/unpark: %q", r.env.events)
	}
}

// A bounded (non-infinite) run keeps its old behavior at the limit: it
// sleeps when launches pause but parks nothing.
func TestBoundedRunDoesNotPark(t *testing.T) {
	r := newRig(t, 2, claimed(1, "a/**"), claimed(2, "b/**"))
	r.on(Options{})
	r.env.limit = 1
	r.tick()
	r.env.usage = Usage{Percent: 1, Pause: true, ResetsAt: r.now.Add(time.Hour)}
	r.tick()
	if len(r.env.parks) != 0 {
		t.Errorf("a bounded run parked tasks: %q", r.env.parks)
	}
}
