package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

func withStats(m *model) *model {
	m.stats = map[string]agentStats{
		"t1": {Tokens: 1_260_000, Ctx: 84_000, CtxMax: 200_000, Activity: "Edit internal/tui/control.go"},
		"t3": {Tokens: 3_400, Ctx: 190_000, CtxMax: 200_000, Activity: "Bash go test ./..."},
	}
	return m
}

// On a wide screen each agent row carries its window, model, status, a
// context bar, tokens and what it is doing now.
func TestAgentRowsShowCtxTokensActivity(t *testing.T) {
	m := withStats(newViewModel(160, 40))
	out := m.viewLeft(110, 30)
	row := linesWith(out, "t1")[0]
	for _, want := range []string{"@1", "opus", "running", "42%", "1.3M", "Edit internal/tui/control.go"} {
		if !strings.Contains(row, want) {
			t.Errorf("t1 row lacks %q: %q", want, row)
		}
	}
	if row := linesWith(out, "t3 ")[0]; !strings.Contains(row, "95%") || !strings.Contains(row, "3.4k") {
		t.Errorf("t3 row: %q", row)
	}
	// Narrow: fewer columns, never too wide, and the title survives.
	for _, w := range []int{30, 36, 40, 60} {
		out := m.viewLeft(w, 30)
		for _, l := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(l); lw > w {
				t.Errorf("width %d: %q is %d wide", w, l, lw)
			}
		}
		if row := linesWith(out, "t1")[0]; !strings.Contains(row, "view") {
			t.Errorf("width %d: the title was squeezed out: %q", w, row)
		}
	}
}

func TestCtxBarColors(t *testing.T) {
	withColor(t)
	if s := ctxCell(agentStats{Ctx: 190_000, CtxMax: 200_000}); !strings.Contains(s, fg(cAlert)) {
		t.Errorf("a nearly full context should be red: %q", s)
	}
	if s := ctxCell(agentStats{Ctx: 20_000, CtxMax: 200_000}); strings.Contains(s, fg(cAlert)) {
		t.Errorf("a light context should not be red: %q", s)
	}
	if s := ctxCell(agentStats{}); lipgloss.Width(s) != ctxW {
		t.Errorf("an unknown context still takes its column: %q", s)
	}
}

// The claims panel lists each live task's globs, the selected task first.
func TestClaimsPanel(t *testing.T) {
	m := newViewModel(160, 40)
	m.tasks[2].Claims = []string{"go.mod", "internal/app/**"}
	m.sel = 2
	out := m.viewLeft(100, 30)
	if !strings.Contains(out, "CLAIMS") {
		t.Fatalf("no claims panel:\n%s", out)
	}
	ls := linesWith(out, "internal/app/**")
	if len(ls) != 1 || !strings.Contains(ls[0], "t3") {
		t.Errorf("t3's claims: %q", ls)
	}
	if i, j := strings.Index(out, "internal/app/**"), strings.Index(out, "internal/tui/**"); i < 0 || j < 0 || i > j {
		t.Errorf("the selected task's claims should come first:\n%s", out)
	}
	for _, w := range []int{30, 40} {
		for _, l := range strings.Split(m.viewLeft(w, 30), "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("width %d: %q too wide", w, l)
			}
		}
	}
	// No claims, no panel; a short screen leaves it out too.
	m.tasks[0].Claims, m.tasks[2].Claims = nil, nil
	if out := m.viewLeft(100, 30); strings.Contains(out, "CLAIMS") {
		t.Errorf("panel without claims:\n%s", out)
	}
}

// readStats sums each task's tokens, estimates its context from its latest
// minute, and takes its activity from its latest tool event.
func TestReadStats(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	b := func(ago time.Duration, task string, tok usage.Tokens, msgs int64) usage.Bucket {
		return usage.Bucket{Key: usage.Key{Minute: now.Add(-ago), Task: task, Model: "claude-opus-5-5"}, Tokens: tok, Messages: msgs}
	}
	if err := st.AddUsage("s1", false, []usage.Bucket{
		b(10*time.Minute, "t1", usage.Tokens{Input: 100, CacheRead: 9_000, Output: 50}, 1),
		b(2*time.Minute, "t1", usage.Tokens{Input: 200, CacheRead: 40_000, CacheCreation: 1_800, Output: 300}, 2),
	}); err != nil {
		t.Fatal(err)
	}
	st.Event("t1", "tool", "Read go.mod")
	st.Event("t1", "tool", "Edit internal/tui/control.go")
	st.Event("t2", "notification", "Claude needs your permission")
	s := readStats(&app.App{Store: st, Cfg: config.Default()}, now)
	t1 := s["t1"]
	if t1.Tokens != 100+9_000+50+200+40_000+1_800+300 {
		t.Errorf("tokens = %d", t1.Tokens)
	}
	if t1.Ctx != (200+40_000+1_800)/2 || t1.CtxMax != 200_000 {
		t.Errorf("ctx = %d/%d", t1.Ctx, t1.CtxMax)
	}
	if t1.Activity != "Edit internal/tui/control.go" {
		t.Errorf("activity = %q", t1.Activity)
	}
	if s["t2"].Activity != "waiting: Claude needs your permission" {
		t.Errorf("t2 activity = %q", s["t2"].Activity)
	}
}

// s starts a spawn request in the chat; p interrupts the selected agent.
func TestSpawnAndPauseKeys(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m := newViewModel(120, 40)
	m.app.Store = st
	m.focus = focusTasks
	m.input.Blur()
	m.Update(runeKey('s'))
	if m.focus != focusChat || !strings.HasPrefix(m.input.Value(), "Spawn an agent") {
		t.Fatalf("s: focus=%d input=%q", m.focus, m.input.Value())
	}
	m.input.Reset()
	m.focus = focusTasks
	_, c := m.Update(runeKey('p'))
	if c == nil {
		t.Fatal("p did nothing")
	}
	msg := c()
	if f, ok := msg.(flashMsg); !ok || !strings.Contains(string(f), "t1") {
		t.Errorf("p on a task saddle has no window for: %#v", msg)
	}
	if m.input.Value() != "" {
		t.Error("task keys leaked into the chat")
	}
}

// A row plus its selection marker never exceeds the list's inner width; the
// box would wrap it onto a second line.
func TestAgentRowNeverWraps(t *testing.T) {
	m := withStats(newViewModel(160, 40))
	m.tasks[0].Title = strings.Repeat("long title ", 10)
	for iw := 20; iw <= 200; iw++ {
		c := layoutCols(iw)
		for _, task := range m.tasks {
			if w := 1 + lipgloss.Width(m.agentRow(task, c)); w > iw {
				t.Fatalf("inner width %d: %s row is %d wide", iw, task.ID, w)
			}
		}
	}
}
