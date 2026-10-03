package tui

import (
	"strings"

	"github.com/brandonapol/saddle/internal/brief"
)

// The brief pane (#37) swaps the control view's peek for a compact brief of
// the selected agent: goal, owned and hands-off paths, done-when, children.

// briefHeads are the brief's section titles, styled as headings.
var briefHeads = map[string]bool{"Goal": true, "Owns": true, "Hands off": true, "Done when": true, "Children": true}

// taskPrompt is a task's prompt, read from the store once: prompts don't
// change after spawn. With no store it is empty and the goal is the title.
func (m *model) taskPrompt(id string) string {
	if p, ok := m.prompts[id]; ok {
		return p
	}
	p := ""
	if m.app != nil && m.app.Store != nil {
		if t, err := m.app.Store.Task(id); err == nil {
			p = t.Prompt
		}
	}
	if m.prompts == nil {
		m.prompts = map[string]string{}
	}
	m.prompts[id] = p
	return p
}

// briefPane is the brief box's title and body for the selected agent, with
// lines no wider than w.
func (m *model) briefPane(w int) (title, body string) {
	t, ok := m.selected()
	if !ok {
		return "BRIEF", sDim.Render(" Select an agent to see its brief.")
	}
	var serial []string
	if m.app != nil {
		serial = m.app.Cfg.Serial
	}
	b, err := brief.Build(t.ID, m.taskPrompt(t.ID), m.tasks, serial)
	if err != nil {
		return "BRIEF · " + t.ID, sDim.Render(" " + err.Error())
	}
	ls := b.Lines(w - 1)
	for i, l := range ls {
		switch {
		case i == 0:
			ls[i] = " " + sText.Bold(true).Render(l)
		case i == 1:
			ls[i] = " " + sDim.Render(l)
		case briefHeads[l]:
			ls[i] = " " + sKey.Render(l)
		default:
			ls[i] = " " + sText.Render(l)
		}
	}
	return "BRIEF · " + t.ID + " " + t.Title, strings.Join(ls, "\n")
}
