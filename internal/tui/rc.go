package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orchrc"
	"github.com/brandonapol/saddle/internal/store"
)

// saddle up --rc (#409): the orchestrator is an interactive Claude Code
// session under Remote Control, in its own tmux window. The bottom pane
// shows that window in place of a shell, and saddle's events reach the
// session through the plugin's wake path, never typed into its pane.

// RunRC opens the TUI around the Remote Control orchestrator s.
func RunRC(a *app.App, s orchrc.Session, opts ...Option) error { return runTUI(a, "", &s, opts...) }

const rcTypeHere = "The orchestrator runs under Remote Control: talk to it in its pane below (alt+` toggles it) or in the Claude app."

func rcIntro(s orchrc.Session) string {
	how := "started"
	switch {
	case s.Reused:
		how = "already running"
	case s.Resumed:
		how = "resumed"
	}
	return fmt.Sprintf("The orchestrator is a Claude Code session under Remote Control (%s). Open %q in the Claude app or claude.ai/code to steer it from anywhere. Its terminal is the pane below: alt+` toggles it, ctrl+r restarts it.", how, s.Name)
}

// useRC makes s the orchestrator: the pane attaches to its window.
func (m *model) useRC(s orchrc.Session) {
	m.orc = &s
	m.launch = s.Launch
	m.termShell = s.Attach
}

// rcShow opens the orchestrator's pane the first time the screen has a size.
func (m *model) rcShow() tea.Cmd {
	if m.orc == nil || m.term != nil || m.termOpen || m.width == 0 {
		return nil
	}
	return m.toggleTerm()
}

// rcDeliver queues the TUI's own worker events (a prompt, a stop without
// done) as orchestrator notices, which the session's wake path takes. Store
// notices stay where they are for it.
func (m *model) rcDeliver() {
	for _, ev := range m.pending {
		if err := m.app.Notify(app.OrchestratorID, store.NoticeAction, ev); err != nil {
			m.app.Store.Event(app.OrchestratorID, "tui_error", err.Error())
		}
	}
	m.pending, m.held = nil, nil
}

// rcRestart brings the session back if its window went away, and reopens
// the pane on it.
func (m *model) rcRestart() tea.Cmd {
	s, err := orchrc.Start(m.app, "", m.orc.Push)
	if err != nil {
		m.addChat(store.ChatEvent, "Restart failed: "+err.Error())
		return nil
	}
	m.useRC(s)
	if m.term != nil {
		m.term.Close()
		m.term = nil
	}
	m.termOpen = false
	m.focusChat()
	m.addChat(store.ChatEvent, rcIntro(s))
	return m.toggleTerm()
}
