package tui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// Urgent chat items lead with a one-sentence summary in red; any detail
// follows in white. An item is urgent when Jev triage flags it, or when the
// orchestrator starts its message with "‼ " (or the ASCII "!! "), the same
// marker the narrator uses for needs-you lines.

var (
	sUrgent  = lipgloss.NewStyle().Foreground(cAlert).Bold(true)
	sDetails = lipgloss.NewStyle().Foreground(cBright)
)

// urgentMark strips a leading urgent marker and reports whether there was one.
func urgentMark(text string) (string, bool) {
	t := strings.TrimLeft(text, " \t")
	for _, mark := range []string{"‼ ", "!! "} {
		if rest, ok := strings.CutPrefix(t, mark); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return text, false
}

// splitSummary splits text after its first sentence, or its first line if
// that comes sooner. A period followed by a lowercase word (as in "e.g. x")
// doesn't end a sentence, and nothing inside a code span does.
func splitSummary(text string) (summary, details string) {
	text = strings.TrimSpace(text)
	r := []rune(text)
	inCode := false
	for i := 0; i < len(r); i++ {
		switch c := r[i]; {
		case c == '`':
			inCode = !inCode
		case c == '\n':
			return strings.TrimSpace(string(r[:i])), strings.TrimSpace(string(r[i+1:]))
		case inCode:
		case c == '.' || c == '!' || c == '?':
			end := i + 1
			for end < len(r) && strings.ContainsRune(".!?*_)\"'", r[end]) {
				end++
			}
			if end == len(r) {
				return text, ""
			}
			if !unicode.IsSpace(r[end]) {
				i = end - 1
				continue
			}
			next := end
			for next < len(r) && unicode.IsSpace(r[next]) {
				next++
			}
			if next < len(r) && unicode.IsLower(r[next]) {
				i = end - 1
				continue
			}
			return string(r[:end]), strings.TrimSpace(string(r[end:]))
		}
	}
	return text, ""
}

// renderUrgent renders text as a red summary sentence over white details,
// both wrapped to width.
func renderUrgent(text string, width int) string {
	sum, details := splitSummary(text)
	out := renderMarkdown(sum, width, sUrgent)
	if details != "" {
		out += "\n" + renderMarkdown(details, width, sDetails)
	}
	return out
}
