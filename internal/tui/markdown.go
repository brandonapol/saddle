package tui

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// A small markdown pass for the chat: inline bold, italic and code, plus
// headings, lists, quotes and fenced code blocks. It is hand-rolled so that it
// wraps to the narrow chat column itself, and anything it doesn't understand
// (like an unclosed **) is shown as typed.

type mdStyle uint8

const (
	mdBold mdStyle = 1 << iota
	mdItalic
	mdCode
	mdDim
)

type mdSpan struct {
	text  string
	style mdStyle
}

var (
	cCode     = lipgloss.Color("#E6C38A")
	mdList    = regexp.MustCompile(`^(\s*)([-*+]|\d{1,3}[.)])\s+(.*)$`)
	mdHeading = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	mdRule    = regexp.MustCompile(`^\s*([-*_])(\s*[-*_]){2,}\s*$`)
)

// renderMarkdown renders text wrapped to width, with base as the style of
// plain text.
func renderMarkdown(text string, width int, base lipgloss.Style) string {
	if width < 8 {
		width = 8
	}
	var out []string
	inFence := false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			for _, l := range hardWrap(strings.TrimRight(line, " \t"), width) {
				out = append(out, styleFor(base, mdCode).Render(l))
			}
			continue
		}
		out = append(out, renderBlockLine(line, width, base)...)
	}
	return strings.Join(out, "\n")
}

// renderBlockLine renders one source line outside a code fence.
func renderBlockLine(line string, width int, base lipgloss.Style) []string {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return []string{""}
	case mdRule.MatchString(line):
		return []string{styleFor(base, mdDim).Render(strings.Repeat("─", min(width, 24)))}
	}
	if m := mdHeading.FindStringSubmatch(trimmed); m != nil {
		return wrapSpans(addStyle(parseInline(m[1]), mdBold), width, "", "", base)
	}
	if strings.HasPrefix(trimmed, ">") {
		body := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		bar := styleFor(base, mdDim).Render("│ ")
		return wrapSpans(addStyle(parseInline(body), mdDim), width, bar, bar, base)
	}
	if m := mdList.FindStringSubmatch(line); m != nil {
		indent := strings.Repeat(" ", min(len(strings.ReplaceAll(m[1], "\t", "  "))/2, 3)*2)
		marker := m[2]
		if strings.ContainsAny(marker, "-*+") {
			marker = "•"
		}
		first := indent + marker + " "
		rest := strings.Repeat(" ", lipgloss.Width(first))
		return wrapSpans(parseInline(m[3]), width, first, rest, base)
	}
	return wrapSpans(parseInline(trimmed), width, "", "", base)
}

func addStyle(spans []mdSpan, s mdStyle) []mdSpan {
	for i := range spans {
		spans[i].style |= s
	}
	return spans
}

// parseInline splits s into styled spans. Markers without a matching close
// stay in the text.
func parseInline(s string) []mdSpan {
	var spans []mdSpan
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			spans = append(spans, mdSpan{text: plain.String()})
			plain.Reset()
		}
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\\' && i+1 < len(r) && strings.ContainsRune("\\`*_", r[i+1]):
			plain.WriteRune(r[i+1])
			i++
			continue
		case c == '`':
			n := run(r, i, '`')
			if j := findCode(r, i+n, n); j >= 0 {
				flush()
				code := string(r[i+n : j])
				if t := strings.TrimSpace(code); t != "" {
					code = t
				}
				spans = append(spans, mdSpan{text: code, style: mdCode})
				i = j + n - 1
				continue
			}
			plain.WriteString(string(r[i : i+n]))
			i += n - 1
			continue
		case c == '*' || c == '_':
			n := min(run(r, i, c), 2)
			if opens(r, i, n) {
				if j := findEmph(r, i+n, c, n); j >= 0 {
					flush()
					st := mdItalic
					if n == 2 {
						st = mdBold
					}
					spans = append(spans, addStyle(parseInline(string(r[i+n:j])), st)...)
					i = j + n - 1
					continue
				}
			}
			// Copy the whole run so a later marker can't pair with half of it.
			n = run(r, i, c)
			plain.WriteString(string(r[i : i+n]))
			i += n - 1
			continue
		}
		plain.WriteRune(c)
	}
	flush()
	return spans
}

// run counts how many times c repeats from r[i].
func run(r []rune, i int, c rune) int {
	n := 0
	for i+n < len(r) && r[i+n] == c {
		n++
	}
	return n
}

// opens reports whether the n-rune emphasis marker at r[i] can open a span:
// it must be followed by a non-space, and an underscore must not sit inside a
// word (snake_case stays as typed).
func opens(r []rune, i, n int) bool {
	if i+n >= len(r) || unicode.IsSpace(r[i+n]) {
		return false
	}
	if r[i] == '_' && i > 0 && isWord(r[i-1]) {
		return false
	}
	return true
}

// findCode finds the backtick run of exactly n that closes a code span.
func findCode(r []rune, from, n int) int {
	for j := from; j < len(r); j++ {
		if r[j] != '`' {
			continue
		}
		k := run(r, j, '`')
		if k == n {
			return j
		}
		j += k - 1
	}
	return -1
}

// findEmph finds where the n-rune emphasis marker c opened before from
// closes. The close must follow a non-space, and an underscore close must not
// be followed by a word character. A *** run can close both a bold and an
// inner italic, so a bold close takes its last two runes.
func findEmph(r []rune, from int, c rune, n int) int {
	for j := from; j < len(r); j++ {
		switch {
		case r[j] == '\\':
			j++
			continue
		case r[j] == '`':
			// Markers inside a code span don't count.
			if k := run(r, j, '`'); findCode(r, j+k, k) >= 0 {
				j = findCode(r, j+k, k) + k - 1
			}
			continue
		case r[j] != c:
			continue
		}
		k := run(r, j, c)
		at := -1
		switch {
		case n == 2 && k >= 2:
			at = j + k - 2
		case n == 1 && (k == 1 || k == 3):
			at = j + k - 1
		}
		intraword := c == '_' && at+n < len(r) && isWord(r[at+n])
		if at > from && !unicode.IsSpace(r[j-1]) && !intraword {
			return at
		}
		j += k - 1
	}
	return -1
}

func isWord(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }

func styleFor(base lipgloss.Style, s mdStyle) lipgloss.Style {
	st := base
	if s&mdBold != 0 {
		st = st.Bold(true)
	}
	if s&mdItalic != 0 {
		st = st.Italic(true)
	}
	if s&mdDim != 0 {
		st = st.Foreground(cDim)
	}
	if s&mdCode != 0 {
		st = st.Foreground(cCode)
	}
	return st
}

// wrapSpans greedily wraps spans to width. first prefixes the first line and
// rest every following one; both count toward the width.
func wrapSpans(spans []mdSpan, width int, first, rest string, base lipgloss.Style) []string {
	type word struct {
		parts []mdSpan
		w     int
	}
	var words []word
	var cur word
	end := func() {
		if len(cur.parts) > 0 {
			words = append(words, cur)
			cur = word{}
		}
	}
	for _, sp := range spans {
		var b strings.Builder
		push := func() {
			if b.Len() > 0 {
				cur.parts = append(cur.parts, mdSpan{text: b.String(), style: sp.style})
				cur.w += lipgloss.Width(b.String())
				b.Reset()
			}
		}
		for _, c := range sp.text {
			if unicode.IsSpace(c) {
				push()
				end()
				continue
			}
			b.WriteRune(c)
		}
		push()
	}
	end()

	render := func(w word) string {
		var b strings.Builder
		for _, p := range w.parts {
			b.WriteString(styleFor(base, p.style).Render(p.text))
		}
		return b.String()
	}
	var lines []string
	var line strings.Builder
	prefix := first
	used := lipgloss.Width(prefix)
	empty := true
	newline := func() {
		lines = append(lines, prefix+line.String())
		line.Reset()
		prefix = rest
		used = lipgloss.Width(prefix)
		empty = true
	}
	avail := func() int { return width - used }
	for _, w := range words {
		gap := 1
		if empty {
			gap = 0
		}
		if !empty && gap+w.w > avail() {
			newline()
			gap = 0
		}
		if w.w > avail() {
			// Too long for any line: break it by runes.
			for _, p := range w.parts {
				for _, c := range p.text {
					cw := lipgloss.Width(string(c))
					if !empty && cw > avail() {
						newline()
					}
					line.WriteString(styleFor(base, p.style).Render(string(c)))
					used += cw
					empty = false
				}
			}
			continue
		}
		if gap > 0 {
			line.WriteString(base.Render(" "))
			used++
		}
		line.WriteString(render(w))
		used += w.w
		empty = false
	}
	if !empty || len(lines) == 0 {
		lines = append(lines, prefix+line.String())
	}
	return lines
}

// hardWrap breaks s into lines of at most width cells, keeping spaces.
func hardWrap(s string, width int) []string {
	var lines []string
	var b strings.Builder
	w := 0
	for _, c := range s {
		cw := lipgloss.Width(string(c))
		if w+cw > width && w > 0 {
			lines = append(lines, b.String())
			b.Reset()
			w = 0
		}
		b.WriteRune(c)
		w += cw
	}
	return append(lines, b.String())
}
