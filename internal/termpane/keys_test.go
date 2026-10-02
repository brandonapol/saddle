package termpane

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyBytes(t *testing.T) {
	cases := []struct {
		name string
		k    tea.KeyMsg
		app  bool
		want string
	}{
		{"runes", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hé")}, false, "hé"},
		{"alt rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b"), Alt: true}, false, "\x1bb"},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, false, "\r"},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}, false, "\x03"},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, false, "\x7f"},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}, false, "\x1b"},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, false, "\t"},
		{"shift+tab", tea.KeyMsg{Type: tea.KeyShiftTab}, false, "\x1b[Z"},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, false, "\x1b[A"},
		{"up app", tea.KeyMsg{Type: tea.KeyUp}, true, "\x1bOA"},
		{"space", tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}, false, " "},
		{"delete", tea.KeyMsg{Type: tea.KeyDelete}, false, "\x1b[3~"},
		{"pgup", tea.KeyMsg{Type: tea.KeyPgUp}, false, "\x1b[5~"},
		{"ctrl+right", tea.KeyMsg{Type: tea.KeyCtrlRight}, false, "\x1b[1;5C"},
		{"f1", tea.KeyMsg{Type: tea.KeyF1}, false, "\x1bOP"},
		{"f5", tea.KeyMsg{Type: tea.KeyF5}, false, "\x1b[15~"},
	}
	for _, c := range cases {
		if got := string(KeyBytes(c.k, c.app)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPasteIsBracketedWhenEnabled(t *testing.T) {
	k := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ls\n"), Paste: true}
	if got := string(PasteBytes(k, true)); got != "\x1b[200~ls\n\x1b[201~" {
		t.Fatalf("got %q", got)
	}
	if got := string(PasteBytes(k, false)); got != "ls\n" {
		t.Fatalf("got %q", got)
	}
}
