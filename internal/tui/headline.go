package tui

import (
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// The state headline (#403) owns the header's top line: one sentence and a
// colour, computed by saddle, saying whether anything needs the owner.

// headlineTTL is how long a computed headline is shown before it is read
// again; the header draws every frame.
const headlineTTL = 2 * time.Second

type headlineEntry struct {
	at time.Time
	h  app.StateHeadline
}

var (
	headlineMu    sync.Mutex
	headlineCache = map[*app.App]headlineEntry{}
)

// headline is the current headline. With a store it is saddle's own,
// cached for headlineTTL; without one (a bare model) it is computed from
// the tasks the TUI holds.
func (m *model) headline(now time.Time) app.StateHeadline {
	if m.app == nil || m.app.Store == nil {
		return app.Headline(m.headlineInput(now))
	}
	headlineMu.Lock()
	defer headlineMu.Unlock()
	if e, ok := headlineCache[m.app]; ok && now.Sub(e.at) < headlineTTL {
		return e.h
	}
	h, err := m.app.Headline(now)
	if err != nil {
		h = app.Headline(m.headlineInput(now))
	}
	headlineCache[m.app] = headlineEntry{at: now, h: h}
	return h
}

// headlineInput is the headline's input from what the model holds.
func (m *model) headlineInput(now time.Time) app.HeadlineInput {
	in := app.HeadlineInput{Now: now}
	for _, t := range m.tasks {
		in.Tasks = append(in.Tasks, store.Task{ID: t.ID, Title: t.Title, Status: t.Status, PR: t.PR})
		if firstWord(t.Train) == store.Queued {
			in.Train = append(in.Train, store.TrainEntry{Task: t.ID, State: store.Queued})
		}
	}
	if m.am != nil {
		in.AutomergeStopped = m.am.Stopped
	}
	if m.ap != nil {
		in.Autopilot = m.ap.On && !m.ap.Paused
	}
	return in
}

// headlineColor maps a headline colour to the TUI's palette.
func headlineColor(c string) lipgloss.Color {
	switch c {
	case app.ColorRed:
		return cAlert
	case app.ColorYellow:
		return cAccent
	case app.ColorGreen:
		return cDone
	}
	return cDim
}

// headlineGlyph marks the class at a glance, even without colour.
func headlineGlyph(class string) string {
	switch class {
	case app.HeadlineNeedsYou:
		return "▲"
	case app.HeadlineBlocked:
		return "■"
	case app.HeadlineDegraded:
		return "◆"
	case app.HeadlineWorking:
		return "●"
	}
	return "○"
}

// renderHeadline is the header's top line at width: the logo, then the
// headline, clipped with an ellipsis when it does not fit.
func renderHeadline(h app.StateHeadline, repo string, width int) string {
	logo := sLogo.Render("SADDLE")
	if pill := repoPill(repo, width-lipgloss.Width(logo)-1, width/3); pill != "" {
		logo += " " + pill
	}
	room := width - lipgloss.Width(logo) - 2
	if room < 4 {
		return lipgloss.NewStyle().MaxWidth(width).Render(logo)
	}
	text := headlineGlyph(h.Class) + " " + h.Text
	if r := []rune(text); len(r) > room {
		text = strings.TrimRight(string(r[:room-1]), " ") + "…"
	}
	style := lipgloss.NewStyle().Foreground(headlineColor(h.Color))
	if h.Class == app.HeadlineNeedsYou || h.Class == app.HeadlineBlocked {
		style = style.Bold(true)
	}
	return logo + "  " + style.Render(text)
}

// repoPill is the repo's name in a box of its own, a different colour from
// the logo's, clipped with an ellipsis to at most limit cells and to what
// room leaves. It is empty when the name is empty or not even a clipped
// pill fits.
func repoPill(repo string, room, limit int) string {
	limit = min(limit, room)
	if repo == "" || limit < 5 {
		return ""
	}
	if r := []rune(repo); len(r) > limit-2 {
		repo = string(r[:limit-3]) + "…"
	}
	return sRepo.Render(repo)
}
