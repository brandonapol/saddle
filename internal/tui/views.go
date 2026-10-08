package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Views. The control view is agents, peek and the orchestrator chat; plan and
// merge show the plan under review and the merge train.
const (
	viewControl = iota
	viewPlan
	viewMerge
)

var viewNames = []string{"control", "plan", "merge"}

// narrowWidth is the width below which the control view shows only the
// focused pane: agents and peek, or the chat.
const narrowWidth = 74

func (m *model) narrow() bool { return m.width < narrowWidth }

// routeKey handles the keys that work in every view: switching views and the
// help overlay. Outside the control view it also swallows the keys the hidden
// chat and agent list would otherwise act on. Quit and the terminal toggle
// always pass through.
func (m *model) routeKey(k tea.KeyMsg) (tea.Cmd, bool) {
	keys := m.keys
	if key.Matches(k, keys.Quit, keys.Terminal) {
		return nil, false
	}
	switch {
	case key.Matches(k, keys.ViewControl):
		m.setView(viewControl)
		return nil, true
	case key.Matches(k, keys.ViewPlan):
		m.setView(viewPlan)
		return m.loadPlan(), true
	case key.Matches(k, keys.ViewMerge):
		m.setView(viewMerge)
		return m.loadTrain(), true
	}
	// The replan note takes typing, help key included.
	if m.view == viewPlan && m.pl.noting {
		return m.planKey(k)
	}
	if m.helpOpen {
		if key.Matches(k, keys.Help, keys.Back) {
			m.helpOpen = false
		}
		return nil, true
	}
	// While the chat takes typing, ? is a character; f1 still opens help.
	typing := m.view == viewControl && m.focus == focusChat
	if key.Matches(k, keys.Help) && (!typing || k.Type == tea.KeyF1) {
		m.helpOpen = true
		return nil, true
	}
	if m.view == viewPlan {
		if c, ok := m.planKey(k); ok {
			return c, true
		}
	}
	if m.view == viewMerge {
		if c, ok := m.mergeKey(k); ok {
			return c, true
		}
	}
	return nil, m.view != viewControl
}

func (m *model) setView(v int) {
	m.view, m.helpOpen = v, false
}

// viewBody renders the screen between the header and the terminal pane.
func (m *model) viewBody(w, h int) string {
	switch {
	case m.helpOpen:
		return m.viewHelp(w, h)
	case m.cp.on:
		return m.viewCopy(w, h)
	case m.view == viewPlan:
		return m.markPane("plan", 0, m.bodyTop, m.viewPlan(w, h))
	case m.view == viewMerge:
		return m.markPane("merge", 0, m.bodyTop, m.viewMerge(w, h))
	}
	if m.narrow() {
		if m.focus == focusTasks {
			return m.markPane("left", 0, m.bodyTop, m.viewLeft(w, h))
		}
		return m.markPane("chat", 0, m.bodyTop, m.viewChat(w, h))
	}
	cw := m.chatWidth()
	left := m.markPane("left", 0, m.bodyTop, m.viewLeft(w-cw, h))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, m.markPane("chat", w-cw, m.bodyTop, m.viewChat(cw, h)))
}

// viewHelp lists every binding by group, in columns as wide as fit.
func (m *model) viewHelp(w, h int) string {
	var sections []string
	keyW := 0
	for _, g := range m.keys.groups() {
		for _, b := range g.keys {
			keyW = max(keyW, lipgloss.Width(b.Help().Key))
		}
	}
	for _, g := range m.keys.groups() {
		rows := []string{sBright.Render(g.title)}
		for _, b := range g.keys {
			hp := b.Help()
			if hp.Key == "" {
				continue // the second of a pair
			}
			rows = append(rows, sKey.Render(fmt.Sprintf("%-*s", keyW, hp.Key))+" "+sDim.Render(hp.Desc))
		}
		sections = append(sections, strings.Join(rows, "\n"))
	}
	inner := w - 2
	var body string
	colW := 0
	for _, s := range sections {
		colW = max(colW, lipgloss.Width(s))
	}
	if cols := inner / (colW + 3); cols >= 2 {
		var rows []string
		for i := 0; i < len(sections); i += cols {
			var row []string
			for _, s := range sections[i:min(i+cols, len(sections))] {
				row = append(row, lipgloss.NewStyle().Width(colW+3).Render(s))
			}
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, row...))
		}
		body = strings.Join(rows, "\n\n")
	} else {
		body = strings.Join(sections, "\n\n")
	}
	return box("KEYS", w, h, true, clip(body, inner))
}

// viewMerge shows the auto-merge PR stacks above the merge train.
func (m *model) viewMerge(w, h int) string {
	rows := append(m.viewStacks(w-2), "")
	rows = append(rows, m.viewTrain(w-2)...)
	title := "MERGE TRAIN"
	if m.graph != nil {
		title += " · opus on merges " + humanTokens(m.graph.MergeOpus)
	}
	return box(title, w, h, false, clip(strings.Join(rows, "\n"), w-2))
}

// clip truncates every line of s to w columns.
func clip(s string, w int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if lipgloss.Width(l) > w {
			lines[i] = lipgloss.NewStyle().MaxWidth(w).Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
