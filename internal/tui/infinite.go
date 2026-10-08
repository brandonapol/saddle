package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/autopilot"
)

// Infinite mode (#285) is autopilot with no stop condition: it keeps topping
// up from the ready queue, asks the orchestrator for more work when the
// queue is empty, and parks at the plan limit until the reset. One key
// flips it from any view; the header shows it while it is on.

// infiniteKeyEnv rebinds the infinite-mode key: comma-separated key names,
// the first shown in help.
const infiniteKeyEnv = "SADDLE_KEY_INFINITE"

func infiniteBinding() key.Binding {
	keys := []string{"alt+i"}
	if v := strings.TrimSpace(os.Getenv(infiniteKeyEnv)); v != "" {
		keys = nil
		for k := range strings.SplitSeq(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(keys[0], "infinite mode on/off"))
}

// infiniter is what the infinite-mode key does: the App call behind `saddle
// autopilot infinite on|off`.
type infiniter interface {
	SetInfinite(on bool) (autopilot.State, error)
}

type appInfiniter struct{ a *app.App }

func (x appInfiniter) SetInfinite(on bool) (autopilot.State, error) {
	return x.a.NewAutopilot(nil).SetInfinite(on)
}

func (m *model) infiniter() infiniter {
	if m.aper != nil {
		return m.aper
	}
	return appInfiniter{m.app}
}

// apDoneMsg is the infinite-mode toggle landing: what to flash, the state it
// saved, and whether it turned auto-merge on too.
type apDoneMsg struct {
	text string
	st   *autopilot.State
	amOn bool
}

func (m *model) infiniteOn() bool { return m.ap != nil && m.ap.On && m.ap.Infinite }

// toggleInfinite flips infinite mode off the UI goroutine. Turning it on
// turns auto-merge on as well, so the pipeline merges without anyone;
// turning it off leaves auto-merge as it is.
func (m *model) toggleInfinite() tea.Cmd {
	if m.apBusy {
		return flashCmd("still switching infinite mode")
	}
	on := !m.infiniteOn()
	wantAm := on && (m.am == nil || !m.am.Enabled || m.am.Stopped != "")
	m.apBusy = true
	x, y := m.infiniter(), m.automerger()
	return func() tea.Msg {
		st, err := x.SetInfinite(on)
		if err != nil {
			return apDoneMsg{text: "infinite mode: " + err.Error()}
		}
		if !on {
			return apDoneMsg{text: "infinite mode off; running tasks go on", st: &st}
		}
		msg := apDoneMsg{text: "infinite mode on: autopilot tops up from " + st.Label() + " issues until the plan limit", st: &st}
		if wantAm {
			if err := y.SetAutomerge(true); err != nil {
				msg.text += "; auto-merge: " + err.Error()
			} else {
				msg.text, msg.amOn = msg.text+"; auto-merge on", true
			}
		}
		return msg
	}
}

func (m *model) infiniteDone(msg apDoneMsg) tea.Cmd {
	m.apBusy = false
	if msg.st != nil {
		m.ap = msg.st
		m.apGen++
	}
	if msg.amOn {
		m.applyAm(func(st *automerge.Status) { st.Enabled, st.Stopped = true, "" })
	}
	m.flash, m.flashAt = msg.text, time.Now()
	return m.refresh()
}

// apHeader is the header's infinite-mode indicator, long and short; both
// are empty while it is off.
func (m *model) apHeader() (long, short string) {
	if !m.infiniteOn() {
		return "", ""
	}
	st := m.ap
	switch {
	case st.Paused:
		return "∞ paused", "∞ paused"
	case !st.SleepUntil.IsZero():
		long = "∞ parked until " + st.SleepUntil.Local().Format("15:04")
		if n := len(st.Parked); n > 0 {
			long += " (" + plural(n, "task") + ")"
		}
		return long, "∞ parked"
	}
	long = "∞ on"
	if l := m.limits; l != nil && !l.Weekly.Unlimited {
		long += fmt.Sprintf(" · %.0f%% wk left", max(0, 1-l.Weekly.Percent)*100)
	}
	return long, "∞ on"
}
