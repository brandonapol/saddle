package gate

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// fake implements every state interface from plain maps.
type fake struct {
	tasks   map[string]TaskInfo
	prs     map[int]bool
	claims  map[string][]string
	commits map[string][]Commit
}

func (f fake) Task(id string) (TaskInfo, bool) { ti, ok := f.tasks[id]; return ti, ok }
func (f fake) PRMerged(n int) (bool, bool)     { m, ok := f.prs[n]; return m, ok }
func (f fake) Claims() map[string][]string     { return f.claims }
func (f fake) Commits(task string) []Commit    { return f.commits[task] }

func (f fake) state() State { return State{Tasks: f, PRs: f, Claims: f, Commits: f} }

func check(t *testing.T, c Cond, s State, now time.Time, ready bool, reason string) {
	t.Helper()
	r := c.Eval(now, s)
	if r.Ready != ready || r.Reason != reason {
		t.Errorf("%s: got {%v %q}, want {%v %q}", c, r.Ready, r.Reason, ready, reason)
	}
}

func TestTaskLanded(t *testing.T) {
	f := fake{tasks: map[string]TaskInfo{
		"t4": {Status: "running"},
		"t5": {Status: "landed", Done: true, Landed: true},
	}}
	s := f.state()
	check(t, TaskLanded("t4"), s, t0, false, "waiting on t4 to land (running)")
	check(t, TaskLanded("t5"), s, t0, true, "t5 landed")
	check(t, TaskLanded("t9"), s, t0, false, "waiting on t9 to land (unknown task)")
	check(t, TaskLanded("t4"), State{}, t0, false, "waiting on t4 to land (no task state)")
}

func TestTaskDone(t *testing.T) {
	f := fake{tasks: map[string]TaskInfo{
		"t4": {Status: "running"},
		"t5": {Status: "done", Done: true},
		"t6": {Status: "landed", Landed: true},
	}}
	s := f.state()
	check(t, TaskDone("t4"), s, t0, false, "waiting on t4 to finish (running)")
	check(t, TaskDone("t5"), s, t0, true, "t5 is done")
	check(t, TaskDone("t6"), s, t0, true, "t6 is done") // landed implies done
	check(t, TaskDone("t9"), s, t0, false, "waiting on t9 to finish (unknown task)")
}

func TestPRMerged(t *testing.T) {
	s := fake{prs: map[int]bool{10: false, 11: true}}.state()
	check(t, PRMerged(10), s, t0, false, "waiting on PR #10 to merge")
	check(t, PRMerged(11), s, t0, true, "PR #11 merged")
	check(t, PRMerged(12), s, t0, false, "waiting on PR #12 to merge (unknown PR)")
	check(t, PRMerged(10), State{}, t0, false, "waiting on PR #10 to merge (no PR state)")
}

func TestClaimFree(t *testing.T) {
	s := fake{claims: map[string][]string{
		"t3": {"internal/gate/**"},
		"t4": {"internal/invoice/**"},
		"t2": {"internal/invoice/period.go"},
	}}.state()
	check(t, ClaimFree("internal/invoice/period.go", "t3"), s, t0, false,
		"waiting on t2 to release internal/invoice/period.go (claimed by t2 as internal/invoice/period.go)")
	check(t, ClaimFree("internal/gate/gate.go", "t3"), s, t0, true, "internal/gate/gate.go is free")
	check(t, ClaimFree("internal/gate/gate.go", ""), s, t0, false,
		"waiting on t3 to release internal/gate/gate.go (claimed by t3 as internal/gate/**)")
	check(t, ClaimFree("web/x.ts", ""), s, t0, true, "web/x.ts is free")
	check(t, ClaimFree("x", ""), State{}, t0, false, "waiting on x to be free (no claim state)")
}

func TestAfter(t *testing.T) {
	c := After(t0.Add(10 * time.Minute))
	check(t, c, State{}, t0, false, "waiting until 12:10:00 UTC (10m0s left)")
	check(t, c, State{}, t0.Add(10*time.Minute), true, "12:10:00 UTC passed")
	check(t, c, State{}, t0.Add(time.Hour), true, "12:10:00 UTC passed")
}

func TestCommitTouching(t *testing.T) {
	f := fake{commits: map[string][]Commit{
		"t4": {
			{SHA: "aaaaaaaaaaaa", At: t0.Add(-time.Minute), Paths: []string{"internal/invoice/period.go"}},
			{SHA: "bbbbbbbbbbbb", At: t0.Add(time.Minute), Paths: []string{"README.md"}},
		},
	}}
	s := f.state()
	c := CommitTouching("t4", t0, "internal/invoice/**")
	// The older commit predates the hold, so it does not count.
	check(t, c, s, t0.Add(2*time.Minute), false, "waiting on t4's commit to internal/invoice/**")

	f.commits["t4"] = append(f.commits["t4"],
		Commit{SHA: "cccccccccccc", At: t0.Add(3 * time.Minute), Paths: []string{"internal/invoice/period.go"}})
	check(t, c, s, t0.Add(4*time.Minute), true, "t4 committed cccccccc to internal/invoice/period.go")

	anyCommit := CommitTouching("t4", t0)
	check(t, anyCommit, s, t0, true, "t4 committed bbbbbbbb")
	check(t, CommitTouching("t5", t0), s, t0, false, "waiting on t5's next commit")
	check(t, c, State{}, t0, false, "waiting on t4's commit to internal/invoice/** (no commit state)")
}

func TestAll(t *testing.T) {
	s := fake{tasks: map[string]TaskInfo{
		"t4": {Status: "running"},
		"t5": {Status: "landed", Done: true, Landed: true},
	}}.state()
	check(t, All(TaskLanded("t5"), TaskLanded("t4"), TaskDone("t4")), s, t0, false,
		"waiting on t4 to land (running); waiting on t4 to finish (running)")
	check(t, All(TaskLanded("t5"), TaskDone("t5")), s, t0, true, "t5 landed; t5 is done")
	check(t, All(), s, t0, true, "no conditions")
}

func TestAny(t *testing.T) {
	s := fake{tasks: map[string]TaskInfo{
		"t4": {Status: "running"},
		"t5": {Status: "landed", Done: true, Landed: true},
	}}.state()
	check(t, Any(TaskLanded("t4"), TaskLanded("t5")), s, t0, true, "t5 landed")
	check(t, Any(TaskLanded("t4"), After(t0.Add(time.Minute))), s, t0, false,
		"waiting on t4 to land (running), or waiting until 12:01:00 UTC (1m0s left)")
	check(t, Any(), s, t0, false, "no conditions")
}

func TestNested(t *testing.T) {
	s := fake{tasks: map[string]TaskInfo{"t4": {Status: "running"}}}.state()
	c := Any(All(TaskLanded("t4"), PRMerged(3)), After(t0))
	check(t, c, s, t0, true, "12:00:00 UTC passed")
	if got, want := c.String(), "any(all(landed(t4), merged(#3)), after(12:00:00 UTC))"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := c.Deps(), []string{"t4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Deps() = %v, want %v", got, want)
	}
}

func TestDeterministic(t *testing.T) {
	// Several other tasks claim the same path; the reason must name the same
	// owner every time despite map iteration order.
	s := fake{claims: map[string][]string{
		"t9": {"a/**"}, "t2": {"a/**"}, "t7": {"a/b"}, "t5": {"a"},
	}}.state()
	c := ClaimFree("a/b", "")
	first := c.Eval(t0, s)
	for range 50 {
		if r := c.Eval(t0, s); r != first {
			t.Fatalf("nondeterministic: %v vs %v", r, first)
		}
	}
	if !strings.HasPrefix(first.Reason, "waiting on t2 ") {
		t.Errorf("want lowest task id named, got %q", first.Reason)
	}
}
