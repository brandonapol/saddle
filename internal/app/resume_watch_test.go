package app

import (
	"testing"
	"time"
)

// #782: after resumeBurst resumes in resumeWindow the watcher gives up, but
// only for a backoff (30m, 1h, 2h, then 2h), after which the task is
// resumed again. Each give-up is reported once.
func TestResumeGateBacksOffInsteadOfGivingUpForGood(t *testing.T) {
	t.Parallel()
	g := newResumeGate()
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	burst := func() {
		t.Helper()
		for i := range resumeBurst {
			if ok, _ := g.allow("t1", now); !ok {
				t.Fatalf("resume %d at %s refused", i+1, now)
			}
			now = now.Add(time.Minute)
		}
	}
	for _, wait := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour} {
		burst()
		ok, gaveUp := g.allow("t1", now)
		if ok || gaveUp != wait {
			t.Fatalf("after a burst: allow = %v, backoff %s; want refused with backoff %s", ok, gaveUp, wait)
		}
		// Refused, quietly, until the backoff ends.
		for _, d := range []time.Duration{time.Minute, wait - time.Minute} {
			if ok, again := g.allow("t1", now.Add(d)); ok || again != 0 {
				t.Fatalf("%s into a %s backoff: allow = %v, give-up %s", d, wait, ok, again)
			}
		}
		now = now.Add(wait)
	}
	// Other tasks are untouched.
	if ok, _ := g.allow("t2", now); !ok {
		t.Fatal("another task was refused")
	}
}

// #782: only the agent's own hook events are a heartbeat. Saddle's events
// about a dead task (ci_error, restack, resume_gave_up) don't keep it from
// being shown as orphaned.
func TestOrphansIgnoreSaddlesOwnEvents(t *testing.T) {
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one"})
	must(t, err)
	serverDies(ft)
	old := OrphanAfter
	OrphanAfter = time.Second
	t.Cleanup(func() { OrphanAfter = old })
	time.Sleep(2100 * time.Millisecond) // events are stamped in whole seconds
	a.Store.Event(tk.ID, "ci_error", "gh: rate limited")
	a.Store.Event(tk.ID, "resume_gave_up", "resumed 3 times in 30m")
	o, err := a.Orphans()
	must(t, err)
	if !o[tk.ID] {
		t.Fatal("a windowless task with only saddle's own fresh events is not orphaned")
	}
	a.Store.Event(tk.ID, "tool", "Edit x.go")
	if o, _ := a.Orphans(); o[tk.ID] {
		t.Fatal("a fresh tool call is a heartbeat, but the task is still orphaned")
	}
}
