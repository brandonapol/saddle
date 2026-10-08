package automerge

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// collapseRig is a two-PR stack, t1 then t2, with t1 red from a flaky test
// and t2, which holds the fix, green: the 2026-10-03 deadlock (#196). Its
// Collapse records each call and whether the train lock was held then.
type collapseRig struct {
	*rig
	calls    []string
	lockedAt []bool
	locked   bool
	result   string
	err      error
}

func newCollapseRig(t *testing.T, tasks ...string) *collapseRig {
	t.Helper()
	if len(tasks) == 0 {
		tasks = []string{"t1", "t2"}
	}
	c := &collapseRig{rig: newRig(t, "", tasks...)}
	c.gh.prs["pr/"+tasks[0]].Checks = ChecksFail
	c.result = "pr/" + tasks[len(tasks)-1]
	c.w.Lock = func() (func(), bool, error) {
		c.locked = true
		return func() { c.locked = false }, true, nil
	}
	c.w.Collapse = func(stack string) (string, error) {
		c.calls = append(c.calls, stack)
		c.lockedAt = append(c.lockedAt, c.locked)
		return c.result, c.err
	}
	return c
}

func TestCheckCollapsesRedBottom(t *testing.T) {
	c := newCollapseRig(t)
	c.on(t)
	st := check(t, c.w)
	if !slices.Equal(c.calls, []string{"t1"}) {
		t.Fatalf("collapse calls = %v, want one for stack t1", c.calls)
	}
	if c.lockedAt[0] {
		t.Fatal("collapse ran under the train lock; it takes the lock itself")
	}
	if st.Merged != "pr/t2" {
		t.Fatalf("merged = %q, want pr/t2", st.Merged)
	}
	i := slices.IndexFunc(c.events, func(e event) bool { return e.kind == EventCollapsed })
	if i < 0 || c.events[i].task != "t1" || !strings.Contains(c.events[i].data, "pr/t2") {
		t.Fatalf("events = %+v, want %s for t1 naming pr/t2", c.events, EventCollapsed)
	}
	if len(c.gh.merged) != 0 || c.restacks != 0 {
		t.Fatalf("watcher merged %v / restacked %d itself; collapse does both", c.gh.merged, c.restacks)
	}
	if s, _ := c.w.Status(); s.Merged != "pr/t2" {
		t.Fatalf("saved status merged = %q", s.Merged)
	}
}

func TestCollapseNotTried(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []string
		edit  func(c *collapseRig)
	}{
		{"off", nil, func(c *collapseRig) {
			if err := c.w.SetEnabled(false); err != nil {
				panic(err)
			}
		}},
		{"held", nil, func(c *collapseRig) { _ = c.w.Hold("t1") }},
		{"needs-human bottom", nil, func(c *collapseRig) { c.gh.prs["pr/t1"].Labels = []string{NeedsHuman} }},
		{"needs-human top", nil, func(c *collapseRig) { c.gh.prs["pr/t2"].Labels = []string{NeedsHuman} }},
		{"at risk", nil, func(c *collapseRig) { c.risk["t1"] = "drifted" }},
		{"draft top", nil, func(c *collapseRig) { c.gh.prs["pr/t2"].Draft = true }},
		{"conflicting bottom", nil, func(c *collapseRig) { c.gh.prs["pr/t1"].Mergeable = "CONFLICTING" }},
		{"single PR", []string{"t1"}, func(*collapseRig) {}},
		{"green bottom", nil, func(c *collapseRig) { c.gh.prs["pr/t1"].Checks = ChecksPass }},
		{"pending bottom", nil, func(c *collapseRig) { c.gh.prs["pr/t1"].Checks = ChecksPending }},
		{"no green PR above", nil, func(c *collapseRig) { c.gh.prs["pr/t2"].Checks = ChecksPending }},
		{"stopped", nil, func(c *collapseRig) {
			s, _ := Load(c.w.Path)
			s.Stopped = "earlier failure"
			_ = s.Save(c.w.Path)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCollapseRig(t, tc.tasks...)
			c.on(t)
			tc.edit(c)
			st := check(t, c.w)
			if tc.name == "green bottom" {
				// A ready bottom merges the normal way.
				if len(c.calls) != 0 || len(c.gh.merged) != 1 {
					t.Fatalf("calls %v merged %v", c.calls, c.gh.merged)
				}
				return
			}
			if len(c.calls) != 0 || st.Merged != "" {
				t.Fatalf("collapse tried: calls %v merged %q", c.calls, st.Merged)
			}
		})
	}
}

// A ready stack goes first: one PR action per check.
func TestReadyStackMergesBeforeCollapse(t *testing.T) {
	c := newCollapseRig(t)
	c.entries = append(c.entries, Entry{Task: "t9", Branch: "saddle/t9", PR: "pr/t9"})
	c.gh.prs["pr/t9"] = ready("t9", "main")
	c.on(t)
	st := check(t, c.w)
	if st.Merged != "pr/t9" || len(c.calls) != 0 {
		t.Fatalf("merged %q, collapse calls %v; want t9 merged and no collapse", st.Merged, c.calls)
	}
}

// The first qualifying stack collapses; a held one before it is skipped.
func TestCollapseSkipsHeldStack(t *testing.T) {
	c := newCollapseRig(t)
	for _, id := range []string{"t3", "t4"} {
		c.entries = append(c.entries, Entry{Task: id, Branch: "saddle/" + id, PR: "pr/" + id})
	}
	c.gh.prs["pr/t3"] = ready("t3", "main")
	c.gh.prs["pr/t3"].Checks = ChecksFail
	c.gh.prs["pr/t4"] = ready("t4", "saddle/t3")
	c.on(t)
	if err := c.w.Hold("t1"); err != nil {
		t.Fatal(err)
	}
	check(t, c.w)
	if !slices.Equal(c.calls, []string{"t3"}) {
		t.Fatalf("collapse calls = %v, want only t3", c.calls)
	}
}

// When AutoCollapse finds the stack doesn't qualify, nothing happens and
// nothing stops.
func TestCollapseDeclined(t *testing.T) {
	c := newCollapseRig(t)
	c.result = ""
	c.on(t)
	st := check(t, c.w)
	if len(c.calls) != 1 || st.Merged != "" || st.Stopped != "" || slices.Contains(c.kinds(), EventCollapsed) {
		t.Fatalf("declined collapse: calls %v status %+v events %v", c.calls, st, c.kinds())
	}
}

// A failed collapse stops the watcher with a notice and isn't retried.
func TestCollapseFailureStops(t *testing.T) {
	c := newCollapseRig(t)
	c.err = errors.New("CI on the combined head is red")
	c.on(t)
	st := check(t, c.w)
	if st.Stopped == "" || !strings.Contains(st.Stopped, "combined head") {
		t.Fatalf("stopped = %q", st.Stopped)
	}
	if !slices.Contains(c.kinds(), EventFailed) {
		t.Fatalf("events = %v", c.kinds())
	}
	if len(c.notices) == 0 || !c.notices[len(c.notices)-1].action || !strings.Contains(c.notices[len(c.notices)-1].text, "automerge on") {
		t.Fatalf("notices = %+v", c.notices)
	}
	check(t, c.w)
	if len(c.calls) != 1 {
		t.Fatalf("collapse retried after a failure: %v", c.calls)
	}
}

// Status says a collapse is the next action for the red stack.
func TestPlanSaysCollapseIsNext(t *testing.T) {
	c := newCollapseRig(t, "t1", "t2", "t3")
	c.gh.prs["pr/t2"].Checks = ChecksFail
	st, err := c.w.Plan()
	if err != nil {
		t.Fatal(err)
	}
	s := st.Stacks[0]
	if s.Collapse != "pr/t3" || !strings.Contains(s.Why, "red") || !strings.Contains(s.Why, "collapse") || !strings.Contains(s.Why, "pr/t3") {
		t.Fatalf("stack = %+v, want collapse into pr/t3 in why", s)
	}
	// Without the hook there is no collapse to promise.
	c.w.Collapse = nil
	st, _ = c.w.Plan()
	if s := st.Stacks[0]; s.Collapse != "" || strings.Contains(s.Why, "collapse") {
		t.Fatalf("no hook but stack = %+v", s)
	}
	if len(c.calls) != 0 {
		t.Fatal("Plan collapsed")
	}
}

// Red CI the ci-red watcher holds comes in through AtRisk on the red layer
// and everything above it; collapse leaves that stack alone.
func TestCollapseNotPastCIRed(t *testing.T) {
	c := newCollapseRig(t)
	c.risk["t1"] = "CI is red on its PR (CI / test)"
	c.risk["t2"] = "CI is red on t1 below it (CI / test)"
	c.on(t)
	st := check(t, c.w)
	if len(c.calls) != 0 || st.Merged != "" || st.Stacks[0].Collapse != "" {
		t.Fatalf("collapsed past ci-red: calls %v status %+v", c.calls, st)
	}
	if !strings.Contains(st.Stacks[0].Why, "CI is red on its PR") {
		t.Fatalf("why = %q, want the ci-red reason", st.Stacks[0].Why)
	}
}
