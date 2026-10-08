package autopilot

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeEnv is a pipeline in memory: issues, tasks, a cap and a usage gauge.
// Spawned tasks run until the test finishes them.
type fakeEnv struct {
	issues  []Issue
	tasks   []Task
	limit   int
	usage   Usage
	backlog time.Time

	reconciled, landed int
	spawns             []SpawnReq
	spawnErr           map[int]error
	nudges             []string
	busy               bool
	notices            []string
	events             []string
	parks, unparked    []string
}

func (f *fakeEnv) Park() ([]string, error) {
	var out []string
	for i, t := range f.tasks {
		if t.Live && !t.Paused {
			f.tasks[i].Paused = true
			out = append(out, t.ID)
		}
	}
	f.parks = append(f.parks, strings.Join(out, ","))
	return out, nil
}

func (f *fakeEnv) Unpark(id string) error {
	for i, t := range f.tasks {
		if t.ID == id {
			f.tasks[i].Paused = false
		}
	}
	f.unparked = append(f.unparked, id)
	return nil
}

func (f *fakeEnv) Tasks() ([]Task, error) { return slices.Clone(f.tasks), nil }
func (f *fakeEnv) Ready(label string) ([]Issue, error) {
	return slices.Clone(f.issues), nil
}

func (f *fakeEnv) Capacity() (int, int, error) {
	n := 0
	for _, t := range f.tasks {
		if t.Live {
			n++
		}
	}
	return n, f.limit, nil
}
func (f *fakeEnv) Usage(time.Time) (Usage, error) { return f.usage, nil }
func (f *fakeEnv) Reconcile() ([]string, error)   { f.reconciled++; return nil, nil }
func (f *fakeEnv) Land() (int, error)             { f.landed++; return 0, nil }
func (f *fakeEnv) Backlog() (time.Time, bool)     { return f.backlog, !f.backlog.IsZero() }
func (f *fakeEnv) Notify(interrupt bool, s string) {
	f.notices = append(f.notices, fmt.Sprint(interrupt, " ", s))
}
func (f *fakeEnv) Event(kind, data string) { f.events = append(f.events, kind+": "+data) }
func (f *fakeEnv) Nudge(s string) (bool, error) {
	if f.busy {
		return false, nil
	}
	f.nudges = append(f.nudges, s)
	return true, nil
}

func (f *fakeEnv) Spawn(r SpawnReq) (string, error) {
	if err := f.spawnErr[r.Issue]; err != nil {
		return "", err
	}
	f.spawns = append(f.spawns, r)
	id := fmt.Sprintf("t%d", len(f.tasks)+1)
	f.tasks = append(f.tasks, Task{ID: id, Issue: r.Issue, Claims: r.Claims, Live: true})
	return id, nil
}

// finish marks the task working on issue n done and its issue closed.
func (f *fakeEnv) finish(n int) {
	for i, t := range f.tasks {
		if t.Issue == n {
			f.tasks[i].Live = false
		}
	}
	f.issues = slices.DeleteFunc(f.issues, func(is Issue) bool { return is.Number == n })
}

func (f *fakeEnv) spawned() []int {
	var out []int
	for _, r := range f.spawns {
		out = append(out, r.Issue)
	}
	return out
}

func (f *fakeEnv) hasEvent(prefix string) bool {
	return slices.ContainsFunc(f.events, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

type rig struct {
	t   *testing.T
	env *fakeEnv
	d   *Driver
	now time.Time
}

func newRig(t *testing.T, limit int, issues ...Issue) *rig {
	r := &rig{t: t, env: &fakeEnv{issues: issues, limit: limit}, now: time.Date(2026, 10, 8, 22, 0, 0, 0, time.UTC)}
	r.d = &Driver{Path: filepath.Join(t.TempDir(), "autopilot.json"), Env: r.env,
		Now: func() time.Time { return r.now }, Rules: "rules", DoneWhen: "done when"}
	return r
}

func (r *rig) on(o Options) {
	r.t.Helper()
	if _, err := r.d.Enable(o); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) tick() Report {
	r.t.Helper()
	rep, err := r.d.Tick()
	if err != nil {
		r.t.Fatal(err)
	}
	r.now = r.now.Add(DefaultInterval)
	return rep
}

func (r *rig) state() State {
	r.t.Helper()
	st, err := Load(r.d.Path)
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func claimed(n int, globs ...string) Issue {
	return Issue{Number: n, Title: fmt.Sprintf("issue %d", n), Body: "claims: " + strings.Join(globs, ", ")}
}

func TestTickDoesNothingWhenOff(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	rep := r.tick()
	if rep.Decision != "off" || r.env.reconciled+r.env.landed+len(r.env.spawns) != 0 {
		t.Fatalf("off tick did work: %+v %+v", rep, r.env)
	}
}

func TestTopUpToCapWithDisjointClaims(t *testing.T) {
	r := newRig(t, 2,
		claimed(1, "a/**"),
		claimed(2, "a/x.go"), // overlaps #1
		claimed(3, "b/**"),
		claimed(4, "c/**"))
	r.on(Options{})
	rep := r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("spawned %v, want #1 and #3 (cap 2, #2 overlaps #1)", got)
	}
	if !strings.Contains(rep.Skipped[2], "a/**") {
		t.Errorf("why #2 waits = %q, want the overlapping claim", rep.Skipped[2])
	}
	if r.env.reconciled != 1 || r.env.landed != 1 {
		t.Errorf("reconcile %d, land %d; want both once per tick", r.env.reconciled, r.env.landed)
	}
	r.tick()
	if len(r.env.spawns) != 2 {
		t.Fatalf("spawned past the cap: %v", r.env.spawned())
	}
	r.env.finish(1)
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 3, 2}) {
		t.Fatalf("after #1 finished, spawned %v; want #2 next (oldest first)", got)
	}
	if sp := r.env.spawns[0]; sp.Model != "opus" || !slices.Equal(sp.Claims, []string{"a/**"}) || !strings.Contains(sp.Prompt, "rules") || !strings.Contains(sp.Prompt, "done when") {
		t.Errorf("spawn request = %+v", sp)
	}
	if st := r.state(); len(st.Spawned) != 3 || st.LastTick.IsZero() || st.LastDecision == "" {
		t.Errorf("state = %+v", st)
	}
}

func TestSkipsIssuesWithPROrTask(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"), claimed(2, "b/**"), claimed(3, "c/**"))
	r.env.issues[0].HasPR = true
	r.env.tasks = []Task{{ID: "t9", Issue: 2, Claims: []string{"b/**"}}} // done, not live: still has a task
	r.on(Options{})
	rep := r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{3}) {
		t.Fatalf("spawned %v, want only #3", got)
	}
	if !strings.Contains(rep.Skipped[1], "PR") || !strings.Contains(rep.Skipped[2], "t9") {
		t.Errorf("skips = %v", rep.Skipped)
	}
}

func TestUndeclaredClaimsRunAlone(t *testing.T) {
	r := newRig(t, 4, Issue{Number: 1, Title: "vague"}, claimed(2, "b/**"))
	r.env.tasks = []Task{{ID: "t1", Claims: []string{"z/**"}, Live: true}}
	r.on(Options{})
	rep := r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{2}) {
		t.Fatalf("spawned %v; #1 declares no claims so it waits for an empty pipeline", got)
	}
	if !strings.Contains(rep.Skipped[1], "no claims") {
		t.Errorf("why #1 waits = %q", rep.Skipped[1])
	}
	r.env.tasks[0].Live = false
	r.env.finish(2)
	r.env.issues = append(r.env.issues, claimed(3, "c/**"))
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("spawned %v, want #1 alone once nothing runs", got)
	}
	r.tick()
	if len(r.env.spawns) != 2 {
		t.Fatalf("spawned %v next to a claimless task", r.env.spawned())
	}
}

func TestAfterLinesWaitForTheirDependency(t *testing.T) {
	dep := claimed(1, "a/**")
	next := Issue{Number: 2, Title: "builds on 1", Body: "claims: b/**\nafter: #1"}
	r := newRig(t, 4, dep, next)
	r.on(Options{})
	rep := r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1}) || !strings.Contains(rep.Skipped[2], "#1") {
		t.Fatalf("spawned %v, skips %v; #2 waits for #1", got, rep.Skipped)
	}
	r.env.tasks[0].Live = false // done; its issue stays open until the PR merges
	r.env.issues[0].HasPR = true
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("spawned %v once #1 was done", got)
	}
	if after := r.env.spawns[1].After; !slices.Equal(after, []string{"t1"}) {
		t.Errorf("#2 stacks after %v, want t1", after)
	}
}

func TestModelScopeFromTicket(t *testing.T) {
	r := newRig(t, 4, Issue{Number: 1, Title: "small", Body: "**Model scope: Sonnet.** rename\nclaims: a/**"})
	r.on(Options{})
	r.tick()
	if m := r.env.spawns[0].Model; m != "sonnet" {
		t.Fatalf("model = %q", m)
	}
}

func TestUsagePauseSleepsUntilReset(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"), claimed(2, "b/**"))
	r.on(Options{})
	reset := r.now.Add(90 * time.Minute)
	r.env.usage = Usage{Percent: 1.02, Pause: true, ResetsAt: reset}
	rep := r.tick()
	if len(r.env.spawns) != 0 || !rep.Sleeping {
		t.Fatalf("spawned under a usage pause: %+v", rep)
	}
	if st := r.state(); !st.SleepUntil.Equal(reset) || !strings.Contains(st.LastDecision, "sleeping") {
		t.Fatalf("state = %+v", st)
	}
	if !r.env.hasEvent(EventSleep) {
		t.Errorf("no sleep event: %v", r.env.events)
	}
	r.env.usage = Usage{} // the estimate drops early, but the reset is the plan
	r.tick()
	if len(r.env.spawns) != 0 {
		t.Fatal("woke before the reset time")
	}
	r.now = reset
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("after the reset spawned %v", got)
	}
	if !r.env.hasEvent(EventWake) || !r.state().SleepUntil.IsZero() {
		t.Errorf("no wake: %v", r.env.events)
	}
}

func TestSpawnRefusedForLaunchPauseSleeps(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.env.spawnErr = map[int]error{1: fmt.Errorf("spawn: %w", ErrPaused)}
	r.on(Options{})
	rep := r.tick()
	if !rep.Sleeping || r.state().SleepUntil.IsZero() {
		t.Fatalf("a launch pause from spawn doesn't sleep: %+v", rep)
	}
}

func TestMaxTasksDrainsThenStopsWithSummary(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"), claimed(2, "b/**"), claimed(3, "c/**"))
	r.on(Options{Stop: Stop{MaxTasks: 2}})
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("spawned %v with max-tasks 2", got)
	}
	r.tick()
	if st := r.state(); !st.On || !strings.Contains(st.Draining, "max tasks") {
		t.Fatalf("not draining: %+v", st)
	}
	r.env.finish(1)
	r.env.finish(2)
	rep := r.tick()
	st := r.state()
	if st.On || rep.Stopped == "" || !strings.Contains(st.Summary, "2 tasks") || !strings.Contains(st.Summary, "#1") {
		t.Fatalf("after draining: rep %+v state %+v", rep, st)
	}
	if len(r.env.notices) != 1 || !strings.HasPrefix(r.env.notices[0], "true autopilot stopped: max tasks") {
		t.Errorf("notices = %q, want one interrupt", r.env.notices)
	}
	if !r.env.hasEvent(EventStopped) {
		t.Errorf("no stop event")
	}
	r.tick()
	if len(r.env.notices) != 1 {
		t.Errorf("stopped twice")
	}
}

func TestUntilTimeStopsSpawning(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{Stop: Stop{Until: r.now}})
	r.tick()
	if len(r.env.spawns) != 0 || r.state().On {
		t.Fatalf("spawned past --until or kept running: %+v", r.state())
	}
	if !strings.Contains(r.state().Stopped, "time reached") {
		t.Errorf("stopped = %q", r.state().Stopped)
	}
}

func TestUntilUsageDrains(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"), claimed(2, "b/**"))
	r.env.limit = 1
	r.on(Options{Stop: Stop{UntilUsage: 0.8}})
	r.tick()
	r.env.usage.Percent = 0.85
	r.env.finish(1)
	r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{1}) {
		t.Fatalf("spawned %v past the usage stop", got)
	}
	if st := r.state(); st.On || !strings.Contains(st.Stopped, "usage") {
		t.Fatalf("state = %+v", st)
	}
}

func TestQueueEmptyStops(t *testing.T) {
	r := newRig(t, 4)
	r.on(Options{})
	r.tick()
	if st := r.state(); st.On || st.Stopped != "queue empty" {
		t.Fatalf("state = %+v", st)
	}
}

func TestStallNudgeIsDedupedAndWaitsForAnIdleOrchestrator(t *testing.T) {
	// #2 overlaps the running #1: ready work, a free slot, nothing spawnable.
	r := newRig(t, 4, claimed(1, "a/**"), claimed(2, "a/**"))
	r.on(Options{})
	r.tick()
	r.tick()
	if len(r.env.nudges) != 0 {
		t.Fatalf("nudged before StallAfter: %q", r.env.nudges)
	}
	r.env.busy = true
	r.now = r.now.Add(DefaultStallAfter)
	r.tick()
	if len(r.env.nudges) != 0 {
		t.Fatal("nudged a busy orchestrator")
	}
	r.env.busy = false
	r.tick()
	if len(r.env.nudges) != 1 {
		t.Fatalf("nudges = %q, want one", r.env.nudges)
	}
	n := r.env.nudges[0]
	if strings.Contains(n, "\n") || !strings.Contains(n, "1/4 running") || !strings.Contains(n, "1 ready") {
		t.Errorf("nudge = %q; want one line with a state digest", n)
	}
	for range 5 {
		r.tick()
	}
	if len(r.env.nudges) != 1 {
		t.Fatalf("nudge repeated: %q", r.env.nudges)
	}
	r.now = r.now.Add(DefaultNudgeEvery)
	r.tick()
	if len(r.env.nudges) != 2 {
		t.Fatalf("no repeat after NudgeEvery: %q", r.env.nudges)
	}
	if len(r.env.notices) != 0 {
		t.Errorf("a nudge went out as a notice: %q", r.env.notices)
	}
}

func TestStallNudgeWhenOrchestratorIgnoresNotices(t *testing.T) {
	r := newRig(t, 1, claimed(1, "a/**"), claimed(2, "b/**"))
	r.on(Options{})
	r.tick()
	r.env.backlog = r.now
	r.now = r.now.Add(DefaultStallAfter)
	r.tick()
	if len(r.env.nudges) != 1 || !strings.Contains(r.env.nudges[0], "notices waiting") {
		t.Fatalf("nudges = %q", r.env.nudges)
	}
}

func TestPauseResumeOff(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"), claimed(2, "b/**"))
	r.on(Options{})
	if _, err := r.d.Pause(); err != nil {
		t.Fatal(err)
	}
	if rep := r.tick(); len(r.env.spawns) != 0 || !strings.Contains(rep.Decision, "paused") {
		t.Fatalf("paused tick spawned: %+v", rep)
	}
	if _, err := r.d.Resume(); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if len(r.env.spawns) != 2 {
		t.Fatalf("resume didn't top up: %v", r.env.spawned())
	}
	st, err := r.d.Disable()
	if err != nil || st.On || st.Stopped != "turned off" || st.Summary == "" {
		t.Fatalf("off = %+v, %v", st, err)
	}
	if len(r.env.notices) != 0 {
		t.Errorf("the owner's own off interrupted them: %q", r.env.notices)
	}
}

// A toggle written while a tick runs wins over the tick's save.
func TestControlChangeDuringTickWins(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	r.env.spawnErr = map[int]error{}
	d := r.d
	d.Env = hookEnv{r.env, func() { _, _ = d.Pause() }}
	r.tick()
	if st := r.state(); !st.Paused {
		t.Fatalf("pause lost to a concurrent tick: %+v", st)
	}
}

type hookEnv struct {
	*fakeEnv
	during func()
}

func (h hookEnv) Land() (int, error) { h.during(); return h.fakeEnv.Land() }

func TestSpawnErrorSkipsOnlyThatIssue(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"), claimed(2, "b/**"))
	r.env.spawnErr = map[int]error{1: errors.New("claim conflict with t7")}
	r.on(Options{})
	rep := r.tick()
	if got := r.env.spawned(); !slices.Equal(got, []int{2}) || !strings.Contains(rep.Skipped[1], "t7") {
		t.Fatalf("spawned %v skips %v", got, rep.Skipped)
	}
}

// An issue this run spawned is never spawned again, even when the task
// doesn't record its issue.
func TestNeverRespawnsAnIssue(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	r.tick()
	r.env.tasks[0].Issue = 0
	r.env.tasks[0].Live = false
	rep := r.tick()
	if len(r.env.spawns) != 1 || !strings.Contains(rep.Skipped[1], "t1") {
		t.Fatalf("respawned #1: %v, skips %v", r.env.spawned(), rep.Skipped)
	}
}
