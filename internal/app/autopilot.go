package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/config"
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
	rs, err := e.a.Land()
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

func (e *autopilotEnv) Spawn(r autopilot.SpawnReq) (string, error) {
	model := r.Model
	if e.a.Cfg.Harness == config.HarnessGrok {
		model = "" // model scope names Claude models
	}
	t, err := e.a.Spawn(SpawnReq{Title: r.Title, Prompt: r.Prompt, Claims: r.Claims, Model: model, Issue: r.Issue, After: r.After})
	switch {
	case errors.Is(err, ErrLaunchesPaused):
		return "", fmt.Errorf("%w: %v", autopilot.ErrPaused, err)
	case err != nil && strings.Contains(err.Error(), "at concurrency cap"):
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
