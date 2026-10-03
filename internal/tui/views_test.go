package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// newViewModel is a model that can render a full screen without tmux, an
// orchestrator or a store.
func newViewModel(w, h int) *model {
	cfg := config.Default()
	cfg.Base, cfg.Integration, cfg.Session = "main", "saddle/integration", "saddle-repo"
	m := &model{
		app:    &app.App{Cfg: cfg},
		launch: agent.Launch{Model: "opus"},
		keys:   newKeyMap(),
		prefix: "C-b",
		follow: true,
		input:  textarea.New(),
		vp:     viewport.New(40, 10),
		tasks: []mcpserver.TaskView{
			{ID: "t1", Title: "view router", Status: store.Running, Model: "opus", Window: "@1", Claims: []string{"internal/tui/**"}},
			{ID: "t2", Title: "usage graph", Status: store.Done, Model: "sonnet", Train: "queued"},
			{ID: "t3", Title: "conflicted work", Status: store.Conflict, Model: "sonnet", Window: "@3", Train: "conflict: go.mod"},
			{ID: "t4", Title: "landed work", Status: store.Landed, Model: "haiku", Train: "landed"},
		},
	}
	m.input.Focus()
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func altKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true} }
func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// checkScreen fails if any line is wider than w or the screen isn't h lines.
func checkScreen(t *testing.T, name, out string, w, h int) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Errorf("%s at %dx%d: %d lines", name, w, h, len(lines))
	}
	for _, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("%s at %dx%d: line %q is %d wide", name, w, h, l, lw)
		}
	}
}

// alt+1/2/3 switch views from any focus, even while typing in the chat, and
// never reach the chat input.
func TestViewSwitchKeys(t *testing.T) {
	m := newViewModel(120, 40)
	for _, c := range []struct {
		k    rune
		want int
	}{{'2', viewPlan}, {'3', viewMerge}, {'1', viewControl}, {'3', viewMerge}} {
		m.Update(altKey(c.k))
		if m.view != c.want {
			t.Errorf("alt+%c: view = %d, want %d", c.k, m.view, c.want)
		}
	}
	if m.input.Value() != "" {
		t.Errorf("view keys leaked into the chat: %q", m.input.Value())
	}
	// Outside the control view, typing does not go to the hidden chat.
	m.Update(runeKey('a'))
	if m.input.Value() != "" {
		t.Errorf("merge view leaked a key into the chat: %q", m.input.Value())
	}
	m.Update(altKey('1'))
	m.Update(runeKey('a'))
	if m.input.Value() != "a" {
		t.Errorf("back on control the chat should take typing, got %q", m.input.Value())
	}
}

// Every view fills the screen exactly, with no line over the width, from a
// narrow split pane to a wide terminal.
func TestViewsFitTheScreen(t *testing.T) {
	for _, w := range []int{30, 36, 40, 120} {
		for _, v := range []int{viewControl, viewPlan, viewMerge} {
			m := newViewModel(w, 30)
			m.view = v
			checkScreen(t, viewNames[v], m.View(), w, 30)
		}
		m := newViewModel(w, 30)
		m.helpOpen = true
		checkScreen(t, "help", m.View(), w, 30)
	}
}

// The header is one line at any width, and on a wide screen names the views,
// the base and integration branches, the counts and the train.
func TestHeaderShowsViewsBranchesCountsTrain(t *testing.T) {
	for _, w := range []int{30, 40, 120} {
		m := newViewModel(w, 30)
		h := m.viewHeader()
		if lipgloss.Height(h) != 1 || lipgloss.Width(h) > w {
			t.Errorf("width %d: header %q is %dx%d", w, h, lipgloss.Width(h), lipgloss.Height(h))
		}
		if !strings.Contains(h, "SADDLE") || !strings.Contains(h, "control") {
			t.Errorf("width %d: header lacks the logo or the current view: %q", w, h)
		}
	}
	m := newViewModel(160, 30)
	h := m.viewHeader()
	for _, want := range []string{"1 control", "2 plan", "3 merge", "main → saddle/integration", "1 running", "1 need attention", "train 1 queued", "1 conflict"} {
		if !strings.Contains(h, want) {
			t.Errorf("header lacks %q: %q", want, h)
		}
	}
	m.Update(altKey('3'))
	m.width = 30
	if got := m.viewHeader(); !strings.Contains(got, "merge") {
		t.Errorf("narrow header should name the current view: %q", got)
	}
}

// The merge view lists the train; with no plan the plan view says how to
// make one.
func TestMergeAndPlanViews(t *testing.T) {
	m := newViewModel(120, 30)
	m.view = viewMerge
	out := m.View()
	for _, want := range []string{"MERGE TRAIN", "t2", "queued", "t3", "conflict: go.mod", "t4"} {
		if !strings.Contains(out, want) {
			t.Errorf("merge view lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "view router") {
		t.Errorf("a task outside the train is in the merge view:\n%s", out)
	}
	m.view = viewPlan
	if out := m.View(); !strings.Contains(out, "PLAN") || !strings.Contains(out, "saddle plan") {
		t.Errorf("plan view:\n%s", out)
	}
}

// ? (f1 from the chat) opens a help overlay listing every binding; esc
// closes it.
func TestHelpOverlayListsEveryBinding(t *testing.T) {
	m := newViewModel(120, 60)
	m.Update(tea.KeyMsg{Type: tea.KeyF1})
	if !m.helpOpen {
		t.Fatal("f1 from the chat should open help")
	}
	out := m.View()
	for _, g := range m.keys.groups() {
		for _, b := range g.keys {
			if h := b.Help(); !strings.Contains(out, h.Key) || !strings.Contains(out, h.Desc) {
				t.Errorf("help overlay lacks %q %q", h.Key, h.Desc)
			}
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.helpOpen {
		t.Fatal("esc should close help")
	}
	m.focus = focusTasks
	m.input.Blur()
	m.Update(runeKey('?'))
	if !m.helpOpen {
		t.Fatal("? on the agent list should open help")
	}
	m.Update(runeKey('?'))
	if m.helpOpen {
		t.Fatal("? again should close help")
	}
}

// Every binding in the keymap is in exactly one help group, so the overlay
// can't fall behind the keymap.
func TestHelpGroupsCoverKeyMap(t *testing.T) {
	km := newKeyMap()
	seen := map[string]int{}
	for _, g := range km.groups() {
		for _, b := range g.keys {
			seen[strings.Join(b.Keys(), ",")+"|"+b.Help().Desc]++
		}
	}
	v := reflect.ValueOf(km)
	for i := range v.NumField() {
		b := v.Field(i).Interface().(key.Binding)
		id := strings.Join(b.Keys(), ",") + "|" + b.Help().Desc
		if seen[id] != 1 {
			t.Errorf("binding %s is in %d help groups, want 1", v.Type().Field(i).Name, seen[id])
		}
	}
}
