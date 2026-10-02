package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Bubble Tea v1 assumes a short read ends on an event boundary. It does not
// always, so a wheel report like ESC[<65;139;21M can be cut in two and parsed
// as keys: esc or alt+[ first, then runes such as "<65;1" and "39;21M". Left
// alone those runes land in the input box. mouseScrub spots the pieces and
// drops them.
type mouseScrub struct{ state int }

const (
	scrubIdle    = iota
	scrubEsc     // saw a bare esc; a report continues with "[<"
	scrubBracket // saw alt+[; a report continues with "<"
	scrubReport  // inside the parameters; continues with digits and ';'
)

// drop reports whether k is a piece of a split mouse report. A bare esc is
// never dropped, since it may be real; only the runes after it are.
func (s *mouseScrub) drop(k tea.KeyMsg) bool {
	if k.Paste {
		s.state = scrubIdle
		return false
	}
	if k.Type == tea.KeyEscape && !k.Alt {
		s.state = scrubEsc
		return false
	}
	if k.Type == tea.KeyRunes && k.Alt && string(k.Runes) == "[" {
		s.state = scrubBracket
		return true
	}
	if k.Type != tea.KeyRunes || k.Alt {
		s.state = scrubIdle
		return false
	}
	text := string(k.Runes)
	var rest string
	var ok bool
	switch s.state {
	case scrubEsc:
		rest, ok = strings.CutPrefix(text, "[<")
	case scrubBracket:
		rest, ok = strings.CutPrefix(text, "<")
	case scrubReport:
		rest, ok = text, true
	}
	if !ok || !reportParams(rest) {
		s.state = scrubIdle
		return false
	}
	if strings.HasSuffix(rest, "M") || strings.HasSuffix(rest, "m") {
		s.state = scrubIdle
	} else {
		s.state = scrubReport
	}
	return true
}

// reportParams reports whether s could be the tail of SGR mouse parameters:
// digits and semicolons, optionally ending in the M or m final byte.
func reportParams(s string) bool {
	s = strings.TrimSuffix(strings.TrimSuffix(s, "M"), "m")
	for _, r := range s {
		if (r < '0' || r > '9') && r != ';' {
			return false
		}
	}
	return true
}
