// Package engine is saddle's headless watcher for an orchestrator that is not
// the TUI: the user's own Claude Code session running the saddle plugin. It
// does what the TUI's refresh loop does for its headless orchestrator, minus
// the rendering: it notices agents that sit on a prompt or stop without
// calling done, and queues those events as notices for the orchestrator.
package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
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
	primed  bool
}

func New(a *app.App) *Engine {
	return &Engine{a: a, now: time.Now, prev: map[string]string{}, screens: map[string]*screen{}}
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
		if t.Window != "" && (t.Status == store.Running || t.Status == store.Idle) {
			if s, err := e.a.Peek(t.ID, 25); err == nil {
				screens[t.ID] = s
			}
		}
	}
	e.watchScreens(ts, screens)
	e.transitions(ts)
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
	for id, s := range screens {
		st := e.screens[id]
		if st == nil || st.text != s {
			e.screens[id] = &screen{text: s, since: e.now()}
			continue
		}
		if st.acted || e.now().Sub(st.since) < settle {
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
func (e *Engine) transitions(ts []mcpserver.TaskView) {
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
			ev = fmt.Sprintf("%s (%s) stopped without calling done. It may be asking something, stuck, or finished without saying so.", t.ID, t.Title)
		default:
			continue
		}
		screen, _ := e.a.Peek(t.ID, 30)
		e.escalate(t.ID, ev, screen)
	}
}

func (e *Engine) escalate(task, text, screen string) {
	if screen != "" {
		text += "\nIts screen:\n```\n" + screen + "\n```"
	}
	if err := e.a.Notify(app.OrchestratorID, store.NoticeAction, text); err != nil {
		e.a.Store.Event(task, "engine_error", err.Error())
	}
}
