package soak

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/e2e"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/sentinel"
	"github.com/brandonapol/saddle/internal/store"
)

// Excluded is the adapter the soak's policy disables. An excluded-adapter
// change's ticket asks for it anyway.
const Excluded = "codex"

// Options shape a run.
type Options struct {
	// MaxTicks ends a run that hasn't settled (default 40 + 4 per change).
	MaxTicks int
	// Settle is the longest a tick waits for running agents (default 8s).
	Settle time.Duration
	// Deadline ends a run that hasn't settled by then (zero: none), so a
	// stalled soak still reports its outcomes instead of timing out.
	Deadline time.Time
	Budgets  Budgets
	// Logf logs progress; nil is quiet.
	Logf func(format string, args ...any)
}

// WorldOptions is the e2e config a scenario runs under: the local gate
// builds the module (a change without its dependency fails it), autopilot
// may run four at once, and the excluded adapter is disabled.
func WorldOptions() e2e.Options {
	return e2e.Options{
		Top:     "concurrency = 4\n",
		TestCmd: "go build ./...",
		Tables:  "[adapters." + Excluded + "]\ndisabled = true\n\n[ci]\nrepair_attempts = 2\n",
	}
}

// Run is one scenario in flight on a World.
type Run struct {
	W     *e2e.World
	Sc    Scenario
	Opt   Options
	State string // the harness's directory: agent scripts, hung and released markers

	a      *app.App
	pilot  *autopilot.Driver
	ci     *CI
	ledger Ledger
	rng    *rand.Rand
	tick   int
	// killPending is an engine kill that hasn't caught a land mid-run yet.
	killPending bool
	// throttle is fakegh's rate limit this tick ("" for none).
	throttle string
	start    time.Time
	actions  []string
}

// Start sets the World up for sc: a Go module on main, one fake-agent
// script for every task id, the issues on fakegh, auto-merge on and
// autopilot on in infinite mode. That is configuration before the run;
// nothing after it is a human action.
func Start(w *e2e.World, sc Scenario, opt Options) *Run {
	t := w.T
	t.Helper()
	if opt.MaxTicks == 0 {
		opt.MaxTicks = 40 + 4*len(sc.Changes)
	}
	if opt.Settle == 0 {
		opt.Settle = 8 * time.Second
	}
	if opt.Budgets == nil {
		opt.Budgets = DefaultBudgets
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	// A task whose window is gone is an orphan at once: a killed worker
	// is resumed on the next tick, not ten real minutes later.
	old := app.OrphanAfter
	app.OrphanAfter = 0
	t.Cleanup(func() { app.OrphanAfter = old })
	r := &Run{W: w, Sc: sc, Opt: opt, State: filepath.Join(w.Root, "soak"), rng: rand.New(rand.NewSource(sc.Seed ^ 0x5eed)), start: time.Now()}
	for _, d := range []string{"changes", "hung", "released"} {
		must(t, os.MkdirAll(filepath.Join(r.State, d), 0o755))
	}
	for _, c := range sc.Changes {
		must(t, os.WriteFile(filepath.Join(r.State, "changes", c.ID+".sh"), []byte(ChangeScript(c, r.State)), 0o755))
	}
	must(t, os.WriteFile(filepath.Join(r.State, "repair.sh"), []byte(RepairScript), 0o755))
	dispatch := filepath.Join(r.State, "agent.sh")
	must(t, os.WriteFile(dispatch, []byte(DispatchScript(r.State)), 0o755))
	// Task ids are handed out in spawn order, repairs among the changes,
	// so every id runs the same dispatcher.
	script := fa.Script{Steps: []fa.Step{fa.Run("sh " + shq(dispatch)), fa.Done("Soak work.")}}
	for i := 1; i <= 4*len(sc.Changes)+20; i++ {
		must(t, script.Save(w.Scripts, fmt.Sprintf("t%d", i)))
	}

	w.WriteFile("go.mod", GoMod)
	w.Git(w.Repo, "add", "-A")
	w.Git(w.Repo, "commit", "-q", "-m", "soak module")
	w.Git(w.Repo, "push", "-q", "origin", "main")

	must(t, w.GH.Update(func(s *fakegh.State) error {
		s.Repo.AllowSquash, s.Repo.AllowMerge, s.Repo.DeleteBranch = true, false, true
		s.Protected = true
		s.Required = []string{CheckName}
		s.DefaultChecks = []fakegh.Check{{Name: CheckName, Workflow: "CI", State: fakegh.Pending}}
		s.StacksDisabled = !sc.AsyncMerge
		for _, c := range sc.Changes {
			s.Issues = append(s.Issues, &fakegh.Issue{Number: c.Issue, Title: IssueTitle(c), Body: IssueBody(c, sc, Excluded), State: "OPEN", Labels: []string{autopilot.DefaultReadyLabel}})
		}
		s.Next = max(s.Next, len(sc.Changes)+1)
		return nil
	}))
	w.MustSaddle("automerge", "on")

	r.a = w.App()
	r.ci = NewCI(w, r.Opt.Logf)
	r.ci.Flaky = r.flaky
	r.pilot = r.a.NewAutopilot(r.ready)
	_, err := r.pilot.Enable(autopilot.Options{})
	must(t, err)
	_, err = r.pilot.SetInfinite(true)
	must(t, err)
	opt.Logf("soak: %s", sc)
	for _, c := range sc.Changes {
		opt.Logf("soak: %s (#%d) after %v faults %v", c.ID, c.Issue, c.After, c.Faults)
	}
	for _, e := range sc.Events {
		opt.Logf("soak: %s at tick %d for %d", e.Kind, e.At, e.Ticks)
	}
	return r
}

// ready is autopilot's ready queue: the open issues on fakegh, read
// through the rate limit like gh issue list would be.
func (r *Run) ready(string) ([]autopilot.Issue, error) {
	s, err := r.W.GH.Load()
	if err != nil {
		return nil, err
	}
	if s.Throttle != "" {
		return nil, fmt.Errorf("gh issue list: %w", forge.ErrRateLimited)
	}
	var out []autopilot.Issue
	for _, is := range s.Issues {
		if is.State == "OPEN" && slices.Contains(is.Labels, autopilot.DefaultReadyLabel) {
			out = append(out, autopilot.Issue{Number: is.Number, Title: is.Title, Body: is.Body, Labels: is.Labels})
		}
	}
	return out, nil
}

// flaky names the flaky change whose task branch is branch, or "".
func (r *Run) flaky(branch string) string {
	ts, err := r.a.Store.Tasks()
	if err != nil {
		return ""
	}
	for _, t := range ts {
		if t.Branch == branch {
			if c, ok := r.changeOf(t); ok && c.Has(Flaky) {
				return c.ID
			}
		}
	}
	return ""
}

// simNow is the run's simulated clock: Sc.TickMinutes per tick.
func (r *Run) simNow() time.Time {
	return r.start.Add(time.Duration(r.tick*r.Sc.TickMinutes) * time.Minute)
}

// Drive ticks until every change is terminal or MaxTicks pass, then
// returns the result.
func (r *Run) Drive() Result {
	r.pilot.Now = r.simNow
	for r.tick = 0; r.tick < r.Opt.MaxTicks; r.tick++ {
		r.Tick()
		if r.tick%5 == 4 {
			for _, o := range r.outcomes() {
				if o.State == Stuck {
					r.Opt.Logf("tick %d: %s %s: %s", r.tick, o.Change, o.Task, o.Detail)
				}
			}
		}
		if r.settled() {
			break
		}
		if !r.Opt.Deadline.IsZero() && time.Now().After(r.Opt.Deadline) {
			r.Opt.Logf("tick %d: out of time", r.tick)
			break
		}
	}
	return r.Result()
}

// Tick is one pass of everything `saddle up` runs, with the faults due now.
func (r *Run) Tick() {
	t := r.W.T
	t.Helper()
	active := r.Sc.Active(r.tick)
	throttle := ""
	if slices.Contains(active, RateLimit) {
		throttle = fakegh.ThrottlePrimary
	}
	r.throttle = throttle
	r.quota()
	r.ci.RedBase = slices.Contains(active, RedBase)

	var phases []string
	mark := time.Now()
	lap := func(name string) {
		r.quota()
		phases = append(phases, fmt.Sprintf("%s %s", name, time.Since(mark).Round(10*time.Millisecond)))
		mark = time.Now()
	}
	r.killHung()
	if slices.Contains(active, EngineKill) {
		r.killPending = true
	}
	if r.killPending {
		r.killPending = !r.killLand()
	}
	rep, err := r.pilot.Tick()
	if err != nil {
		r.Opt.Logf("tick %d: autopilot: %v", r.tick, err)
	}
	lap("autopilot")
	r.settle()
	lap("settle")
	r.ci.Grade()
	lap("ci")
	ctx := context.Background()
	if _, err := sentinel.NewCIRed(r.a).Check(ctx); err != nil {
		r.Opt.Logf("tick %d: ci-red: %v", r.tick, err)
	}
	lap("ci-red")
	if _, err := sentinel.New(r.a).Check(); err != nil {
		r.Opt.Logf("tick %d: sentinel: %v", r.tick, err)
	}
	lap("sentinel")
	// The sentinel's stall budget, on the simulated clock: cards whose
	// budget ran out take their default, as they would unattended.
	if _, err := r.a.StallTick(r.simNow()); err != nil {
		r.Opt.Logf("tick %d: stall: %v", r.tick, err)
	}
	lap("stall")
	if st, err := r.a.NewAutomerge(nil).Check(); err != nil {
		r.Opt.Logf("tick %d: automerge: %v", r.tick, err)
	} else if st.Merged != "" {
		r.Opt.Logf("tick %d: merged %s", r.tick, st.Merged)
	}
	lap("automerge")
	vs, err := r.a.CheckInvariants(time.Now())
	lap("invariants")
	if err != nil {
		r.Opt.Logf("tick %d: invariants: %v", r.tick, err)
	}
	r.ledger.Observe(r.tick, vs)
	r.Opt.Logf("tick %d %v: spawned %d, landed %d, %d violations; %s [%s]", r.tick, active, len(rep.Spawned), rep.Landed, len(vs), rep.Decision, strings.Join(phases, ", "))
	for _, v := range vs {
		r.Opt.Logf("tick %d: violation %s", r.tick, v)
	}
}

// quota sets fakegh's rate limit for this tick. The window is simulated
// time, so the spent bucket always resets some real seconds from now:
// saddle's hold (which it reads from gh api rate_limit) lifts, and its
// next call is limited again until the window ends. It is refreshed
// before each phase so a hold never falls back to its 15-minute guess.
func (r *Run) quota() {
	must(r.W.T, r.W.GH.Update(func(s *fakegh.State) error {
		s.Throttle = r.throttle
		s.Quota = fakegh.Quota{Limit: 5000, Remaining: 5000, Reset: time.Now().Add(10 * time.Second).Unix()}
		if r.throttle != "" {
			s.Quota.Remaining = 0
		}
		return nil
	}))
}

// killHung kills the window of every killed-worker change whose agent
// hangs, then lets the script through, so the resumed agent finishes.
func (r *Run) killHung() {
	ents, _ := os.ReadDir(filepath.Join(r.State, "hung"))
	for _, e := range ents {
		id := e.Name()
		rel := filepath.Join(r.State, "released", id)
		if _, err := os.Stat(rel); err == nil {
			continue
		}
		c, ok := r.Sc.Change(id)
		if !ok {
			continue
		}
		tk, ok := r.taskOf(c)
		if !ok || tk.Window == "" {
			continue
		}
		if err := r.a.Tmux.KillWindow(tk.Window); err != nil {
			r.Opt.Logf("tick %d: kill %s's window: %v", r.tick, tk.ID, err)
			continue
		}
		r.Opt.Logf("tick %d: killed %s (%s)", r.tick, tk.ID, id)
		must(r.W.T, os.WriteFile(rel, nil, 0o644))
	}
}

// killLand runs the train as the engine would and kills it partway: the
// engine dying mid-reconcile. The next ticks must heal what it left. It
// reports whether the kill caught a land mid-run; with nothing queued, or
// a land that finished first, the kill waits for the next tick.
func (r *Run) killLand() bool {
	q, err := r.a.Queue()
	if err != nil || !slices.ContainsFunc(q, func(e store.TrainEntry) bool { return e.State == store.Queued }) {
		return false
	}
	after := time.Duration(100+r.rng.Intn(900)) * time.Millisecond
	cmd := exec.Command(r.W.Bins.Saddle, "land")
	cmd.Dir, cmd.Env = r.W.Repo, r.W.Env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		r.Opt.Logf("tick %d: start land: %v", r.tick, err)
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		r.Opt.Logf("tick %d: land finished before the kill (%v): %s", r.tick, err, firstLine(out.String()))
		return false
	case <-time.After(after):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		r.Opt.Logf("tick %d: killed saddle land after %s", r.tick, after)
		return true
	}
}

// settle waits up to Settle for running agents to stop.
func (r *Run) settle() {
	deadline := time.Now().Add(r.Opt.Settle)
	for time.Now().Before(deadline) {
		ts, err := r.a.Store.Tasks()
		if err != nil {
			return
		}
		busy := slices.ContainsFunc(ts, func(t store.Task) bool {
			return t.Role == store.RoleWorker && t.Status == store.Running && !r.hung(t)
		})
		if !busy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ts, err := r.a.Store.Tasks(); err == nil {
		var busy []string
		for _, t := range ts {
			if t.Role == store.RoleWorker && t.Status == store.Running && !r.hung(t) {
				busy = append(busy, t.ID)
			}
		}
		r.Opt.Logf("tick %d: still running after %s: %v", r.tick, r.Opt.Settle, busy)
	}
}

// hung reports whether t is a killed-worker change still hanging.
func (r *Run) hung(t store.Task) bool {
	c, ok := r.changeOf(t)
	if !ok || !c.Has(KilledWorker) {
		return false
	}
	_, err := os.Stat(filepath.Join(r.State, "released", c.ID))
	return err != nil
}

// taskOf is the latest task autopilot spawned for c: the one titled as
// its issue. Repairs carry the issue number too, so the title tells them
// apart.
func (r *Run) taskOf(c Change) (store.Task, bool) {
	ts, err := r.a.Store.Tasks()
	if err != nil {
		return store.Task{}, false
	}
	var best store.Task
	found := false
	for _, t := range ts {
		if t.Role == store.RoleWorker && t.Title == IssueTitle(c) {
			best, found = t, true
		}
	}
	return best, found
}

// changeOf is the change task t was spawned for, or false for a repair.
func (r *Run) changeOf(t store.Task) (Change, bool) {
	c, ok := r.Sc.ByIssue(t.Issue)
	if !ok || t.Title != IssueTitle(c) {
		return Change{}, false
	}
	return c, true
}

// settled reports whether every change is terminal.
func (r *Run) settled() bool {
	for _, o := range r.outcomes() {
		if o.State == Stuck {
			return false
		}
	}
	return true
}

// Events is saddle's event log, oldest first, one line each, for a
// failing run's report.
func (r *Run) Events() []string {
	evs, err := r.a.Store.EventsSince(0)
	if err != nil {
		return []string{err.Error()}
	}
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%s %s: %s", e.Task, e.Kind, firstLine(e.Data)))
	}
	return out
}

// Result is the run so far.
func (r *Run) Result() Result {
	res := Result{Scenario: r.Sc, Ticks: r.tick + 1, Outcomes: r.outcomes(), Ledger: &r.ledger, Actions: append([]string(nil), r.actions...)}
	res.Repairs = r.repairs()
	if ts, err := r.a.Store.Tasks(); err == nil {
		for _, t := range ts {
			if t.Adapter == Excluded {
				res.Excluded = append(res.Excluded, t.ID+" ("+t.Title+")")
			}
		}
	}
	if cards, err := r.a.OpenDecisions(); err == nil {
		for _, c := range cards {
			res.Actions = append(res.Actions, "decision card left for a human: "+c.ID+": "+c.Headline)
		}
	}
	return res
}

// outcomes is where each change stands: merged when main has its package,
// dropped when it has a blocker issue and no live task, else stuck.
func (r *Run) outcomes() []Outcome {
	s, err := r.W.GH.Load()
	if err != nil {
		return nil
	}
	main := r.W.Git(r.W.Origin, "ls-tree", "-r", "--name-only", "main")
	onMain := map[string]bool{}
	for _, l := range strings.Split(main, "\n") {
		onMain[l] = true
	}
	var out []Outcome
	for _, c := range r.Sc.Changes {
		o := Outcome{Change: c.ID, State: Stuck}
		tk, ok := r.taskOf(c)
		if ok {
			o.Task = tk.ID
		}
		path, _ := GoFile(c)
		switch {
		case onMain[path]:
			o.State = Merged
		case ok && !tk.Active() && r.blocker(s, tk.ID):
			o.State, o.Detail = Dropped, "blocker issue"
		case !ok:
			o.Detail = "never spawned"
		default:
			o.Detail = r.describe(s, tk)
		}
		out = append(out, o)
	}
	return out
}

func (r *Run) blocker(s *fakegh.State, task string) bool {
	return slices.ContainsFunc(s.Issues, func(is *fakegh.Issue) bool {
		return slices.Contains(is.Labels, app.BlockerLabel) && strings.Contains(is.Body+is.Title, task)
	})
}

// describe says where a stuck task is, for the failure message.
func (r *Run) describe(s *fakegh.State, t store.Task) string {
	d := "task " + t.Status
	if q, err := r.a.Queue(); err == nil {
		for _, e := range q {
			if e.Task == t.ID {
				d += ", train " + e.State
				if e.Note != "" {
					d += " (" + firstLine(e.Note) + ")"
				}
			}
		}
	}
	if t.PR != "" {
		for _, p := range s.PRs {
			if s.PRURL(p.Number) == t.PR {
				var cs []string
				for _, c := range p.Checks {
					cs = append(cs, c.Name+"="+c.State)
				}
				d += fmt.Sprintf(", PR #%d %s on %s [%s]", p.Number, p.State, p.Base, strings.Join(cs, " "))
			}
		}
	} else {
		d += ", no PR"
	}
	return d
}

// repairs lists every worker task that isn't a scenario change, with the
// checks the CI watcher spawned it for.
func (r *Run) repairs() []Repair {
	ts, err := r.a.Store.Tasks()
	if err != nil {
		return nil
	}
	checks := map[string][]string{}
	fors := map[string]string{}
	if cr, err := r.a.CIRed(); err == nil {
		for _, rec := range cr.Repairs {
			fors[rec.Repair] = rec.Task
			for _, f := range rec.Fails {
				checks[rec.Repair] = append(checks[rec.Repair], f.Check)
			}
		}
	}
	var out []Repair
	for _, t := range ts {
		if t.Role != store.RoleWorker {
			continue
		}
		if _, ok := r.changeOf(t); ok {
			continue
		}
		f := fors[t.ID]
		if f == "" {
			f = t.Parent
		}
		out = append(out, Repair{Task: t.ID, For: f, Checks: checks[t.ID]})
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func must(t interface{ Fatal(...any) }, err error) {
	if err != nil {
		t.Fatal(err)
	}
}
