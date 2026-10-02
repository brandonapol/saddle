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
	for _, v := range []int{viewControl, viewPlan, viewMerge} {
		for _, focus := range []int{focusChat, focusTasks} {
			for _, help := range []bool{false, true} {
				m.view, m.focus, m.helpOpen = v, focus, help
				hs := m.help()
				views := false
				for _, b := range hs {
					h := b.Help()
					if h.Key == "" || h.Desc == "" {
						t.Errorf("binding %v has no help", b.Keys())
					}
					if len(b.Keys()) == 0 && !strings.HasPrefix(h.Key, "C-a") {
						t.Errorf("help %q has no key behind it", h.Key)
					}
					views = views || h.Key == m.keys.ViewControl.Help().Key
				}
				if !views && !help {
					t.Errorf("view %d focus %d: footer lacks the view keys", v, focus)
				}
			}
		}
	}
	// The real keys reach their bindings.
	alt := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n"), Alt: true}
	if !key.Matches(alt, m.keys.NextAgent) {
		t.Error("alt+n should match NextAgent")
	}
	m.view, m.focus, m.helpOpen = viewControl, focusTasks, false
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

func TestPeekTitleShowsAgentPosition(t *testing.T) {
	m := &model{keys: newKeyMap(), tasks: []mcpserver.TaskView{
		{ID: "t1", Title: "first", Status: store.Running, Window: "@1"},
		{ID: "t2", Title: "landed", Status: store.Landed},
		{ID: "t3", Title: "a fairly long task title here", Status: store.NeedsYou, Window: "@3"},
	}, sel: 2}
	for _, w := range []int{30, 40, 80} {
		out := m.viewLeft(w, 20)
		for _, l := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(l); lw > w {
				t.Errorf("width %d: line %q is %d wide", w, l, lw)
			}
		}
		if !strings.Contains(out, "2/2") || !strings.Contains(out, "alt+n/p") {
			t.Errorf("width %d: peek should show position 2/2 and the switch keys:\n%s", w, out)
		}
	}
	m.tasks = m.tasks[:1]
	m.sel = 0
	if out := m.viewLeft(80, 20); strings.Contains(out, "alt+n/p") {
		t.Errorf("with one live agent there is nothing to switch to:\n%s", out)
	}
}

func TestPeekTitleOnDeadSelection(t *testing.T) {
	m := &model{keys: newKeyMap(), tasks: []mcpserver.TaskView{
		{ID: "t1", Status: store.Running, Window: "@1"},
		{ID: "t2", Status: store.Landed},
		{ID: "t3", Status: store.Running, Window: "@3"},
	}, sel: 1}
	if out := m.viewLeft(80, 20); !strings.Contains(out, "2 live alt+n/p") || strings.Contains(out, "0/2") {
		t.Errorf("a landed selection should count live agents, not a position:\n%s", out)
	}
}
