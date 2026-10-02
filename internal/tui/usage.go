package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// stateColor is how a plan-limit window's state renders.
func stateColor(s usage.State) lipgloss.Color {
	switch s {
	case usage.Over:
		return cAlert
	case usage.Warn:
		return cAccent
	}
	return cDone
}

// hasUsage reports whether the strip has anything to show: a cap, or usage.
func hasUsage(e usage.LimitEstimate) bool {
	for _, w := range []usage.WindowEstimate{e.FiveHour, e.Weekly} {
		if !w.Unlimited || w.Tokens.Total() > 0 {
			return true
		}
	}
	return false
}

// usageStrip renders the 5-hour and weekly windows on one line of width w:
// a bar and percent for a capped window, colored ok/warn/over, and the
// window's tokens and $-equivalent.
func usageStrip(e usage.LimitEstimate, w int) string {
	var parts []string
	for _, win := range []usage.WindowEstimate{e.FiveHour, e.Weekly} {
		parts = append(parts, usageWindow(win))
	}
	line := " " + strings.Join(parts, sFaint.Render("  │  "))
	return lipgloss.NewStyle().MaxWidth(w).Render(line)
}

func usageWindow(win usage.WindowEstimate) string {
	s := sDim.Render(win.Name) + " "
	totals := sDim.Render(fmt.Sprintf("%s tok · $%.2f", humanTokens(win.Tokens.Total()), win.USD))
	if win.Unlimited {
		return s + totals
	}
	c := lipgloss.NewStyle().Foreground(stateColor(win.State))
	s += c.Render(bar(win.Percent, 10)+fmt.Sprintf(" %.0f%%", win.Percent*100)) + " "
	if win.State != usage.OK {
		s += c.Bold(true).Render(win.State.String()) + " "
	}
	s += totals
	if win.ResetIn > 0 {
		s += sDim.Render(" · resets " + shortDur(win.ResetIn))
	}
	return s
}

func bar(frac float64, n int) string {
	full := int(frac*float64(n) + 0.5)
	full = min(max(full, 0), n)
	return "▕" + strings.Repeat("█", full) + strings.Repeat("░", n-full) + "▏"
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

func shortDur(d time.Duration) string {
	d = d.Round(time.Minute)
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

// narrSink hands narrator lines to the UI. It never blocks the narrator: if
// the UI falls a whole buffer behind, lines are dropped.
type narrSink chan narrator.Line

func (s narrSink) Emit(l narrator.Line) {
	select {
	case s <- l:
	default:
	}
}

type narrMsg struct{ line narrator.Line }

func (m *model) waitNarr() tea.Cmd {
	ch := m.narr
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return narrMsg{<-ch} }
}

// narratorNeedsYou reports whether a chat line is a narrator needs-you line.
func narratorNeedsYou(c chatLine) bool {
	return c.role == store.ChatNarrator && strings.HasPrefix(c.text, "‼ ")
}
