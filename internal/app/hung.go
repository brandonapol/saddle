package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// Hung workers (#625). A worker can stop mid-task with nothing to say so:
// silent after a context compaction with notices piling up, parked on a
// model call that never comes back, or under a gate that never ends. The
// hung-worker watch flags a running worker, or an idle one with unread
// notices, whose progress fingerprint (its screen without timers and
// spinners, its branch head and its working tree) sat still for [stall]
// hung_after, and recovers it one step per window:
//
//  1. nudge: an action notice, typed in with the wake line;
//  2. press Enter, for a TUI sitting on a half-sent line or a summary;
//  3. take over: relaunch the task in place, same worktree, branch and
//     claims, on a hooked adapter, preferring one other than the one that
//     hung.
//
// A gate or test process in its worktree past twice its own timeout (#432)
// is stopped first, whatever else holds. Nothing is ever reset or removed,
// a task the owner asked saddle not to kill is only nudged, and after two
// takeovers saddle hands the task to the orchestrator. Done-orphaned
// workers belong to the #467 watch. See docs/hung-workers.md.

// DecisionWorkerHung is the card for a hung worker.
const DecisionWorkerHung = "worker-hung"

// DefaultHungAfter is how long a worker may make no progress before the
// watch acts, and how long each step waits.
const DefaultHungAfter = 20 * time.Minute

// maxTakeovers is how many times one task is relaunched before saddle
// stops and asks the orchestrator.
const maxTakeovers = 2

// Hung recovery steps, in order.
const (
	HungWatching = ""        // still, but not for long enough yet
	HungNudged   = "nudged"  // told it looks stuck
	HungEntered  = "entered" // Enter pressed in its pane
	HungKept     = "kept"    // due for a takeover the owner ruled out
	HungGaveUp   = "gave-up" // taken over maxTakeovers times already
)

// Hung-worker events.
const (
	EventWorkerHung     = "worker_hung"
	EventHungGateKilled = "hung_gate_killed"
	EventHungNudge      = "worker_hung_nudge"
	EventHungEnter      = "worker_hung_enter"
	EventHungTakeover   = "worker_takeover"
	EventHungRecovered  = "worker_hung_recovered"
	EventHungKeep       = "hung_keep"
	EventHungGaveUp     = "worker_hung_gave_up"
)

// HungWorker is one worker the watch is watching or recovering.
type HungWorker struct {
	Task   string    `json:"task"`
	Print  string    `json:"print"` // its progress fingerprint when last seen
	Since  time.Time `json:"since"` // the fingerprint has not changed since
	Step   string    `json:"step,omitempty"`
	StepAt time.Time `json:"step_at,omitzero"`
	Card   string    `json:"card,omitempty"`
	// Termed are the overdue gate processes already sent SIGTERM; one
	// still alive on the next look gets SIGKILL.
	Termed []int `json:"termed,omitempty"`
}

// Flagged reports whether the watch has called h hung.
func (h HungWorker) Flagged() bool { return h.Step != HungWatching }

// hungProcs lists processes for the hung-gate check; tests replace it.
var hungProcs = psProcs

// signalProc signals a process; tests replace it.
var signalProc = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

func (a *App) hungPath() string { return a.stateDir("hung-workers.json") }

// HungWorkers lists the workers the watch is watching or recovering.
func (a *App) HungWorkers() ([]HungWorker, error) {
	m, err := a.readHung()
	out := make([]HungWorker, 0, len(m))
	for _, h := range m {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out, err
}

// FlaggedHung lists only the workers flagged hung.
func (a *App) FlaggedHung() ([]HungWorker, error) {
	all, err := a.HungWorkers()
	return slices.DeleteFunc(all, func(h HungWorker) bool { return !h.Flagged() }), err
}

func (a *App) readHung() (map[string]HungWorker, error) {
	m := map[string]HungWorker{}
	b, err := os.ReadFile(a.hungPath())
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]HungWorker{}, fmt.Errorf("%s: %w", a.hungPath(), err)
	}
	return m, nil
}

func (a *App) writeHung(m map[string]HungWorker) error {
	if len(m) == 0 {
		if err := os.Remove(a.hungPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.hungPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.hungPath())
}

// normScreen keeps a screen's words: digits, spinners, box drawing and
// punctuation go, so an elapsed timer or a token counter ticking over a
// hung model call is not progress, and any new line of output is.
func normScreen(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return r
		}
		return ' '
	}, tmux.StripANSI(s))
	return strings.Join(strings.Fields(s), " ")
}

// progressPrint is t's progress fingerprint: its screen's words, its
// branch head, and its working tree's changes with their mtimes.
func (a *App) progressPrint(t store.Task) string {
	h := sha256.New()
	if a.ownWindow(t) {
		if s, err := a.Tmux.Capture(t.Window, 30); err == nil {
			h.Write([]byte(normScreen(s)))
		}
	}
	h.Write([]byte{0})
	head, _ := gitx.Run(t.Worktree, "rev-parse", "HEAD")
	h.Write([]byte(head))
	h.Write([]byte{0})
	st, _ := gitx.Run(t.Worktree, "status", "--porcelain", "--untracked-files=all")
	for _, l := range strings.Split(st, "\n") {
		if len(l) < 4 {
			continue
		}
		h.Write([]byte(l))
		p := l[3:]
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		if fi, err := os.Stat(filepath.Join(t.Worktree, strings.Trim(p, `"`))); err == nil {
			h.Write([]byte(strconv.FormatInt(fi.ModTime().UnixNano(), 10)))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// keepRe is a "do not kill" request in a task's prompt.
var keepRe = regexp.MustCompile(`(?i)\b(do not|don't|dont|never) kill\b`)

// KeepWorker records that task must not be killed by the hung-worker
// watch: it is still nudged, never taken over.
func (a *App) KeepWorker(task, why string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if why == "" {
		why = "asked"
	}
	a.Store.Event(t.ID, EventHungKeep, why)
	return nil
}

// HungTick runs the hung-worker watch at now (#625): it stops overdue gate
// processes under live workers, flags every watched worker whose progress
// fingerprint sat still for [stall] hung_after, and moves each flagged one
// a step along its recovery. It returns the tasks it acted on.
func (a *App) HungTick(now time.Time) ([]string, error) {
	window := a.StallConfig().HungAfter
	if window < 0 {
		return nil, a.writeHung(nil)
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	evs, err := a.Store.Events(-1)
	if err != nil {
		return nil, err
	}
	kept, takeovers := map[string]bool{}, map[string]int{}
	for _, e := range evs {
		switch e.Kind {
		case EventHungKeep:
			kept[e.Task] = true
		case EventHungTakeover:
			takeovers[e.Task]++
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
	orphans, err := a.readOrphans()
	if err != nil {
		return nil, err
	}
	m, err := a.readHung()
	if err != nil {
		return nil, err
	}
	answered := a.hungCardsKept()
	var procs []Proc
	listed := false
	var acted []string
	var errs []error
	drop := func(id string) {
		if h, ok := m[id]; ok {
			errs = append(errs, a.closeHungCard(h, now))
			delete(m, id)
		}
	}
	seen := map[string]bool{}
	for _, t := range ts {
		// Store checks first: tmux is asked only about a watched worker.
		if t.Role != store.RoleWorker || (t.Status != store.Running && t.Status != store.Idle) || queued[t.ID] {
			continue
		}
		seen[t.ID] = true
		h, had := m[t.ID]
		if !listed {
			procs, _ = hungProcs()
			listed = true
		}
		overdue, busy := taskGates(procs, t)
		if len(overdue) > 0 {
			if !had {
				h = HungWorker{Task: t.ID, Since: now}
			}
			errs = append(errs, a.stopHungGates(t, &h, overdue))
			m[t.ID] = h
			acted = append(acted, t.ID)
			continue
		}
		if _, ok := orphans[t.ID]; ok {
			drop(t.ID) // the done-orphan watch owns it (#467)
			continue
		}
		pending, _ := a.Store.PendingNotices(t.ID)
		if t.Status != store.Running && pending == 0 {
			drop(t.ID) // idle with nothing unread: the engine reports a stop
			continue
		}
		if !a.ownWindow(t) {
			drop(t.ID) // no window of ours: ResumeLost's, not ours
			continue
		}
		if had && answered[h.Card] && !kept[t.ID] {
			a.Store.Event(t.ID, EventHungKeep, "the owner answered "+h.Card)
			kept[t.ID] = true
		}
		p := a.progressPrint(t)
		if busy || doneJobLive(hooksState(t.Worktree), t.ID) || !had || h.Print != p {
			if had && h.Flagged() {
				a.Store.Event(t.ID, EventHungRecovered, "it made progress")
				errs = append(errs, a.closeHungCard(h, now))
			}
			m[t.ID] = HungWorker{Task: t.ID, Print: p, Since: now}
			continue
		}
		h.Termed = nil
		stepped, gone, err := a.hungStep(t, &h, now, window, pending, kept[t.ID] || keepRe.MatchString(t.Prompt), takeovers[t.ID])
		errs = append(errs, err)
		if stepped {
			acted = append(acted, t.ID)
		}
		if gone {
			delete(m, t.ID)
			continue
		}
		m[t.ID] = h
	}
	for id := range m {
		if !seen[id] {
			drop(id) // finished, queued, parked or gone
		}
	}
	errs = append(errs, a.writeHung(m))
	return acted, errors.Join(errs...)
}

// taskGates splits the gate and test processes running in t's worktree
// into those past twice their timeout, and whether any other one runs.
func taskGates(procs []Proc, t store.Task) (overdue []HungGate, busy bool) {
	ts := []store.Task{t}
	for _, h := range HungGates(func() ([]Proc, error) { return procs, nil }, ts) {
		if h.Task == t.ID {
			overdue = append(overdue, h)
		}
	}
	for _, p := range procs {
		if _, ok := gateTimeout(p.Args); ok && procTask(p, ts) == t.ID {
			busy = true
		}
	}
	return overdue, busy && len(overdue) == 0
}

// stopHungGates ends gate processes past twice their timeout under t:
// SIGTERM first, SIGKILL for one that outlived it. The agent hears why.
func (a *App) stopHungGates(t store.Task, h *HungWorker, hung []HungGate) error {
	var termed []int
	var lines []string
	for _, g := range hung {
		sig := syscall.SIGTERM
		if slices.Contains(h.Termed, g.PID) {
			sig = syscall.SIGKILL
		}
		what := fmt.Sprintf("%q (pid %d) ran %s, past twice its %s timeout", g.Args, g.PID, fmtDuration(g.Elapsed), fmtDuration(g.Timeout))
		if err := signalProc(g.PID, sig); err != nil {
			a.Store.Event(t.ID, EventHungGateKilled, what+": "+err.Error())
			continue
		}
		termed = append(termed, g.PID)
		a.Store.Event(t.ID, EventHungGateKilled, what+": sent "+sig.String())
		lines = append(lines, what)
	}
	h.Termed = termed
	if len(lines) == 0 {
		return nil
	}
	msg := "Saddle stopped a hung gate in your worktree: " + strings.Join(lines, "; ") +
		". Find out why it never finished (a test waiting forever, a deadlock) before you run it again, then carry on."
	return errors.Join(a.Store.Notify(t.ID, store.NoticeAction, msg),
		a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q: saddle stopped a hung gate: %s.", t.ID, t.Title, strings.Join(lines, "; "))))
}

// hungNudge is what a hung worker is told.
const hungNudge = "Saddle has seen no progress from you for %s: your screen, branch and working tree haven't changed%s. " +
	"If you are stuck (after a context compaction, or on a model call that never came back), read your notices, run git status, and carry on with your task. " +
	"If you are finished, commit and call the saddle done tool. If you can't go on, say why in one line."

// hungStep moves h one step along its recovery when its current step's
// window ran out at now. stepped says it acted; gone that the task was
// taken over, so the watch on it ends.
func (a *App) hungStep(t store.Task, h *HungWorker, now time.Time, window time.Duration, pending int, kept bool, takeovers int) (stepped, gone bool, err error) {
	from := h.Since
	if h.Flagged() {
		from = h.StepAt
	}
	if now.Sub(from) < window {
		return false, false, nil
	}
	still := fmtDuration(now.Sub(h.Since))
	switch h.Step {
	case HungWatching:
		unread := ""
		if pending > 0 {
			unread = fmt.Sprintf(" and %d notices sit unread", pending)
		}
		a.Store.Event(t.ID, EventWorkerHung, fmt.Sprintf("no progress for %s%s", still, unread))
		err := a.openHungCard(t, h, now, unread)
		h.Step, h.StepAt = HungNudged, now
		err = errors.Join(err, a.Store.Notify(t.ID, store.NoticeAction, fmt.Sprintf(hungNudge, still, unread)))
		a.Store.Event(t.ID, EventHungNudge, a.typeWake(t))
		h.Print = a.progressPrint(t) // what the nudge drew is not the agent moving
		return true, false, err
	case HungNudged:
		h.Step, h.StepAt = HungEntered, now
		how := "pressed Enter"
		if err := a.Tmux.SendKeys(t.Window, "Enter"); err != nil {
			how = "Enter failed: " + err.Error()
		}
		a.Store.Event(t.ID, EventHungEnter, how)
		h.Print = a.progressPrint(t)
		return true, false, nil
	case HungEntered:
		switch {
		case kept:
			h.Step, h.StepAt = HungKept, now
			a.Store.Event(t.ID, EventHungKeep, "due for a takeover; kept, as asked")
			return true, false, a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"%s %q has made no progress for %s through a nudge and an Enter. You asked saddle not to kill it, so it is left running: peek at it, or kill it yourself.",
				t.ID, t.Title, still))
		case takeovers >= maxTakeovers:
			h.Step, h.StepAt = HungGaveUp, now
			a.Store.Event(t.ID, EventHungGaveUp, fmt.Sprintf("hung again after %d takeovers", takeovers))
			return true, false, a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"%s %q hung again after saddle relaunched it %d times, so saddle stopped relaunching it. Its commits are on %s: find out why it keeps hanging, then kill it or spawn a fresh task on that branch.",
				t.ID, t.Title, takeovers, t.Branch))
		}
		return true, true, a.takeOver(t, h, now, still)
	}
	return false, false, nil
}

// takeOver relaunches t in place (step 3): its uncommitted work is
// snapshotted, its window killed, and a new agent starts in the same
// worktree on the same branch with the same claims, on a hooked adapter.
func (a *App) takeOver(t store.Task, h *HungWorker, now time.Time, still string) error {
	from := a.taskAdapter(t).Name()
	to := a.takeoverAdapter(from, now)
	ad, err := agent.ByName(to)
	if err != nil {
		return err
	}
	all, err := a.Store.Claims()
	if err != nil {
		return err
	}
	a.snapshotWIP(t.ID, t.Worktree)
	if err := a.Tmux.KillWindow(t.Window); err != nil {
		a.Store.Event(t.ID, "takeover_kill_failed", err.Error())
	}
	if to != from {
		t.Adapter, t.Model = to, ""
		if err := errors.Join(a.Store.SetField(t.ID, "adapter", to), a.Store.SetField(t.ID, "model", "")); err != nil {
			return err
		}
	}
	prompt := strings.TrimSpace(t.Prompt + "\n\n" + takeoverNote(from, to, still, a.Cfg.Integration))
	win, err := a.start(t, all[t.ID], ad, "", prompt)
	_ = a.ResolveDecision(h.Card, now)
	if err != nil {
		// Its window is gone; ResumeLost relaunches it.
		_ = a.Store.SetField(t.ID, "window", "")
		a.Store.Event(t.ID, EventHungTakeover, "relaunch failed: "+oneLine(err.Error()))
		return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"%s %q hung for %s, so saddle closed its session, but relaunching it on %s failed: %v\nIts commits are on %s and its uncommitted work in %s.",
			t.ID, t.Title, still, to, err, t.Branch, wipRef(t.ID)))
	}
	if err := a.Store.SetStatus(t.ID, store.Running); err != nil {
		return err
	}
	a.Store.Event(t.ID, EventHungTakeover, fmt.Sprintf("%s -> %s window %s", from, to, win))
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"%s %q made no progress for %s through a nudge and an Enter, so saddle relaunched it on %s in the same worktree and branch; its commits, uncommitted work and claims carry over. Nothing needs doing.",
		t.ID, t.Title, still, to))
}

// takeoverAdapter is the adapter a takeover runs on: a hooked one that can
// run now, other than from when there is one, else from itself.
func (a *App) takeoverAdapter(from string, now time.Time) string {
	same := ""
	for _, c := range a.rankAdapters(pickReq{def: from, claims: true, crowded: true}, a.ExhaustedAdapters(now), now) {
		ad, err := agent.ByName(c.Name)
		if c.Rank <= 0 || err != nil || !ad.Hooks() {
			continue
		}
		if c.Name != from {
			return c.Name
		}
		same = c.Name
	}
	if same != "" {
		return same
	}
	return from
}

// takeoverNote tells the new agent it is taking over a hung session.
func takeoverNote(from, to, still, integration string) string {
	return fmt.Sprintf("[saddle] You are taking over this task: the %s agent working on it made no progress for %s and didn't answer a nudge, so saddle relaunched it on %s (#625). "+
		"This worktree and branch hold its work so far, commits and uncommitted changes alike. "+
		"Start with git status and git log %s..HEAD to see what it did, then finish the task; don't redo work that is already there.",
		from, still, to, integration)
}

// openHungCard posts the worker-hung card for t.
func (a *App) openHungCard(t store.Task, h *HungWorker, now time.Time, unread string) error {
	c := DecisionCard{
		ID:       DecisionWorkerHung + ":" + t.ID + "@" + strconv.FormatInt(h.Since.Unix(), 10),
		Kind:     DecisionWorkerHung,
		To:       OrchestratorID,
		Task:     t.ID,
		Headline: fmt.Sprintf("%s has made no progress for %s%s.", taskLabel(t), fmtDuration(now.Sub(h.Since)), unread),
		What:     "Its screen, branch and working tree sat still; saddle is nudging it, then pressing Enter, then relaunching it in place on a hooked adapter.",
		Blocking: "Its task: nothing moves until it does. Its commits and uncommitted work stay either way.",
		Options: []DecisionOption{
			{Label: "Let saddle recover it", Cost: "nothing; each step waits " + fmtDuration(a.StallConfig().HungAfter), Action: "nothing", Recommended: true},
			{Label: "Don't kill it", Cost: "it may stay stuck; saddle only nudges", Action: "nothing more: answering keeps " + t.ID + " (the keep tool does the same)"},
		},
		Default: 1,
		Timeout: DefaultDecisionTimeout,
		Opened:  now,
	}
	if err := a.OpenDecision(c); err != nil {
		return err
	}
	h.Card = c.ID
	return nil
}

// hungCardsKept lists the worker-hung cards the owner closed with "Don't
// kill it".
func (a *App) hungCardsKept() map[string]bool {
	out := map[string]bool{}
	cs, _ := a.Decisions()
	for _, c := range cs {
		if c.Kind == DecisionWorkerHung && !c.Open() && c.By == DecidedByOwner && c.Answer == 2 {
			out[c.ID] = true
		}
	}
	return out
}

// closeHungCard ends the watch on h: its card closes.
func (a *App) closeHungCard(h HungWorker, now time.Time) error {
	if h.Card == "" {
		return nil
	}
	return a.ResolveDecision(h.Card, now)
}
