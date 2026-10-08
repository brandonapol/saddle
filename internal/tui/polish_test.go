package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// stubProc is an orchestrator whose busy state the test sets.
type stubProc struct {
	busy, interrupting bool
	sent               []string
	events             chan orch.Event
}

func (p *stubProc) Busy() bool                { return p.busy }
func (p *stubProc) Send(text string) error    { p.sent = append(p.sent, text); p.busy = true; return nil }
func (p *stubProc) Events() <-chan orch.Event { return p.events }
func (p *stubProc) Interrupt() error          { p.interrupting = true; return nil }
func (p *stubProc) Interrupting() bool        { return p.interrupting }
func (p *stubProc) Close()                    {}

// P1: an error the orchestrator also said as text shows once.
func TestErrorShowsOnce(t *testing.T) {
	m := slashModel(t)
	m.proc = &stubProc{}
	m.handleEvent(orch.Event{Kind: orch.Text, Text: "API Error: 500 upstream down"})
	m.handleEvent(orch.Event{Kind: orch.Error, Text: "API Error: 500 upstream down"})
	m.renderChat()
	if n := strings.Count(m.vp.View(), "upstream down"); n != 1 {
		t.Fatalf("the error shows %d times:\n%s", n, m.vp.View())
	}
	if !strings.Contains(m.vp.View(), "Orchestrator error") {
		t.Fatalf("the error isn't marked as one:\n%s", m.vp.View())
	}
	// An error with no matching text still shows.
	m.handleEvent(orch.Event{Kind: orch.Error, Text: "rate limited"})
	m.renderChat()
	if !strings.Contains(m.vp.View(), "Orchestrator error: rate limited") {
		t.Fatalf("a bare error was lost:\n%s", m.vp.View())
	}
}

// P2: at 80 columns the header keeps what needs attention and the
// orchestrator's state, and drops the session and branch names first.
func TestNarrowHeaderKeepsImportantStatus(t *testing.T) {
	m := newViewModel(80, 24)
	m.proc = &stubProc{busy: true}
	m.tasks = append(m.tasks, mcpserver.TaskView{ID: "t5", Status: store.NeedsYou, Window: "@5"})
	h := m.viewHeader()
	for _, want := range []string{"▲ 2 need you", "working"} {
		if !strings.Contains(h, want) {
			t.Errorf("80-column header lacks %q: %q", want, h)
		}
	}
	if w := lipgloss.Width(h); w > 80 {
		t.Errorf("header is %d wide", w)
	}
	m.app.Cfg.Session = "saddle-repo"
	if wide := newViewModel(200, 40).viewHeader(); !strings.Contains(wide, "saddle-repo") || !strings.Contains(wide, "main → saddle/integration") {
		t.Errorf("a wide header lost the session or branches: %q", wide)
	}
}

// P2: auto-merge never shows as the cryptic "am off".
func TestHeaderAutoMergeNotCryptic(t *testing.T) {
	m := newViewModel(80, 24)
	m.am = &automerge.Status{}
	for _, w := range []int{60, 80, 100, 160} {
		m.width = w
		if h := m.viewHeader(); strings.Contains(h, "am off") || strings.Contains(h, "am on") {
			t.Errorf("width %d: %q", w, h)
		}
	}
}

// P2: on a narrow screen the chat says how to reach the hidden agents.
func TestNarrowChatHintsAtAgents(t *testing.T) {
	m := newViewModel(60, 20)
	m.focus = focusChat
	if v := m.View(); !strings.Contains(v, "tab: 4 agents") {
		t.Fatalf("no hint at the hidden agent list:\n%s", v)
	}
}

// P3: the claims box says how many rows it couldn't show.
func TestClaimsOverflowShowsMore(t *testing.T) {
	m := newViewModel(160, 40)
	m.tasks = nil
	for i := 1; i <= 7; i++ {
		m.tasks = append(m.tasks, mcpserver.TaskView{ID: fmt.Sprintf("t%d", i), Status: store.Running, Window: "@1", Claims: []string{fmt.Sprintf("dir%d/**", i)}})
	}
	out := m.viewLeft(100, 36)
	if !strings.Contains(out, "+4 more") {
		t.Fatalf("no overflow count:\n%s", out)
	}
}

// P4: agents with the same status sort by number: t8, t9, t10.
func TestSortTasksNumericIDs(t *testing.T) {
	ts := []mcpserver.TaskView{
		{ID: "t10", Status: store.Running}, {ID: "t8", Status: store.Running},
		{ID: "t9", Status: store.Running}, {ID: "t2", Status: store.NeedsYou},
	}
	sortTasks(ts)
	var got []string
	for _, t := range ts {
		got = append(got, t.ID)
	}
	if strings.Join(got, ",") != "t2,t8,t9,t10" {
		t.Fatalf("order = %v", got)
	}
}

// P5: idle agents aren't "need attention"; they count apart.
func TestIdleIsNotNeedAttention(t *testing.T) {
	m := newViewModel(200, 40)
	m.tasks = []mcpserver.TaskView{{ID: "t1", Status: store.Idle}, {ID: "t2", Status: store.Idle}}
	h := m.viewHeader()
	if strings.Contains(h, "need") || !strings.Contains(h, "2 idle") {
		t.Fatalf("header = %q", h)
	}
}

// P5: Claude Code's notification texts read short in the status column.
func TestActivityShortensNotifications(t *testing.T) {
	cases := map[string]string{
		"Claude is waiting for your input":         "waiting for input",
		"Claude needs your permission to use Bash": "needs permission: Bash",
		"something else":                           "waiting: something else",
	}
	for in, want := range cases {
		if got, _ := activity(store.Event{Kind: "notification", Data: in}); got != want {
			t.Errorf("activity(%q) = %q, want %q", in, got, want)
		}
	}
}

// P6: the help overlay lists each binding once and says where / goes.
func TestHelpOverlayNoDuplicates(t *testing.T) {
	m := newViewModel(200, 60)
	v := m.viewHelp(200, 60)
	for _, l := range strings.Split(v, "\n") {
		f := strings.Fields(strings.Trim(l, "│ "))
		for i, k := range f {
			// A pair's second key on a row of its own.
			if slices.Contains([]string{"alt+p", "alt+2", "alt+3", "K", "-", "[", "<", "ctrl+u", "ctrl+pgdn", "k"}, k) && (i == 0 || strings.HasSuffix(f[i-1], "…")) {
				t.Errorf("help lists a pair's second key alone: %q", l)
			}
		}
	}
	for _, want := range []string{"alt+n/p", "alt+1/2/3", "orchestrator's skills", "skill in that agent"} {
		if !strings.Contains(v, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// P7: a message sent while the orchestrator works waits in the TUI,
// marked queued, and enters the conversation when its turn comes, so each
// reply follows its message.
func TestQueuedMessagesSendInOrder(t *testing.T) {
	m := slashModel(t)
	p := &stubProc{}
	m.proc = p
	m.sendUser("first")
	m.sendUser("second")
	if len(p.sent) != 1 {
		t.Fatalf("sent while busy: %q", p.sent)
	}
	m.renderChat()
	if v := m.vp.View(); !strings.Contains(v, "queued") || !strings.Contains(v, "second") {
		t.Fatalf("the queued message isn't shown as queued:\n%s", v)
	}
	m.handleEvent(orch.Event{Kind: orch.Text, Text: "reply one"})
	p.busy = false
	m.handleEvent(orch.Event{Kind: orch.Result})
	if len(p.sent) != 2 || p.sent[1] != "second" {
		t.Fatalf("after the turn: sent %q", p.sent)
	}
	var order []string
	for _, c := range m.chat {
		if c.role == store.ChatUser || c.role == store.ChatAssistant {
			order = append(order, c.text)
		}
	}
	if got := strings.Join(order, "|"); !strings.HasSuffix(got, "first|reply one|second") {
		t.Fatalf("chat order = %s", got)
	}
}

// P8: accepting a completion clears a stale completion flash.
func TestCompletionClearsFlash(t *testing.T) {
	m := slashModel(t)
	typeText(m, "/zzz")
	if c, ok := m.completeInput(); ok && c != nil {
		m.Update(c())
	}
	if m.flash == "" {
		t.Fatal("no flash for a failed completion")
	}
	m.input.SetValue("/bud")
	m.completeInput()
	if m.flash != "" {
		t.Fatalf("flash lingers after a completion: %q", m.flash)
	}
}

// P9: the focused pane is marked without color.
func TestFocusMarkedWithoutColor(t *testing.T) {
	m := newViewModel(160, 40)
	m.focus = focusTasks
	v := m.View()
	if !strings.Contains(v, "▶ AGENTS") || strings.Contains(v, "▶ ORCHESTRATOR") {
		t.Fatalf("agents focus isn't marked:\n%s", v)
	}
	m.focus = focusChat
	v = m.View()
	if strings.Contains(v, "▶ AGENTS") || !strings.Contains(v, "▶ ORCHESTRATOR") {
		t.Fatalf("chat focus isn't marked:\n%s", v)
	}
}

// P10: s opens a spawn prompt; enter spawns from it, not via a prefilled
// chat message.
func TestSpawnKeyOpensSpawnPrompt(t *testing.T) {
	m := slashModel(t)
	var got []string
	m.spawner = func(title, prompt string) (string, error) { got = append(got, title, prompt); return "t9", nil }
	m.focus = focusTasks
	m.key(runeKey('s'))
	if m.focus != focusChat || !strings.Contains(m.input.Prompt, "spawn") || m.input.Value() != "" {
		t.Fatalf("s didn't open a spawn prompt: focus=%d prompt=%q value=%q", m.focus, m.input.Prompt, m.input.Value())
	}
	typeText(m, "fix the flaky test\nin internal/x")
	c, _ := m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if c != nil {
		m.Update(c())
	}
	if len(got) != 2 || got[0] != "fix the flaky test" || !strings.Contains(got[1], "internal/x") {
		t.Fatalf("spawned %q", got)
	}
	if m.target != "" || m.input.Prompt != "› " {
		t.Fatal("the input stayed aimed at spawn")
	}
}
