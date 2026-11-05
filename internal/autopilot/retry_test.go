package autopilot

import (
	"slices"
	"strings"
	"testing"
)

// #767: a task that vanished from Tasks (killed and pruned) frees its
// issue. The run waits out the retry backoff instead of calling the queue
// empty, then respawns with what the last attempt said.
func TestTickRespawnsIssueWhoseTaskWasKilled(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	r.tick()
	r.env.tasks = nil // killed, and its record is gone
	rep := r.tick()
	if st := r.state(); !st.On {
		t.Fatalf("a killed task ended the run: %+v", st)
	}
	if len(r.env.spawns) != 1 || !strings.Contains(rep.Skipped[1], "retry") {
		t.Fatalf("spawned %v, skips %v; want #1 waiting on the retry backoff", r.env.spawned(), rep.Skipped)
	}
	if strings.Contains(rep.Decision, "0 ready") {
		t.Errorf("an issue waiting on its retry isn't ready: %s", rep.Decision)
	}
	r.now = r.now.Add(DefaultRetryBackoff)
	rep = r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 1}) || len(rep.Spawned) != 1 {
		t.Fatalf("spawned %v, want #1 again", got)
	}
	if p := r.env.spawns[1].Prompt; !strings.Contains(p, "Previous attempt") || !strings.Contains(p, "t1") {
		t.Errorf("the retry's brief doesn't name the last attempt:\n%s", p)
	}
	if st := r.state(); !st.On || st.Spawned[1].Attempts != 2 {
		t.Errorf("state = %+v", st)
	}
}

func TestTickRespawnsIssueWhoseTaskFailedWithItsSummary(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	r.tick()
	r.env.tasks[0] = Task{ID: "t1", Issue: 1, End: EndNoChange, Summary: "could not reproduce the hang", Why: "watchdog killed it"}
	r.tick()
	r.now = r.now.Add(DefaultRetryBackoff)
	r.tick()
	if len(r.env.spawns) != 2 {
		t.Fatalf("spawned %v, want a retry of #1", r.env.spawned())
	}
	p := r.env.spawns[1].Prompt
	for _, want := range []string{"could not reproduce the hang", "watchdog killed it"} {
		if !strings.Contains(p, want) {
			t.Errorf("retry brief lacks %q:\n%s", want, p)
		}
	}
	if len(r.env.closes) != 0 {
		t.Errorf("a no-change that isn't done closed the issue: %q", r.env.closes)
	}
}

func TestTickGivesUpOnIssueAfterTwoRetries(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	for i := range MaxRetries + 1 {
		r.tick()
		if len(r.env.spawns) != i+1 {
			t.Fatalf("attempt %d: spawned %v", i+1, r.env.spawned())
		}
		r.env.tasks[len(r.env.tasks)-1].Live = false
		r.env.tasks[len(r.env.tasks)-1].End = EndKilled
		r.env.tasks[len(r.env.tasks)-1].Why = "hung"
		if i < MaxRetries {
			r.tick() // sees it dead; the backoff starts
			r.now = r.now.Add(DefaultRetryBackoff)
		}
	}
	rep := r.tick()
	if len(r.env.spawns) != MaxRetries+1 {
		t.Fatalf("spawned %v past the retry budget", r.env.spawned())
	}
	if len(r.env.gaveUp) != 1 || !strings.HasPrefix(r.env.gaveUp[0], "#1: ") || !strings.Contains(r.env.gaveUp[0], "hung") {
		t.Fatalf("give-up calls = %q, want one needs-human with the reasons", r.env.gaveUp)
	}
	if !strings.Contains(rep.Skipped[1], NeedsHumanLabel) {
		t.Errorf("why #1 waits = %q", rep.Skipped[1])
	}
	if st := r.state(); st.On || st.Stopped != "queue empty" {
		t.Errorf("a given-up issue still counts as ready: %+v", st)
	}
	r.on(Options{}) // a later run: the label keeps it out
	r.tick()
	if len(r.env.gaveUp) != 1 || len(r.env.spawns) != MaxRetries+1 {
		t.Errorf("commented again: %q", r.env.gaveUp)
	}
}

// An issue labelled needs-human is not ready: the owner removes the label
// to give it a fresh budget.
func TestNeedsHumanIssueIsNotReady(t *testing.T) {
	is := claimed(1, "a/**")
	is.Labels = []string{NeedsHumanLabel}
	r := newRig(t, 4, is)
	r.on(Options{})
	r.tick()
	if len(r.env.spawns) != 0 || r.state().On {
		t.Fatalf("spawned %v; state %+v", r.env.spawned(), r.state())
	}
}

func TestNoChangeAlreadyDoneClosesTheIssue(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	r.tick()
	r.env.tasks[0] = Task{ID: "t1", Issue: 1, End: EndNoChange, Summary: "This was already fixed on main by #40."}
	r.tick()
	if len(r.env.closes) != 1 || !strings.Contains(r.env.closes[0], "already fixed") {
		t.Fatalf("closes = %q", r.env.closes)
	}
	r.now = r.now.Add(DefaultRetryBackoff)
	r.tick()
	if len(r.env.spawns) != 1 || len(r.env.closes) != 1 {
		t.Fatalf("spawned %v closes %q after closing it", r.env.spawned(), r.env.closes)
	}
}

func TestBlockedAfterKilledDepIsNotSatisfied(t *testing.T) {
	dep := claimed(1, "a/**")
	next := Issue{Number: 2, Title: "builds on 1", Body: "claims: b/**\nafter: #1"}
	r := newRig(t, 4, dep, next)
	r.env.tasks = []Task{{ID: "t9", Issue: 1, End: EndKilled}}
	r.on(Options{})
	rep := r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1}) {
		t.Fatalf("spawned %v; #2 must wait for #1's work to land", got)
	}
	if !strings.Contains(rep.Skipped[2], "#1") {
		t.Errorf("why #2 waits = %q", rep.Skipped[2])
	}
	// A dep that is only queued hasn't landed either.
	r.env.tasks[1].Live, r.env.tasks[1].Queued = false, true
	if rep := r.tick(); len(r.env.spawns) != 1 || !strings.Contains(rep.Skipped[2], "#1") {
		t.Fatalf("spawned %v on a dep still in the train: %v", r.env.spawned(), rep.Skipped)
	}
	r.env.tasks[1].Queued, r.env.tasks[1].Landed = false, true
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 2}) || !slices.Equal(r.env.spawns[1].After, []string{r.env.tasks[1].ID}) {
		t.Fatalf("spawned %v after %v once #1 landed", got, r.env.spawns[len(r.env.spawns)-1].After)
	}
}

// A killed dep whose issue was closed anyway counts as done.
func TestKilledDepWithClosedIssueIsSatisfied(t *testing.T) {
	next := Issue{Number: 2, Title: "builds on 1", Body: "claims: b/**\nafter: #1"}
	r := newRig(t, 4, next)
	r.env.tasks = []Task{{ID: "t9", Issue: 1, End: EndKilled}}
	r.env.closed = map[int]bool{1: true}
	r.on(Options{})
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{2}) {
		t.Fatalf("spawned %v, want #2 since #1 is closed", got)
	}
}

func TestAlreadyDone(t *testing.T) {
	for s, want := range map[string]bool{
		"Already fixed on main by #40.":           true,
		"The feature was already implemented.":    true,
		"Nothing to do: the check exists already": true,
		"no changes needed":                       true,
		"Couldn't reproduce; gave up":             false,
		"":                                        false,
	} {
		if got := AlreadyDone(s); got != want {
			t.Errorf("AlreadyDone(%q) = %v, want %v", s, got, want)
		}
	}
}
