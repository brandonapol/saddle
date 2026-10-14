package app

import (
	"encoding/json"
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
	if ready == nil {
		ready = a.ghReadyIssues
	}
	done := ""
	if cmd := strings.TrimSpace(a.Cfg.Test.Cmd); cmd != "" {
		done = "`" + cmd + "` passes."
	}
	return &autopilot.Driver{
		Path:     a.autopilotPath(),
		Env:      &autopilotEnv{a: a, ready: ready},
		Now:      time.Now,
		Rules:    a.agentRules(),
		DoneWhen: done,
	}
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
	var out []autopilot.Task
	for _, t := range ts {
		if t.Role != store.RoleWorker || t.Status == store.Killed {
			continue
		}
		out = append(out, autopilot.Task{
			ID: t.ID, Issue: t.Issue, Claims: all[t.ID],
			Live:   liveStatus(t.Status) || t.Status == StatusPaused,
			Queued: t.Status == store.Done,
			Paused: t.Status == StatusPaused,
		})
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

// Land runs the train when something is queued, and publishes PRs when
// anything landed. It skips both when nothing waits.
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
	if n > 0 && !e.a.Cfg.CI.Disabled {
		if _, err := e.a.PRs(); err != nil {
			return n, fmt.Errorf("prs: %w", err)
		}
	}
	return n, nil
}

// spawnReq is autopilot's spawn as the app's: the same chooser as the
// orchestrator's spawns picks the adapter from the ticket's difficulty and
// needs, unless its Provider: line names one (#505). An old Model scope
// names a Claude model; it is a hint Spawn drops on other adapters.
func (e *autopilotEnv) spawnReq(r autopilot.SpawnReq) SpawnReq {
	return SpawnReq{Title: r.Title, Prompt: r.Prompt, Claims: slices.Clone(r.Claims), Model: r.Model, Issue: r.Issue, After: r.After,
		Adapter: r.Adapter, Difficulty: r.Difficulty, Needs: r.Needs}
}

// Route is where a ticket would go now, for autopilot's plan (#505).
func (e *autopilotEnv) Route(r autopilot.SpawnReq) (string, string, string, error) {
	return e.a.RoutePreview(e.spawnReq(r))
}

func (e *autopilotEnv) Spawn(r autopilot.SpawnReq) (string, error) {
	t, err := e.a.Spawn(e.spawnReq(r))
	switch {
	case errors.Is(err, ErrLaunchesPaused):
		return "", fmt.Errorf("%w: %v", autopilot.ErrPaused, err)
	case err != nil && (strings.Contains(err.Error(), "at concurrency cap") || errors.Is(err, ErrBackpressure)):
		return "", fmt.Errorf("%w: %v", autopilot.ErrAtCap, err)
	}
	return t.ID, err
}

// Nudge types into the orchestrator's input the way compaction does: only
// when it is idle and the owner has no draft.
func (e *autopilotEnv) Nudge(text string) (bool, error) {
	t := e.a.CompactTarget()
	if t == nil || t.Busy() || t.Drafting() {
		return false, nil
	}
	return true, t.Send(text)
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
		if t.Role != store.RoleWorker || !t.Active() || t.Status == store.Done {
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
		pr := ""
		if n := prNumber(byID[en.Task].PR); n != "" {
			pr = " (PR #" + n + ")"
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
	out, err := gh(a.Root, "issue", "list", "--state", "open", "--label", label, "--limit", "200",
		"--json", "number,title,body,createdAt")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse gh issue list: %w", err)
	}
	closed := map[int]bool{}
	if out, err := gh(a.Root, "pr", "list", "--state", "open", "--limit", "200", "--json", "body"); err == nil {
		var prs []struct {
			Body string `json:"body"`
		}
		if json.Unmarshal([]byte(out), &prs) == nil {
			for _, p := range prs {
				for _, m := range closesRef.FindAllStringSubmatch(p.Body, -1) {
					n, _ := strconv.Atoi(m[1])
					closed[n] = true
				}
			}
		}
	}
	is := make([]autopilot.Issue, len(raw))
	for i, r := range raw {
		is[i] = autopilot.Issue{Number: r.Number, Title: r.Title, Body: r.Body, Created: r.CreatedAt, HasPR: closed[r.Number]}
	}
	return is, nil
}
