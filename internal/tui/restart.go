package tui

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// An orchestrator that exits restarts on its own (#792): an overnight run
// must not wait for someone to press ctrl+r. Restarts back off so a crash
// loop doesn't spin, and one that ran out of quota with no adapter left
// waits for the first reset.

const (
	restartBaseDefault = 2 * time.Second
	restartMax         = 5 * time.Minute
)

// restartMsg fires a scheduled restart of p; a newer schedule, or a
// process ctrl+r already replaced, makes it stale.
type restartMsg struct {
	gen int
	p   orchProc
}

// restartDelay is how long to wait before the next restart, doubling with
// each one since the orchestrator last finished a turn.
func (m *model) restartDelay() time.Duration {
	d := cmp.Or(m.restartBase, restartBaseDefault) << min(m.restarts, 20)
	if d <= 0 || d > restartMax {
		d = restartMax
	}
	m.restarts++
	return d
}

// scheduleRestart arranges for the stopped orchestrator to come back and
// says when. A known quota reset wins over a shorter backoff.
func (m *model) scheduleRestart(why string) tea.Cmd {
	delay := m.restartDelay()
	if !m.quotaReset.IsZero() {
		delay = max(delay, time.Until(m.quotaReset))
		m.quotaReset = time.Time{}
	}
	m.restartGen++
	gen, p := m.restartGen, m.proc
	m.addChat(store.ChatEvent, fmt.Sprintf("%s Saddle restarts it in %s; ctrl+r restarts it now.", why, delay.Round(time.Second)))
	return tea.Tick(delay, func(time.Time) tea.Msg { return restartMsg{gen: gen, p: p} })
}

// restart runs a scheduled restart, unless it went stale.
func (m *model) restart(msg restartMsg) tea.Cmd {
	if msg.gen != m.restartGen || m.proc != msg.p || m.orc != nil {
		return nil
	}
	if err := m.startProc(m.sessionToResume()); err != nil {
		return m.scheduleRestart("Restarting the orchestrator failed: " + err.Error() + ".")
	}
	m.addChat(store.ChatEvent, "Orchestrator restarted.")
	m.deliver()
	return m.waitEvent()
}

// noteQuotaReset records when the first orchestrator adapter that is out
// of quota comes back, for the restart after its exit.
func (m *model) noteQuotaReset() {
	var first time.Time
	for name, ex := range m.app.ExhaustedAdapters(time.Now()) {
		if !slices.Contains(app.OrchestratorAdapters, name) || ex.Until.IsZero() {
			continue
		}
		if first.IsZero() || ex.Until.Before(first) {
			first = ex.Until
		}
	}
	m.quotaReset = first
}
