package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
)

// heavyState is the machine's heavy-run queue (#242): a segment in the
// header and the runs view, which lists every holder and waiter and kills
// a lease after asking.
type heavyState struct {
	v       *app.HeavyRuns // as last read; nil until read
	err     string         // the last read's error, shown in the runs view
	sel     string         // the selected lease
	confirm string         // the lease x asked to kill, waiting on y; "" when not asking
	busy    bool           // a kill is running
	loading bool
	ticks   int

	// Hooks; nil means the app's.
	read func() (app.HeavyRuns, error)
	kill func(lease string) error
}

type (
	heavyMsg struct {
		v   app.HeavyRuns
		err error
	}
	// heavyDoneMsg is a kill landing: what to flash.
	heavyDoneMsg string
)

// heavyEvery is how many ticks apart the queue is read outside the runs
// view; in it, every tick.
const heavyEvery = 3

// heavyTick reads the queue when it is due.
func (m *model) heavyTick() tea.Cmd {
	m.hv.ticks++
	if m.view != viewRuns && m.hv.ticks%heavyEvery != 1 {
		return nil
	}
	return m.loadHeavy()
}

// loadHeavy reads the queue off the UI goroutine.
func (m *model) loadHeavy() tea.Cmd {
	if m.hv.loading {
		return nil
	}
	m.hv.loading = true
	read := m.hv.read
	if read == nil {
		read = m.app.HeavyRuns
	}
	return func() tea.Msg {
		v, err := read()
		return heavyMsg{v: v, err: err}
	}
}

func (m *model) heavyLoaded(msg heavyMsg) {
	m.hv.loading = false
	if msg.err != nil {
		m.hv.err = msg.err.Error()
		return
	}
	m.hv.v, m.hv.err = &msg.v, ""
	rows := m.heavyRows()
	if len(rows) > 0 && !slices.ContainsFunc(rows, func(r heavyRow) bool { return r.e.Lease == m.hv.sel }) {
		m.hv.sel = rows[0].e.Lease
	}
}

// heavyRow is one lease in the runs view's order: each busy class's
// holders, then its waiters.
type heavyRow struct {
	class  app.HeavyClass
	e      app.HeavyEntry
	holder bool
}

func (m *model) heavyRows() []heavyRow {
	if m.hv.v == nil {
		return nil
	}
	var rows []heavyRow
	for _, c := range m.hv.v.Classes {
		for _, h := range c.Holders {
			rows = append(rows, heavyRow{class: c, e: h, holder: true})
		}
		for _, w := range c.Waiters {
			rows = append(rows, heavyRow{class: c, e: w})
		}
	}
	return rows
}

func (m *model) heavySelected() (heavyRow, bool) {
	for _, r := range m.heavyRows() {
		if r.e.Lease == m.hv.sel {
			return r, true
		}
	}
	return heavyRow{}, false
}

func (m *model) moveHeavySel(dir int) {
	rows := m.heavyRows()
	if len(rows) == 0 {
		return
	}
	i := slices.IndexFunc(rows, func(r heavyRow) bool { return r.e.Lease == m.hv.sel })
	m.hv.sel = rows[min(max(i+dir, 0), len(rows)-1)].e.Lease
}

func (m *model) heavyRepo() string {
	if m.hv.v == nil {
		return ""
	}
	return m.hv.v.Repo
}

// heavyConfirmKey answers the kill prompt: y kills the lease, any other key
// keeps it. Every key is swallowed while asking.
func (m *model) heavyConfirmKey(k tea.KeyMsg) tea.Cmd {
	lease := m.hv.confirm
	m.hv.confirm = ""
	if !key.Matches(k, m.keys.Confirm) {
		return flashCmd("kept lease " + shortLease(lease))
	}
	if m.hv.busy {
		return flashCmd("still killing a lease")
	}
	m.hv.busy = true
	m.flash, m.flashAt = "killing lease "+shortLease(lease)+"…", time.Now()
	kill := m.hv.kill
	if kill == nil {
		kill = m.app.KillHeavyLease
	}
	return func() tea.Msg {
		if err := kill(lease); err != nil {
			return heavyDoneMsg("killing lease " + shortLease(lease) + ": " + err.Error())
		}
		return heavyDoneMsg("killed lease " + shortLease(lease) + "; its command keeps running without a slot")
	}
}

// heavyKey handles the runs view's keys; ok is false for any other key.
func (m *model) heavyKey(k tea.KeyMsg) (tea.Cmd, bool) {
	keys := m.keys
	switch {
	case key.Matches(k, keys.Down):
		m.moveHeavySel(1)
		return nil, true
	case key.Matches(k, keys.Up):
		m.moveHeavySel(-1)
		return nil, true
	case key.Matches(k, keys.KillLease):
		r, ok := m.heavySelected()
		if !ok {
			return flashCmd("no heavy run to kill"), true
		}
		m.hv.confirm = r.e.Lease
		return nil, true
	}
	return nil, false
}

// heavyPrompt is the footer while a kill waits on y.
func (m *model) heavyPrompt() string {
	who, cmd := "", ""
	for _, r := range m.heavyRows() {
		if r.e.Lease == m.hv.confirm {
			who, cmd = r.e.Who(m.heavyRepo()), r.e.Cmd
		}
	}
	return fmt.Sprintf("Kill lease %s (%s, %s)? y kills it, any other key keeps it", shortLease(m.hv.confirm), who, truncate(cmd, 30))
}

// heavySegments are the header's heavy-run parts, one per busy class:
// "go-test ▸t83 3m · 2 waiting", alert-colored when a holder is overdue.
func (m *model) heavySegments() []string {
	if m.hv.v == nil {
		return nil
	}
	var out []string
	for _, c := range m.hv.v.Classes {
		if !c.Busy() {
			continue
		}
		color := cAccent
		if slices.ContainsFunc(c.Holders, func(e app.HeavyEntry) bool { return e.Overdue }) {
			color = cAlert
		}
		out = append(out, lipgloss.NewStyle().Foreground(color).Render(c.Segment(m.hv.v.Repo)))
	}
	return out
}

// viewRuns is the runs view: per busy class its slots, then the holders
// and the waiters in grant order, the selected lease marked.
func (m *model) viewRuns(w, h int) string {
	title := "HEAVY RUNS"
	var rows []string
	switch {
	case m.hv.err != "" && m.hv.v == nil:
		rows = append(rows, lipgloss.NewStyle().Foreground(cAlert).Render("can't read the heavy-run queue: "+m.hv.err))
	case m.hv.v == nil:
		rows = append(rows, sDim.Render("Reading the heavy-run queue…"))
	case !m.hv.v.Busy():
		title += " · " + m.hv.v.Mode
		rows = append(rows, sDim.Render("Nothing is running or waiting. saddle run queues CPU-heavy commands here."))
	default:
		title += " · " + m.hv.v.Mode
		repo := m.hv.v.Repo
		color := func(c lipgloss.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }
		for _, c := range m.hv.v.Classes {
			if !c.Busy() {
				continue
			}
			head := fmt.Sprintf("%d/%d slots busy", len(c.Holders), c.Slots)
			if n := len(c.Waiters); n > 0 {
				head += fmt.Sprintf(" · %d waiting", n)
			}
			head += " · max_run " + app.RoughDuration(c.MaxRun())
			if c.Drained {
				head += " · " + color(cAlert, "drained")
			}
			if len(rows) > 0 {
				rows = append(rows, "")
			}
			rows = append(rows, sBright.Render(c.Class)+"  "+sDim.Render(head))
			mark := func(e app.HeavyEntry) string {
				if e.Lease == m.hv.sel {
					return sKey.Render("› ")
				}
				return "  "
			}
			for _, e := range c.Holders {
				age := app.RoughDuration(e.Age())
				if e.Overdue {
					age = color(cAlert, age+" overdue")
				}
				rows = append(rows, mark(e)+color(cRun, "▸ "+e.Who(repo))+"  "+e.Cmd+"  "+age+sDim.Render("  lease "+shortLease(e.Lease)))
			}
			for _, e := range c.Waiters {
				wait := "waiting " + app.RoughDuration(e.Age())
				if e.ETAMS > 0 {
					wait += " · ~" + app.RoughDuration(e.ETA()) + " to go"
				}
				rows = append(rows, mark(e)+fmt.Sprintf("#%d %s", e.Position, e.Who(repo))+"  "+e.Cmd+"  "+sDim.Render(wait+"  lease "+shortLease(e.Lease)))
			}
		}
		if m.hv.err != "" {
			rows = append(rows, "", color(cAlert, "last read failed: "+m.hv.err))
		}
	}
	return box(title, w, h, false, clip(strings.Join(rows, "\n"), w-2))
}

// shortLease is the first 8 characters of a lease token, enough for saddle
// runq kill.
func shortLease(token string) string {
	if len(token) > 8 {
		return token[:8]
	}
	return token
}
