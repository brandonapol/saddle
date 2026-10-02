package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/triage"
	"github.com/brandonapol/saddle/internal/usage"
)

func estimate(fiveHour, weekly int64, l usage.Limits) usage.LimitEstimate {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	bs := []usage.Bucket{
		{Key: usage.Key{Minute: now.Add(-time.Hour), Model: "claude-opus-5-5"}, Tokens: usage.Tokens{Input: fiveHour}, Messages: 1},
		{Key: usage.Key{Minute: now.Add(-48 * time.Hour), Model: "claude-opus-5-5"}, Tokens: usage.Tokens{Input: weekly - fiveHour}, Messages: 1},
	}
	return usage.EstimateBuckets(bs, l, now)
}

func TestUsageStripShowsEachWindowAgainstItsCap(t *testing.T) {
	l := usage.Limits{FiveHour: usage.Cap{Tokens: 1000}, Weekly: usage.Cap{Tokens: 10000}}
	got := usageStrip(estimate(500, 9000, l), 160)
	for _, want := range []string{"5h", "50%", "weekly", "90%", "warn", "resets"} {
		if !strings.Contains(got, want) {
			t.Errorf("strip %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "over") {
		t.Errorf("no window is over: %q", got)
	}
	got = usageStrip(estimate(1500, 1500, l), 160)
	if !strings.Contains(got, "150%") || !strings.Contains(got, "over") {
		t.Errorf("over strip = %q", got)
	}
	if w := lipgloss.Width(usageStrip(estimate(1500, 1500, l), 40)); w > 40 {
		t.Errorf("strip is %d wide in 40 columns", w)
	}
}

// With no caps the strip still shows what each window used, without a bar.
func TestUsageStripUnlimitedShowsTotals(t *testing.T) {
	got := usageStrip(estimate(1_500_000, 2_000_000, usage.Limits{}), 160)
	if !strings.Contains(got, "1.5M tok") || !strings.Contains(got, "2.0M tok") || strings.Contains(got, "%") {
		t.Errorf("unlimited strip = %q", got)
	}
}

func TestUsageStateStyles(t *testing.T) {
	if stateColor(usage.OK) != cDone || stateColor(usage.Warn) != cAccent || stateColor(usage.Over) != cAlert {
		t.Fatal("ok/warn/over must render done/accent/alert")
	}
}

// The strip sits above the shortcuts once a refresh brings an estimate, and
// stays hidden while there is nothing to show.
func TestFooterCarriesUsageStrip(t *testing.T) {
	m := &model{app: &app.App{Cfg: config.Default()}, width: 160, height: 40, keys: newKeyMap(), input: textarea.New(), vp: viewport.New(40, 10)}
	if strings.Contains(m.viewFooter(), "5h") {
		t.Fatal("strip shown before any estimate")
	}
	empty := estimate(0, 0, usage.Limits{})
	m.Update(refreshMsg{limits: &empty})
	if lipgloss.Height(m.viewFooter()) != 1 {
		t.Fatal("strip shown with no usage and no caps")
	}
	e := estimate(500, 900, usage.Limits{FiveHour: usage.Cap{Tokens: 1000}})
	m.Update(refreshMsg{limits: &e})
	f := m.viewFooter()
	if lipgloss.Height(f) != 2 || !strings.Contains(f, "5h") {
		t.Fatalf("footer = %q", f)
	}
}

// Narrator lines land in the chat thread and the store; needs-you lines keep
// the ‼ marker and render as alerts.
func TestNarratorLinesJoinTheThread(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m := &model{app: &app.App{Store: st, Cfg: config.Default()}}
	m.Update(narrMsg{narrator.Line{Task: "t1", Text: "landed the login page"}})
	m.Update(narrMsg{narrator.Line{Task: "t2", Text: "waiting on Bash permission", NeedsYou: true}})
	if len(m.chat) != 2 || m.chat[0].text != "t1: landed the login page" || m.chat[1].text != "‼ t2: waiting on Bash permission" {
		t.Fatalf("chat = %+v", m.chat)
	}
	hist, _ := st.Chat(10)
	if len(hist) != 2 || hist[1].Role != store.ChatNarrator {
		t.Fatalf("stored chat = %+v", hist)
	}
	wrap := lipgloss.NewStyle().Width(60)
	if got := renderLine(m.chat[1], 62, wrap); !strings.Contains(got, "‼ t2: waiting on Bash permission") {
		t.Fatalf("needs-you render = %q", got)
	}
	if !narratorNeedsYou(m.chat[1]) || narratorNeedsYou(m.chat[0]) {
		t.Fatal("needs-you detection")
	}
}

func TestNarratorSinkNeverBlocks(t *testing.T) {
	s := make(narrSink, 1)
	s.Emit(narrator.Line{Text: "a"})
	s.Emit(narrator.Line{Text: "b"}) // full: dropped, not blocked
	if l := <-s; l.Text != "a" {
		t.Fatalf("got %+v", l)
	}
}

// On a narrow terminal the footer stays two lines, usage over shortcuts, and
// the agent-switch keys survive the cut.
func TestFooterNarrowKeepsSwitchKeys(t *testing.T) {
	e := estimate(500, 900, usage.Limits{FiveHour: usage.Cap{Tokens: 1000}})
	for _, w := range []int{40, 60} {
		m := &model{app: &app.App{Cfg: config.Default()}, width: w, keys: newKeyMap(), prefix: "C-b", focus: focusTasks, limits: &e}
		f := m.viewFooter()
		lines := strings.Split(f, "\n")
		if len(lines) != 2 || !strings.Contains(lines[0], "5h") || !strings.Contains(lines[1], "alt+n/p") {
			t.Errorf("width %d: footer = %q", w, f)
		}
		for _, l := range lines {
			if lipgloss.Width(l) > w {
				t.Errorf("width %d: footer line %q too wide", w, l)
			}
		}
	}
}

// An orchestrator reply that leads with the urgent marker is urgent at once,
// without waiting on Jev, and keeps its marker in the store so it renders the
// same after a restart.
func TestOrchestratorUrgentMarker(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m := &model{app: &app.App{Store: st, Cfg: config.Default()}, jev: &triage.Client{}, eventTurn: true}
	if cmd := m.handleEvent(orch.Event{Kind: orch.Text, Text: "‼ t3 is blocked on a conflict. Details."}); cmd != nil {
		t.Error("a marked reply should not wait on Jev triage")
	}
	if c := m.chat[len(m.chat)-1]; c.attn != attnUrgent {
		t.Errorf("marked reply attn = %d, want urgent", c.attn)
	}
	if cmd := m.handleEvent(orch.Event{Kind: orch.Text, Text: "All good."}); cmd == nil {
		t.Error("an unmarked event reply should still go to Jev")
	}
	hist, _ := st.Chat(10)
	if len(hist) < 1 || !strings.HasPrefix(hist[0].Text, "‼ ") {
		t.Errorf("stored history = %+v", hist)
	}
}
