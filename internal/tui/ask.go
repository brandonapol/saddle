package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/store"
)

// asker answers questions about the agents. *narrator.Narrator satisfies it.
type asker interface {
	Ask(ctx context.Context, q narrator.Question) (narrator.Line, error)
}

// narratorTarget aims the chat input at the narrator. It can't be a task id.
const narratorTarget = "@narrator"

// askScreenLines is how much of an agent's screen goes with a question.
const askScreenLines = 40

// Narrator questions and answers live in the chat log as narrator lines
// marked with these prefixes.
const (
	askQ = "» "
	askA = "↳ "
)

type askedMsg struct {
	line narrator.Line
	err  error
}

func (m *model) asking() bool { return m.target == narratorTarget }

// aimAtNarrator points the chat input at the narrator, or back at the
// orchestrator if it already was.
func (m *model) aimAtNarrator() tea.Cmd {
	if m.asking() {
		m.unaim()
		return nil
	}
	if m.asker == nil {
		return func() tea.Msg {
			return flashMsg("narrator is off: set ANTHROPIC_API_KEY and narrator.daily_cap_usd")
		}
	}
	m.target, m.askScreen = narratorTarget, false
	m.focus = focusChat
	m.input.Prompt = "? "
	m.input.Placeholder = "Ask the narrator… (alt+s sends the selected agent's screen)"
	m.input.Focus()
	return nil
}

// toggleAskScreen opts the selected agent's screen in or out of the next
// question.
func (m *model) toggleAskScreen() tea.Cmd {
	if t, ok := m.selected(); !ok || !live(t.Status, t.Window) {
		return func() tea.Msg { return flashMsg("select an agent with a live window to send its screen") }
	}
	m.askScreen = !m.askScreen
	return nil
}

// submitAsk threads the question into the chat and asks off the UI goroutine.
func (m *model) submitAsk(text string) tea.Cmd {
	q := narrator.Question{Text: text}
	line := askQ + text
	capture := m.capture
	if capture == nil {
		capture = m.app.Peek
	}
	if t, ok := m.selected(); ok && m.askScreen {
		q.Task = t.ID
		line += " (with " + t.ID + "'s screen)"
	}
	m.input.Reset()
	m.unaim()
	m.addChat(store.ChatNarrator, line)
	m.follow = true
	n := m.asker
	return func() tea.Msg {
		if q.Task != "" {
			q.Screen, _ = capture(q.Task, askScreenLines)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		l, err := n.Ask(ctx, q)
		return askedMsg{line: l, err: err}
	}
}

func (m *model) answered(msg askedMsg) {
	if msg.err != nil {
		m.addChat(store.ChatNarrator, askA+"couldn't answer: "+msg.err.Error())
		return
	}
	m.addChat(store.ChatNarrator, askA+msg.line.Text)
}

// chatTitle names who the chat input talks to.
func (m *model) chatTitle() string {
	if m.target == spawnTarget {
		return "SPAWN AN AGENT"
	}
	if !m.asking() {
		title := "ORCHESTRATOR · " + m.launch.Model
		if m.narrow() && len(m.tasks) > 0 {
			// The agent list is hidden; say how to get to it.
			title += fmt.Sprintf(" · tab: %d agent", len(m.tasks))
			if len(m.tasks) > 1 {
				title += "s"
			}
		}
		return title
	}
	title := "ASK NARRATOR"
	if t, ok := m.selected(); ok && m.askScreen {
		title += " · with " + t.ID + "'s screen"
	}
	return title
}

// renderNarrator renders a narrator line: a needs-you alert, a question to
// the narrator, its answer, or plain narration.
func renderNarrator(c chatLine, w int) string {
	switch {
	case narratorNeedsYou(c):
		return renderUrgent(c.text, w-2)
	case strings.HasPrefix(c.text, askQ):
		return lipgloss.NewStyle().Width(w - 2).Foreground(cAccent).Render(c.text)
	case strings.HasPrefix(c.text, askA):
		return renderMarkdown(c.text, w-2, sText)
	}
	return renderMarkdown("· "+c.text, w-2, sDim)
}
