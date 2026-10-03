package termpane

import (
	"reflect"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// ExtendedKey decodes a key that a terminal sent in the kitty keyboard
// protocol (CSI code;mods u) or xterm's modifyOtherKeys (CSI 27;mods;code ~).
// Bubble Tea v1 does not parse either and hands them on as an unexported
// unknown-CSI message, so ctrl+` or alt+` from such a terminal would be lost.
// The result is the key a legacy terminal would have sent: ctrl+` is ctrl+@
// (NUL), alt+` is alt plus the backtick rune. Key releases and keys it cannot
// express are not decoded.
func ExtendedKey(msg tea.Msg) (tea.KeyMsg, bool) {
	seq, ok := unknownCSI(msg)
	if !ok {
		return tea.KeyMsg{}, false
	}
	body, ok := strings.CutPrefix(string(seq), "\x1b[")
	if !ok || body == "" {
		return tea.KeyMsg{}, false
	}
	var code, mods string
	switch final := body[len(body)-1]; final {
	case 'u':
		code, mods, _ = strings.Cut(body[:len(body)-1], ";")
	case '~':
		f := strings.Split(body[:len(body)-1], ";")
		if len(f) != 3 || f[0] != "27" {
			return tea.KeyMsg{}, false
		}
		code, mods = f[2], f[1]
	default:
		return tea.KeyMsg{}, false
	}
	// kitty may add the shifted and base-layout keys after colons, and the
	// event type after the modifiers.
	code, shifted, _ := strings.Cut(code, ":")
	shifted, _, _ = strings.Cut(shifted, ":")
	mods, event, _ := strings.Cut(mods, ":")
	if event != "" && event != "1" && event != "2" { // 3 is a release
		return tea.KeyMsg{}, false
	}
	c, err := strconv.Atoi(code)
	if err != nil || c <= 0 {
		return tea.KeyMsg{}, false
	}
	m := 1
	if mods != "" {
		if m, err = strconv.Atoi(mods); err != nil || m < 1 {
			return tea.KeyMsg{}, false
		}
	}
	bits := m - 1
	shift, alt, ctrl := bits&1 != 0, bits&2 != 0, bits&4 != 0
	if bits&^7 != 0 { // super, hyper, meta: no legacy encoding
		return tea.KeyMsg{}, false
	}
	r := rune(c)
	if shift {
		if s, err := strconv.Atoi(shifted); err == nil && s > 0 {
			r = rune(s)
		} else if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
	}
	k, ok := legacyKey(r, ctrl)
	if !ok {
		return tea.KeyMsg{}, false
	}
	k.Alt = alt
	return k, true
}

// legacyKey is r as a legacy terminal sends it, with ctrl held when ctrl is set.
func legacyKey(r rune, ctrl bool) (tea.KeyMsg, bool) {
	switch r {
	case '\r':
		return tea.KeyMsg{Type: tea.KeyEnter}, !ctrl
	case '\t':
		return tea.KeyMsg{Type: tea.KeyTab}, !ctrl
	case 0x1b:
		return tea.KeyMsg{Type: tea.KeyEsc}, !ctrl
	case 0x7f:
		return tea.KeyMsg{Type: tea.KeyBackspace}, !ctrl
	}
	if !ctrl {
		if r == ' ' {
			return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, true
		}
		if r < ' ' || (r >= 0x7f && r < 0xa0) {
			return tea.KeyMsg{}, false
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, true
	}
	switch {
	case r >= 'a' && r <= 'z':
		return tea.KeyMsg{Type: tea.KeyCtrlA + tea.KeyType(r-'a')}, true
	case r >= 'A' && r <= 'Z':
		return tea.KeyMsg{Type: tea.KeyCtrlA + tea.KeyType(r-'A')}, true
	case r == '`' || r == '@' || r == ' ' || r == '2':
		return tea.KeyMsg{Type: tea.KeyCtrlAt}, true
	case r == '[':
		return tea.KeyMsg{Type: tea.KeyEsc}, true
	case r == '\\':
		return tea.KeyMsg{Type: tea.KeyCtrlBackslash}, true
	case r == ']':
		return tea.KeyMsg{Type: tea.KeyCtrlCloseBracket}, true
	case r == '^' || r == '6':
		return tea.KeyMsg{Type: tea.KeyCtrlCaret}, true
	case r == '_' || r == '-':
		return tea.KeyMsg{Type: tea.KeyCtrlUnderscore}, true
	}
	return tea.KeyMsg{}, false
}

// unknownCSI returns the bytes of Bubble Tea's unexported
// unknownCSISequenceMsg, the whole sequence from ESC on.
func unknownCSI(msg tea.Msg) ([]byte, bool) {
	if msg == nil {
		return nil, false
	}
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 || v.Type().Name() != "unknownCSISequenceMsg" {
		return nil, false
	}
	return v.Bytes(), true
}
