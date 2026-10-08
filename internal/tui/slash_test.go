package tui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// slashModel is a control-view model with a store and the commands a fake
// session reported.
func slashModel(t *testing.T) *model {
	t.Helper()
	m := newViewModel(120, 40)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m.app.Store = st
	m.focus = focusChat
	m.launch.Deny = agent.OrchestratorDeny()
	m.handleEvent(orch.Event{Kind: orch.Commands, Commands: []orch.Command{
		{Name: "budget", Description: "Check how much budget is left", Skill: true},
		{Name: "loop", Description: "Run a prompt on an interval", ArgHint: "[interval] <prompt>", Skill: true},
		{Name: "saddle:status", Description: "Show saddle's agents", Skill: true},
	}})
	return m
}

// fakeProc starts a stand-in orchestrator that records its stdin.
func fakeProc(t *testing.T, m *model) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "stdin")
	p, err := orch.Start(exec.Command("sh", "-c", "cat > "+out))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	m.proc = p
	return out
}

// sentUser waits for the user messages the fake orchestrator received.
func sentUser(t *testing.T, path string, n int) []string {
	t.Helper()
	var got []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(path)
		got = nil
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var v struct {
				Type    string `json:"type"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(l), &v) == nil && v.Type == "user" {
				got = append(got, v.Message.Content)
			}
		}
		if len(got) >= n {
			break
		}
	}
	return got
}

func TestSlashOpensAutocompleteFromSessionCommands(t *testing.T) {
	m := slashModel(t)
	typeText(m, "/")
	v := m.View()
	for _, want := range []string{"/budget", "Check how much budget is left", "/loop", "[interval] <prompt>", "/saddle:status", "/help"} {
		if !strings.Contains(v, want) {
			t.Errorf("autocomplete is missing %q:\n%s", want, v)
		}
	}
	typeText(m, "lo")
	if v := m.View(); !strings.Contains(v, "/loop") || strings.Contains(v, "/budget") {
		t.Errorf("/lo should list only loop:\n%s", v)
	}
	press(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.input.Value(); got != "/loop " {
		t.Fatalf("tab completed to %q", got)
	}
	if v := m.View(); strings.Contains(v, "Run a prompt on an interval") {
		t.Errorf("the list should close once args start:\n%s", v)
	}
}

func TestSlashAutocompletePicksWithArrows(t *testing.T) {
	m := slashModel(t)
	typeText(m, "/")
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, tea.KeyMsg{Type: tea.KeyUp})
	press(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.input.Value(); got != "/help " {
		t.Fatalf("down, down, up, tab = %q, want the second entry", got)
	}
	// Plugin skills complete on the part after "plugin:".
	m.input.Reset()
	typeText(m, "/sta")
	press(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.input.Value(); got != "/saddle:status " {
		t.Fatalf("/sta tab = %q", got)
	}
}

// Before the session reports its commands (or under grok), the list comes
// from the skill directories on disk.
func TestSlashAutocompleteFallsBackToDisk(t *testing.T) {
	m := newViewModel(120, 40)
	m.focus = focusChat
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills", "deploy", "SKILL.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	typeText(m, "/dep")
	if v := m.View(); !strings.Contains(v, "/deploy") {
		t.Errorf("fallback list is missing a user skill:\n%s", v)
	}
}

func TestSlashSkillGoesToOrchestratorAsTyped(t *testing.T) {
	m := slashModel(t)
	stdin := fakeProc(t, m)
	typeText(m, "/budget this week")
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := sentUser(t, stdin, 1); len(got) != 1 || got[0] != "/budget this week" {
		t.Fatalf("orchestrator got %q", got)
	}
}

// /help belongs to the TUI: it opens the key overlay and never reaches the
// orchestrator.
func TestTUIOwnedSlashCommandStaysLocal(t *testing.T) {
	m := slashModel(t)
	stdin := fakeProc(t, m)
	typeText(m, "/help")
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.helpOpen {
		t.Fatal("/help should open the help overlay")
	}
	if m.input.Value() != "" {
		t.Fatalf("input = %q", m.input.Value())
	}
	if v := m.View(); !strings.Contains(v, "skills & commands") {
		t.Errorf("help overlay should mention / completion:\n%s", v)
	}
	press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m.focus = focusChat
	typeText(m, "/budget")
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := sentUser(t, stdin, 1); len(got) != 1 || got[0] != "/budget" {
		t.Fatalf("orchestrator got %q; /help must not be sent", got)
	}
}

// A bundled skill that needs tools the orchestrator doesn't have says so
// before it runs, and points at running it in a worker.
func TestSlashWarnsWhenSkillNeedsDeniedTools(t *testing.T) {
	m := slashModel(t)
	stdin := fakeProc(t, m)
	typeText(m, "/loop 5m check CI")
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := sentUser(t, stdin, 1); len(got) != 1 || got[0] != "/loop 5m check CI" {
		t.Fatalf("orchestrator got %q", got)
	}
	last := m.chat[len(m.chat)-1]
	if last.role != store.ChatEvent || !strings.Contains(last.text, "CronCreate") || !strings.Contains(last.text, "agent row") {
		t.Fatalf("last chat line = %+v", last)
	}
}

// What a skill or command produces shows up in the chat, even when it isn't
// an assistant message: local command output, refused tool calls, and a
// result whose text no assistant event carried.
func TestSlashResultsReachTheChat(t *testing.T) {
	m := slashModel(t)
	m.handleEvent(orch.Event{Kind: orch.Local, Text: "Total cost: $0.12"})
	m.handleEvent(orch.Event{Kind: orch.Result, Text: "Total cost: $0.12"})
	m.handleEvent(orch.Event{Kind: orch.Denied, Text: "Write CLAUDE.md"})
	m.handleEvent(orch.Event{Kind: orch.Result, Text: "Unknown skill: nope"})
	m.handleEvent(orch.Event{Kind: orch.Text, Text: "pong"})
	m.handleEvent(orch.Event{Kind: orch.Result, Text: "pong"})
	var got []string
	for _, c := range m.chat {
		got = append(got, c.role+": "+c.text)
	}
	want := []string{
		store.ChatAssistant + ": Total cost: $0.12",
		store.ChatEvent + ": ",
		store.ChatAssistant + ": Unknown skill: nope",
		store.ChatAssistant + ": pong",
	}
	if len(got) != len(want) {
		t.Fatalf("chat = %q", got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("chat[%d] = %q, want prefix %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(got[1], "Write CLAUDE.md") || !strings.Contains(got[1], "agent row") {
		t.Errorf("denial line = %q", got[1])
	}
}
