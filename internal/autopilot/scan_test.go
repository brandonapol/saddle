package autopilot

import (
	"slices"
	"strings"
	"testing"
)

func unassignedTickets() []Issue {
	return []Issue{
		{Number: 10, Title: "docs drift", Labels: []string{"docs"}},
		{Number: 11, Title: "pick a cache backend", Labels: []string{"owner-decision"}},
		{Number: 12, Title: "crash on start", Labels: []string{"bug"}, Body: "claims: internal/start/**"},
		{Number: 13, Title: "slow status", Labels: []string{"perf"}},
		{Number: 14, Title: "flaky train test", Labels: []string{"tests"}},
	}
}

// #733: with eligible unassigned tickets and nothing running, every nudge
// hands the orchestrator a concrete batch, the focus rotates, and the run
// never goes idle or stops.
func TestNudgesCarryATicketBatchAndNeverGoIdle(t *testing.T) {
	r := newRig(t, 4)
	r.env.unassigned = unassignedTickets()
	r.on(Options{})
	for range 3 * (DefaultIdleRepeats + 1) {
		r.tick()
		r.now = r.now.Add(DefaultNudgeEvery)
	}
	if st := r.state(); !st.On || st.Escalated {
		t.Fatalf("the run stopped or went idle with eligible tickets: %+v", st)
	}
	if r.env.hasEvent(EventIdle) || len(r.env.notices) != 0 {
		t.Fatalf("went idle: events %q notices %q", r.env.events, r.env.notices)
	}
	if len(r.env.nudges) <= DefaultIdleRepeats {
		t.Fatalf("nudges = %d, want more than %d", len(r.env.nudges), DefaultIdleRepeats)
	}
	for i, n := range r.env.nudges[:3] {
		if !strings.Contains(n, "#12 crash on start") || !strings.Contains(n, "claims internal/start/**") {
			t.Errorf("nudge %d carries no ticket batch with claims: %s", i, n)
		}
		if strings.Contains(n, "\n") {
			t.Errorf("nudge %d is not one line", i)
		}
	}
	for i, focus := range []string{"bugs", "perf", "tests"} {
		if !strings.Contains(r.env.nudges[i], "focus: "+focus) {
			t.Errorf("nudge %d should focus on %s: %s", i, focus, r.env.nudges[i])
		}
	}
	if !strings.Contains(r.env.nudges[0], "#11") || !strings.Contains(r.env.nudges[0], "decision card") {
		t.Errorf("an owner-choice ticket should be spawned as a reversible PR with a decision card: %s", r.env.nudges[0])
	}
}

func TestTicketBatchSortsBugsFirstAndSkipsTaken(t *testing.T) {
	is := unassignedTickets()
	is = append(is, Issue{Number: 15, Title: "has a PR", Labels: []string{"bug"}, HasPR: true},
		Issue{Number: 16, Title: "human", Labels: []string{"bug", NeedsHumanLabel}})
	got := rankTickets(eligibleTickets(is, nil, nil), 0)
	var nums []int
	for _, x := range got {
		nums = append(nums, x.Number)
	}
	if !slices.Equal(nums, []int{12, 13, 14, 10, 11}) {
		t.Fatalf("batch order %v, want bug, perf, tests, docs, design", nums)
	}
	got = rankTickets(eligibleTickets(is, nil, nil), 3) // docs round
	if got[0].Number != 10 {
		t.Fatalf("docs round starts with #%d", got[0].Number)
	}
}

// Only a full scan with zero eligible tickets lets an empty queue end the run.
func TestQueueEmptyStopsOnlyWhenTheScanIsEmpty(t *testing.T) {
	r := newRig(t, 4)
	r.env.unassigned = unassignedTickets()[:1]
	r.on(Options{})
	r.tick()
	if st := r.state(); !st.On {
		t.Fatalf("stopped with an eligible ticket open: %+v", st)
	}
	r.env.unassigned = nil
	r.now = r.now.Add(DefaultScanEvery)
	r.tick()
	if st := r.state(); st.On || st.Stopped != "queue empty" {
		t.Fatalf("state = %+v", st)
	}
}
