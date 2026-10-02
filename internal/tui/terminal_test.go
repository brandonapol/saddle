package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/termpane"
)

func newTermModel(t *testing.T) *model {
	t.Helper()
	m := &model{
		app:       &app.App{Root: t.TempDir()},
		keys:      newKeyMap(),
		follow:    true,
		input:     textarea.New(),
		vp:        viewport.New(40, 10),
		termShell: "/bin/sh",
	}
	m.input.Focus()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	t.Cleanup(func() {
		if m.term != nil {
			m.term.Close()
		}
	})
	return m
}

func toggleKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlAt} }
func escKey() tea.KeyMsg    { return tea.KeyMsg{Type: tea.KeyEsc} }

func typeText(m *model, s string) {
	for _, r := range s {
		switch r {
		case '\r':
			m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		default:
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
}

// waitScreen waits until the terminal shows want. A trailing newline in want
// means it must end a line.
func waitScreen(t *testing.T, tm *termpane.Term, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var ls []string
		for _, l := range strings.Split(tm.Text(), "\n") {
			ls = append(ls, strings.TrimRight(l, " "))
		}
		if strings.Contains(strings.Join(ls, "\n")+"\n", want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal never showed %q; got:\n%s", want, tm.Text())
}

// runCmd runs a tea.Cmd and feeds its message back, as the program loop does.
// openShell opens the pane and waits for the shell's prompt, so typed keys
// aren't flushed by the shell setting up its line editor.
func openShell(t *testing.T, m *model) tea.Cmd {
	t.Helper()
	_, c := m.Update(toggleKey())
	if m.term == nil {
		t.Fatal("toggle did not start a shell")
	}
	waitScreen(t, m.term, "$")
	return c
}

func runCmd(t *testing.T, m *model, c tea.Cmd) tea.Cmd {
	t.Helper()
	got := make(chan tea.Msg, 1)
	go func() { got <- c() }()
	select {
	case msg := <-got:
		_, next := m.Update(msg)
		return next
	case <-time.After(5 * time.Second):
		t.Fatal("command never returned")
		return nil
	}
}

func TestTerminalToggleOpensShellInRepoRoot(t *testing.T) {
	m := newTermModel(t)
	if m.termOpen || m.term != nil {
		t.Fatal("pane open before toggle")
	}
	m.Update(toggleKey())
	if !m.termOpen || m.term == nil || m.focus != focusTerm {
		t.Fatalf("toggle did not open and focus the pane (open=%v focus=%d)", m.termOpen, m.focus)
	}
	waitScreen(t, m.term, "$")
	typeText(m, "pwd\r")
	waitScreen(t, m.term, m.app.Root)
	if v := m.View(); !strings.Contains(v, "TERMINAL") {
		t.Fatal("view has no terminal pane")
	}
}

func TestTerminalFocusForwardsKeysNotShortcuts(t *testing.T) {
	m := newTermModel(t)
	openShell(t, m)
	m.Update(ctrlC())
	if m.quitArmed() {
		t.Fatal("ctrl+c in the terminal must go to the shell, not arm quitting")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != focusTerm {
		t.Fatal("tab must go to the shell, not switch panes")
	}
	typeText(m, "echo for-the-shell\r")
	waitScreen(t, m.term, "for-the-shell\n")
	if m.input.Value() != "" {
		t.Fatalf("chat input got terminal keys: %q", m.input.Value())
	}
}

func TestTerminalEscReturnsFocusToChat(t *testing.T) {
	m := newTermModel(t)
	m.Update(toggleKey())
	m.Update(escKey())
	if m.focus != focusChat || !m.input.Focused() {
		t.Fatalf("esc did not return focus to chat (focus=%d)", m.focus)
	}
	if !m.termOpen {
		t.Fatal("esc should leave the pane visible")
	}
	m.Update(toggleKey())
	if m.focus != focusTerm {
		t.Fatal("toggle from chat should refocus the visible pane")
	}
}

func TestTerminalToggleFromTerminalHidesPane(t *testing.T) {
	m := newTermModel(t)
	m.Update(toggleKey())
	tm := m.term
	m.Update(toggleKey())
	if m.termOpen || m.focus != focusChat {
		t.Fatalf("toggle should hide the pane and focus chat (open=%v focus=%d)", m.termOpen, m.focus)
	}
	m.Update(toggleKey())
	if m.term != tm {
		t.Fatal("reopening should keep the running shell")
	}
}

func TestTerminalEscGoesToFullScreenProgram(t *testing.T) {
	m := newTermModel(t)
	openShell(t, m)
	typeText(m, `printf '\033[?1049h'; read x`+"\r")
	deadline := time.Now().Add(5 * time.Second)
	for !m.term.AltScreen() {
		if time.Now().After(deadline) {
			t.Fatal("program never entered the alternate screen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.Update(escKey())
	if m.focus != focusTerm {
		t.Fatal("esc in a full-screen program (vim) must reach the program")
	}
}

func TestTerminalShellExitRestartsOnNextToggle(t *testing.T) {
	m := newTermModel(t)
	c := openShell(t, m)
	old := m.term
	typeText(m, "exit\r")
	for c != nil && m.term == old {
		c = runCmd(t, m, c)
	}
	if m.term != nil || m.termOpen || m.focus != focusChat {
		t.Fatalf("after exit: term=%v open=%v focus=%d", m.term != nil, m.termOpen, m.focus)
	}
	m.Update(toggleKey())
	if m.term == nil || m.term == old || m.term.Exited() {
		t.Fatal("toggle after exit should start a new shell")
	}
	waitScreen(t, m.term, "$")
	typeText(m, "echo again\r")
	waitScreen(t, m.term, "again\n")
}

func TestTerminalStaleExitIgnored(t *testing.T) {
	m := newTermModel(t)
	m.Update(toggleKey())
	cur := m.term
	m.Update(termExitMsg{t: &termpane.Term{}})
	if m.term != cur || !m.termOpen {
		t.Fatal("an exit from an old shell must not close the current one")
	}
}

func TestTerminalResizeFollowsWindow(t *testing.T) {
	m := newTermModel(t)
	m.Update(toggleKey())
	cols, rows := m.term.Size()
	if cols != 118 || rows < 4 {
		t.Fatalf("pane size %dx%d, want 118 cols", cols, rows)
	}
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 60})
	c2, r2 := m.term.Size()
	if c2 != 88 || r2 <= rows {
		t.Fatalf("after resize %dx%d (was %dx%d)", c2, r2, cols, rows)
	}
	waitScreen(t, m.term, "$")
	typeText(m, "stty size\r")
	waitScreen(t, m.term, " 88")
	if got := strings.Count(m.View(), "\n") + 1; got != 60 {
		t.Fatalf("view is %d lines, want the window height 60", got)
	}
}

func TestTerminalHelpShowsToggle(t *testing.T) {
	m := newTermModel(t)
	if !strings.Contains(m.viewKeys(200), "ctrl+`") {
		t.Fatal("chat help should advertise the terminal toggle")
	}
	m.Update(toggleKey())
	if !strings.Contains(m.viewKeys(200), "esc") {
		t.Fatal("terminal help should say how to get back")
	}
}
