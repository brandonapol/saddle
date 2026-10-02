package termpane

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxScrollback is how many lines scrolled off the top are kept.
const maxScrollback = 2000

// Style attribute bits.
const (
	attrBold = 1 << iota
	attrDim
	attrItalic
	attrUnderline
	attrBlink
	attrReverse
	attrHidden
	attrStrike
)

// style is a cell's rendition. fg and bg hold SGR parameters ("31",
// "38;5;200", "48;2;1;2;3"); empty means default.
type style struct {
	fg, bg string
	attrs  uint8
}

// sgr is the escape sequence that sets st from a reset state.
func (st style) sgr() string {
	p := []string{"0"}
	for i, code := range []string{"1", "2", "3", "4", "5", "7", "8", "9"} {
		if st.attrs&(1<<i) != 0 {
			p = append(p, code)
		}
	}
	if st.fg != "" {
		p = append(p, st.fg)
	}
	if st.bg != "" {
		p = append(p, st.bg)
	}
	return "\x1b[" + strings.Join(p, ";") + "m"
}

type cell struct {
	r  rune
	st style
}

type row []cell

func blankRow(cols int, st style) row {
	r := make(row, cols)
	for i := range r {
		r[i] = cell{r: ' ', st: style{bg: st.bg}}
	}
	return r
}

// buffer is one screen's grid: the main screen or the alternate one.
type buffer struct {
	rows []row
}

// savedCursor is DECSC state.
type savedCursor struct {
	x, y int
	st   style
}

// Parser states.
const (
	stGround = iota
	stEscape
	stCSI
	stOSC
	stOSCEsc // ESC seen inside an OSC: maybe the ST terminator
	stCharset
)

// Screen is a small VT100/xterm emulator: enough of the protocol for shells,
// editors and pagers. It is not safe for concurrent use; Term serializes it.
type Screen struct {
	cols, rows int
	main, alt  buffer
	cur        *buffer
	x, y       int
	pending    bool // the last write filled the last column; wrap before the next
	st         style
	top, bot   int // scroll region, inclusive
	saved      savedCursor
	mainSaved  savedCursor // cursor saved on entering the alt screen
	scrollback []row

	appCursor, bracketedPaste, hideCursor bool

	state   int
	params  []byte
	utf     []byte
	replies []byte
}

// NewScreen returns a blank screen of the given size.
func NewScreen(cols, rows int) *Screen {
	cols, rows = max(cols, 1), max(rows, 1)
	s := &Screen{cols: cols, rows: rows}
	s.main.rows = s.blankRows(rows)
	s.alt.rows = s.blankRows(rows)
	s.cur = &s.main
	s.bot = rows - 1
	return s
}

func (s *Screen) blankRows(n int) []row {
	rs := make([]row, n)
	for i := range rs {
		rs[i] = blankRow(s.cols, style{})
	}
	return rs
}

// Size returns the screen's columns and rows.
func (s *Screen) Size() (cols, rows int) { return s.cols, s.rows }

// Cursor returns the cursor's column and row, zero based.
func (s *Screen) Cursor() (x, y int) { return s.x, s.y }

// AltScreen reports whether a full-screen program switched to the alternate screen.
func (s *Screen) AltScreen() bool { return s.cur == &s.alt }

// AppCursor reports DECCKM: arrow keys send ESC O x instead of ESC [ x.
func (s *Screen) AppCursor() bool { return s.appCursor }

// BracketedPaste reports whether the program asked for bracketed paste.
func (s *Screen) BracketedPaste() bool { return s.bracketedPaste }

// CursorVisible reports whether the program left the cursor shown.
func (s *Screen) CursorVisible() bool { return !s.hideCursor }

// Scrollback returns the lines that scrolled off the main screen, oldest first.
func (s *Screen) Scrollback() []string {
	out := make([]string, len(s.scrollback))
	for i, r := range s.scrollback {
		out[i] = r.text()
	}
	return out
}

// TakeReplies returns and clears bytes the emulator owes the program, like
// cursor position reports.
func (s *Screen) TakeReplies() []byte {
	r := s.replies
	s.replies = nil
	return r
}

func (r row) text() string {
	var b strings.Builder
	for _, c := range r {
		b.WriteRune(c.r)
	}
	return b.String()
}

// Text returns the visible screen as plain text, one line per row.
func (s *Screen) Text() string {
	ls := make([]string, s.rows)
	for i, r := range s.cur.rows {
		ls[i] = r.text()
	}
	return strings.Join(ls, "\n")
}

// Render returns the screen with its colors, scrolled back offset lines into
// history. The cell at (cx, cy) is drawn in reverse video as the cursor;
// pass -1 to draw none.
func (s *Screen) Render(offset, cx, cy int) string {
	var hist []row
	if s.cur == &s.main {
		hist = s.scrollback
	}
	offset = min(max(offset, 0), len(hist))
	all := hist[len(hist)-offset:]
	view := make([]row, 0, s.rows)
	view = append(view, all...)
	view = append(view, s.cur.rows...)
	view = view[:s.rows]
	var b strings.Builder
	for y, r := range view {
		if y > 0 {
			b.WriteByte('\n')
		}
		var last style
		started := false
		for x, c := range r {
			st := c.st
			if offset == 0 && x == cx && y == cy {
				st.attrs ^= attrReverse
			}
			if !started || st != last {
				b.WriteString(st.sgr())
				last, started = st, true
			}
			b.WriteRune(c.r)
		}
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// Resize changes the screen size. Shrinking pushes rows above the cursor into
// scrollback so the cursor stays on screen.
func (s *Screen) Resize(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	if cols == s.cols && rows == s.rows {
		return
	}
	for _, b := range []*buffer{&s.main, &s.alt} {
		for i, r := range b.rows {
			b.rows[i] = fitRow(r, cols)
		}
	}
	s.cols = cols
	for _, b := range []*buffer{&s.main, &s.alt} {
		if over := len(b.rows) - rows; over > 0 {
			// Drop from the top only as far as the cursor allows; then the bottom.
			cy := &s.y
			if b != s.cur {
				cy = &s.mainSaved.y // only main can be inactive
			}
			drop := min(over, max(*cy-(rows-1), 0))
			if b == &s.main {
				for _, r := range b.rows[:drop] {
					s.pushScrollback(r)
				}
			}
			b.rows = b.rows[drop : drop+rows]
			*cy -= drop
		}
		for len(b.rows) < rows {
			b.rows = append(b.rows, blankRow(cols, style{}))
		}
	}
	s.rows = rows
	s.top, s.bot = 0, rows-1
	s.x, s.y = min(s.x, cols-1), min(max(s.y, 0), rows-1)
	s.pending = false
}

func fitRow(r row, cols int) row {
	if len(r) >= cols {
		return r[:cols]
	}
	return append(r, blankRow(cols-len(r), style{})...)
}

func (s *Screen) pushScrollback(r row) {
	s.scrollback = append(s.scrollback, r)
	if over := len(s.scrollback) - maxScrollback; over > 0 {
		s.scrollback = append(s.scrollback[:0:0], s.scrollback[over:]...)
	}
}

// Feed passes program output to the emulator. Escape sequences and UTF-8
// characters may be split across calls.
func (s *Screen) Feed(p []byte) {
	for _, c := range p {
		s.feed(c)
	}
}

func (s *Screen) feed(c byte) {
	switch s.state {
	case stEscape:
		s.escape(c)
		return
	case stCSI:
		if c >= 0x40 && c <= 0x7e {
			s.state = stGround
			s.csi(c)
		} else if c == 0x1b {
			s.state = stEscape
		} else if c >= 0x20 {
			s.params = append(s.params, c)
		} else {
			s.control(c) // C0 controls act even mid-sequence.
		}
		return
	case stOSC:
		switch c {
		case 0x07:
			s.state = stGround
		case 0x1b:
			s.state = stOSCEsc
		}
		return
	case stOSCEsc:
		if c == '\\' {
			s.state = stGround
		} else {
			s.state = stOSC
		}
		return
	case stCharset:
		s.state = stGround
		return
	}
	if len(s.utf) > 0 || c >= 0x80 {
		s.utf = append(s.utf, c)
		if utf8.FullRune(s.utf) {
			r, _ := utf8.DecodeRune(s.utf)
			s.utf = s.utf[:0]
			s.print(r)
		}
		return
	}
	if c < 0x20 || c == 0x7f {
		s.control(c)
		return
	}
	s.print(rune(c))
}

func (s *Screen) control(c byte) {
	switch c {
	case 0x1b:
		s.state = stEscape
		s.params = s.params[:0]
	case '\r':
		s.x, s.pending = 0, false
	case '\n', 0x0b, 0x0c:
		s.lineFeed()
	case '\b':
		if s.x > 0 {
			s.x--
		}
		s.pending = false
	case '\t':
		s.x = min((s.x/8+1)*8, s.cols-1)
	}
}

func (s *Screen) escape(c byte) {
	s.state = stGround
	switch c {
	case '[':
		s.state = stCSI
		s.params = s.params[:0]
	case ']', 'P', '_', '^': // OSC, DCS, APC, PM: skip to the terminator
		s.state = stOSC
	case '(', ')', '*', '+':
		s.state = stCharset
	case '7':
		s.saved = savedCursor{s.x, s.y, s.st}
	case '8':
		s.x, s.y, s.st = s.saved.x, min(s.saved.y, s.rows-1), s.saved.st
		s.x = min(s.x, s.cols-1)
		s.pending = false
	case 'D':
		s.lineFeed()
	case 'E':
		s.x = 0
		s.lineFeed()
	case 'M':
		if s.y == s.top {
			s.scrollDown(1)
		} else if s.y > 0 {
			s.y--
		}
	case 'c':
		*s = *NewScreen(s.cols, s.rows)
	}
}

func (s *Screen) print(r rune) {
	if s.pending {
		s.x = 0
		s.lineFeed()
	}
	s.cur.rows[s.y][s.x] = cell{r: r, st: s.st}
	if s.x == s.cols-1 {
		s.pending = true
	} else {
		s.x++
	}
}

func (s *Screen) lineFeed() {
	s.pending = false
	if s.y == s.bot {
		s.scrollUp(1)
	} else if s.y < s.rows-1 {
		s.y++
	}
}

// scrollUp moves the scroll region up n lines. Lines leaving the top of a
// full-height main screen go to scrollback.
func (s *Screen) scrollUp(n int) {
	rs := s.cur.rows
	for range min(n, s.bot-s.top+1) {
		if s.cur == &s.main && s.top == 0 {
			s.pushScrollback(rs[s.top])
		}
		copy(rs[s.top:s.bot], rs[s.top+1:s.bot+1])
		rs[s.bot] = blankRow(s.cols, s.st)
	}
}

func (s *Screen) scrollDown(n int) {
	rs := s.cur.rows
	for range min(n, s.bot-s.top+1) {
		copy(rs[s.top+1:s.bot+1], rs[s.top:s.bot])
		rs[s.top] = blankRow(s.cols, s.st)
	}
}

// csiParams splits "1;2" into numbers; missing values are 0.
func csiParams(p string) []int {
	if p == "" {
		return nil
	}
	parts := strings.Split(p, ";")
	out := make([]int, len(parts))
	for i, s := range parts {
		if j := strings.IndexByte(s, ':'); j >= 0 {
			s = s[:j]
		}
		out[i], _ = strconv.Atoi(s)
	}
	return out
}

func (s *Screen) csi(final byte) {
	raw := string(s.params)
	private := strings.HasPrefix(raw, "?")
	raw = strings.TrimLeft(raw, "?>=<")
	raw = strings.TrimRight(raw, " !\"#$%&'*+,-./")
	ps := csiParams(raw)
	arg := func(i, def int) int {
		if i < len(ps) && ps[i] > 0 {
			return ps[i]
		}
		return def
	}
	n := arg(0, 1)
	s.pending = false
	switch final {
	case 'A':
		s.y = max(s.y-n, s.minY())
	case 'B', 'e':
		s.y = min(s.y+n, s.maxY())
	case 'C', 'a':
		s.x = min(s.x+n, s.cols-1)
	case 'D':
		s.x = max(s.x-n, 0)
	case 'E':
		s.x, s.y = 0, min(s.y+n, s.maxY())
	case 'F':
		s.x, s.y = 0, max(s.y-n, s.minY())
	case 'G', '`':
		s.x = min(n-1, s.cols-1)
	case 'd':
		s.y = min(n-1, s.rows-1)
	case 'H', 'f':
		s.y, s.x = min(arg(0, 1)-1, s.rows-1), min(arg(1, 1)-1, s.cols-1)
	case 'J':
		s.eraseDisplay(arg(0, 0))
	case 'K':
		s.eraseLine(arg(0, 0))
	case 'L':
		if s.y >= s.top && s.y <= s.bot {
			top := s.top
			s.top = s.y
			s.scrollDown(n)
			s.top = top
		}
	case 'M':
		if s.y >= s.top && s.y <= s.bot {
			top := s.top
			s.top = s.y
			s.scrollUpNoHistory(n)
			s.top = top
		}
	case 'P':
		r := s.cur.rows[s.y]
		n = min(n, s.cols-s.x)
		copy(r[s.x:], r[s.x+n:])
		for i := s.cols - n; i < s.cols; i++ {
			r[i] = cell{r: ' ', st: style{bg: s.st.bg}}
		}
	case '@':
		r := s.cur.rows[s.y]
		n = min(n, s.cols-s.x)
		copy(r[s.x+n:], r[s.x:s.cols-n])
		for i := s.x; i < s.x+n; i++ {
			r[i] = cell{r: ' ', st: style{bg: s.st.bg}}
		}
	case 'X':
		r := s.cur.rows[s.y]
		for i := s.x; i < min(s.x+n, s.cols); i++ {
			r[i] = cell{r: ' ', st: style{bg: s.st.bg}}
		}
	case 'S':
		s.scrollUp(n)
	case 'T':
		s.scrollDown(n)
	case 'r':
		top, bot := arg(0, 1)-1, arg(1, s.rows)-1
		if top < bot && bot < s.rows {
			s.top, s.bot = top, bot
			s.x, s.y = 0, 0
		}
	case 'm':
		s.sgr(ps)
	case 's':
		s.saved = savedCursor{s.x, s.y, s.st}
	case 'u':
		s.x, s.y = min(s.saved.x, s.cols-1), min(s.saved.y, s.rows-1)
	case 'n':
		if !private && arg(0, 0) == 6 {
			s.replies = fmt.Appendf(s.replies, "\x1b[%d;%dR", s.y+1, s.x+1)
		} else if arg(0, 0) == 5 {
			s.replies = append(s.replies, "\x1b[0n"...)
		}
	case 'c':
		if !strings.HasPrefix(string(s.params), ">") {
			s.replies = append(s.replies, "\x1b[?1;2c"...)
		}
	case 'h', 'l':
		if private {
			for _, p := range ps {
				s.privateMode(p, final == 'h')
			}
		}
	}
}

// minY and maxY bound vertical cursor moves to the scroll region when the
// cursor is inside it.
func (s *Screen) minY() int {
	if s.y >= s.top {
		return s.top
	}
	return 0
}

func (s *Screen) maxY() int {
	if s.y <= s.bot {
		return s.bot
	}
	return s.rows - 1
}

func (s *Screen) scrollUpNoHistory(n int) {
	rs := s.cur.rows
	for range min(n, s.bot-s.top+1) {
		copy(rs[s.top:s.bot], rs[s.top+1:s.bot+1])
		rs[s.bot] = blankRow(s.cols, s.st)
	}
}

func (s *Screen) eraseDisplay(mode int) {
	rs := s.cur.rows
	switch mode {
	case 0:
		s.eraseLine(0)
		for y := s.y + 1; y < s.rows; y++ {
			rs[y] = blankRow(s.cols, s.st)
		}
	case 1:
		s.eraseLine(1)
		for y := 0; y < s.y; y++ {
			rs[y] = blankRow(s.cols, s.st)
		}
	case 2, 3:
		for y := range rs {
			rs[y] = blankRow(s.cols, s.st)
		}
		if mode == 3 {
			s.scrollback = nil
		}
	}
}

func (s *Screen) eraseLine(mode int) {
	r := s.cur.rows[s.y]
	from, to := s.x, s.cols
	switch mode {
	case 1:
		from, to = 0, s.x+1
	case 2:
		from = 0
	}
	for i := from; i < to; i++ {
		r[i] = cell{r: ' ', st: style{bg: s.st.bg}}
	}
}

func (s *Screen) privateMode(p int, on bool) {
	switch p {
	case 1:
		s.appCursor = on
	case 25:
		s.hideCursor = !on
	case 2004:
		s.bracketedPaste = on
	case 47, 1047, 1049:
		if on == (s.cur == &s.alt) {
			return
		}
		if on {
			s.mainSaved = savedCursor{s.x, s.y, s.st}
			s.alt.rows = s.blankRows(s.rows)
			s.cur = &s.alt
		} else {
			s.cur = &s.main
			s.x, s.y, s.st = min(s.mainSaved.x, s.cols-1), min(s.mainSaved.y, s.rows-1), s.mainSaved.st
		}
		s.top, s.bot = 0, s.rows-1
		s.pending = false
	}
}

func (s *Screen) sgr(ps []int) {
	if len(ps) == 0 {
		ps = []int{0}
	}
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		switch {
		case p == 0:
			s.st = style{}
		case p >= 1 && p <= 9 && p != 6:
			bit := map[int]uint8{1: attrBold, 2: attrDim, 3: attrItalic, 4: attrUnderline, 5: attrBlink, 7: attrReverse, 8: attrHidden, 9: attrStrike}[p]
			s.st.attrs |= bit
		case p == 22:
			s.st.attrs &^= attrBold | attrDim
		case p == 23:
			s.st.attrs &^= attrItalic
		case p == 24:
			s.st.attrs &^= attrUnderline
		case p == 25:
			s.st.attrs &^= attrBlink
		case p == 27:
			s.st.attrs &^= attrReverse
		case p == 28:
			s.st.attrs &^= attrHidden
		case p == 29:
			s.st.attrs &^= attrStrike
		case p >= 30 && p <= 37, p >= 90 && p <= 97:
			s.st.fg = strconv.Itoa(p)
		case p == 39:
			s.st.fg = ""
		case p >= 40 && p <= 47, p >= 100 && p <= 107:
			s.st.bg = strconv.Itoa(p)
		case p == 49:
			s.st.bg = ""
		case p == 38 || p == 48:
			var spec string
			switch {
			case i+2 < len(ps) && ps[i+1] == 5:
				spec = fmt.Sprintf("%d;5;%d", p, ps[i+2])
				i += 2
			case i+4 < len(ps) && ps[i+1] == 2:
				spec = fmt.Sprintf("%d;2;%d;%d;%d", p, ps[i+2], ps[i+3], ps[i+4])
				i += 4
			default:
				i = len(ps)
				continue
			}
			if p == 38 {
				s.st.fg = spec
			} else {
				s.st.bg = spec
			}
		}
	}
}
