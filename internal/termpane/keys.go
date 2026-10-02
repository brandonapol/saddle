package termpane

import (
	tea "github.com/charmbracelet/bubbletea"
)

// specialKeys are xterm's encodings of keys that are not plain characters.
// Arrow, home and end keys are listed in their normal-mode form; KeyBytes
// switches them to ESC O in application cursor mode.
var specialKeys = map[tea.KeyType]string{
	tea.KeyUp:     "\x1b[A",
	tea.KeyDown:   "\x1b[B",
	tea.KeyRight:  "\x1b[C",
	tea.KeyLeft:   "\x1b[D",
	tea.KeyHome:   "\x1b[H",
	tea.KeyEnd:    "\x1b[F",
	tea.KeyInsert: "\x1b[2~",
	tea.KeyDelete: "\x1b[3~",
	tea.KeyPgUp:   "\x1b[5~",
	tea.KeyPgDown: "\x1b[6~",

	tea.KeyShiftTab: "\x1b[Z",

	tea.KeyShiftUp: "\x1b[1;2A", tea.KeyShiftDown: "\x1b[1;2B", tea.KeyShiftRight: "\x1b[1;2C", tea.KeyShiftLeft: "\x1b[1;2D",
	tea.KeyCtrlUp: "\x1b[1;5A", tea.KeyCtrlDown: "\x1b[1;5B", tea.KeyCtrlRight: "\x1b[1;5C", tea.KeyCtrlLeft: "\x1b[1;5D",
	tea.KeyCtrlShiftUp: "\x1b[1;6A", tea.KeyCtrlShiftDown: "\x1b[1;6B", tea.KeyCtrlShiftRight: "\x1b[1;6C", tea.KeyCtrlShiftLeft: "\x1b[1;6D",
	tea.KeyShiftHome: "\x1b[1;2H", tea.KeyShiftEnd: "\x1b[1;2F",
	tea.KeyCtrlHome: "\x1b[1;5H", tea.KeyCtrlEnd: "\x1b[1;5F",
	tea.KeyCtrlShiftHome: "\x1b[1;6H", tea.KeyCtrlShiftEnd: "\x1b[1;6F",
	tea.KeyCtrlPgUp: "\x1b[5;5~", tea.KeyCtrlPgDown: "\x1b[6;5~",

	tea.KeyF1: "\x1bOP", tea.KeyF2: "\x1bOQ", tea.KeyF3: "\x1bOR", tea.KeyF4: "\x1bOS",
	tea.KeyF5: "\x1b[15~", tea.KeyF6: "\x1b[17~", tea.KeyF7: "\x1b[18~", tea.KeyF8: "\x1b[19~",
	tea.KeyF9: "\x1b[20~", tea.KeyF10: "\x1b[21~", tea.KeyF11: "\x1b[23~", tea.KeyF12: "\x1b[24~",
}

// KeyBytes encodes a key press as the bytes a terminal would send. appCursor
// is DECCKM, set by programs like vim and less.
func KeyBytes(k tea.KeyMsg, appCursor bool) []byte {
	var out []byte
	switch {
	case k.Type == tea.KeyRunes || k.Type == tea.KeySpace:
		out = []byte(string(k.Runes))
		if len(out) == 0 && k.Type == tea.KeySpace {
			out = []byte(" ")
		}
	case k.Type >= 0 && k.Type <= 0x1f, k.Type == tea.KeyBackspace:
		// Control characters, enter, tab, esc and backspace are their own byte.
		out = []byte{byte(k.Type)}
	default:
		s, ok := specialKeys[k.Type]
		if !ok {
			return nil
		}
		if appCursor && len(s) == 3 && s[1] == '[' && s[2] >= 'A' && s[2] <= 'H' {
			s = "\x1bO" + s[2:]
		}
		out = []byte(s)
	}
	if k.Alt {
		out = append([]byte{0x1b}, out...)
	}
	return out
}

// PasteBytes encodes pasted text, wrapped in bracketed-paste markers when the
// program asked for them.
func PasteBytes(k tea.KeyMsg, bracketed bool) []byte {
	s := string(k.Runes)
	if bracketed {
		s = "\x1b[200~" + s + "\x1b[201~"
	}
	return []byte(s)
}
