package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Bubble Tea v1 assumes a short read ends on an event boundary. It does not
// always, so a wheel report like ESC[<65;139;21M can be cut in two and parsed
// as keys: esc or alt+[ first, then runes such as "<65;1" and "39;21M". Left
// alone those runes land in the input box. mouseScrub spots the pieces and
// drops them.
//
// A real alt+[ looks like the start of a report (#201), so mouseScrub holds
// it until the next key decides what it was, or a timer says nothing
// followed.
type mouseScrub struct {
	state int
	held  *tea.KeyMsg // the alt+[ waiting for the next key
	seq   int         // tells a stale timer from the current one
}

// escWait is how long a held key waits for the next one.
const escWait = 25 * time.Millisecond

// escFlushMsg is a held key's timer firing.
type escFlushMsg struct{ seq int }

const (
	scrubIdle    = iota
	scrubEsc     // saw a bare esc; a report continues with "[<"
	scrubBracket // saw alt+[; a report continues with "<"
	scrubReport  // inside the parameters; continues with digits and ';'
)

func isEsc(k tea.KeyMsg) bool { return k.Type == tea.KeyEscape && !k.Alt && !k.Paste }

func isAltBracket(k tea.KeyMsg) bool {
	return k.Type == tea.KeyRunes && k.Alt && !k.Paste && string(k.Runes) == "["
}

func plainRunes(k tea.KeyMsg) bool { return k.Type == tea.KeyRunes && !k.Alt && !k.Paste }

// feed takes the next key and returns the keys to act on now, in order, and
// a timer to run when it starts holding one. A dropped piece of a mouse
// report returns nothing.
func (s *mouseScrub) feed(k tea.KeyMsg) ([]tea.KeyMsg, tea.Cmd) {
	if s.held != nil {
		h := *s.held
		s.held = nil
		if plainRunes(k) {
			prefix := "<"
			if isEsc(h) {
				prefix = "[<"
			}
			if rest, ok := strings.CutPrefix(string(k.Runes), prefix); ok && reportParams(rest) {
				s.report(rest)
				return nil, nil
			}
		}
		s.state = scrubIdle
		out, c := s.feed(k)
		return append([]tea.KeyMsg{h}, out...), c
	}
	if isAltBracket(k) {
		return nil, s.hold(k)
	}
	if isEsc(k) {
		s.state = scrubEsc
		return []tea.KeyMsg{k}, nil
	}
	if !plainRunes(k) {
		s.state = scrubIdle
		return []tea.KeyMsg{k}, nil
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
		return []tea.KeyMsg{k}, nil
	}
	s.report(rest)
	return nil, nil
}

// hold keeps k back and starts its timer.
func (s *mouseScrub) hold(k tea.KeyMsg) tea.Cmd {
	s.held = &k
	s.seq++
	seq := s.seq
	return tea.Tick(escWait, func(time.Time) tea.Msg { return escFlushMsg{seq} })
}

// flush releases the held key when timer seq is the current one. Pieces of a
// report may still follow it, so the scrubber keeps watching for them.
func (s *mouseScrub) flush(seq int) []tea.KeyMsg {
	if s.held == nil || seq != s.seq {
		return nil
	}
	return s.release()
}

// release hands back the held key, if any.
func (s *mouseScrub) release() []tea.KeyMsg {
	if s.held == nil {
		return nil
	}
	h := *s.held
	s.held = nil
	s.state = scrubBracket
	if isEsc(h) {
		s.state = scrubEsc
	}
	return []tea.KeyMsg{h}
}

// report moves past report parameters rest: done at the final byte, else
// inside the report.
func (s *mouseScrub) report(rest string) {
	if strings.HasSuffix(rest, "M") || strings.HasSuffix(rest, "m") {
		s.state = scrubIdle
	} else {
		s.state = scrubReport
	}
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
