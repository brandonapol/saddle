package tui

import (
	"os"
	"path/filepath"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/termpane"
)

// The terminal pane: a shell in a pty along the bottom of the screen.

type (
	termOutMsg  struct{ t *termpane.Term }
	termExitMsg struct{ t *termpane.Term }
)

// waitTerm delivers the shell's next output or its exit.
func (m *model) waitTerm() tea.Cmd {
	t := m.term
	return func() tea.Msg {
		select {
		case <-t.Updates():
			return termOutMsg{t}
		case <-t.Done():
			return termExitMsg{t}
		}
	}
}

func (m *model) shell() string {
	if m.termShell != "" {
		return m.termShell
	}
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

// toggleTerm opens and focuses the pane, starting a shell if none is running.
// From the pane it hides it again; the shell keeps running.
func (m *model) toggleTerm() tea.Cmd {
	if m.focus == focusTerm {
		m.termOpen = false
		m.focusChat()
		m.layout()
		return nil
	}
	var c tea.Cmd
	if m.term == nil {
		m.termOpen = true // so layout sizes the pane before the shell starts
		m.layout()
		cols, rows := m.termSize()
		t, err := termpane.Start(m.shell(), m.app.Root, cols, rows)
		if err != nil {
			m.termOpen = false
			m.layout()
			return func() tea.Msg { return flashMsg("terminal: " + err.Error()) }
		}
		m.term = t
		c = m.waitTerm()
	}
	m.termOpen = true
	m.focus = focusTerm
	m.input.Blur()
	m.layout()
	return c
}

func (m *model) focusChat() {
	m.focus = focusChat
	m.input.Focus()
}

// termKey handles a key while the pane has focus: everything but the way
// out goes to the shell.
func (m *model) termKey(k tea.KeyMsg) tea.Cmd {
	keys := m.keys
	switch {
	case key.Matches(k, keys.Terminal):
		return m.toggleTerm()
	case key.Matches(k, keys.TermBack) && !m.term.AltScreen():
		// Full-screen programs (vim, less) need esc; the toggle still works there.
		m.focusChat()
		return nil
	case key.Matches(k, keys.TermScrollUp) && !m.term.AltScreen():
		_, rows := m.term.Size()
		m.term.ScrollBy(rows / 2)
		return nil
	case key.Matches(k, keys.TermScrollDown) && !m.term.AltScreen():
		_, rows := m.term.Size()
		m.term.ScrollBy(-rows / 2)
		return nil
	}
	if err := m.term.SendKey(k); err != nil {
		return func() tea.Msg { return flashMsg("terminal: " + err.Error()) }
	}
	return nil
}

func (m *model) termEvent(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case termOutMsg:
		if msg.t == m.term {
			return m.waitTerm()
		}
	case termExitMsg:
		if msg.t != m.term {
			return nil
		}
		m.term.Close()
		m.term, m.termOpen = nil, false
		if m.focus == focusTerm {
			m.focusChat()
		}
		m.layout()
		return func() tea.Msg { return flashMsg("shell exited; ctrl+` starts a new one") }
	}
	return nil
}

// termHeight is the pane's height, borders included, within a body of height h.
func (m *model) termHeight(h int) int {
	if !m.termOpen {
		return 0
	}
	return min(max(h/3, 6), 16, max(h-8, 0))
}

// termSize is the shell's screen size inside the pane's border.
func (m *model) termSize() (cols, rows int) {
	return max(m.width-2, 1), max(m.termHeight(m.bodyHeight())-2, 1)
}

// resizeTerm keeps the shell's screen the size of the pane.
func (m *model) resizeTerm() {
	if m.term == nil || !m.termOpen {
		return
	}
	cols, rows := m.termSize()
	_ = m.term.Resize(cols, rows)
}

// termMouse scrolls the pane's history with the wheel over it. It reports
// whether the event was over the pane.
func (m *model) termMouse(msg tea.MouseMsg) bool {
	if m.term == nil || !m.termOpen {
		return false
	}
	bottom := m.height - lipgloss.Height(m.viewFooter())
	if msg.Y < bottom-m.termHeight(m.bodyHeight()) || msg.Y >= bottom {
		return false
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.term.ScrollBy(3)
	case tea.MouseButtonWheelDown:
		m.term.ScrollBy(-3)
	}
	return true
}

func (m *model) viewTerm(h int) string {
	title := "TERMINAL · " + filepath.Base(m.shell())
	if n := m.term.Scrolled(); n > 0 {
		title += " · history"
	}
	focused := m.focus == focusTerm
	return box(title, m.width, h, focused, m.term.View(focused))
}
