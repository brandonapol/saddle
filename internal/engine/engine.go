// Package engine is saddle's headless watcher for an orchestrator that is not
// the TUI: the user's own Claude Code session running the saddle plugin. It
// does what the TUI's refresh loop does for its headless orchestrator, minus
// the rendering: it notices agents that sit on a prompt or stop without
// calling done, and queues those events as notices for the orchestrator.
// A worker Claude Code parked on a usage limit is marked paused until the
// banner clears, with one notice and no keys sent into its pane (#180).
package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// settle is how long a screen must sit unchanged before a prompt on it counts.
const settle = 4 * time.Second

type screen struct {
	text  string
	since time.Time
	acted bool
}

// Engine watches worker tasks. Tick is called once a second by Run.
type Engine struct {
	a       *app.App
	now     func() time.Time
	prev    map[string]string // task -> status at the last tick
	screens map[string]*screen
	parked  map[string]bool // tasks paused on a usage-limit banner
	primed  bool
}

func New(a *app.App) *Engine {
	return &Engine{a: a, now: time.Now, prev: map[string]string{}, screens: map[string]*screen{}, parked: map[string]bool{}}
}

// Run ticks every second until ctx ends. A failed tick is recorded as an
// event and the next one tries again.
func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := e.Tick(); err != nil {
			e.a.Store.Event(app.OrchestratorID, "engine_error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick reads every worker's status and screen once and reports what changed.
func (e *Engine) Tick() error {
	all, err := mcpserver.Tasks(e.a)
	if err != nil {
		return err
	}
	var ts []mcpserver.TaskView
	for _, t := range all {
		if t.ID != app.OrchestratorID {
			ts = append(ts, t)
		}
	}
	screens := map[string]string{}
	for _, t := range ts {
		// Paused tasks are watched for a usage-limit banner clearing; Peek
		// fails for the ones saddle down stopped, whose window is gone.
		if t.Window != "" && (t.Status == store.Running || t.Status == store.Idle || t.Status == app.StatusPaused) {
			if s, err := e.a.Peek(t.ID, 25); err == nil {
				screens[t.ID] = s
			}
		}
	}
	e.watchScreens(ts, screens)
	e.transitions(ts, screens)
	return nil
}

// watchScreens catches prompts no hook reports (like Claude Code's
// folder-trust dialog): a prompt that sits unchanged for a few seconds marks
// the worker as needing attention, which transitions then reports.
func (e *Engine) watchScreens(ts []mcpserver.TaskView, screens map[string]string) {
	status := map[string]string{}
	for _, t := range ts {
		status[t.ID] = t.Status
	}
	for id := range e.parked {
		if _, ok := screens[id]; !ok {
			delete(e.parked, id) // its window is gone: an ordinary paused task now
		}
	}
	for id, s := range screens {
		if e.parked[id] {
			if b, on := usage.DetectLimitBanner(s); !on {
				e.unpark(id, status[id])
			} else if status[id] != app.StatusPaused {
				e.park(ts, id, status[id], b, s)
			}
		}
		st := e.screens[id]
		if st == nil || st.text != s {
			e.screens[id] = &screen{text: s, since: e.now()}
			continue
		}
		if st.acted || e.now().Sub(st.since) < settle {
			continue
		}
		if b, on := usage.DetectLimitBanner(s); on {
			st.acted = true
			e.park(ts, id, status[id], b, s)
			continue
		}
		if status[id] == app.StatusPaused {
			continue
		}
		kind := app.DetectPrompt(s)
		if kind == app.PromptNone {
			continue
		}
		st.acted = true
		if kind == app.PromptTrust && e.a.RootTrusted() {
			if err := e.a.SendKeys(id, "", []string{"Down", "Enter"}); err == nil {
				e.a.Store.Event(id, "trust_accepted", "")
				continue
			}
		}
		switch status[id] {
		case store.Running, store.Idle:
			_ = e.a.Store.SetStatus(id, store.NeedsYou) // transitions reports it on this tick
		case store.NeedsYou:
			e.escalate(id, id+" is still waiting on a prompt after the last answer.", s)
		}
	}
}

// transitions reports workers that started waiting on a prompt or stopped
// without calling done. The first tick only records where things stand, so
// restarting the engine doesn't re-report old states.
func (e *Engine) transitions(ts []mcpserver.TaskView, screens map[string]string) {
	first := !e.primed
	e.primed = true
	for _, t := range ts {
		status := t.Status
		if s, err := e.a.Store.Task(t.ID); err == nil {
			status = s.Status // watchScreens may have just changed it
		}
		was, seen := e.prev[t.ID]
		e.prev[t.ID] = status
		if first || !seen || was == status {
			continue
		}
		var ev string
		switch status {
		case store.NeedsYou:
			ev = fmt.Sprintf("%s (%s) is waiting on a permission prompt or a question.", t.ID, t.Title)
		case store.Idle:
			if strings.HasPrefix(t.Train, store.Queued) {
				continue
			}
			if _, on := usage.DetectLimitBanner(screens[t.ID]); on {
				continue // hit a usage limit: watchScreens parks it once the banner settles
			}
			ev = fmt.Sprintf("%s (%s) stopped without calling done. It may be asking something, stuck, or finished without saying so.", t.ID, t.Title)
		default:
			continue
		}
		screen, _ := e.a.Peek(t.ID, 30)
		e.escalate(t.ID, ev, screen)
	}
}

// park marks a worker sitting on a usage-limit banner paused and tells the
// orchestrator once. Nothing is typed into the pane: Escape there cancels
// Claude Code's automatic restart. A worker already paused when the engine
// first sees its banner was parked by an earlier engine and is adopted
// quietly; one a hook set back to idle under the banner is re-paused quietly.
func (e *Engine) park(ts []mcpserver.TaskView, id, status string, b usage.LimitBanner, screen string) {
	if status != app.StatusPaused {
		if err := e.a.Store.SetStatus(id, app.StatusPaused); err != nil {
			e.a.Store.Event(id, "engine_error", err.Error())
			return
		}
	}
	if e.parked[id] || status == app.StatusPaused {
		e.parked[id] = true
		return
	}
	e.parked[id] = true
	resets := "after the limit resets"
	if b.Resets != "" {
		resets = "at " + b.Resets
	}
	e.a.Store.Event(id, "usage_parked", b.Resets)
	title := ""
	for _, t := range ts {
		if t.ID == id {
			title = " (" + t.Title + ")"
		}
	}
	e.escalate(id, fmt.Sprintf("%s%s is parked on a Claude usage limit and continues on its own %s. "+
		"It shows as paused until then. Nothing needs answering; don't send it keys (Escape cancels the automatic restart).", id, title, resets), screen)
}

// unpark puts a worker whose usage-limit banner cleared back to running,
// without a notice. A hook may have done it already.
func (e *Engine) unpark(id, status string) {
	delete(e.parked, id)
	if status == app.StatusPaused {
		if err := e.a.Store.SetStatus(id, store.Running); err != nil {
			e.a.Store.Event(id, "engine_error", err.Error())
			return
		}
	}
	e.a.Store.Event(id, "usage_resumed", "")
}

func (e *Engine) escalate(task, text, screen string) {
	if screen != "" {
		text += "\nIts screen:\n```\n" + screen + "\n```"
	}
	if err := e.a.Notify(app.OrchestratorID, store.NoticeAction, text); err != nil {
		e.a.Store.Event(task, "engine_error", err.Error())
	}
}
