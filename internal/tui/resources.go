package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// verdict is the gauge's advice. The words carry it: colour only repeats them,
// so NO_COLOR still says what to do.
type verdict int

const (
	verdictOK verdict = iota
	verdictBusy
	verdictOver
)

func (v verdict) String() string {
	switch v {
	case verdictOK:
		return "OK to open another"
	case verdictBusy:
		return "Busy: wait"
	default:
		return "Overloaded: don't"
	}
}

func verdictColor(v verdict) lipgloss.Color {
	switch v {
	case verdictOK:
		return cDone
	case verdictBusy:
		return cAccent
	default:
		return cAlert
	}
}

// resourceSnap is one sample of machine pressure. Bytes are bytes.
// View renders the last one; it never samples.
type resourceSnap struct {
	Load1 float64
	CPUs  int

	MemTotal  uint64
	MemAvail  uint64
	SwapTotal uint64
	SwapFree  uint64
}

func (s resourceSnap) loadPerCore() float64 {
	if s.CPUs <= 0 {
		return 0
	}
	return s.Load1 / float64(s.CPUs)
}

func (s resourceSnap) used() uint64 {
	if s.MemAvail >= s.MemTotal {
		return 0
	}
	return s.MemTotal - s.MemAvail
}

func (s resourceSnap) availPct() float64 {
	if s.MemTotal == 0 {
		return 0
	}
	return float64(s.MemAvail) / float64(s.MemTotal) * 100
}

func (s resourceSnap) swapUsed() uint64 {
	if s.SwapFree >= s.SwapTotal {
		return 0
	}
	return s.SwapTotal - s.SwapFree
}

// judgeResources is the worse of CPU and RAM. loadPerCore is the 1-minute
// load average divided by cores. availPct is available RAM as a percent of
// total. OK is under ok_load and above ok_ram_pct. Busy runs up through
// busy_load, and down through busy_ram_pct. Past either is overloaded.
func judgeResources(loadPerCore, availPct float64, th config.TUIResources) verdict {
	return worse(levelLoad(loadPerCore, th), levelRAM(availPct, th))
}

func levelLoad(per float64, th config.TUIResources) verdict {
	switch {
	case per > th.BusyLoad:
		return verdictOver
	case per >= th.OKLoad:
		return verdictBusy
	default:
		return verdictOK
	}
}

func levelRAM(availPct float64, th config.TUIResources) verdict {
	switch {
	case availPct < th.BusyRAMPct:
		return verdictOver
	case availPct <= th.OKRAMPct:
		return verdictBusy
	default:
		return verdictOK
	}
}

func worse(a, b verdict) verdict {
	if a > b {
		return a
	}
	return b
}

// resourceWideMin is the narrowest pane that fits the two-line gauge.
// Below it, the gauge is one line so it does not wrap under the box.
const resourceWideMin = 56

// renderResources draws the gauge. narrow forces the one-line form, which
// is what a narrow terminal uses even when the pane is wide enough for two.
// The verdict is coloured; the words stay either way.
func renderResources(s resourceSnap, th config.TUIResources, agents int, heavy string, width int, narrow bool) string {
	if width < 8 || s.CPUs <= 0 || s.MemTotal == 0 {
		return ""
	}
	v := judgeResources(s.loadPerCore(), s.availPct(), th)
	if narrow || width < resourceWideMin {
		return clipWidth(renderResourceLine(s, v, agents, heavy), width)
	}
	cpu := fmt.Sprintf("CPU %s load %.1f / %d cores  %s",
		bar(clamp01(s.loadPerCore()), 10), s.Load1, s.CPUs, paintVerdict(v))
	ram := fmt.Sprintf("RAM %s %s%s",
		bar(clamp01(usedFrac(s)), 10), ramWide(s), resourceTail(agents, heavy))
	return clipWidth(cpu, width) + "\n" + clipWidth(ram, width)
}

func renderResourceLine(s resourceSnap, v verdict, agents int, heavy string) string {
	line := fmt.Sprintf("CPU %.1f/%d RAM %s/%s %s · %s",
		s.Load1, s.CPUs, gb(s.used(), 1), gb(s.MemTotal, 0), paintVerdict(v), agentWord(agents))
	if heavy != "" {
		line += " · heavy " + heavy
	}
	if sw := formatSwap(s.swapUsed()); sw != "" {
		line += " · swap " + sw
	}
	return line
}

func resourceTail(agents int, heavy string) string {
	s := "  · " + agentWord(agents)
	if heavy != "" {
		s += " · heavy " + heavy
	}
	return s
}

func ramWide(s resourceSnap) string {
	t := fmt.Sprintf("%s / %s GB free %s", gb(s.used(), 1), gb(s.MemTotal, 0), gb(s.MemAvail, 0))
	if sw := formatSwap(s.swapUsed()); sw != "" {
		t += "  swap " + sw
	}
	return t
}

func agentWord(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}

func paintVerdict(v verdict) string {
	return lipgloss.NewStyle().Foreground(verdictColor(v)).Render(v.String())
}

func usedFrac(s resourceSnap) float64 {
	if s.MemTotal == 0 {
		return 0
	}
	return float64(s.used()) / float64(s.MemTotal)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func gb(b uint64, prec int) string {
	return strconv.FormatFloat(float64(b)/(1<<30), 'f', prec, 64)
}

// formatSwap is the swap in use, or "" when none is. Small amounts stay in
// KB so a few pages don't round down to nothing.
func formatSwap(b uint64) string {
	switch {
	case b == 0:
		return ""
	case b < 1<<20:
		return fmt.Sprintf("%d KB", (b+1023)>>10)
	case b < 1<<30:
		mb := float64(b) / (1 << 20)
		if mb >= 10 {
			return fmt.Sprintf("%.0f MB", mb)
		}
		return fmt.Sprintf("%.1f MB", mb)
	default:
		return gb(b, 1) + " GB"
	}
}

func clipWidth(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// resourceState is the gauge's last sample. View only reads it. The sampler
// runs in a tea.Cmd from the tick, never from View.
type resourceState struct {
	snap    resourceSnap
	have    bool
	loading bool
	ticks   int
	read    func() (resourceSnap, error) // nil reads the host
}

type resourcesMsg struct {
	snap resourceSnap
	err  error
}

// resourcesEvery is how many one-second ticks apart the gauge samples.
const resourcesEvery = 3

func (m *model) resourcesOn() bool {
	return m.app != nil && m.app.Cfg.TUI.Resources.Enabled
}

// resourcesTick samples when it is due. The command reads off the UI
// goroutine; View does not call it.
func (m *model) resourcesTick() tea.Cmd {
	if !m.resourcesOn() {
		return nil
	}
	m.rs.ticks++
	if m.rs.ticks%resourcesEvery != 1 {
		return nil
	}
	return m.loadResources()
}

func (m *model) loadResources() tea.Cmd {
	if !m.resourcesOn() || m.rs.loading {
		return nil
	}
	m.rs.loading = true
	read := m.rs.read
	if read == nil {
		read = readHostResources
	}
	return func() tea.Msg {
		snap, err := read()
		return resourcesMsg{snap: snap, err: err}
	}
}

func (m *model) resourcesLoaded(msg resourcesMsg) {
	m.rs.loading = false
	if msg.err != nil || msg.snap.CPUs <= 0 || msg.snap.MemTotal == 0 {
		m.rs.have = false
		return
	}
	m.rs.snap, m.rs.have = msg.snap, true
}

func (m *model) resourcesView(w int) string {
	if !m.resourcesOn() || !m.rs.have {
		return ""
	}
	return renderResources(m.rs.snap, m.app.Cfg.TUI.Resources, runningAgents(m.tasks), heavyHolderNote(m.hv.v), w, m.narrow())
}

func runningAgents(tasks []mcpserver.TaskView) int {
	n := 0
	for _, t := range tasks {
		if t.Status == store.Running {
			n++
		}
	}
	return n
}

// heavyHolderNote names classes that hold a heavy-run slot, "" when none do.
func heavyHolderNote(v *app.HeavyRuns) string {
	if v == nil {
		return ""
	}
	var parts []string
	for _, c := range v.Classes {
		n := len(c.Holders)
		switch n {
		case 0:
			continue
		case 1:
			parts = append(parts, c.Class)
		default:
			parts = append(parts, fmt.Sprintf("%s ×%d", c.Class, n))
		}
	}
	return strings.Join(parts, ", ")
}
