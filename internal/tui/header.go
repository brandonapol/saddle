package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/store"
)

// viewHeader is one line at any width: the logo and current view always,
// the orchestrator's state at the right, then what needs the user, task
// counts, the train, auto-merge and the session and branches as room
// allows, most important first.
func (m *model) viewHeader() string {
	counts := map[string]int{}
	queued, stuck := 0, 0
	for _, t := range m.tasks {
		counts[t.Status]++
		switch state := firstWord(t.Train); {
		case waiting(state):
			queued++
		case trainStuck(state):
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

	// Auto-merge that is on, stopped or holding stacks leads; off trails.
	amLong, amShort, amColor := m.amHeader()
	amFirst := amLong != "" && amColor != cDim

	var parts []string
	opt := func(n int, c lipgloss.Color, s string) {
		if n > 0 {
			parts = append(parts, color(c, s, n))
		}
	}
	opt(counts[store.NeedsYou]+counts[store.Conflict], cAlert, "▲ %d need you")
	opt(counts[store.Running], cRun, "● %d running")
	opt(counts[store.Idle], cAccent, "◐ %d idle")
	if queued+stuck > 0 {
		train := color(cAccent, "◆ train %d queued", queued)
		if stuck > 0 {
			train += color(cAlert, " · %d conflict", stuck)
		}
		parts = append(parts, train)
	}
	parts = append(parts, m.heavySegments()...)
	opt(counts[store.Landed], cDone, "✓ %d landed")
	tail := []string{sBright.Render(m.app.Cfg.Session), sDim.Render(m.app.Cfg.Base + " → " + m.app.Cfg.Integration)}

	state := "idle"
	switch {
	case m.proc != nil && m.proc.Interrupting():
		state = "interrupting"
	case m.proc != nil && m.proc.Busy():
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
	short := sDim.Render("orch " + state + " ")

	line := sLogo.Render("SADDLE")
	if full := line + "  " + strings.Join(tabs, "  "); lipgloss.Width(full)+lipgloss.Width(short) < m.width {
		line = full
	} else {
		line += " " + current
	}
	// The short state is always kept room for.
	fits := func(s string) bool { return lipgloss.Width(line+"  "+s)+lipgloss.Width(short) < m.width }
	addAM := func() {
		room := m.width - lipgloss.Width(line) - lipgloss.Width(short) - 3
		switch {
		case fits(amLong):
			line += "  " + color(amColor, "%s", amLong)
		case m.am.Stopped != "" && room >= 24:
			line += "  " + color(amColor, "%s", truncate(amLong, room))
		case fits(amShort):
			line += "  " + color(amColor, "%s", amShort)
		}
	}
	// Infinite mode leads everything: it acts on its own around the clock.
	if apLong, apShort := m.apHeader(); apLong != "" {
		style := lipgloss.NewStyle().Foreground(cAccent).Bold(true)
		switch {
		case fits(apLong):
			line += "  " + style.Render(apLong)
		case fits(apShort):
			line += "  " + style.Render(apShort)
		default:
			line += " " + style.Render("∞")
		}
	}
	if amFirst {
		// Auto-merge that acts on its own outranks even the orchestrator's state.
		reserve := short
		short = ""
		addAM()
		if lipgloss.Width(line+"  "+reserve) < m.width {
			short = reserve
		}
	}
	for _, p := range parts {
		if !fits(p) {
			break
		}
		line += "  " + p
	}
	if amLong != "" && !amFirst {
		addAM()
	}
	for _, p := range tail {
		if !fits(p) {
			break
		}
		line += "  " + p
	}
	if lipgloss.Width(line)+lipgloss.Width(right) >= m.width {
		right = sDim.Render("orch " + state + " ")
	}
	if gap := m.width - lipgloss.Width(line) - lipgloss.Width(right); gap >= 1 {
		line += strings.Repeat(" ", gap) + right
	}
	return lipgloss.NewStyle().MaxWidth(m.width).Render(line)
}
