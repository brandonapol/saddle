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
// The same cut splits alt+<key> (ESC then the key) into esc and the key
// (#200), and a real alt+[ looks like the start of a report (#201). So
// mouseScrub holds a bare esc (when wait is set) and an alt+[ until the next
// key decides what they were, or the input has been idle for the wait.
//
// Idle, not just quiet on the clock (#249): on a loaded machine the second
// half of ESC <key> can sit unread in the terminal for longer than the wait
// while Bubble Tea's reader waits for a CPU. The hold lasts until no unread
// input has been seen for a whole wait. Once the reader has the bytes it
// hands their keys over at once, so they reach Update ahead of the flush.
type mouseScrub struct {
	state   int
	wait    time.Duration // how long a bare esc waits for a rune to join; 0 sends it at once
	held    *tea.KeyMsg   // the esc or alt+[ waiting for the next key
	seq     int           // tells a stale timer from the current one
	pending func() bool   // input has arrived but not been read; nil if unknown
}

// escWait is how long the TUI holds a bare esc, as Bubble Tea v2 and vim's
// esckeys do: long enough for the second read of a split ESC <key>, too
// short for a person to notice.
const escWait = 25 * time.Millisecond

// maxHold bounds a hold that unread input keeps extending, should the input
// never be read.
const maxHold = 2 * time.Second

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
			// Bubble Tea queues keys and timers in the order they happen,
			// so a key that gets here before the timer came within the wait,
			// however long the UI took to get to it.
			if isEsc(h) && len(k.Runes) == 1 {
				return s.feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: k.Runes, Alt: true})
			}
		}
		s.state = scrubIdle
		out, c := s.feed(k)
		return append([]tea.KeyMsg{h}, out...), c
	}
	if isAltBracket(k) || (isEsc(k) && s.wait > 0) {
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
	seq, wait, pending := s.seq, s.wait, s.pending
	if wait == 0 {
		wait = escWait
	}
	return func() tea.Msg {
		idleWait(wait, maxHold, pending)
		return escFlushMsg{seq}
	}
}

// idleWait returns once pending has been false for wait, polling it, or
// after limit. A nil pending makes it a plain sleep of wait.
func idleWait(wait, limit time.Duration, pending func() bool) {
	start := time.Now()
	quiet := start
	for {
		if pending != nil && pending() {
			quiet = time.Now()
		}
		now := time.Now()
		if now.Sub(quiet) >= wait || now.Sub(start) >= limit {
			return
		}
		step := wait - now.Sub(quiet)
		if pending != nil {
			step = min(step, time.Millisecond)
		}
		time.Sleep(step)
	}
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
