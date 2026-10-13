package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// Done-orphaned workers (#467). A worker calls done, its gate is cancelled
// (the process serving it went away, saddle stopped, or the branch moved
// past the restarts), the branch is never queued, and the session goes
// quiet. Its work is committed and nothing lands it. The watchdog flags
// such a task once its screen has sat unchanged for [stall] done_orphan,
// and heals it without a human, in order:
//
//  1. re-run the done gate from saddle itself, so the branch queues itself;
//  2. if that can't run or doesn't queue it, wake the agent with a notice
//     whose submit is checked (the input box must empty);
//  3. if the session still hasn't moved after another window, kill it and
//     spawn a finisher on its branch to check it and call done.
//
// A screen change at any point means the agent is alive: the watch starts
// over.

// DecisionDoneOrphaned is the card for a done-orphaned worker.
const DecisionDoneOrphaned = "done-orphaned"

// DefaultDoneOrphan is how long a done-orphaned worker's screen must sit
// still before the watchdog acts, and how long each step gets.
const DefaultDoneOrphan = 20 * time.Minute

// Orphan recovery steps, in order.
const (
	OrphanWatching = ""         // its screen is still; not flagged yet
	OrphanRegated  = "regated"  // saddle re-ran its done gate
	OrphanNudged   = "nudged"   // saddle woke the agent
	OrphanFinished = "finished" // killed, and a finisher spawned
)

// Orphan events.
const (
	EventDoneOrphaned  = "done_orphaned"
	EventOrphanRegate  = "done_orphan_regate"
	EventOrphanNudge   = "done_orphan_nudge"
	EventOrphanFinish  = "done_orphan_finisher"
	EventOrphanRecover = "done_orphan_recovered"
)

// DoneOrphan is one worker the watchdog is watching or healing.
type DoneOrphan struct {
	Task    string    `json:"task"`
	Screen  string    `json:"screen"` // hash of its screen when last seen
	Since   time.Time `json:"since"`  // its screen has not changed since
	Step    string    `json:"step,omitempty"`
	StepAt  time.Time `json:"step_at,omitzero"`
	Card    string    `json:"card,omitempty"`
	Summary string    `json:"summary,omitempty"` // its done call's summary
	// Finisher is the task spawned to finish its work, once there is one.
	Finisher string `json:"finisher,omitempty"`
}

// Flagged reports whether the watchdog has called o done-orphaned.
func (o DoneOrphan) Flagged() bool { return o.Step != OrphanWatching }

func (a *App) orphansPath() string { return a.stateDir("done-orphans.json") }

// DoneOrphans lists the workers the watchdog is watching or healing, by
// task id.
func (a *App) DoneOrphans() ([]DoneOrphan, error) {
	m, err := a.readOrphans()
	if err != nil {
		return nil, err
	}
	out := make([]DoneOrphan, 0, len(m))
	for _, o := range m {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out, nil
}

// FlaggedOrphans lists only the workers flagged done-orphaned.
func (a *App) FlaggedOrphans() ([]DoneOrphan, error) {
	all, err := a.DoneOrphans()
	return slices.DeleteFunc(all, func(o DoneOrphan) bool { return !o.Flagged() }), err
}

func (a *App) readOrphans() (map[string]DoneOrphan, error) {
	m := map[string]DoneOrphan{}
	b, err := os.ReadFile(a.orphansPath())
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]DoneOrphan{}, fmt.Errorf("%s: %w", a.orphansPath(), err)
	}
	return m, nil
}

func (a *App) writeOrphans(m map[string]DoneOrphan) error {
	if len(m) == 0 {
		if err := os.Remove(a.orphansPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.orphansPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.orphansPath())
}

// doneOutcomes are the events that say how a done call ended. The last of
// them for a task tells whether its done is still owed.
var doneOutcomes = map[string]bool{
	EventDoneCalled: true, "done_gate_started": true, "done_gate_restarted": true,
	"done_gate_interrupted": true, "done_gate_stale": true, "done_gate_failed": true,
	"lint_failed": true, "done": true, "no_change": true, EventOrphanRegate: true,
}

// cancelledDone are the outcomes that leave a done call orphaned: the gate
// never said yes or no.
var cancelledDone = map[string]bool{
	EventDoneCalled: true, "done_gate_started": true, "done_gate_restarted": true,
	"done_gate_interrupted": true, "done_gate_stale": true, EventOrphanRegate: true,
}

// orphanOwed reports whether t is a live worker, not queued, that called
// done and is still owed it: its last done call's gate was cancelled, or,
// once flagged, it ended any way short of queueing the branch (a red
// re-run gate leaves the fix to the next steps).
func orphanOwed(t store.Task, last store.Event, called, queued, flagged bool) bool {
	if t.Role != store.RoleWorker || (t.Status != store.Running && t.Status != store.Idle) || queued || !called {
		return false
	}
	return flagged || cancelledDone[last.Kind]
}

// doneJobLive reports whether a done gate for task holds its job lock now.
func doneJobLive(state, task string) bool {
	if state == "" {
		return false
	}
	f, err := os.OpenFile(filepath.Join(state, "done-"+task+".lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// screenHash is a hash of what t's window shows, "" when it has none.
func (a *App) screenHash(t store.Task) string {
	if !a.ownWindow(t) {
		return ""
	}
	s, err := a.Tmux.Capture(t.Window, 30)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// OrphanTick runs the done-orphan watch at now (#467): it flags every
// worker whose done call was cancelled and whose screen sat still for
// [stall] done_orphan, and moves each flagged one a step along its
// recovery. It returns the tasks it acted on.
func (a *App) OrphanTick(now time.Time) ([]string, error) {
	window := a.StallConfig().DoneOrphan
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	evs, err := a.Store.Events(-1)
	if err != nil {
		return nil, err
	}
	last, summaries := map[string]store.Event{}, map[string]string{}
	for _, e := range evs {
		if doneOutcomes[e.Kind] {
			last[e.Task] = e
		}
		if e.Kind == EventDoneCalled {
			summaries[e.Task] = e.Data
		}
	}
	train, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	queued := map[string]bool{}
	for _, e := range train {
		if e.State == store.Queued || e.State == store.OnHold {
			queued[e.Task] = true
		}
	}
	m, err := a.readOrphans()
	if err != nil {
		return nil, err
	}
	var acted []string
	var errs []error
	seen := map[string]bool{}
	for _, t := range ts {
		o, had := m[t.ID]
		e, called := last[t.ID]
		if !orphanOwed(t, e, called, queued[t.ID], had && o.Flagged()) {
			if had {
				errs = append(errs, a.closeOrphan(o, now))
				delete(m, t.ID)
			}
			continue
		}
		summary := summaries[t.ID]
		if doneJobLive(hooksState(t.Worktree), t.ID) {
			// A gate runs: the clock starts when it ends.
			if had && o.Flagged() {
				o.StepAt = now
			} else {
				o = DoneOrphan{Task: t.ID, Screen: a.screenHash(t), Since: now, Summary: summary}
			}
			m[t.ID] = o
			seen[t.ID] = true
			continue
		}
		seen[t.ID] = true
		h := a.screenHash(t)
		if !had || o.Screen != h {
			if had && o.Flagged() {
				a.Store.Event(t.ID, EventOrphanRecover, "its screen moved")
				errs = append(errs, a.closeOrphan(o, now))
			}
			m[t.ID] = DoneOrphan{Task: t.ID, Screen: h, Since: now, Summary: summary}
			continue
		}
		o.Summary = summary
		stepped, err := a.orphanStep(t, &o, now, window)
		errs = append(errs, err)
		if stepped {
			acted = append(acted, t.ID)
		}
		if o.Step == OrphanFinished {
			delete(m, t.ID)
			continue
		}
		m[t.ID] = o
	}
	for id := range m {
		if !seen[id] {
			delete(m, id) // the task is gone
		}
	}
	errs = append(errs, a.writeOrphans(m))
	return acted, errors.Join(errs...)
}

// orphanStep moves o one step along its recovery when its current step's
// window ran out at now. It reports whether it acted.
func (a *App) orphanStep(t store.Task, o *DoneOrphan, now time.Time, window time.Duration) (bool, error) {
	from := o.Since
	if o.Flagged() {
		from = o.StepAt
	}
	if now.Sub(from) < window {
		return false, nil
	}
	switch o.Step {
	case OrphanWatching:
		a.Store.Event(t.ID, EventDoneOrphaned, fmt.Sprintf("done called, never queued, screen still for %s", fmtDuration(now.Sub(o.Since))))
		if err := a.openOrphanCard(t, o, now); err != nil {
			return false, err
		}
		if a.regate(t, o, now) {
			return true, nil
		}
		return true, a.nudgeOrphan(t, o, now)
	case OrphanRegated:
		return true, a.nudgeOrphan(t, o, now)
	case OrphanNudged:
		return true, a.finishOrphan(t, o, now)
	}
	return false, nil
}

// regate re-runs t's done gate from saddle (step 1). It reports whether a
// gate started or queued the branch; false means it couldn't run (a dirty
// tree, say), so the next step is due at once.
func (a *App) regate(t store.Task, o *DoneOrphan, now time.Time) bool {
	o.Step, o.StepAt = OrphanRegated, now
	job, err := a.DoneAsync(t.ID, o.Summary)
	if err != nil {
		a.Store.Event(t.ID, EventOrphanRegate, "can't: "+oneLine(err.Error()))
		return false
	}
	what := "queued at once"
	switch {
	case job.Running:
		what = "gate " + job.ID + " running"
	case job.NoChange:
		what = "nothing to land"
	}
	a.Store.Event(t.ID, EventOrphanRegate, what)
	return true
}

// orphanNudge is what a done-orphaned agent is told.
const orphanNudge = "Your done call never finished: its gate was cancelled and your branch is not queued in the merge train. " +
	"Commit anything left, then call the saddle done tool again. If you can't, say why in one line."

// nudgeOrphan wakes the agent with a notice whose submit it checks (step 2).
func (a *App) nudgeOrphan(t store.Task, o *DoneOrphan, now time.Time) error {
	o.Step, o.StepAt = OrphanNudged, now
	if err := a.Store.Notify(t.ID, store.NoticeAction, orphanNudge); err != nil {
		return err
	}
	how := "no window to type into"
	if a.ownWindow(t) {
		ad := a.taskAdapter(t)
		line := ad.Inject("")
		if !ad.Hooks() {
			ns, _ := a.Store.TakeNotices(t.ID, false)
			line = ad.Inject(store.FormatNotices(ns))
		}
		in := tmux.InputEmpty
		if s, err := a.Tmux.Capture(t.Window, 15); err == nil {
			in = tmux.ReadInput(s)
		}
		if in != tmux.InputEmpty {
			_ = a.Tmux.SendKeys(t.Window, "C-u") // nobody is typing into a hung session
		}
		ok, err := tmux.Submit(a.Tmux, t.Window, line)
		switch {
		case err != nil:
			how = "typing failed: " + err.Error()
		case !ok:
			how = "typed, but the input box never emptied"
		default:
			how = "typed and submitted"
		}
	}
	// What the nudge itself drew is not the agent moving.
	o.Screen = a.screenHash(t)
	a.Store.Event(t.ID, EventOrphanNudge, how)
	return nil
}

// finishOrphan kills t and spawns a finisher on its branch (step 3).
func (a *App) finishOrphan(t store.Task, o *DoneOrphan, now time.Time) error {
	o.Step, o.StepAt = OrphanFinished, now
	cl, err := a.Store.Claims()
	if err != nil {
		return err
	}
	claims := cl[t.ID]
	if err := a.Kill(t.ID, false); err != nil {
		return err
	}
	f, err := a.Spawn(SpawnReq{
		Title:   "finish " + t.ID + ": " + t.Title,
		Prompt:  finisherPrompt(t, o.Summary, a.Cfg.Integration),
		Claims:  claims,
		Model:   t.Model,
		Parent:  t.Parent,
		Base:    t.Branch,
		Issue:   t.Issue,
		Confirm: true,
		Force:   true,
	})
	if err != nil {
		a.Store.Event(t.ID, EventOrphanFinish, "spawn failed: "+oneLine(err.Error()))
		_ = a.ResolveDecision(o.Card, now)
		return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"%s %q called done, was never queued and stopped answering, so saddle killed it. Spawning its finisher failed: %v\nIts commits are on %s: spawn a task on that branch to call done.",
			t.ID, t.Title, err, t.Branch))
	}
	o.Finisher = f.ID
	a.Store.Event(t.ID, EventOrphanFinish, f.ID)
	_ = a.ResolveDecision(o.Card, now)
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"%s %q called done, was never queued and stopped answering through a re-run gate and a nudge, so saddle killed it and spawned %s on its branch to finish it.",
		t.ID, t.Title, f.ID))
}

// finisherPrompt is the finisher's task.
func finisherPrompt(t store.Task, summary, integration string) string {
	p := fmt.Sprintf("Finish %s (%q), whose agent called done and then stopped answering before its branch was queued (#467). "+
		"Your branch starts at its branch %s, so its commits are already yours. "+
		"Sync onto %s (saddle sync), run the repo's check and fix anything it reports, commit, and call the saddle done tool. "+
		"Don't redo or extend the work.", t.ID, t.Title, t.Branch, integration)
	if summary != "" {
		p += "\nIts done summary, to reuse as yours: " + summary
	}
	if t.Prompt != "" {
		p += "\n\nIts original task:\n" + t.Prompt
	}
	return p
}

// openOrphanCard posts the done-orphaned card for t.
func (a *App) openOrphanCard(t store.Task, o *DoneOrphan, now time.Time) error {
	c := DecisionCard{
		ID:       DecisionDoneOrphaned + ":" + t.ID + "@" + strconv.FormatInt(o.Since.Unix(), 10),
		Kind:     DecisionDoneOrphaned,
		To:       OrchestratorID,
		Task:     t.ID,
		Headline: fmt.Sprintf("%s called done but was never queued, and its screen hasn't moved for %s.", taskLabel(t), fmtDuration(now.Sub(o.Since))),
		What:     "Its done gate was cancelled and nothing re-ran it; saddle is re-running the gate, then nudging the agent, then handing its commits to a finisher.",
		Blocking: "Its committed work: nothing lands it until it is queued.",
		Options: []DecisionOption{
			{Label: "Let saddle recover it", Cost: "nothing; each step waits " + fmtDuration(a.StallConfig().DoneOrphan), Action: "nothing", Recommended: true},
			{Label: "Kill it now", Cost: "its session; the branch and commits stay", Action: "saddle kill " + t.ID},
		},
		Default: 1,
		Timeout: DefaultDecisionTimeout,
		Opened:  now,
	}
	if err := a.OpenDecision(c); err != nil {
		return err
	}
	o.Card = c.ID
	return nil
}

// closeOrphan ends the watch on o: its card closes.
func (a *App) closeOrphan(o DoneOrphan, now time.Time) error {
	if o.Card == "" {
		return nil
	}
	return a.ResolveDecision(o.Card, now)
}
