package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// agentStats is what the agent rows show beyond the task itself.
type agentStats struct {
	Tokens   int64  // every token the task has used, cache included
	Ctx      int64  // estimated context size at its latest message
	CtxMax   int64  // its model's context window
	Activity string // what it is doing now, from its latest hook event
}

// ctxRecent is how far back readStats looks for a task's latest message.
const ctxRecent = 6 * time.Hour

// readStats gathers per-task tokens, context and activity from the store.
// Errors leave a field empty: the rows are a view, not a gate.
func readStats(a *app.App, now time.Time) map[string]agentStats {
	out := map[string]agentStats{}
	if a == nil || a.Store == nil {
		return out
	}
	if ts, err := a.Store.UsageByTask(time.Time{}, time.Time{}); err == nil {
		for _, t := range ts {
			s := out[t.Key]
			s.Tokens = t.Total()
			out[t.Key] = s
		}
	}
	// Buckets are per minute, so a minute's prompt tokens over its messages
	// is the average context of those turns: close to the current one.
	if bs, err := a.Store.UsageBuckets(now.Add(-ctxRecent)); err == nil {
		for _, b := range bs {
			if b.Messages == 0 {
				continue
			}
			s := out[b.Task]
			s.Ctx = (b.Input + b.CacheRead + b.CacheCreation) / b.Messages
			s.CtxMax = ctxLimit(b.Model)
			out[b.Task] = s
		}
	}
	if es, err := a.Store.Events(500); err == nil {
		for _, e := range es {
			act, ok := activity(e)
			if !ok || e.Task == "" {
				continue
			}
			s := out[e.Task]
			s.Activity = act
			out[e.Task] = s
		}
	}
	return out
}

// activity describes what an event says the agent is doing.
func activity(e store.Event) (string, bool) {
	switch e.Kind {
	case "tool":
		return e.Data, true
	case "notification":
		return "waiting: " + e.Data, true
	case "stop":
		return "at its prompt", true
	case "session_start":
		return "starting", true
	case "done":
		return "done", true
	}
	return "", false
}

// ctxLimit is a model's context window in tokens.
func ctxLimit(model string) int64 {
	if strings.Contains(strings.ToLower(model), "1m") {
		return 1_000_000
	}
	return 200_000
}

// Agent row column widths.
const (
	idW     = 5
	statusW = 9
	modelW  = 6
	ctxW    = 12 // ▕█████▏  42%
	tokW    = 6
	winW    = 4
	actMinW = 14
	titleMn = 10
)

// ctxCell is a 5-cell bar and percent of the context window, red past 80%.
func ctxCell(s agentStats) string {
	if s.CtxMax <= 0 || s.Ctx <= 0 {
		return sFaint.Render(fmt.Sprintf("%-*s", ctxW, " ctx —"))
	}
	frac := float64(s.Ctx) / float64(s.CtxMax)
	c := cRun
	switch {
	case frac >= 0.8:
		c = cAlert
	case frac >= 0.6:
		c = cAccent
	}
	return lipgloss.NewStyle().Foreground(c).Render(bar(frac, 5) + fmt.Sprintf(" %3.0f%%", min(frac, 9.99)*100))
}

// agentCols decides which optional columns fit a row of inner width iw, and
// how wide the title and activity get.
type agentCols struct {
	status, model, ctx, tokens, window bool
	titleW, actW                       int
}

func layoutCols(iw int) agentCols {
	var c agentCols
	rem := iw - 3 - idW - 1 // marker, glyph, space, id, space
	take := func(w int) bool {
		if rem-w-1 >= titleMn {
			rem -= w + 1
			return true
		}
		return false
	}
	c.status = take(statusW)
	c.model = take(modelW)
	c.ctx = take(ctxW)
	c.tokens = take(tokW)
	c.window = take(winW)
	c.titleW = rem
	if rem-1-actMinW >= titleMn+8 {
		c.titleW = min(max(rem/2, titleMn+8), 32)
		c.actW = rem - c.titleW - 1
	}
	return c
}

func (m *model) agentRow(t mcpserver.TaskView, c agentCols) string {
	g, gc := glyph(t.Status)
	s := m.stats[t.ID]
	row := lipgloss.NewStyle().Foreground(gc).Render(g) + " " + sDim.Render(fmt.Sprintf("%-*s", idW, t.ID)) + " " +
		sText.Render(fmt.Sprintf("%-*s", c.titleW, truncate(t.Title, c.titleW)))
	if c.model {
		row += " " + lipgloss.NewStyle().Foreground(modelColor(t.Model)).Render(fmt.Sprintf("%-*s", modelW, truncate(t.Model, modelW)))
	}
	if c.status {
		row += " " + sDim.Render(fmt.Sprintf("%-*s", statusW, truncate(statusLabel(t.Status), statusW)))
	}
	if c.ctx {
		row += " " + ctxCell(s)
	}
	if c.tokens {
		tok := ""
		if s.Tokens > 0 {
			tok = humanTokens(s.Tokens)
		}
		row += " " + sDim.Render(fmt.Sprintf("%*s", tokW, tok))
	}
	if c.window {
		row += " " + sFaint.Render(fmt.Sprintf("%-*s", winW, truncate(t.Window, winW)))
	}
	if c.actW > 0 && s.Activity != "" && live(t.Status, t.Window) {
		row += " " + sDim.Render(truncate(s.Activity, c.actW))
	}
	return row
}

// claimRows lists each task's claims on one line, the selected task first.
func (m *model) claimRows(w int) []string {
	var rows []string
	add := func(i int, t mcpserver.TaskView) {
		if len(t.Claims) == 0 {
			return
		}
		id := sDim
		if i == m.sel {
			id = sKey
		}
		rows = append(rows, id.Render(fmt.Sprintf("%-*s", idW, t.ID))+" "+sText.Render(truncate(strings.Join(t.Claims, ", "), w-idW-1)))
	}
	if t, ok := m.selected(); ok {
		add(m.sel, t)
	}
	for i, t := range m.tasks {
		if i != m.sel {
			add(i, t)
		}
	}
	return rows
}

// viewLeft is the agents list, the claims panel and the peek of the
// selected agent, stacked in a column of width w and height h.
func (m *model) viewLeft(w, h int) string {
	listH := len(m.tasks) + 2
	if listH < 5 {
		listH = 5
	}
	if listH > h/2 {
		listH = h / 2
	}
	var rows []string
	if len(m.tasks) == 0 {
		rows = append(rows, sDim.Render(" No agents yet. Ask the orchestrator to start some."))
	}
	cols := layoutCols(w - 2)
	for i, t := range m.tasks {
		row := m.agentRow(t, cols)
		if i == m.sel {
			row = lipgloss.NewStyle().Foreground(cAccent).Render("›") + row
			if m.focus == focusTasks {
				row = lipgloss.NewStyle().Background(cSelBg).Width(w - 2).Render(row)
			}
		} else {
			row = " " + row
		}
		rows = append(rows, row)
	}
	parts := []string{box("AGENTS", w, listH, m.focus == focusTasks, strings.Join(rows, "\n"))}

	claimsH := 0
	if cr := m.claimRows(w - 2); len(cr) > 0 {
		claimsH = min(len(cr)+2, 6)
		if h-listH-claimsH < 6 {
			claimsH = 0
		} else {
			parts = append(parts, box("CLAIMS", w, claimsH, false, strings.Join(cr, "\n")))
		}
	}

	peekH := h - listH - claimsH
	title := "PEEK"
	body := sDim.Render(" Select an agent to see its terminal.")
	if t, ok := m.selected(); ok {
		title = "PEEK · " + t.ID + " " + t.Title
		if i, n := m.livePos(); n > 1 {
			// Make switching discoverable where the user is looking.
			pos := fmt.Sprintf("%d/%d", i, n)
			if i == 0 {
				pos = fmt.Sprintf("%d live", n)
			}
			title = fmt.Sprintf("PEEK %s %s · %s %s", pos, m.keys.NextAgent.Help().Key, t.ID, t.Title)
		}
		if m.peek != "" {
			lines := strings.Split(m.peek, "\n")
			if n := peekH - 2; len(lines) > n && n > 0 {
				lines = lines[len(lines)-n:]
			}
			for i, l := range lines {
				lines[i] = truncate(l, w-3)
			}
			body = sText.Render(strings.Join(lines, "\n"))
		} else if t.Window == "" || t.Status == store.Landed || t.Status == store.Killed {
			body = sDim.Render(" Window closed (" + statusLabel(t.Status) + ").")
			if t.PR != "" {
				body += "\n " + sDim.Render(t.PR)
			}
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(parts, box(title, w, peekH, false, body))...)
}

// startSpawn moves to the chat with a spawn request to finish: the
// orchestrator picks claims and writes the brief.
func (m *model) startSpawn() {
	m.focus = focusChat
	m.input.Focus()
	m.input.SetValue("Spawn an agent to ")
	m.input.CursorEnd()
}

// pause interrupts the selected agent's current turn with Esc. It stops at
// its prompt until told to go on.
func (m *model) pause() tea.Cmd {
	t, ok := m.selected()
	if !ok {
		return nil
	}
	id, a := t.ID, m.app
	return func() tea.Msg {
		if err := a.SendKeys(id, "", []string{"Escape"}); err != nil {
			return flashMsg("pause: " + err.Error())
		}
		return flashMsg("paused " + id + " (Esc); message it to go on")
	}
}
