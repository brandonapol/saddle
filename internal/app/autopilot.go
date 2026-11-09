package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/forecast"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Autopilot (#256) lives in internal/autopilot; this wires its driver to the
// store, the train, spawn, plan limits, the resume logic and the
// orchestrator's safe-send input. Merging, restacking and collapse stay
// with the auto-merge watcher. Its state is a file under .saddle/.

func (a *App) autopilotPath() string { return a.stateDir("autopilot.json") }

// ReadyIssues lists the open issues carrying label, oldest first, each
// marked when an open PR already closes it.
type ReadyIssues func(label string) ([]autopilot.Issue, error)

// NewAutopilot returns the autopilot driver for the repo; nil ready reads
// the queue from GitHub with gh.
func (a *App) NewAutopilot(ready ReadyIssues) *autopilot.Driver {
	var unassigned ReadyIssues
	if ready == nil {
		ready, unassigned = a.ghReadyIssues, func(string) ([]autopilot.Issue, error) { return a.ghUnassignedIssues() }
	}
	done := ""
	if cmd := strings.TrimSpace(a.Cfg.Test.Cmd); cmd != "" {
		done = "`" + cmd + "` passes."
	}
	env := &autopilotEnv{a: a, ready: ready, unassigned: unassigned}
	d := &autopilot.Driver{
		Path:     a.autopilotPath(),
		Env:      env,
		Now:      time.Now,
		Rules:    a.agentRules(),
		DoneWhen: done,
	}
	env.now = func() time.Time { return d.Now() }
	return d
}

// AutopilotState is the autopilot state file as last saved.
func (a *App) AutopilotState() (autopilot.State, error) { return autopilot.Load(a.autopilotPath()) }

// agentRules is the repo's agent instructions: AGENTS.md, else CLAUDE.md.
func (a *App) agentRules() string {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if b, err := os.ReadFile(filepath.Join(a.Root, name)); err == nil {
			return string(b)
		}
	}
	return ""
}

type autopilotEnv struct {
	a     *App
	ready ReadyIssues
	// unassigned scans the tracker for open, unassigned, maintainer-opened
	// issues (#733); nil scans nothing.
	unassigned ReadyIssues
	// asked holds the tickets whose ErrNeedsConfirm already reached the
	// orchestrator, so a refusal repeated every tick surfaces once (#604).
	asked map[string]bool
	// now is the driver's clock, for how long a draft has held a nudge.
	now func() time.Time
}

func (e *autopilotEnv) Tasks() ([]autopilot.Task, error) {
	ts, err := e.a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	all, err := e.a.Store.Claims()
	if err != nil {
		return nil, err
	}
	var kills map[string]string
	var out []autopilot.Task
	for _, t := range ts {
		if t.Role != store.RoleWorker {
			continue
		}
		at := autopilot.Task{
			ID: t.ID, Issue: t.Issue, Claims: all[t.ID],
			Live:    liveStatus(t.Status) || t.Status == StatusPaused,
			Queued:  t.Status == store.Done || t.Status == store.PROpen,
			Paused:  t.Status == StatusPaused,
			Landed:  t.Status == store.Landed || t.Status == store.Merged,
			Summary: t.Summary,
			Created: t.CreatedAt,
		}
		// Killed, failed and no-change tasks free their issue for a retry (#767).
		switch t.Status {
		case store.Killed:
			if kills == nil {
				if kills, err = e.a.Store.LatestEventData("kill"); err != nil {
					return nil, err
				}
			}
			at.End, at.Why = autopilot.EndKilled, kills[t.ID]
		case StatusFailed:
			at.End, at.Why, at.Summary = autopilot.EndFailed, t.Summary, ""
		case store.NoChange:
			at.End, at.Why = autopilot.EndNoChange, "its branch had nothing to land"
		}
		out = append(out, at)
	}
	return out, nil
}

func (e *autopilotEnv) Ready(label string) ([]autopilot.Issue, error) { return e.ready(label) }

func (e *autopilotEnv) Capacity() (int, int, error) {
	c, err := e.a.Concurrency()
	return c.Running, c.Limit, err
}

func (e *autopilotEnv) Usage(now time.Time) (autopilot.Usage, error) {
	est, err := e.a.Limits(now)
	if err != nil {
		return autopilot.Usage{}, err
	}
	u := autopilot.Usage{Pause: est.ShouldPauseLaunches()}
	for _, w := range []usage.WindowEstimate{est.FiveHour, est.Weekly} {
		u.Percent = max(u.Percent, w.Percent)
		if w.State == usage.Over && w.ResetsAt.After(u.ResetsAt) {
			u.ResetsAt = w.ResetsAt // the pause lifts when every full window has reset
		}
	}
	return u, nil
}

// Reconcile resumes orphans: live tasks with no window and no heartbeat,
// which the resume watcher gave up on or never saw. They would otherwise
// sit as running forever and hold a slot.
func (e *autopilotEnv) Reconcile() ([]string, error) {
	orphans, err := e.a.Orphans()
	if err != nil {
		return nil, err
	}
	var out []string
	var errs []error
	for id := range orphans {
		if _, err := e.a.Resume(id); err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, id)
	}
	return out, errors.Join(errs...)
}

// Land runs the train when something is queued; landing opens the landed
// tasks' PRs itself (#819). It skips both when nothing waits.
func (e *autopilotEnv) Land() (int, error) {
	q, err := e.a.Queue()
	if err != nil {
		return 0, err
	}
	queued := false
	for _, en := range q {
		queued = queued || en.State == store.Queued
	}
	if !queued {
		return 0, nil
	}
	rs, err := e.a.LandRecorded()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rs {
		if r.State == store.Landed {
			n++
		}
	}
	return n, nil
}

// spawnReq is autopilot's spawn as the app's: the same chooser as the
// orchestrator's spawns picks the adapter from the ticket's difficulty and
// needs, unless its Provider: line names one (#505). An old Model scope
// names a Claude model; it is a hint Spawn drops on other adapters.
func (e *autopilotEnv) spawnReq(r autopilot.SpawnReq) SpawnReq {
	// A Provider: line naming a disabled adapter falls to the chooser (#839).
	if e.a.Cfg.AdapterDisabled(r.Adapter) {
		r.Adapter = ""
	}
	return SpawnReq{Title: r.Title, Prompt: r.Prompt, Claims: slices.Clone(r.Claims), ModelHint: r.Model, Issue: r.Issue, After: r.After,
		Adapter: r.Adapter, Difficulty: r.Difficulty, Needs: r.Needs}
}

// Route is where a ticket would go now, for autopilot's plan (#505).
func (e *autopilotEnv) Route(r autopilot.SpawnReq) (string, string, string, error) {
	return e.a.RoutePreview(e.spawnReq(r))
}

func (e *autopilotEnv) Spawn(r autopilot.SpawnReq) (string, error) {
	// Repairs jump the queue (#768): a pending one gets the free slot first,
	// and while one still waits on the cap no ticket starts.
	e.a.RetryRepairs()
	if err := e.a.repairFirst(); err != nil {
		return "", fmt.Errorf("%w: %v", autopilot.ErrAtCap, err)
	}
	t, err := e.a.Spawn(e.spawnReq(r))
	switch {
	case errors.Is(err, ErrLaunchesPaused):
		return "", fmt.Errorf("%w: %v", autopilot.ErrPaused, err)
	case err != nil && (strings.Contains(err.Error(), "at concurrency cap") || errors.Is(err, ErrBackpressure)):
		return "", fmt.Errorf("%w: %v", autopilot.ErrAtCap, err)
	case errors.Is(err, ErrNeedsConfirm):
		e.askConfirm(r, err)
	}
	return t.ID, err
}

// askConfirm tells the orchestrator, once per ticket, that autopilot can't
// start it until someone confirms: autopilot never answers the #340 guard
// itself, and a refusal it only logged would loop silently (#119).
func (e *autopilotEnv) askConfirm(r autopilot.SpawnReq, err error) {
	key := r.Title
	if r.Issue > 0 {
		key = "#" + strconv.Itoa(r.Issue)
	}
	if e.asked[key] {
		return
	}
	if e.asked == nil {
		e.asked = map[string]bool{}
	}
	e.asked[key] = true
	e.Notify(true, fmt.Sprintf("Autopilot can't start %q: %v. Spawn it yourself with confirm=true if it is new work, or drop it from the ready queue.", r.Title, err))
}

// Nudge types into the orchestrator's input the way compaction does: only
// when it is idle and the owner has no draft. With no input to type into
// (the plugin engine, a stopped orchestrator), or a draft older than
// DraftBlockMax, it goes out as an action notice, which the TUI, the mod or
// `plugin wait` delivers (#791). Either way the nudge counts as sent.
func (e *autopilotEnv) Nudge(text string) (bool, error) {
	t := e.a.CompactTarget()
	if t != nil && t.Busy() {
		return false, nil
	}
	if e.a.draftHolds(t, e.clock()) {
		return false, nil
	}
	if t == nil || t.Drafting() {
		return true, e.a.Notify(OrchestratorID, store.NoticeAction, text)
	}
	return true, t.Send(text)
}

func (e *autopilotEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e *autopilotEnv) Backlog() (time.Time, bool) {
	since, ok, err := e.a.Store.OldestPendingAction(OrchestratorID)
	return since, ok && err == nil
}

func (e *autopilotEnv) Notify(interrupt bool, text string) {
	kind := store.NoticeInfo
	if interrupt {
		kind = store.NoticeAction
	}
	_ = e.a.Notify(OrchestratorID, kind, text)
}

func (e *autopilotEnv) Event(kind, data string) { e.a.Store.Event("", kind, data) }

// Park stops every live worker the way saddle down does: work snapshotted,
// claims and worktrees kept, status paused.
func (e *autopilotEnv) Park() ([]string, error) { return e.a.Pause() }

// Unpark resumes a parked task in a new window; one whose window outlived
// the park just goes back to running.
func (e *autopilotEnv) Unpark(task string) error {
	t, err := e.a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.Status == StatusPaused && t.Window != "" && e.a.ownWindow(t) {
		return e.a.Store.SetStatus(task, store.Running)
	}
	_, err = e.a.Resume(task)
	return err
}

// Snapshot is the pipeline from saddle's own records (#463): the owner's
// open decision cards, needs-you tasks and held train entries as blockers,
// the live workers, the PRs still open and the work already merged, and
// the merge train. It reads local state only, so a tick stays cheap.
func (e *autopilotEnv) Snapshot(time.Time) (autopilot.Snapshot, error) {
	var s autopilot.Snapshot
	cards, err := e.a.OpenDecisions()
	if err != nil {
		return s, err
	}
	carded := map[string]bool{}
	for _, c := range cards {
		if c.To == OrchestratorID {
			s.Blockers = append(s.Blockers, autopilot.Blocker{What: c.Headline, Since: c.Opened})
			carded[c.Task] = true
		}
	}
	ts, err := e.a.Store.Tasks()
	if err != nil {
		return s, err
	}
	byID := map[string]store.Task{}
	for _, t := range ts {
		byID[t.ID] = t
		if t.Role != store.RoleWorker || !t.Active() || t.Status == store.Done || t.Status == store.PROpen {
			continue
		}
		s.Tasks = append(s.Tasks, t.ID+" "+t.Status)
		if t.Status == store.NeedsYou && !carded[t.ID] {
			s.Blockers = append(s.Blockers, autopilot.Blocker{What: t.ID + " needs you (" + oneLine(t.Title) + ")"})
		}
	}
	train, err := e.a.Store.Train()
	if err != nil {
		return s, err
	}
	for i := len(train) - 1; i >= 0; i-- { // newest first
		en := train[i]
		if byID[en.Task].Status == store.Killed {
			continue // a dead task's leftover record is not live work (#721)
		}
		pr := ""
		if n := prShort(byID[en.Task].PR); n != "" {
			pr = " (PR " + n + ")"
		}
		switch en.State {
		case store.TrainOK:
			if pr != "" {
				s.OpenPRs = append(s.OpenPRs, en.Task+pr)
			}
		case TrainMerged:
			s.Merged = append(s.Merged, en.Task+pr)
		case store.OnHold:
			s.Queue = append(s.Queue, en.Task+" on hold")
			s.Blockers = append(s.Blockers, autopilot.Blocker{What: en.Task + " is held in the merge train"})
		case store.Queued, store.TestFailed, store.TrainError:
			s.Queue = append(s.Queue, en.Task+" "+en.State)
		}
	}
	return s, nil
}

// closesRef finds the issues a PR body closes.
var closesRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#(\d+)`)

// ghReadyIssues asks gh for the open issues with label and the open PRs
// that close any of them.
func (a *App) ghReadyIssues(label string) ([]autopilot.Issue, error) {
	ctx, f := context.Background(), a.Forge()
	raw, err := f.ListIssues(ctx, forge.IssueQuery{Label: label, State: "open", Limit: 200},
		forge.IssueNumber|forge.IssueTitle|forge.IssueBody|forge.IssueCreated|forge.IssueLabels)
	if err != nil {
		return nil, err
	}
	closed := a.closedByOpenPRs(ctx, f)
	is := make([]autopilot.Issue, len(raw))
	for i, r := range raw {
		is[i] = autopilot.Issue{Number: r.Number, Title: r.Title, Body: r.Body, Created: r.Created, HasPR: closed[r.Number], Labels: r.Labels}
	}
	return is, nil
}

// closedByOpenPRs is the issues an open PR closes.
func (a *App) closedByOpenPRs(ctx context.Context, f forge.Forge) map[int]bool {
	closed := map[int]bool{}
	if prs, err := f.ListPRs(ctx, forge.PRQuery{State: "open", Limit: 200}, forge.PRBody); err == nil {
		for _, p := range prs {
			for _, m := range closesRef.FindAllStringSubmatch(p.Body, -1) {
				n, _ := strconv.Atoi(m[1])
				closed[n] = true
			}
		}
	}
	return closed
}

// ghUnassignedIssues is the tracker scan through the forge (#733).
func (a *App) ghUnassignedIssues() ([]autopilot.Issue, error) {
	return a.unassignedVia(context.Background(), a.Forge())
}

// unassignedVia scans f; a forge that can't list them has nothing to scan.
func (a *App) unassignedVia(ctx context.Context, f forge.Forge) ([]autopilot.Issue, error) {
	raw, err := f.UnassignedIssues(ctx)
	if errors.Is(err, forge.ErrUnsupported) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	closed := a.closedByOpenPRs(ctx, f)
	is := make([]autopilot.Issue, len(raw))
	for i, r := range raw {
		is[i] = autopilot.Issue{Number: r.Number, Title: r.Title, Body: r.Body, Created: r.Created, HasPR: closed[r.Number], Labels: r.Labels}
	}
	return is, nil
}

// Unassigned is autopilot's tracker scan (#733).
func (e *autopilotEnv) Unassigned() ([]autopilot.Issue, error) {
	if e.unassigned == nil {
		return nil, nil
	}
	return e.unassigned("")
}

// CloseIssue closes an issue a no-change task found already done (#767).
func (e *autopilotEnv) CloseIssue(n int, comment string) error {
	return e.a.Forge().CloseIssue(context.Background(), n, comment)
}

// GiveUp labels an issue needs-human, where the forge can, and comments
// why, once its retries ran out (#767). Without the label the driver's own
// record keeps the issue out for the rest of the run.
func (e *autopilotEnv) GiveUp(n int, comment string) error {
	ctx, f := context.Background(), e.a.Forge()
	lerr := f.AddIssueLabel(ctx, n, autopilot.NeedsHumanLabel)
	if errors.Is(lerr, forge.ErrUnsupported) {
		lerr = nil
	}
	return errors.Join(lerr, f.CommentIssue(ctx, n, comment))
}

// IssueClosed reads whether issue n is closed.
func (e *autopilotEnv) IssueClosed(n int) (bool, error) {
	is, err := e.a.Forge().Issue(context.Background(), "", n, forge.IssueState)
	return strings.EqualFold(is.State, "closed"), err
}

// ShareClaims lets autopilot start a ticket whose claims overlap live
// tasks' when claims are symbol-granular (#627): the edit-time rules then
// arbitrate the shared files, and the forecaster (rule 4) says whether the
// pair is safe to run side by side. A pair rated medium or worse waits, as
// does anything in file mode. free is want less the globs another task holds,
// so the spawn isn't refused for them; it is never nil.
func (e *autopilotEnv) ShareClaims(want, with []string) ([]string, bool, error) {
	a := e.a
	if !a.SymbolMode() {
		return nil, false, nil
	}
	held, err := a.Store.Claims()
	if err != nil {
		return nil, false, err
	}
	const candidate = "candidate"
	f, err := a.forecastWith([]forecast.Task{{ID: candidate, Claims: want}})
	if err != nil {
		return nil, false, err
	}
	for _, p := range f.Result.Pairs {
		if (p.A == candidate || p.B == candidate) && p.Level >= forecast.Medium {
			return nil, false, nil
		}
	}
	conflicts := claims.Conflicts(held, "", want)
	free := []string{}
	for _, w := range want {
		if _, taken := conflicts[w]; !taken {
			free = append(free, w)
		}
	}
	return free, true, nil
}
