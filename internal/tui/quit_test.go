package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func isQuit(c tea.Cmd) bool {
	if c == nil {
		return false
	}
	// The arming tick blocks for the whole window; only tea.Quit returns at once.
	got := make(chan tea.Msg, 1)
	go func() { got <- c() }()
	select {
	case msg := <-got:
		_, ok := msg.(tea.QuitMsg)
		return ok
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

func ctrlC() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlC} }

func TestSingleCtrlCDoesNotQuitAndShowsHint(t *testing.T) {
	m := &model{width: 120, keys: newKeyMap()}
	c, handled := m.key(ctrlC())
	if !handled || isQuit(c) {
		t.Fatalf("first ctrl+c must be handled without quitting (handled=%v)", handled)
	}
	if !m.quitArmed() {
		t.Fatal("first ctrl+c should arm quitting")
	}
	if got := m.viewFooter(); !strings.Contains(got, "Press Ctrl+C again to quit") {
		t.Fatalf("status line missing hint: %q", got)
	}
}

func TestSecondCtrlCWithinWindowQuits(t *testing.T) {
	m := &model{width: 120, keys: newKeyMap()}
	m.key(ctrlC())
	c, _ := m.key(ctrlC())
	if !isQuit(c) {
		t.Fatal("second ctrl+c within the window must quit")
	}
}

func TestCtrlCAfterWindowExpiresRearms(t *testing.T) {
	m := &model{width: 120, keys: newKeyMap()}
	m.key(ctrlC())
	m.quitArmedAt = time.Now().Add(-quitWindow - time.Millisecond)
	if m.quitArmed() {
		t.Fatal("window should have expired")
	}
	c, _ := m.key(ctrlC())
	if isQuit(c) {
		t.Fatal("ctrl+c after the window must not quit")
	}
	if !m.quitArmed() {
		t.Fatal("ctrl+c after expiry should start over")
	}
}

func TestOtherKeyDisarms(t *testing.T) {
	m := &model{width: 120, keys: newKeyMap()}
	m.key(ctrlC())
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if m.quitArmed() {
		t.Fatal("another key must disarm")
	}
	c, _ := m.key(ctrlC())
	if isQuit(c) {
		t.Fatal("ctrl+c after disarm must not quit")
	}
}
