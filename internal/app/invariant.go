package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/invariants"
	"github.com/brandonapol/saddle/internal/store"
)

// The invariant monitor (#825, ADR 0004). Every sentinel tick it gathers a
// snapshot of saddle's stores, git and one forge read, runs
// invariants.Check, and reports each new violation once: an
// invariant_violated event and one notice to the orchestrator. A violation
// stays quiet until it clears (invariant_cleared), and may report again if
// it recurs. Open violations show in status and doctor. It only detects:
// the reconciler heals.

// Events the monitor logs. Data: "<kind> <subject>: <detail>".
const (
	EventInvariantViolated = "invariant_violated"
	EventInvariantCleared  = "invariant_cleared"
)

// latchWindow is how far back a card that closed with its default failed
// counts as a latch: older ones were already reported, or predate the
// monitor.
const latchWindow = 7 * 24 * time.Hour

// invariantPRTimeout bounds the monitor's one forge read.
const invariantPRTimeout = 30 * time.Second

// invariantOpenPRs reads the open PRs; tests replace it.
var invariantOpenPRs = func(a *App, ctx context.Context) ([]forge.PR, error) {
	return a.Forge().OpenPRs(ctx)
}

func (a *App) invariantsPath() string { return a.stateDir("invariants.json") }

// OpenViolations are the violations the monitor reported and that haven't
// cleared, sorted by key.
func (a *App) OpenViolations() ([]invariants.Violation, error) {
	open, err := a.openViolations()
	if err != nil {
		return nil, err
	}
	out := make([]invariants.Violation, 0, len(open))
	for _, v := range open {
		out = append(out, v)
	}
	slices.SortFunc(out, func(x, y invariants.Violation) int { return strings.Compare(x.Key(), y.Key()) })
	return out, nil
}

func (a *App) openViolations() (map[string]invariants.Violation, error) {
	open := map[string]invariants.Violation{}
	b, err := os.ReadFile(a.invariantsPath())
	if os.IsNotExist(err) {
		return open, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &open); err != nil {
		return map[string]invariants.Violation{}, nil // a torn file reports again, never wedges
	}
	return open, nil
}

func (a *App) setOpenViolations(open map[string]invariants.Violation) error {
	b, err := json.MarshalIndent(open, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.invariantsPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.invariantsPath())
}

// CheckInvariants runs the monitor once at now and returns the violations
// it reported for the first time.
func (a *App) CheckInvariants(now time.Time) ([]invariants.Violation, error) {
	snap, err := a.InvariantSnapshot(now)
	if err != nil {
		return nil, err
	}
	found := invariants.Check(snap)
	open, err := a.openViolations()
	if err != nil {
		return nil, err
	}
	fresh, cleared := invariants.Diff(open, found)
	for _, v := range cleared {
		delete(open, v.Key())
		a.Store.Event(violationTask(v), EventInvariantCleared, v.String())
	}
	for _, v := range found {
		open[v.Key()] = v // keep the latest detail for status
	}
	// Saved before telling anyone, so a crash between the two can't repeat
	// a notice every tick.
	if err := a.setOpenViolations(open); err != nil {
		return nil, err
	}
	for _, v := range fresh {
		a.Store.Event(violationTask(v), EventInvariantViolated, v.String())
	}
	for _, text := range violationNotices(fresh) {
		if err := a.Notify(OrchestratorID, store.NoticeAction, text); err != nil {
			return fresh, err
		}
	}
	return fresh, nil
}

// burstLimit is how many new violations of one kind a tick notifies one by
// one. Past it, as on a first run over old state, they go in one summary
// notice (#825); each still logs its own event.
const burstLimit = 5

// burstShown is how many subjects a summary notice names.
const burstShown = 3

// violationNotices are the orchestrator's notices for fresh, in order: one
// per violation, or one summary for a kind with more than burstLimit.
func violationNotices(fresh []invariants.Violation) []string {
	byKind := map[invariants.Kind][]invariants.Violation{}
	var kinds []invariants.Kind
	for _, v := range fresh {
		if _, ok := byKind[v.Kind]; !ok {
			kinds = append(kinds, v.Kind)
		}
		byKind[v.Kind] = append(byKind[v.Kind], v)
	}
	var out []string
	for _, k := range kinds {
		vs := byKind[k]
		if len(vs) <= burstLimit {
			for _, v := range vs {
				out = append(out, violationNotice(v))
			}
			continue
		}
		out = append(out, burstNotice(k, vs))
	}
	return out
}

// burstNotice is one notice for many new violations of kind.
func burstNotice(kind invariants.Kind, vs []invariants.Violation) string {
	var subjects []string
	for _, v := range vs[:burstShown] {
		subjects = append(subjects, v.Subject)
	}
	return fmt.Sprintf("Invariant violated (%s) on %d subjects: %s and %d more (first: %s). "+
		"Each is logged as an %s event; saddle only detects these for now (#825), and they show in status and doctor until they clear.",
		kind, len(vs), strings.Join(subjects, ", "), len(vs)-burstShown, vs[0].Detail, EventInvariantViolated)
}

// violationNotice is the orchestrator's notice for a new violation.
func violationNotice(v invariants.Violation) string {
	return fmt.Sprintf("Invariant violated (%s) on %s: %s. Saddle only detects this for now (#825); "+
		"it is reported once and shows in status and doctor until it clears.", v.Kind, v.Subject, v.Detail)
}

// violationTask is the task a violation is about, for the event's task
// column, or "".
func violationTask(v invariants.Violation) string {
	s := strings.TrimPrefix(v.Subject, "task ")
	if f := strings.Fields(s); len(f) == 1 && len(s) > 1 && s[0] == 't' && strings.Trim(s[1:], "0123456789") == "" {
		return s
	}
	return ""
}

// ViolationLines are the open violations as status and doctor lines.
func (a *App) ViolationLines() []string {
	vs, err := a.OpenViolations()
	if err != nil {
		return []string{"invariants: " + err.Error()}
	}
	var out []string
	for _, v := range vs {
		out = append(out, "invariant "+v.String())
	}
	return out
}

// InvariantSnapshot gathers what invariants.Check reads at now. A store
// that can't be read leaves its part empty: the monitor reports what it
// can see rather than nothing.
func (a *App) InvariantSnapshot(now time.Time) (invariants.Snapshot, error) {
	s := invariants.Snapshot{Now: now, Base: a.Cfg.Base, Unmerged: map[string]string{}}
	tasks, err := a.Store.Tasks()
	if err != nil {
		return s, err
	}
	byID := map[string]store.Task{}
	for _, t := range tasks {
		byID[t.ID] = t
		if t.Active() && t.Role == store.RoleWorker && t.Branch != "" {
			s.Unmerged[t.ID] = t.Branch
		}
	}
	stack, err := a.landedStack()
	if err != nil {
		return s, err
	}
	a.snapLanded(&s, stack)
	a.snapPRs(&s, stack)
	a.snapLatches(&s, now)
	a.snapStates(&s, tasks, byID)
	a.snapRepairs(&s, byID)
	a.snapSpawns(&s, tasks)
	return s, nil
}

// snapLanded fills I1: the landed tasks in the stack. landedStack drops a
// task once its work is on base, so OnBase stays false.
func (a *App) snapLanded(s *invariants.Snapshot, stack []landedTask) {
	landed, _ := a.Store.LastEventTimes("landed")
	for _, l := range stack {
		if l.Branch != "" {
			s.Unmerged[l.ID] = l.Branch
		}
		if l.Lost != "" {
			continue // its range is unknown; the sentinel reports it
		}
		s.Landed = append(s.Landed, invariants.Landed{Task: l.ID, LandedAt: landed[l.ID], PR: l.PR})
	}
}

// snapPRs fills I2 and I3 from one forge read. Without a remote, or when
// the read fails, PRs aren't compared this tick.
func (a *App) snapPRs(s *invariants.Snapshot, stack []landedTask) {
	var mine []landedTask
	for _, l := range stack {
		if l.PR != "" {
			mine = append(mine, l)
		}
	}
	if len(mine) == 0 || a.remote() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), invariantPRTimeout)
	defer cancel()
	live, err := invariantOpenPRs(a, ctx)
	if err != nil {
		return
	}
	s.PRsKnown = true
	saved := a.savedBases()
	g, _ := a.Gate()
	for _, l := range mine {
		p := invariants.PR{Task: l.ID, URL: l.PR, Branch: l.Branch, SavedBase: saved[l.ID], Deps: a.TaskAfter(l.ID)}
		if i := slices.IndexFunc(live, func(f forge.PR) bool { return f.URL == l.PR || (l.Branch != "" && f.Head == l.Branch) }); i >= 0 {
			p.Open, p.LiveBase = true, live[i].Base
		}
		for _, r := range g.Red {
			if r.Task == l.ID && r.Head == l.To {
				p.Builds = invariants.BuildsNo
			}
		}
		s.PRs = append(s.PRs, p)
	}
}

// snapLatches fills I4: stops with nothing scheduled to retry them.
func (a *App) snapLatches(s *invariants.Snapshot, now time.Time) {
	if f, ok, _ := a.Flag(); ok && f.Acked {
		s.Latches = append(s.Latches, invariants.Latch{What: "stack flag", Subject: f.Task, Detail: "acked at-risk flag (" + f.Cause + ")"})
	}
	if cr, err := a.CIRed(); err == nil {
		for _, l := range cr.Red {
			switch {
			case l.Acked:
				s.Latches = append(s.Latches, invariants.Latch{What: "ci-red", Subject: l.Task, Detail: "red CI on " + l.PR + " was acked"})
			case l.Escalated:
				s.Latches = append(s.Latches, invariants.Latch{What: "ci-red", Subject: l.Task, Detail: "repairs of red CI on " + l.PR + " gave up"})
			}
		}
	}
	if cards, err := a.Decisions(); err == nil {
		for _, c := range cards {
			if c.By == DecidedDefaultFailed && now.Sub(c.Closed) < latchWindow {
				s.Latches = append(s.Latches, invariants.Latch{What: "card", Subject: c.ID, Detail: "its default failed for good (" + firstLineOf(c.LastError) + ")"})
			}
		}
	}
}

// snapStates fills I5: killed tasks back in a live status, cards past
// their timeout, holds and non-terminal train rows with no exit.
func (a *App) snapStates(s *invariants.Snapshot, tasks []store.Task, byID map[string]store.Task) {
	killed, _ := a.Store.LastEventTimes("kill")
	back, _ := a.Store.LastEventTimes("requeued", "resumed")
	for _, t := range tasks {
		if t.Role != store.RoleWorker || killed[t.ID].IsZero() {
			continue
		}
		s.Tasks = append(s.Tasks, invariants.Task{ID: t.ID, Status: t.Status, Live: t.Active(), KilledAt: killed[t.ID], ReturnedAt: back[t.ID]})
	}
	if cards, err := a.OpenDecisions(); err == nil {
		for _, c := range cards {
			if c.Irreversible {
				continue // waits for the owner by design
			}
			due := c.Opened.Add(c.Timeout)
			if c.Timeout <= 0 {
				due = time.Time{}
			} else if c.RetryAfter.After(due) {
				due = c.RetryAfter
			}
			s.States = append(s.States, invariants.State{What: "card", Subject: c.ID, Deadline: due})
		}
	}
	train, err := a.Store.Train()
	if err != nil {
		return
	}
	for _, e := range train {
		t, ok := byID[e.Task]
		switch {
		case invariants.TerminalTrainState(e.State):
			// merged, folded, superseded: done for good, no exit to have (#825)
		case e.State == store.OnHold:
			s.States = append(s.States, invariants.State{What: "hold", Subject: e.Task, State: e.State, Detail: "held in the merge queue"})
		case !ok:
			s.States = append(s.States, invariants.State{What: "train row", Subject: e.Task, State: e.State, Detail: e.State + " for a task that doesn't exist"})
		case e.State != store.TrainOK && store.Finished(t.Status):
			s.States = append(s.States, invariants.State{What: "train row", Subject: e.Task, State: e.State, Detail: e.State + " for a task that is " + t.Status})
		}
	}
}

// snapRepairs fills I7: live CI repairs and the checks they are for.
func (a *App) snapRepairs(s *invariants.Snapshot, byID map[string]store.Task) {
	cr, err := a.CIRed()
	if err != nil {
		return
	}
	for _, r := range cr.Repairs {
		subject := r.Repair
		switch r.State {
		case ciRepairSpawned:
			if t, ok := byID[r.Repair]; !ok || !t.Active() {
				continue
			}
		case ciRepairPending:
			subject = "pending repair of " + r.Task
		default:
			continue
		}
		var checks []string
		if l, ok := cr.Layer(r.Task); ok && l.Head == r.Head {
			checks = append(checks, l.Checks...)
		}
		for _, f := range r.Fails {
			if !slices.Contains(checks, f.Check) {
				checks = append(checks, f.Check)
			}
		}
		s.Repairs = append(s.Repairs, invariants.Repair{Repair: subject, For: r.Task, Checks: checks})
	}
}

// snapSpawns fills I8: how each live worker was spawned, and the policy.
func (a *App) snapSpawns(s *invariants.Snapshot, tasks []store.Task) {
	cfg := a.Cfg
	s.Policy = invariants.Policy{Serial: cfg.Serial, ExcludeModels: map[string][]string{}}
	if len(cfg.AdapterOrder) > 0 {
		s.Policy.Adapters = slices.Clone(cfg.AdapterOrder)
		if cfg.Harness != "" && !slices.Contains(s.Policy.Adapters, cfg.Harness) {
			s.Policy.Adapters = append(s.Policy.Adapters, cfg.Harness)
		}
	}
	names := slices.Concat(cfg.AdapterOrder, []string{cfg.Harness})
	for n := range cfg.Adapters {
		names = append(names, n)
	}
	for n := range config.DefaultExcludeModels {
		names = append(names, n)
	}
	for _, n := range names {
		if x := cfg.ExcludedModels(n); len(x) > 0 {
			s.Policy.ExcludeModels[n] = x
		}
	}
	spawns, err := a.Store.LatestEventData("spawn")
	if err != nil {
		return
	}
	for _, t := range tasks {
		data, ok := spawns[t.ID]
		if !ok || !t.Active() || t.Role != store.RoleWorker {
			continue
		}
		sp := parseSpawnEvent(data)
		sp.Task = t.ID
		s.Spawns = append(s.Spawns, sp)
	}
}

// parseSpawnEvent reads a spawn event's data: "parent=P adapter=A (why)
// model=M claims=C1,C2[ hint]".
func parseSpawnEvent(data string) invariants.Spawn {
	var sp invariants.Spawn
	field := func(key string) string {
		i := strings.Index(data, " "+key+"=")
		if i < 0 {
			if !strings.HasPrefix(data, key+"=") {
				return ""
			}
			i = -1
		}
		v := data[i+len(key)+2:]
		if j := strings.IndexByte(v, ' '); j >= 0 {
			v = v[:j]
		}
		return v
	}
	sp.Adapter, sp.Model = field("adapter"), field("model")
	if c := field("claims"); c != "" {
		sp.Claims = strings.Split(c, ",")
	}
	return sp
}
