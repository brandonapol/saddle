package autopilot

import (
	"strings"
	"testing"
	"time"
)

// The flighthouse night (#463): an owner-gated release, a new dependency
// waiting for an OK, and t34 already merged while the orchestrator still
// believed it held.
func flighthouse(now time.Time) Snapshot {
	return Snapshot{
		Blockers: []Blocker{
			{What: "release tag push denied by the classifier", Since: now.Add(-3*time.Hour - 12*time.Minute)},
			{What: "OK for the new SDK dependency"},
		},
		Tasks:   []string{"t40 running"},
		OpenPRs: []string{"t39 (PR #150)"},
		Merged:  []string{"t34 (PR #147)"},
		Queue:   []string{"t38 on hold"},
	}
}

func TestIdlePromptCarriesSnapshotAndLookahead(t *testing.T) {
	now := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	p := IdlePrompt("the ready queue is empty (0/5 running)", "0/5 running, 0 ready, 4 spawned this run", "saddle:ready", flighthouse(now), now)
	if strings.Contains(p, "\n") {
		t.Fatalf("the prompt is typed into an input box and must be one line: %q", p)
	}
	for _, want := range []string{
		"Owner-blocked (2)", "release tag push denied by the classifier (waiting 3h12m)", "OK for the new SDK dependency (age unknown)",
		"tasks: t40 running", "open PRs: t39 (PR #150)", "merged: t34 (PR #147)", "train: t38 on hold",
		"beat what you remember", "look ahead", "next tasks in the current and next epic", "Proposed only",
		"depend on no Proposed ADR and no pending owner answer", "saddle:ready", "not a status", "Never ask the owner whether to continue",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if empty := IdlePrompt("x", "d", "l", Snapshot{}, now); strings.Contains(empty, "Owner-blocked") || !strings.Contains(empty, "tasks: none") {
		t.Errorf("an empty snapshot: %s", empty)
	}
}

func TestSnapshotListsAreCapped(t *testing.T) {
	var ts []string
	for range snapshotCap + 3 {
		ts = append(ts, "t")
	}
	if got := list(ts); !strings.HasSuffix(got, " and 3 more") {
		t.Errorf("list = %q", got)
	}
}

// An idle infinite run with owner-blocked work nudges the same state at
// most DefaultIdleRepeats times, tells the owner once, then stays quiet
// until the state changes.
func TestIdleRepeatsEscalateOnceThenGoQuiet(t *testing.T) {
	r := newRig(t, 5)
	r.env.snap = flighthouse(r.now)
	r.on(Options{Infinite: true})
	for range 30 { // 25 nudge windows overnight
		r.tick()
		r.now = r.now.Add(DefaultNudgeEvery)
	}
	if len(r.env.nudges) != DefaultIdleRepeats {
		t.Fatalf("nudges = %d, want %d: %q", len(r.env.nudges), DefaultIdleRepeats, r.env.nudges)
	}
	if len(r.env.notices) != 1 || !strings.HasPrefix(r.env.notices[0], "true Autopilot is idle") ||
		!strings.Contains(r.env.notices[0], "release tag push denied") || !strings.Contains(r.env.notices[0], "saddle autopilot off") {
		t.Fatalf("want one interrupting escalation naming the blockers and the way out, got %q", r.env.notices)
	}
	if !r.env.hasEvent(EventIdle) {
		t.Error("no autopilot_idle event")
	}

	// The escalation is an unread notice: a "silent" nudge about it is the
	// same idle state and stays quiet too.
	r.env.backlog = r.now.Add(-time.Hour)
	r.tick()
	if len(r.env.nudges) != DefaultIdleRepeats {
		t.Fatalf("an unread escalation restarted the nudges: %q", r.env.nudges)
	}
	r.env.backlog = time.Time{}

	// The owner answers a blocker: the state changed, so nudging resumes.
	r.env.snap.Blockers = r.env.snap.Blockers[1:]
	r.now = r.now.Add(DefaultNudgeEvery)
	r.tick()
	if len(r.env.nudges) != DefaultIdleRepeats+1 || strings.Contains(r.env.nudges[len(r.env.nudges)-1], "release tag push") {
		t.Fatalf("a changed state should be nudged afresh: %q", r.env.nudges)
	}
	if st := r.state(); st.IdleNudges != 1 || st.Escalated {
		t.Errorf("counter not reset on change: %+v", st)
	}
}

func TestIdleRepeatsResetWhenWorkStarts(t *testing.T) {
	r := newRig(t, 5)
	r.d.IdleRepeats = 1
	r.on(Options{Infinite: true})
	for range 4 {
		r.tick()
		r.now = r.now.Add(DefaultNudgeEvery)
	}
	if len(r.env.nudges) != 1 || len(r.env.notices) != 1 {
		t.Fatalf("nudges %q notices %q", r.env.nudges, r.env.notices)
	}
	r.env.issues = []Issue{claimed(9, "x/**")}
	r.tick()
	if st := r.state(); st.IdleKey != "" || st.IdleNudges != 0 || st.Escalated {
		t.Errorf("starting work should clear the idle count: %+v", st)
	}
}

func TestRunwayPlanOnceWhenAutopilotStarts(t *testing.T) {
	r := newRig(t, 2, claimed(1, "a/**"), claimed(2, "a/**"))
	r.env.snap = flighthouse(r.now)
	r.env.busy = true
	r.on(Options{})
	r.tick()
	if len(r.env.runways) != 0 {
		t.Fatal("the runway request interrupted a busy orchestrator")
	}
	r.env.busy = false
	for range 3 {
		r.tick()
	}
	if len(r.env.runways) != 1 {
		t.Fatalf("runways = %q, want one", r.env.runways)
	}
	p := r.env.runways[0]
	for _, want := range []string{"#1", "#2", "release tag push denied", "next hours", "owner decisions", "up front", "don't wait"} {
		if !strings.Contains(p, want) {
			t.Errorf("runway prompt lacks %q: %s", want, p)
		}
	}
	if strings.Contains(p, "\n") {
		t.Errorf("runway prompt is more than one line: %q", p)
	}
	if !r.env.hasEvent(EventRunway) {
		t.Error("no autopilot_runway event")
	}
	if _, err := r.d.SetInfinite(true); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if len(r.env.runways) != 2 {
		t.Errorf("switching to infinite mode should ask for a new runway: %d", len(r.env.runways))
	}
}
