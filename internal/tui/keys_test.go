package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

func TestCycleAgentSkipsDeadWindows(t *testing.T) {
	m := &model{tasks: []mcpserver.TaskView{
		{ID: "t1", Status: store.Running, Window: "@1"},
		{ID: "t2", Status: store.Landed, Window: ""},
		{ID: "t3", Status: store.NeedsYou, Window: "@3"},
		{ID: "t4", Status: store.Done, Window: "@4"},
	}}
	var seen []string
	for range 3 {
		if !m.cycleAgent(1) {
			t.Fatal("cycleAgent(1) found nothing")
		}
		seen = append(seen, m.tasks[m.sel].ID)
	}
	if got := strings.Join(seen, ","); got != "t3,t1,t3" {
		t.Errorf("forward cycle = %s, want t3,t1,t3", got)
	}
	m.cycleAgent(-1)
	if m.tasks[m.sel].ID != "t1" {
		t.Errorf("back from t3 = %s, want t1", m.tasks[m.sel].ID)
	}

	alone := &model{tasks: []mcpserver.TaskView{{ID: "t1", Status: store.Running, Window: "@1"}}}
	if alone.cycleAgent(1) {
		t.Error("with one agent there is nowhere to cycle")
	}
}

func TestFooterHelpMatchesBindings(t *testing.T) {
	m := &model{keys: newKeyMap(), prefix: "C-a"}
	for _, focus := range []int{focusChat, focusTasks} {
		m.focus = focus
		for _, b := range m.help() {
			h := b.Help()
			if h.Key == "" || h.Desc == "" {
				t.Errorf("binding %v has no help", b.Keys())
			}
			if len(b.Keys()) == 0 && !strings.HasPrefix(h.Key, "C-a") {
				t.Errorf("help %q has no key behind it", h.Key)
			}
		}
	}
	// The real keys reach their bindings.
	alt := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n"), Alt: true}
	if !key.Matches(alt, m.keys.NextAgent) {
		t.Error("alt+n should match NextAgent")
	}
	m.focus = focusTasks
	if s := m.viewKeys(400); !strings.Contains(s, "C-a d") {
		t.Errorf("task footer should show the tmux prefix: %q", s)
	}
}

func TestViewKeysFitsWidth(t *testing.T) {
	m := &model{keys: newKeyMap(), prefix: "C-b"}
	for _, w := range []int{30, 80, 200} {
		if got := lipgloss.Width(m.viewKeys(w)); got > w {
			t.Errorf("footer at width %d is %d wide", w, got)
		}
	}
}
