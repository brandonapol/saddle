package tmux

import (
	"strconv"
	"strings"
	"unicode"
)

// Input is what an agent's prompt input box holds.
type Input int

const (
	InputEmpty Input = iota // nothing typed
	InputGhost              // only grey text Claude Code draws itself: a next-prompt suggestion or a placeholder (#183)
	InputDraft              // text the user typed
)

func (i Input) String() string {
	return [...]string{"empty", "ghost", "draft"}[i]
}

// cell is one visible rune and whether it was drawn grey or faint.
type cell struct {
	r     rune
	ghost bool
}

// ReadInput classifies the last prompt line ("> text", "❯ text", with or
// without a surrounding box border) of a captured pane. A capture with SGR
// escapes (capture-pane -e) tells a faint or grey suggestion from typed
// text; a plain capture can only spot the "Try …" placeholder, so any other
// text counts as a draft.
func ReadInput(pane string) Input {
	styled := strings.Contains(pane, "\x1b[")
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		cells := trimCells(decode(lines[i]), "│")
		if len(cells) == 0 || (cells[0].r != '❯' && cells[0].r != '>') {
			continue
		}
		rest := trimCells(cells[1:], "")
		if len(rest) == 0 {
			return InputEmpty
		}
		if !styled {
			if strings.HasPrefix(cellText(rest), "Try ") {
				return InputGhost
			}
			return InputDraft
		}
		for _, c := range rest {
			if !c.ghost && !unicode.IsSpace(c.r) {
				return InputDraft
			}
		}
		return InputGhost
	}
	return InputEmpty
}

// HasDraft reports whether the prompt input in a captured pane holds text the
// user is typing. Suggestions and placeholder hints don't count.
func HasDraft(pane string) bool { return ReadInput(pane) == InputDraft }

// StripANSI drops escape sequences, leaving the text a plain capture shows.
func StripANSI(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = cellText(decode(l))
	}
	return strings.Join(lines, "\n")
}

func cellText(cs []cell) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteRune(c.r)
	}
	return b.String()
}

// trimCells drops spaces and the given border runes from both ends.
func trimCells(cs []cell, border string) []cell {
	drop := func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(border, r) }
	for len(cs) > 0 && drop(cs[0].r) {
		cs = cs[1:]
	}
	for len(cs) > 0 && drop(cs[len(cs)-1].r) {
		cs = cs[:len(cs)-1]
	}
	return cs
}

// decode turns one captured line into visible runes, tracking whether SGR
// escapes left each one faint or grey. Other escapes are skipped.
func decode(line string) []cell {
	var out []cell
	var faint, grey bool
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\x1b' {
			if rs[i] == '\r' {
				continue
			}
			out = append(out, cell{rs[i], faint || grey})
			continue
		}
		if i+1 >= len(rs) {
			break
		}
		switch rs[i+1] {
		case '[': // CSI: parameters, then a final byte in @..~
			j := i + 2
			for j < len(rs) && (rs[j] < '@' || rs[j] > '~') {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				faint, grey = sgr(string(rs[i+2:j]), faint, grey)
			}
			i = j
		case ']': // OSC: up to BEL or ESC \
			j := i + 2
			for j < len(rs) && rs[j] != '\a' && (rs[j] != '\x1b' || j+1 >= len(rs) || rs[j+1] != '\\') {
				j++
			}
			if j < len(rs) && rs[j] == '\x1b' {
				j++
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// sgr applies one Select Graphic Rendition sequence to the faint and grey
// foreground state.
func sgr(params string, faint, grey bool) (bool, bool) {
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	if len(ps) == 0 {
		return false, false
	}
	n := make([]int, len(ps))
	for i, p := range ps {
		n[i], _ = strconv.Atoi(p)
	}
	for i := 0; i < len(n); i++ {
		switch v := n[i]; {
		case v == 0:
			faint, grey = false, false
		case v == 2:
			faint = true
		case v == 22:
			faint = false
		case v == 39:
			grey = false
		case v == 90:
			grey = true
		case v >= 30 && v <= 37, v >= 91 && v <= 97:
			grey = false
		case v == 38 && i+2 < len(n) && n[i+1] == 5:
			c := n[i+2]
			grey = c == 8 || (c >= 238 && c <= 250)
			i += 2
		case v == 38 && i+4 < len(n) && n[i+1] == 2:
			grey = greyRGB(n[i+2], n[i+3], n[i+4])
			i += 4
		case v == 48 && i+2 < len(n) && n[i+1] == 5:
			i += 2
		case v == 48 && i+4 < len(n) && n[i+1] == 2:
			i += 4
		}
	}
	return faint, grey
}

// greyRGB reports whether a truecolor foreground is a mid grey.
func greyRGB(r, g, b int) bool {
	lo, hi := min(r, g, b), max(r, g, b)
	avg := (r + g + b) / 3
	return hi-lo <= 12 && avg >= 90 && avg <= 180
}
