package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// confirmAsk is a destructive agent-list key waiting on y (#264).
type confirmAsk struct {
	prompt string         // the footer's question
	kept   string         // the flash when the answer is no
	run    func() tea.Msg // what y does, off the UI goroutine
}

// askKill asks before x kills task: a kill ends its live session.
func (m *model) askKill(task string) {
	kill := m.killer
	if kill == nil {
		kill = func(id string) error { return m.app.Kill(id, false) }
	}
	m.confirm = &confirmAsk{
		prompt: "Kill " + task + "? It ends the agent's session and releases its claims. y kills it, any other key keeps it",
		kept:   "kept " + task,
		run: func() tea.Msg {
			if err := kill(task); err != nil {
				return flashMsg(err.Error())
			}
			return flashMsg("killed " + task)
		},
	}
}

// askLand asks before L runs the merge train.
func (m *model) askLand() {
	land := m.lander
	if land == nil {
		land = m.app.Land
	}
	m.confirm = &confirmAsk{
		prompt: "Land the queued branches now? y runs the merge train, any other key cancels",
		kept:   "land cancelled",
		run: func() tea.Msg {
			rs, err := land()
			if err != nil {
				return flashMsg("land: " + err.Error())
			}
			var parts []string
			for _, r := range rs {
				parts = append(parts, r.Task+" "+r.State)
			}
			if len(parts) == 0 {
				return flashMsg("train is empty")
			}
			return flashMsg("land: " + strings.Join(parts, ", "))
		},
	}
}

// confirmKey answers the prompt: y runs it, any other key cancels. Every
// key is swallowed while asking.
func (m *model) confirmKey(k tea.KeyMsg) tea.Cmd {
	c := m.confirm
	m.confirm = nil
	if !key.Matches(k, m.keys.Confirm) {
		return flashCmd(c.kept)
	}
	return c.run
}
