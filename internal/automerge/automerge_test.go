package automerge

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeGH struct {
	prs      map[string]*PR
	merged   []string
	mergeErr error
	method   string
}

func (f *fakeGH) PR(url string) (PR, error) {
	p, ok := f.prs[url]
	if !ok {
		return PR{}, errors.New("no such PR " + url)
	}
	return *p, nil
}

func (f *fakeGH) MergeMethod() (string, error) {
	if f.method == "" {
		return "squash", nil
	}
	return f.method, nil
}

func (f *fakeGH) Merge(url, method, head string) error {
	if f.mergeErr != nil {
		return f.mergeErr
	}
	p := f.prs[url]
	if p.HeadSHA != head {
		return errors.New("head moved")
	}
	f.merged = append(f.merged, url+" "+method)
	p.State = "MERGED"
	return nil
}

type event struct{ task, kind, data string }

type notice struct {
	action bool
	text   string
}

type rig struct {
	w        *Watcher
	gh       *fakeGH
	entries  []Entry
	events   []event
	notices  []notice
	restacks int
	risk     map[string]string
	behind   map[string]int
	restack  func() error
}

func ready(task, base string) *PR {
	return &PR{URL: "pr/" + task, State: "OPEN", Mergeable: "MERGEABLE", MergeState: "CLEAN",
		Base: base, Head: "saddle/" + task, HeadSHA: "sha-" + task, Checks: ChecksPass}
}

// newRig tracks a linear stack of tasks, bottom first, each PR ready and
// based on the one below it.
func newRig(t *testing.T, path string, tasks ...string) *rig {
	t.Helper()
	r := &rig{gh: &fakeGH{prs: map[string]*PR{}}, risk: map[string]string{}, behind: map[string]int{}}
	base := "main"
	for _, id := range tasks {
		r.entries = append(r.entries, Entry{Task: id, Branch: "saddle/" + id, PR: "pr/" + id})
		r.gh.prs["pr/"+id] = ready(id, base)
		base = "saddle/" + id
	}
	if path == "" {
		path = filepath.Join(t.TempDir(), "automerge.json")
	}
	r.w = &Watcher{
		Path:    path,
		Base:    "main",
		GH:      r.gh,
		Entries: func() ([]Entry, error) { return r.entries, nil },
		AtRisk:  func(task string) string { return r.risk[task] },
		Behind:  func(head string) int { return r.behind[head] },
		Restack: func() error {
			r.restacks++
			if r.restack != nil {
				return r.restack()
			}
			return nil
		},
		Event:  func(task, kind, data string) { r.events = append(r.events, event{task, kind, data}) },
		Notify: func(action bool, text string) { r.notices = append(r.notices, notice{action, text}) },
	}
	return r
}

// retarget is what restack does after a merge: PRs on a merged PR's branch
// move to main.
func (r *rig) retarget() error {
	for _, e := range r.entries {
		p := r.gh.prs[e.PR]
		if p.State != "MERGED" {
			continue
		}
		for _, q := range r.gh.prs {
			if q.Base == e.Branch {
				q.Base = "main"
			}
		}
	}
	return nil
}

func (r *rig) kinds() []string {
	var out []string
	for _, e := range r.events {
		out = append(out, e.kind)
	}
	return out
}

func (r *rig) on(t *testing.T) {
	t.Helper()
	if err := r.w.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, w *Watcher) Status {
	t.Helper()
	st, err := w.Check()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestOffByDefault(t *testing.T) {
	r := newRig(t, "", "t1")
	st := check(t, r.w)
	if st.Enabled || len(r.gh.merged) != 0 || r.restacks != 0 {
		t.Fatalf("off by default: enabled=%v merged=%v restacks=%d", st.Enabled, r.gh.merged, r.restacks)
	}
	if len(st.Stacks) != 1 || st.Stacks[0].Next != "pr/t1" || !st.Stacks[0].Ready {
		t.Fatalf("status still tracks the stack: %+v", st.Stacks)
	}
}

func TestConfigDefaultOnMerges(t *testing.T) {
	r := newRig(t, "", "t1")
	r.w.Default = true
	if st := check(t, r.w); !st.Enabled || st.Source != SourceConfig || len(r.gh.merged) != 1 {
		t.Fatalf("config on: %+v merged %v", st, r.gh.merged)
	}
	// The runtime toggle overrides config.
	r2 := newRig(t, "", "t1")
	r2.w.Default = true
	if err := r2.w.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if st := check(t, r2.w); st.Enabled || st.Source != SourceRuntime || len(r2.gh.merged) != 0 {
		t.Fatalf("runtime off over config on: %+v merged %v", st, r2.gh.merged)
	}
}

func TestMergesReadyBottomThenRestacks(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.on(t)
	order := []string{}
	r.restack = func() error { order = append(order, "restack after "+strings.Join(r.gh.merged, ",")); return nil }
	check(t, r.w)
	if !slices.Equal(r.gh.merged, []string{"pr/t1 squash"}) {
		t.Fatalf("merged = %v, want only the bottom PR", r.gh.merged)
	}
	if !slices.Equal(order, []string{"restack after pr/t1 squash"}) {
		t.Fatalf("restack = %v", order)
	}
	if !slices.Contains(r.kinds(), EventMerged) {
		t.Fatalf("no merge event: %v", r.events)
	}
}

func TestMergeMethodFromRepo(t *testing.T) {
	r := newRig(t, "", "t1")
	r.gh.method = "rebase"
	r.on(t)
	check(t, r.w)
	if !slices.Equal(r.gh.merged, []string{"pr/t1 rebase"}) {
		t.Fatalf("merged = %v", r.gh.merged)
	}
}

func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(r *rig, p *PR)
		kind string
		why  string
	}{
		{"pending CI", func(_ *rig, p *PR) { p.Checks = ChecksPending }, EventWaiting, "pending"},
		{"red CI", func(_ *rig, p *PR) { p.Checks = ChecksFail }, EventRefused, "red"},
		{"needs-human", func(_ *rig, p *PR) { p.Labels = []string{"bug", NeedsHuman} }, EventRefused, NeedsHuman},
		{"draft", func(_ *rig, p *PR) { p.Draft = true }, EventRefused, "draft"},
		{"conflicting", func(_ *rig, p *PR) { p.Mergeable, p.MergeState = "CONFLICTING", "DIRTY" }, EventRefused, "conflict"},
		{"blocked by protection", func(_ *rig, p *PR) { p.MergeState = "BLOCKED" }, EventWaiting, "BLOCKED"},
		{"unknown mergeability", func(_ *rig, p *PR) { p.Mergeable, p.MergeState = "UNKNOWN", "UNKNOWN" }, EventWaiting, "computing"},
		{"stale base", func(_ *rig, p *PR) { p.Base = "saddle/t0" }, EventWaiting, "restack"},
		{"stack at risk", func(r *rig, _ *PR) { r.risk["t1"] = "t1's branch drifted" }, EventRefused, "at risk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "", "t1")
			r.on(t)
			tc.edit(r, r.gh.prs["pr/t1"])
			st := check(t, r.w)
			if len(r.gh.merged) != 0 || r.restacks != 0 {
				t.Fatalf("merged %v (restacks %d)", r.gh.merged, r.restacks)
			}
			if s := st.Stacks[0]; s.Ready || !strings.Contains(s.Why, tc.why) {
				t.Fatalf("status = %+v, want why containing %q", s, tc.why)
			}
			i := slices.IndexFunc(r.events, func(e event) bool { return e.kind == tc.kind })
			if i < 0 || !strings.Contains(r.events[i].data, tc.why) || r.events[i].task != "t1" {
				t.Fatalf("events = %+v, want %s with %q", r.events, tc.kind, tc.why)
			}
			// The same refusal isn't logged again every tick.
			n := len(r.events)
			check(t, r.w)
			if len(r.events) != n {
				t.Fatalf("refusal logged again: %+v", r.events[n:])
			}
		})
	}
}

func TestPendingThenGreenMerges(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.gh.prs["pr/t1"].Checks = ChecksPending
	check(t, r.w)
	if len(r.gh.merged) != 0 {
		t.Fatal("merged on pending CI")
	}
	r.gh.prs["pr/t1"].Checks = ChecksPass
	check(t, r.w)
	if len(r.gh.merged) != 1 {
		t.Fatal("didn't merge once CI passed")
	}
}

func TestStopsAfterMergeFailure(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.gh.mergeErr = errors.New("required review missing")
	st := check(t, r.w)
	if !strings.Contains(st.Stopped, "required review missing") {
		t.Fatalf("stopped = %q", st.Stopped)
	}
	if len(r.notices) != 1 || !r.notices[0].action || !strings.Contains(r.notices[0].text, "required review missing") {
		t.Fatalf("notices = %+v", r.notices)
	}
	if !slices.Contains(r.kinds(), EventFailed) || r.restacks != 0 {
		t.Fatalf("events %v, restacks %d", r.kinds(), r.restacks)
	}
	// No retry loop: later ticks don't try again, even once GitHub would allow it.
	r.gh.mergeErr = nil
	check(t, r.w)
	check(t, r.w)
	if len(r.gh.merged) != 0 || len(r.notices) != 1 {
		t.Fatalf("retried after a failure: merged %v notices %d", r.gh.merged, len(r.notices))
	}
	// Turning it on again is the escape hatch.
	r.on(t)
	if st := check(t, r.w); st.Stopped != "" || len(r.gh.merged) != 1 {
		t.Fatalf("on didn't resume: %+v merged %v", st, r.gh.merged)
	}
}

func TestStopsAfterRestackFailure(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.on(t)
	r.restack = func() error { return errors.New("restack stopped: t2 conflicts") }
	st := check(t, r.w)
	if !strings.Contains(st.Stopped, "t2 conflicts") || len(r.gh.merged) != 1 {
		t.Fatalf("stopped = %q merged %v", st.Stopped, r.gh.merged)
	}
	r.restack = r.retarget
	check(t, r.w)
	if len(r.gh.merged) != 1 {
		t.Fatalf("kept merging after a failed restack: %v", r.gh.merged)
	}
}

func TestTogglePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "automerge.json")
	r := newRig(t, path, "t1")
	r.on(t)
	if err := r.w.Hold("t9"); err != nil {
		t.Fatal(err)
	}
	again := newRig(t, path, "t1") // a new saddle up
	st, err := again.w.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || !slices.Equal(st.Holds, []string{"t9"}) {
		t.Fatalf("after restart: %+v", st)
	}
	check(t, again.w)
	if len(again.gh.merged) != 1 {
		t.Fatal("restarted watcher doesn't merge")
	}
}

func TestMultiPRStackMergesBottomUp(t *testing.T) {
	r := newRig(t, "", "t1", "t2", "t3")
	r.on(t)
	r.restack = r.retarget
	for range 5 {
		check(t, r.w)
	}
	want := []string{"pr/t1 squash", "pr/t2 squash", "pr/t3 squash"}
	if !slices.Equal(r.gh.merged, want) {
		t.Fatalf("merged = %v, want %v", r.gh.merged, want)
	}
	if r.restacks != 3 {
		t.Fatalf("restacks = %d", r.restacks)
	}
	if st := check(t, r.w); len(st.Stacks) != 0 {
		t.Fatalf("empty stack still listed: %+v", st.Stacks)
	}
}

func TestMergesNothingAboveTheBottom(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.on(t)
	r.gh.prs["pr/t1"].Checks = ChecksFail
	check(t, r.w)
	if len(r.gh.merged) != 0 {
		t.Fatalf("merged above a red bottom: %v", r.gh.merged)
	}
}

// Two independent stacks: graph comes from the PR bases.
func TestGraphFromPRBases(t *testing.T) {
	r := newRig(t, "", "t1", "t2", "t3")
	r.gh.prs["pr/t2"].Base = "main" // t2 starts its own stack; t3 sits on t2
	r.gh.prs["pr/t3"].Base = "saddle/t2"
	st := check(t, r.w)
	if len(st.Stacks) != 2 {
		t.Fatalf("stacks = %+v", st.Stacks)
	}
	a, b := st.Stacks[0], st.Stacks[1]
	if a.ID != "t1" || len(a.Nodes) != 1 || b.ID != "t2" || len(b.Nodes) != 2 || b.Nodes[1].Task != "t3" || b.Nodes[1].Base != "saddle/t2" {
		t.Fatalf("graph = %+v", st.Stacks)
	}
}

func TestHoldPreventsMergeWhileOn(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.on(t)
	if err := r.w.Hold("t2"); err != nil { // any member holds the whole stack
		t.Fatal(err)
	}
	st := check(t, r.w)
	if len(r.gh.merged) != 0 {
		t.Fatalf("merged a held stack: %v", r.gh.merged)
	}
	if s := st.Stacks[0]; !s.Held || !strings.Contains(s.Why, "held") || len(s.Nodes) != 2 {
		t.Fatalf("held stack status = %+v", s)
	}
	if !slices.Contains(r.kinds(), EventRefused) {
		t.Fatalf("no refusal logged for the hold: %v", r.events)
	}

	// Release by the stack's name (its bottom task) resumes it.
	if err := r.w.Release("t1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.w.Status(); len(st.Holds) != 0 {
		t.Fatalf("holds after release = %v", st.Holds)
	}
	check(t, r.w)
	if len(r.gh.merged) != 1 {
		t.Fatalf("release didn't resume: %v", r.gh.merged)
	}
}

func TestHoldOnlyThatStack(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.gh.prs["pr/t2"].Base = "main"
	r.on(t)
	if err := r.w.Hold("t1"); err != nil {
		t.Fatal(err)
	}
	check(t, r.w)
	if !slices.Equal(r.gh.merged, []string{"pr/t2 squash"}) {
		t.Fatalf("merged = %v, want only the unheld stack", r.gh.merged)
	}
}

func TestHeldStackBehindIsFlaggedInfo(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.on(t)
	if err := r.w.Hold("t1"); err != nil {
		t.Fatal(err)
	}
	r.behind["sha-t2"] = 3 // the top of the stack lacks 3 commits of main
	st := check(t, r.w)
	if st.Stacks[0].Behind != 3 {
		t.Fatalf("behind = %d", st.Stacks[0].Behind)
	}
	if len(r.notices) != 1 || r.notices[0].action || !strings.Contains(r.notices[0].text, "stack rebase t1") {
		t.Fatalf("notices = %+v, want one info notice", r.notices)
	}
	if !slices.Contains(r.kinds(), EventBehind) {
		t.Fatalf("events = %v", r.kinds())
	}
	for _, p := range r.gh.prs {
		if slices.Contains(p.Labels, NeedsHuman) {
			t.Fatal("behind flag labeled needs-human")
		}
	}
	check(t, r.w)
	if len(r.notices) != 1 {
		t.Fatalf("behind flagged again: %+v", r.notices)
	}
	// Rebased: the flag lifts, and falling behind again flags it again.
	r.behind["sha-t2"] = 0
	check(t, r.w)
	r.behind["sha-t2"] = 1
	check(t, r.w)
	if len(r.notices) != 2 {
		t.Fatalf("notices = %+v", r.notices)
	}
}

func TestStatusIsReadOnly(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	n := len(r.events)
	st, err := r.w.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.gh.merged) != 0 || len(r.events) != n || r.restacks != 0 {
		t.Fatal("Plan acted")
	}
	if st.Stacks[0].Next != "pr/t1" || !st.Stacks[0].Ready {
		t.Fatalf("plan = %+v", st.Stacks)
	}
	// The last check is kept for readers without GitHub (the TUI).
	check(t, r.w)
	s, err := Load(r.w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Last.Checked.IsZero() || len(s.Last.Stacks) != 0 && s.Last.Stacks[0].ID != "t1" {
		t.Fatalf("last = %+v", s.Last)
	}
}

func TestBusyTrainSkipsTick(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.w.Lock = func() (func(), bool, error) { return nil, false, nil }
	st := check(t, r.w)
	if !st.Busy || len(r.gh.merged) != 0 {
		t.Fatalf("busy: %+v merged %v", st, r.gh.merged)
	}
}

func TestChecksSummary(t *testing.T) {
	for _, tc := range []struct {
		rollup []Check
		want   string
	}{
		{nil, ChecksNone},
		{[]Check{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "COMPLETED", Conclusion: "SKIPPED"}}, ChecksPass},
		{[]Check{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "IN_PROGRESS"}}, ChecksPending},
		{[]Check{{Status: "COMPLETED", Conclusion: "FAILURE"}, {Status: "IN_PROGRESS"}}, ChecksFail},
		{[]Check{{State: "PENDING"}}, ChecksPending},
		{[]Check{{State: "SUCCESS"}}, ChecksPass},
		{[]Check{{State: "ERROR"}}, ChecksFail},
		{[]Check{{Status: "COMPLETED", Conclusion: "CANCELLED"}}, ChecksFail},
	} {
		if got := summarize(tc.rollup); got != tc.want {
			t.Errorf("summarize(%+v) = %s, want %s", tc.rollup, got, tc.want)
		}
	}
}
