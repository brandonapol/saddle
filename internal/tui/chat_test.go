package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/store"
)

// renderPlain renders one chat line at width w and checks that no line is
// wider than w.
func renderPlain(t *testing.T, c chatLine, w int) string {
	t.Helper()
	out := renderLine(c, w, lipgloss.NewStyle().Width(w-2))
	for _, l := range strings.Split(out, "\n") {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("line %q is %d wide, max %d", l, lw, w)
		}
	}
	return out
}

func TestChatRendersMarkdownInEveryRole(t *testing.T) {
	text := "**t3** landed; run `make check` before **t4**."
	for _, role := range []string{store.ChatAssistant, store.ChatNarrator, store.ChatEvent} {
		for _, w := range []int{30, 36, 40} {
			got := renderPlain(t, chatLine{role: role, text: text}, w)
			if strings.Contains(got, "**") || strings.Contains(got, "`") {
				t.Errorf("%s at width %d shows raw markdown:\n%s", role, w, got)
			}
			if !strings.Contains(strings.Join(strings.Fields(got), " "), "t3 landed; run make check") {
				t.Errorf("%s at width %d lost text:\n%s", role, w, got)
			}
		}
	}
	// The user's own message stays as typed.
	if got := renderPlain(t, chatLine{role: store.ChatUser, text: text}, 40); !strings.Contains(got, "**t3**") {
		t.Errorf("user line should be shown as typed:\n%s", got)
	}
}
