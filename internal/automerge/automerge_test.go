package automerge

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeGH struct {
	prs      map[string]*PR
	merged   []string
	mergeErr error
	method   string
	onPR     func(url string) // runs at each PR read, as if gh were slow
}

func (f *fakeGH) PR(url string) (PR, error) {
	if f.onPR != nil {
		f.onPR(url)
	}
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

// idles lists the idle events' data.
func (r *rig) idles() []string {
	var out []string
	for _, e := range r.events {
		if e.kind == EventIdle {
			out = append(out, e.data)
		}
	}
	return out
}

// clock is a settable Now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// The 2026-10-03 bug: every tick found the train lock held and returned
// without saving or logging anything, so status called ready PRs ready while
// nothing merged for an hour. A busy tick now records itself, says why the
// ready PR waits and when the next check is, and logs it once per interval.
func TestBusyTickRecordsWhyAndRetriesSoon(t *testing.T) {
	r := newRig(t, "", "t3")
	c := &clock{time.Date(2026, 10, 3, 12, 41, 0, 0, time.Local)}
	r.w.Now = c.now
	r.on(t)
	// The last check saw t3 ready, then the train got busy.
	s, _ := Load(r.w.Path)
	st, _ := r.w.plan(s)
	s.Last = st
	if err := s.Save(r.w.Path); err != nil {
		t.Fatal(err)
	}
	r.w.Lock = func() (func(), bool, error) { return nil, false, nil }

	c.t = c.t.Add(2 * time.Minute)
	busySince := c.t
	got := check(t, r.w)
	if !got.Busy || !got.BusySince.Equal(busySince) || !got.Next.Equal(c.t.Add(DefaultBusyRetry)) {
		t.Fatalf("busy tick = busy %v since %v next %v", got.Busy, got.BusySince, got.Next)
	}
	if len(got.Stacks) != 1 || !got.Stacks[0].Ready || !strings.Contains(got.Stacks[0].Blocked, "train lock is busy") {
		t.Fatalf("stacks = %+v", got.Stacks)
	}
	saved, err := r.w.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Busy || !saved.Next.Equal(got.Next) {
		t.Fatalf("busy tick not saved: %+v", saved)
	}
	if idle := r.idles(); len(idle) != 1 || !strings.Contains(idle[0], "pr/t3 ready but not merged: the train lock is busy") {
		t.Fatalf("idle events = %q", idle)
	}

	// Retries inside the interval keep BusySince and don't log again.
	c.t = c.t.Add(DefaultBusyRetry)
	got = check(t, r.w)
	if !got.BusySince.Equal(busySince) || len(r.idles()) != 1 {
		t.Fatalf("retry: since %v, idle %q", got.BusySince, r.idles())
	}
	// After an interval the same reason is logged again.
	c.t = c.t.Add(DefaultInterval)
	check(t, r.w)
	if len(r.idles()) != 2 {
		t.Fatalf("idle after an interval = %q", r.idles())
	}

	// The lock frees: the next tick merges and clears busy.
	r.w.Lock = func() (func(), bool, error) { return func() {}, true, nil }
	got = check(t, r.w)
	if got.Busy || !got.BusySince.IsZero() || got.Merged != "pr/t3" {
		t.Fatalf("after the lock freed: %+v", got)
	}
}

func TestRunRetriesSoonAfterBusyTick(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	var tries atomic.Int32
	r.w.Lock = func() (func(), bool, error) {
		if tries.Add(1) < 3 {
			return nil, false, nil
		}
		return func() {}, true, nil
	}
	r.w.Interval, r.w.BusyRetry = time.Hour, time.Millisecond
	merged := make(chan struct{})
	r.w.Restack = func() error { close(merged); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.w.Run(ctx) }()
	select {
	case <-merged:
	case <-time.After(10 * time.Second):
		t.Fatal("Run waited a whole interval after a busy tick")
	}
	cancel()
	<-done
	if len(r.gh.merged) != 1 {
		t.Fatalf("merged = %v", r.gh.merged)
	}
}

func TestIdleEventWhenStoppedNotWhenOff(t *testing.T) {
	r := newRig(t, "", "t1")
	check(t, r.w)
	if len(r.idles()) != 0 {
		t.Fatalf("logged idle while off: %q", r.idles())
	}
	st := check(t, r.w)
	if !strings.Contains(st.Stacks[0].Blocked, "auto-merge is off") {
		t.Fatalf("off: blocked = %q", st.Stacks[0].Blocked)
	}
	r.on(t)
	s, _ := Load(r.w.Path)
	s.Stopped = "merging pr/x failed"
	if err := s.Save(r.w.Path); err != nil {
		t.Fatal(err)
	}
	st = check(t, r.w)
	if len(r.gh.merged) != 0 || !strings.Contains(st.Stacks[0].Blocked, "stopped") {
		t.Fatalf("stopped: merged %v, %+v", r.gh.merged, st.Stacks)
	}
	if idle := r.idles(); len(idle) != 1 || !strings.Contains(idle[0], "merging pr/x failed") {
		t.Fatalf("idle = %q", idle)
	}
}

func TestPlanSaysWhyEachReadyStackWaits(t *testing.T) {
	r := newRig(t, "", "t1")
	r.entries = append(r.entries, Entry{Task: "t2", Branch: "saddle/t2", PR: "pr/t2"})
	r.gh.prs["pr/t2"] = ready("t2", "main")
	c := &clock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)}
	r.w.Now = c.now
	r.on(t)

	st, err := r.w.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Stacks[0].Blocked, "no watcher has checked yet") || !strings.Contains(st.Stacks[1].Blocked, "after pr/t1") {
		t.Fatalf("before any check: %q / %q", st.Stacks[0].Blocked, st.Stacks[1].Blocked)
	}
	got := check(t, r.w)
	if got.Merged != "pr/t1" || !strings.Contains(got.Stacks[1].Blocked, "one PR merges per check and this one merged pr/t1") {
		t.Fatalf("check: %+v", got)
	}
	st, _ = r.w.Plan()
	if len(st.Stacks) != 1 || st.Stacks[0].Blocked != "the watcher merges it at its next check, at 12:02:00" {
		t.Fatalf("plan after a check: %+v", st.Stacks)
	}
	// A watcher long overdue is called out: nothing is running it.
	c.t = c.t.Add(time.Hour)
	st, _ = r.w.Plan()
	if !strings.Contains(st.Stacks[0].Blocked, "was due at 12:02:00 and hasn't checked since 12:00:00") {
		t.Fatalf("overdue: %q", st.Stacks[0].Blocked)
	}
}

// Each CLI call and restart builds a new Watcher; a refusal is still logged
// once per change of reason.
func TestRefusalLoggedOnceAcrossWatchers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "automerge.json")
	r := newRig(t, path, "t1")
	r.gh.prs["pr/t1"].Checks = ChecksFail
	r.on(t)
	check(t, r.w)
	r2 := newRig(t, path, "t1")
	r2.gh.prs["pr/t1"].Checks = ChecksFail
	check(t, r2.w)
	if n := len(r.events) + len(r2.events); slices.Index(r.kinds(), EventRefused) < 0 || slices.Contains(r2.kinds(), EventRefused) {
		t.Fatalf("refusals: first %v, second %v (%d events)", r.kinds(), r2.kinds(), n)
	}
}

// A hold or a toggle saved while a check reads GitHub survives the check's
// save (#209): a check writes only the fields it owns.
func TestHoldAndToggleDuringCheckSurvive(t *testing.T) {
	r := newRig(t, "", "t1")
	r.gh.prs["pr/t1"].Checks = ChecksPending // nothing merges
	r.gh.onPR = func(string) {
		r.gh.onPR = nil
		if err := r.w.Hold("t1"); err != nil {
			t.Error(err)
		}
		if err := r.w.SetEnabled(true); err != nil {
			t.Error(err)
		}
	}
	st := check(t, r.w)
	saved, err := r.w.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []Status{st, saved} {
		if !x.Enabled || x.Source != SourceRuntime || !slices.Equal(x.Holds, []string{"t1"}) {
			t.Fatalf("lost the hold or toggle: enabled %v (%s), holds %v", x.Enabled, x.Source, x.Holds)
		}
	}
	if !saved.Stacks[0].Held {
		t.Fatalf("saved stack not held: %+v", saved.Stacks[0])
	}
}

// The same for a busy tick, which reads GitHub without the train lock.
func TestHoldDuringBusyTickSurvives(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.w.Lock = func() (func(), bool, error) { return nil, false, nil }
	r.gh.onPR = func(string) {
		r.gh.onPR = nil
		if err := r.w.Hold("t1"); err != nil {
			t.Error(err)
		}
	}
	check(t, r.w)
	if saved, _ := r.w.Status(); !slices.Equal(saved.Holds, []string{"t1"}) || !saved.Busy {
		t.Fatalf("busy tick lost the hold: holds %v busy %v", saved.Holds, saved.Busy)
	}
}

// A stack held, or auto-merge turned off, while a check reads GitHub is not
// merged by that check.
func TestHoldOrOffDuringCheckPreventsMerge(t *testing.T) {
	for name, act := range map[string]func(w *Watcher) error{
		"hold": func(w *Watcher) error { return w.Hold("t1") },
		"off":  func(w *Watcher) error { return w.SetEnabled(false) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "", "t1")
			r.on(t)
			r.gh.onPR = func(string) {
				r.gh.onPR = nil
				if err := act(r.w); err != nil {
					t.Error(err)
				}
			}
			st := check(t, r.w)
			if len(r.gh.merged) != 0 || st.Merged != "" {
				t.Fatalf("merged %v after %s during the check", r.gh.merged, name)
			}
		})
	}
}

// A stop cleared by `on` while a check runs stays cleared.
func TestOnDuringCheckClearsStop(t *testing.T) {
	r := newRig(t, "", "t1")
	r.gh.prs["pr/t1"].Checks = ChecksPending
	r.on(t)
	s, _ := Load(r.w.Path)
	s.Stopped = "merging pr/x failed"
	if err := s.Save(r.w.Path); err != nil {
		t.Fatal(err)
	}
	r.gh.onPR = func(string) {
		r.gh.onPR = nil
		if err := r.w.SetEnabled(true); err != nil {
			t.Error(err)
		}
	}
	check(t, r.w)
	if saved, _ := r.w.Status(); saved.Stopped != "" {
		t.Fatalf("stop came back: %q", saved.Stopped)
	}
}
