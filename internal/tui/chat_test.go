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

func TestSplitSummary(t *testing.T) {
	cases := []struct{ in, sum, rest string }{
		{"t3 is blocked on a conflict. It touches go.mod.", "t3 is blocked on a conflict.", "It touches go.mod."},
		{"Only one sentence", "Only one sentence", ""},
		{"Done!", "Done!", ""},
		{"CI failed for t4\n```\nFAIL x\n```", "CI failed for t4", "```\nFAIL x\n```"},
		{"Pick one, e.g. rebase or merge? Both work.", "Pick one, e.g. rebase or merge?", "Both work."},
		{"**t3 needs a decision.** Options: a or b.", "**t3 needs a decision.**", "Options: a or b."},
		{"Run `go test ./... ` first. Then land.", "Run `go test ./... ` first.", "Then land."},
		{"v1.2 broke the build. Fix it.", "v1.2 broke the build.", "Fix it."},
	}
	for _, c := range cases {
		sum, rest := splitSummary(c.in)
		if sum != c.sum || rest != c.rest {
			t.Errorf("splitSummary(%q) = %q, %q; want %q, %q", c.in, sum, rest, c.sum, c.rest)
		}
	}
}

func TestUrgentMark(t *testing.T) {
	for in, want := range map[string]string{"‼ t3 is stuck.": "t3 is stuck.", "!! t3 is stuck.": "t3 is stuck.", "  ‼ x": "x"} {
		if got, ok := urgentMark(in); !ok || got != want {
			t.Errorf("urgentMark(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"t3 is fine", "!!!", "‼", "Wow!! nice"} {
		if _, ok := urgentMark(in); ok {
			t.Errorf("urgentMark(%q) should not be urgent", in)
		}
	}
}

// withColor renders in truecolor so tests can see which color each part got.
func withColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0) // termenv.TrueColor, without importing termenv
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// fg is the SGR parameter lipgloss emits for foreground c, like "38;2;r;g;b".
func fg(c lipgloss.Color) string {
	s := lipgloss.NewStyle().Foreground(c).Render("x")
	return strings.TrimSuffix(strings.TrimPrefix(s, "\x1b["), "mx\x1b[0m")
}

// linesWith returns the rendered lines containing s.
func linesWith(out, s string) []string {
	var ls []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			ls = append(ls, l)
		}
	}
	return ls
}

func TestUrgentRendersRedSummaryWhiteDetails(t *testing.T) {
	withColor(t)
	ansiRed, ansiWhite := fg(cAlert), fg(cBright)
	if !strings.HasPrefix(ansiRed, "38;") || !strings.HasPrefix(ansiWhite, "38;") {
		t.Fatalf("no color codes: %q %q", ansiRed, ansiWhite)
	}
	text := "**t3** is blocked on a merge conflict in internal/app. The other side renamed `Spawn`; rebase onto integration or ask t5 to keep the old name."
	for _, c := range []chatLine{
		{role: store.ChatAssistant, text: text, attn: attnUrgent}, // flagged by Jev
		{role: store.ChatAssistant, text: "‼ " + text},            // flagged by the orchestrator
		{role: store.ChatAssistant, text: "!! " + text},
	} {
		for _, w := range []int{30, 40, 70} {
			out := renderLine(c, w, lipgloss.NewStyle().Width(w-2))
			for _, l := range strings.Split(out, "\n") {
				if lw := lipgloss.Width(l); lw > w {
					t.Errorf("width %d: line %q is %d wide", w, l, lw)
				}
			}
			if strings.Contains(out, "‼") || strings.Contains(out, "!!") || strings.Contains(out, "**") {
				t.Errorf("width %d: marker or markdown left in:\n%s", w, out)
			}
			// The summary wraps (never truncates) and every piece of it is red.
			for _, word := range []string{"blocked", "conflict", "internal/app."} {
				ls := linesWith(out, word)
				if len(ls) != 1 || !strings.Contains(ls[0], ansiRed) {
					t.Errorf("width %d: summary word %q not red: %q", w, word, ls)
				}
			}
			for _, word := range []string{"renamed", "integration", "name."} {
				ls := linesWith(out, word)
				if len(ls) != 1 || strings.Contains(ls[0], ansiRed) || !strings.Contains(ls[0], ansiWhite) {
					t.Errorf("width %d: detail word %q should be white: %q", w, word, ls)
				}
			}
		}
	}
	// Non-urgent replies stay in the normal style.
	out := renderLine(chatLine{role: store.ChatAssistant, text: text}, 40, lipgloss.NewStyle())
	if strings.Contains(out, ansiRed) {
		t.Errorf("a normal reply should have no red:\n%s", out)
	}
}

func TestNarratorNeedsYouSummaryRed(t *testing.T) {
	withColor(t)
	ansiRed := fg(cAlert)
	out := renderLine(chatLine{role: store.ChatNarrator, text: "‼ t2: waiting on Bash permission. It wants to run rm -rf build."}, 36, lipgloss.NewStyle())
	if ls := linesWith(out, "waiting"); len(ls) != 1 || !strings.Contains(ls[0], ansiRed) {
		t.Errorf("summary not red: %q", ls)
	}
	if ls := linesWith(out, "wants"); len(ls) != 1 || strings.Contains(ls[0], ansiRed) {
		t.Errorf("detail should not be red: %q", ls)
	}
}
