package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"
)

func briefModel(w, h int) *model {
	m := newViewModel(w, h)
	m.app.Cfg.Serial = []string{"go.mod"}
	m.tasks[0].Parent = "t0"
	m.tasks[1].Parent = "t1"
	m.tasks[2].Claims = []string{"internal/app/**"}
	m.prompts = map[string]string{"t1": "Route the views through one switch.\n\nDone when:\n- [ ] views tests pass\n"}
	m.focus = focusTasks
	m.sel = 0
	return m
}

// #37: b on the agents list swaps the peek for the selected agent's brief,
// and b again brings the peek back.
func TestBriefPaneToggles(t *testing.T) {
	m := briefModel(120, 40)
	if out := m.viewLeft(80, 30); strings.Contains(out, "BRIEF") {
		t.Fatalf("the brief pane should start hidden:\n%s", out)
	}
	m.Update(runeKey('b'))
	out := m.viewLeft(80, 30)
	if !strings.Contains(out, "BRIEF · t1") || strings.Contains(out, "PEEK") {
		t.Fatalf("b should show the brief in place of the peek:\n%s", out)
	}
	m.Update(runeKey('b'))
	if out := m.viewLeft(80, 30); strings.Contains(out, "BRIEF") || !strings.Contains(out, "PEEK") {
		t.Fatalf("b again should bring the peek back:\n%s", out)
	}
}

func TestBriefPaneRendersAt36And120(t *testing.T) {
	for _, w := range []int{36, 120} {
		m := briefModel(w, 40)
		m.Update(runeKey('b'))
		out := m.View()
		checkScreen(t, "brief", out, w, 40)
		for _, want := range []string{"BRIEF", "Goal", "Owns", "internal/tui/**", "Hands off", "go.mod", "Done when", "Children", "t2"} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: brief lacks %q:\n%s", w, want, out)
			}
		}
		if w == 120 {
			for _, want := range []string{"Route the views through one switch.", "[ ] views tests pass", "internal/app/** (t3)", "t2 done usage graph"} {
				if !strings.Contains(out, want) {
					t.Errorf("width %d: brief lacks %q:\n%s", w, want, out)
				}
			}
		}
	}
}

// The brief follows the selection and falls back to the title with no prompt.
func TestBriefPaneFollowsSelection(t *testing.T) {
	m := briefModel(120, 40)
	m.briefOn = true
	m.sel = 2
	out := m.viewLeft(80, 30)
	if !strings.Contains(out, "BRIEF · t3") || !strings.Contains(out, "conflicted work") {
		t.Errorf("brief for t3:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(l); lw > 80 {
			t.Errorf("%q is %d wide", l, lw)
		}
	}
}

func TestBriefKeyInHelp(t *testing.T) {
	m := briefModel(120, 40)
	found := false
	for _, b := range m.help() {
		if key.Matches(runeKey('b'), b) {
			found = true
		}
	}
	if !found {
		t.Error("the agents footer should offer the brief key")
	}
}
