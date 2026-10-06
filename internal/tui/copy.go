package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// Copying text out of saddle's panes. The TUI captures the mouse, which
// blocks the terminal's own selection (shift+drag still gets it), so it
// offers two ways of its own: copy mode (ctrl+y) picks lines with the
// keyboard, and a drag inside a pane copies the rows it covered on release.
// Both put plain text on the clipboard: no colors, no box borders.

// copyState is copy mode: a snapshot of one pane's lines with a cursor and
// an optional selection.
type copyState struct {
	on     bool
	srcs   []string // panes that can be copied from, in tab order
	src    int
	lines  []string
	cur    int
	anchor int // the selection's other end; -1 when nothing is selected
	top    int // first line shown
}

var sCopySel = lipgloss.NewStyle().Background(lipgloss.Color("#3A4552")).Foreground(cBright)

// stripCodes drops escape sequences from s.
func stripCodes(s string) string { return tmux.StripANSI(s) }

// plainLines splits s into lines without escape codes or trailing blanks.
func plainLines(s string) []string {
	lines := strings.Split(stripCodes(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// boxLines is the text inside rendered boxes: border rows dropped, the
// side borders cut off. Boxes stacked in one column work too.
func boxLines(s string) []string {
	var out []string
	for _, l := range plainLines(s) {
		if inner, ok := boxRow(l); ok {
			out = append(out, inner)
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// boxRow is one rendered box row's content, or false for a border row.
func boxRow(l string) (string, bool) {
	t := strings.TrimSpace(l)
	if strings.HasPrefix(t, "╭") || strings.HasPrefix(t, "╰") {
		return "", false
	}
	t = strings.TrimPrefix(t, "│")
	t = strings.TrimSuffix(strings.TrimRight(t, " "), "│")
	t = strings.TrimRight(t, " ")
	if t != "" && strings.Trim(t, "─") == "" {
		return "", false // a rule inside a box, like the one above the chat input
	}
	return t, true
}

// dedent drops the indent every non-blank line shares.
func dedent(lines []string) []string {
	cut := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if cut < 0 || n < cut {
			cut = n
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= cut && cut > 0 {
			l = l[cut:]
		}
		out[i] = l
	}
	return out
}

// copySources lists what copy mode can copy from in the current view.
func (m *model) copySources() []string {
	var srcs []string
	switch m.view {
	case viewPlan:
		srcs = []string{"plan"}
	case viewMerge:
		srcs = []string{"merge"}
	default:
		peek := "peek"
		if m.briefOn {
			peek = "brief"
		}
		srcs = []string{"chat", peek}
		if m.focus == focusTasks {
			srcs = []string{peek, "chat"}
		}
	}
	if m.term != nil && m.termOpen {
		srcs = append(srcs, "terminal")
	}
	return srcs
}

// copyLines is a source's text. The chat is the messages as written, so
// copied text has no hard wraps at the pane's width.
func (m *model) copyLines(src string) []string {
	switch src {
	case "chat":
		var out []string
		for _, c := range m.chat {
			text := c.text
			switch c.role {
			case store.ChatUser:
				out = append(out, "you")
			case store.ChatAssistant:
				out = append(out, "saddle")
				if t, ok := urgentMark(text); ok {
					text = t
				}
			case store.ChatTool:
				text = "⚙ " + text
			}
			out = append(out, plainLines(text)...)
			out = append(out, "")
		}
		return plainLines(strings.Join(out, "\n"))
	case "peek":
		return plainLines(m.peek)
	case "brief":
		_, body := m.briefPane(max(m.width-2, 10))
		return plainLines(body)
	case "plan":
		return boxLines(m.viewPlan(m.width, max(m.bodyHeight(), 3)))
	case "merge":
		return boxLines(m.viewMerge(m.width, max(m.bodyHeight(), 3)))
	case "terminal":
		if m.term != nil {
			return plainLines(m.term.Text())
		}
	}
	return nil
}

// enterCopy starts copy mode on the focused pane, cursor on its last line.
func (m *model) enterCopy() {
	m.cp = copyState{on: true, srcs: m.copySources()}
	m.input.Blur()
	m.loadCopy()
}

func (m *model) loadCopy() {
	m.cp.lines = m.copyLines(m.cp.srcs[m.cp.src])
	m.cp.cur, m.cp.anchor, m.cp.top = max(len(m.cp.lines)-1, 0), -1, 0
}

func (m *model) exitCopy() {
	m.cp = copyState{}
	if m.focus == focusChat && m.view == viewControl {
		m.input.Focus()
	}
}

// copyKey handles a key in copy mode. Every key stops here.
func (m *model) copyKey(k tea.KeyMsg) tea.Cmd {
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !k.Paste {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			if m.cp.on {
				cmds = append(cmds, m.copyKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: k.Alt}))
			}
		}
		return tea.Batch(cmds...)
	}
	keys := m.keys
	n := len(m.cp.lines)
	move := func(d int) { m.cp.cur = min(max(m.cp.cur+d, 0), max(n-1, 0)) }
	half := max(m.bodyHeight()/2, 1)
	switch {
	case key.Matches(k, keys.CopyExit, keys.CopyMode):
		m.exitCopy()
	case key.Matches(k, keys.Down):
		move(1)
	case key.Matches(k, keys.Up):
		move(-1)
	case key.Matches(k, keys.PageDown, keys.CopyHalfDown):
		move(half)
	case key.Matches(k, keys.PageUp, keys.CopyHalfUp):
		move(-half)
	case key.Matches(k, keys.CopyTop):
		m.cp.cur = 0
	case key.Matches(k, keys.CopyBottom):
		move(n)
	case key.Matches(k, keys.CopySelect):
		if m.cp.anchor >= 0 {
			m.cp.anchor = -1
		} else {
			m.cp.anchor = m.cp.cur
		}
	case key.Matches(k, keys.CopySource):
		d := 1
		if k.Type == tea.KeyShiftTab {
			d = len(m.cp.srcs) - 1
		}
		m.cp.src = (m.cp.src + d) % len(m.cp.srcs)
		m.loadCopy()
	case key.Matches(k, keys.CopyAll):
		lines := m.cp.lines
		m.exitCopy()
		return m.copyText(lines)
	case key.Matches(k, keys.CopyYank):
		lo, hi := m.copyRange()
		var lines []string
		if n > 0 {
			lines = m.cp.lines[lo : hi+1]
		}
		m.exitCopy()
		return m.copyText(lines)
	}
	return nil
}

// copyRange is the selected lines, or the cursor line with no selection.
func (m *model) copyRange() (lo, hi int) {
	if m.cp.anchor < 0 {
		return m.cp.cur, m.cp.cur
	}
	return min(m.cp.anchor, m.cp.cur), max(m.cp.anchor, m.cp.cur)
}

// copyText puts lines on the clipboard off the UI goroutine and flashes
// what happened.
func (m *model) copyText(lines []string) tea.Cmd {
	lines = dedent(lines)
	text := strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		return flashCmd("nothing to copy")
	}
	cl := systemClipboard()
	if m.clip != nil {
		cl = *m.clip
	}
	n := len(lines)
	return func() tea.Msg { return flashMsg(cl.copy(text).flash(n)) }
}

// viewCopy shows the source's lines full width, the cursor and selection
// highlighted.
func (m *model) viewCopy(w, h int) string {
	cp := &m.cp
	rows := max(h-2, 1)
	if cp.cur < cp.top {
		cp.top = cp.cur
	}
	if cp.cur >= cp.top+rows {
		cp.top = cp.cur - rows + 1
	}
	lo, hi := m.copyRange()
	var out []string
	if len(cp.lines) == 0 {
		out = append(out, sDim.Render(" Nothing to copy here. tab: next pane, esc: back"))
	}
	for i := cp.top; i < min(cp.top+rows, len(cp.lines)); i++ {
		mark := " "
		if i == cp.cur {
			mark = "›"
		}
		line := mark + " " + truncate(cp.lines[i], w-5)
		switch {
		case i >= lo && i <= hi && (cp.anchor >= 0 || i == cp.cur):
			line = sCopySel.Width(w - 2).Render(line)
		default:
			line = sText.Render(line)
		}
		out = append(out, line)
	}
	title := fmt.Sprintf("COPY · %s · %d/%d", cp.srcs[cp.src], min(cp.cur+1, len(cp.lines)), len(cp.lines))
	if cp.anchor >= 0 {
		title += fmt.Sprintf(" · %d selected", hi-lo+1)
	}
	return box(title, w, h, true, strings.Join(out, "\n"))
}

// Mouse selection.

// screenPane is a pane as last drawn: its top-left corner and rendering.
type screenPane struct {
	name string
	x, y int
	text string
}

// mouseDrag is a left-button drag in progress, in rows of one pane.
type mouseDrag struct {
	pane     string
	from, to int
	moved    bool
}

// markPane records a pane drawn at x, y for mouse selection, and highlights
// the rows a drag over it covers.
func (m *model) markPane(name string, x, y int, s string) string {
	m.panes = append(m.panes, screenPane{name: name, x: x, y: y, text: s})
	d := m.drag
	if d == nil || d.pane != name || !d.moved {
		return s
	}
	lines := strings.Split(s, "\n")
	lo, hi := min(d.from, d.to), max(d.from, d.to)
	for i := max(lo, 0); i <= hi && i < len(lines); i++ {
		lines[i] = sCopySel.Render(stripCodes(lines[i]))
	}
	return strings.Join(lines, "\n")
}

func (m *model) paneAt(x, y int) (screenPane, bool) {
	for _, p := range m.panes {
		if x >= p.x && x < p.x+lipgloss.Width(p.text) && y >= p.y && y < p.y+lipgloss.Height(p.text) {
			return p, true
		}
	}
	return screenPane{}, false
}

func (m *model) paneNamed(name string) (screenPane, bool) {
	for _, p := range m.panes {
		if p.name == name {
			return p, true
		}
	}
	return screenPane{}, false
}

// mouseSelect handles left-button drags: press starts one in the pane under
// the pointer, motion extends it, release copies the rows it covered. A
// click that never moved copies nothing. It reports whether it took msg.
func (m *model) mouseSelect(msg tea.MouseMsg) (tea.Cmd, bool) {
	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		p, ok := m.paneAt(msg.X, msg.Y)
		if !ok {
			m.drag = nil
			return nil, false
		}
		m.drag = &mouseDrag{pane: p.name, from: msg.Y - p.y, to: msg.Y - p.y}
		return nil, true
	case m.drag == nil:
		return nil, false
	case msg.Action == tea.MouseActionMotion:
		if p, ok := m.paneNamed(m.drag.pane); ok {
			m.drag.to = min(max(msg.Y-p.y, 0), lipgloss.Height(p.text)-1)
			m.drag.moved = true
		}
		return nil, true
	case msg.Action == tea.MouseActionRelease:
		d := m.drag
		m.drag = nil
		p, ok := m.paneNamed(d.pane)
		if !ok || !d.moved {
			return nil, true
		}
		rows := strings.Split(p.text, "\n")
		lo, hi := min(d.from, d.to), min(max(d.from, d.to), len(rows)-1)
		var lines []string
		for _, r := range rows[lo : hi+1] {
			if l, ok := boxRow(stripCodes(r)); ok {
				lines = append(lines, l)
			}
		}
		return m.copyText(lines), true
	}
	return nil, false
}
