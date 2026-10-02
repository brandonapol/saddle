package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/store"
)

// viewHeader is one line at any width: the logo and current view always,
// then the view tabs, branches, task counts, train and orchestrator state as
// room allows, most important first.
func (m *model) viewHeader() string {
	counts := map[string]int{}
	queued, stuck := 0, 0
	for _, t := range m.tasks {
		counts[t.Status]++
		switch firstWord(t.Train) {
		case "", store.TrainOK:
		case store.Queued:
			queued++
		default:
			stuck++
		}
	}
	color := func(c lipgloss.Color, s string, a ...any) string {
		return lipgloss.NewStyle().Foreground(c).Render(fmt.Sprintf(s, a...))
	}
	var tabs []string
	for i, n := range viewNames {
		if i == m.view {
			tabs = append(tabs, lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render(fmt.Sprintf("%d %s", i+1, n)))
		} else {
			tabs = append(tabs, sDim.Render(fmt.Sprintf("%d %s", i+1, n)))
		}
	}
	current := lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render(viewNames[m.view])

	var parts []string
	opt := func(n int, c lipgloss.Color, s string) {
		if n > 0 {
			parts = append(parts, color(c, s, n))
		}
	}
	parts = append(parts, sBright.Render(m.app.Cfg.Session), sDim.Render(m.app.Cfg.Base+" → "+m.app.Cfg.Integration))
	opt(counts[store.Running], cRun, "● %d running")
	opt(counts[store.NeedsYou]+counts[store.Conflict]+counts[store.Idle], cAlert, "▲ %d need attention")
	if queued+stuck > 0 {
		train := color(cAccent, "◆ train %d queued", queued)
		if stuck > 0 {
			train += color(cAlert, " · %d conflict", stuck)
		}
		parts = append(parts, train)
	}
	opt(counts[store.Landed], cDone, "✓ %d landed")

	state := "idle"
	if m.proc != nil && m.proc.Busy() {
		state = "working"
	}
	extra := ""
	if m.jev != nil {
		extra = " · jev triage"
	}
	if m.narr != nil {
		extra += " · narrator"
	}
	right := sDim.Render(fmt.Sprintf("orchestrator %s · %s%s · $%.2f ", m.launch.Model, state, extra, m.cost))

	line := sLogo.Render("SADDLE")
	if full := line + "  " + strings.Join(tabs, "  "); lipgloss.Width(full) < m.width {
		line = full
	} else {
		line += " " + current
	}
	for _, p := range parts {
		if lipgloss.Width(line+"  "+p) >= m.width {
			break
		}
		line += "  " + p
	}
	if gap := m.width - lipgloss.Width(line) - lipgloss.Width(right); gap >= 1 {
		line += strings.Repeat(" ", gap) + right
	}
	return lipgloss.NewStyle().MaxWidth(m.width).Render(line)
}
