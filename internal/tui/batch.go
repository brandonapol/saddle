package tui

import (
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
)

// Batch mode (#913, ADR 0011): every landed task is one layer of one linear
// stack, and auto-merge fast-forwards base to the highest green layer. One
// key flips [train] output between "batch" and "per-task" (individual PRs)
// from any view, the way infinite mode flips; it is saved in
// .saddle/config.toml, so it outlives the TUI. The header shows it while on.

// batchKeyEnv rebinds the batch key: comma-separated key names, the first
// shown in help.
const batchKeyEnv = "SADDLE_KEY_BATCH"

func batchBinding() key.Binding {
	keys := []string{"alt+b"}
	if v := strings.TrimSpace(os.Getenv(batchKeyEnv)); v != "" {
		keys = nil
		for k := range strings.SplitSeq(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(keys[0], "batch stack / individual PRs"))
}

// batcher is what the batch key does: the App calls behind `saddle config
// set train.output batch|per-task`.
type batcher interface {
	BatchMode() bool
	SetConfig(key, value string) (string, error)
}

func (m *model) batcher() batcher {
	if m.bter != nil {
		return m.bter
	}
	return m.app
}

// batchDoneMsg is the batch toggle landing.
type batchDoneMsg struct {
	text string
	on   bool
	amOn bool
}

func (m *model) batchOn() bool {
	if m.bter == nil && m.app == nil {
		return false
	}
	return m.batcher().BatchMode()
}

// toggleBatch flips batch mode off the UI goroutine. Turning it on turns
// auto-merge on as well, since a batch only merges while auto-merge runs;
// turning it off leaves auto-merge as it is.
func (m *model) toggleBatch() tea.Cmd {
	if m.btBusy {
		return flashCmd("still switching batch mode")
	}
	on := !m.batchOn()
	wantAm := on && (m.am == nil || !m.am.Enabled)
	m.btBusy = true
	x, y := m.batcher(), m.automerger()
	return func() tea.Msg {
		value := "per-task"
		if on {
			value = app.OutputBatch
		}
		if _, err := x.SetConfig("train.output", value); err != nil {
			return batchDoneMsg{text: "batch mode: " + err.Error(), on: !on}
		}
		if !on {
			return batchDoneMsg{text: "batch mode off: each task gets its own PR on base, stacked only on a real dependency"}
		}
		msg := batchDoneMsg{text: "batch mode on: one linear stack, merged by fast-forwarding base to its highest green layer", on: true}
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

func (m *model) batchDone(msg batchDoneMsg) tea.Cmd {
	m.btBusy = false
	if msg.amOn {
		m.applyAm(func(st *automerge.Status) { st.Enabled = true })
	}
	m.flash, m.flashAt = msg.text, time.Now()
	return m.refresh()
}

// btHeader is the header's batch indicator, long and short; both empty while
// batch mode is off.
func (m *model) btHeader() (long, short string) {
	if !m.batchOn() {
		return "", ""
	}
	return "▤ batch → " + m.baseName(), "▤ batch"
}

func (m *model) baseName() string {
	if m.app == nil {
		return "main"
	}
	return m.app.Cfg.Base
}
