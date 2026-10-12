package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// Folding (#402): an orchestrator message shows its headline, the first
// paragraph, and folds the rest (test names, file lists, reasoning) behind
// alt+d. Saddle's own relay lines fold the same way: a done notice shows
// which task finished, not the worker's summary, and a digest line keeps
// the "saddle notices --all" hint to its folded part. Consecutive digest
// lines merge into one rolling line.

// foldHint is the marker under a headline whose details are folded.
const foldHint = "▸ details · alt+d"

// doneNoticeRe matches the chat line of a task's done notice; the worker's
// summary follows the colon.
var doneNoticeRe = regexp.MustCompile(`^(\S+ ".*") is done and queued in the merge train: (.+)$`)

// splitHeadline splits text at its first blank line outside a code fence:
// the headline paragraph, and the details after it, if any.
func splitHeadline(text string) (head, details string) {
	text = strings.TrimSpace(text)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	inFence := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
		}
		if t == "" && !inFence && i > 0 {
			return strings.TrimSpace(strings.Join(lines[:i], "\n")), strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}
	return text, ""
}

// foldText is what a chat line shows folded and what it folds away.
// details is "" for a line that doesn't fold. Needs-you messages never
// fold: the owner should see all of them.
func foldText(c chatLine) (head, details string) {
	switch c.role {
	case store.ChatAssistant:
		if _, urgent := urgentMark(c.text); urgent || c.attn == attnUrgent {
			return c.text, ""
		}
		return splitHeadline(c.text)
	case store.ChatEvent:
		if m := doneNoticeRe.FindStringSubmatch(c.text); m != nil {
			return m[1] + " is done and queued to land.", m[2]
		}
		if app.IsDigest(c.text) {
			return c.text, app.DigestHint
		}
	}
	return c.text, ""
}

// renderFolded renders a chat line with its details folded, or shown dimmed
// below the headline when open. Both wrap to the chat width.
func renderFolded(c chatLine, w int, wrap lipgloss.Style, open bool) string {
	head, details := foldText(c)
	if details == "" {
		return renderLine(c, w, wrap)
	}
	h := c
	h.text = head
	out := renderLine(h, w, wrap)
	trail := strings.HasSuffix(out, "\n")
	out = strings.TrimSuffix(out, "\n") + "\n"
	if open {
		out += renderMarkdown(details, w-2, sDim)
	} else {
		out += sFaint.Render(wrap.Render("  " + foldHint))
	}
	if trail {
		out += "\n"
	}
	return out
}

// appendChat adds c to the chat, merging a digest line into the digest line
// right before it.
func appendChat(chat []chatLine, c chatLine) []chatLine {
	if n := len(chat); n > 0 && c.role == store.ChatEvent && chat[n-1].role == store.ChatEvent {
		if merged, ok := app.MergeDigests(chat[n-1].text, c.text); ok {
			chat[n-1] = chatLine{role: store.ChatEvent, text: merged}
			return chat
		}
	}
	return append(chat, c)
}

// toggleDetails opens or folds the details under every headline.
func (m *model) toggleDetails() {
	m.details = !m.details
	for i := range m.chat {
		m.chat[i].out = ""
	}
	m.renderChat()
}
