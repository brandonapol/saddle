package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/store"
)

// simplerCmd re-explains the orchestrator's last message, or the current
// state, in three sentences for someone with no context (#402). It goes to
// the narrator's cheap model, not the orchestrator.
const simplerCmd = "simpler"

// simplerPrompt is the narrator question for /simpler. last is the
// orchestrator's last message, "" when there is none.
func simplerPrompt(last string) string {
	if last == "" {
		return "Explain what is going on right now, simply, in three sentences, for someone with no context. No code identifiers, test names or file paths."
	}
	return "Re-explain this message from the orchestrator simply, in three sentences, for someone with no context. No code identifiers, test names or file paths. Say whether anything needs them.\n\nMessage:\n" + last
}

// lastAssistant is the orchestrator's last message in the chat, or "".
func (m *model) lastAssistant() string {
	for i := len(m.chat) - 1; i >= 0; i-- {
		if m.chat[i].role == store.ChatAssistant {
			return m.chat[i].text
		}
	}
	return ""
}

// localSlash runs the slash commands the TUI handles itself. ok is false
// for anything else, which goes to the orchestrator.
func (m *model) localSlash(c slashCmd) (tea.Cmd, bool) {
	if c.name != simplerCmd {
		return nil, false
	}
	return m.simpler(), true
}

// simpler asks the narrator for the plain re-explanation and threads it into
// the chat like any narrator answer.
func (m *model) simpler() tea.Cmd {
	if m.asker == nil {
		return func() tea.Msg {
			return flashMsg("/simpler needs the narrator: set ANTHROPIC_API_KEY and narrator.daily_cap_usd")
		}
	}
	q := narrator.Question{Text: simplerPrompt(m.lastAssistant())}
	m.addChat(store.ChatNarrator, askQ+"/"+simplerCmd)
	m.follow = true
	n := m.asker
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		l, err := n.Ask(ctx, q)
		return askedMsg{line: l, err: err}
	}
}
