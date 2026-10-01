package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestParseInline(t *testing.T) {
	cases := []struct {
		in   string
		want []mdSpan
	}{
		{"plain text", []mdSpan{{"plain text", 0}}},
		{"a **bold** b", []mdSpan{{"a ", 0}, {"bold", mdBold}, {" b", 0}}},
		{"__bold__", []mdSpan{{"bold", mdBold}}},
		{"an *em* word", []mdSpan{{"an ", 0}, {"em", mdItalic}, {" word", 0}}},
		{"run `go test` now", []mdSpan{{"run ", 0}, {"go test", mdCode}, {" now", 0}}},
		{"``a ` b``", []mdSpan{{"a ` b", mdCode}}},
		{"**bold `code`**", []mdSpan{{"bold ", mdBold}, {"code", mdBold | mdCode}}},
		{"***both***", []mdSpan{{"both", mdBold | mdItalic}}},
		{"**a *b* c**", []mdSpan{{"a ", mdBold}, {"b", mdBold | mdItalic}, {" c", mdBold}}},
		{"`**not bold**`", []mdSpan{{"**not bold**", mdCode}}},
		// Unbalanced or non-emphasis markers stay as typed.
		{"**open", []mdSpan{{"**open", 0}}},
		{"close**", []mdSpan{{"close**", 0}}},
		{"a ** b ** c", []mdSpan{{"a ** b ** c", 0}}},
		{"2 * 3 * 4", []mdSpan{{"2 * 3 * 4", 0}}},
		{"`unclosed", []mdSpan{{"`unclosed", 0}}},
		{"snake_case_name", []mdSpan{{"snake_case_name", 0}}},
		{"**bold** and **open", []mdSpan{{"bold", mdBold}, {" and **open", 0}}},
		{`\*\*literal\*\*`, []mdSpan{{"**literal**", 0}}},
	}
	for _, c := range cases {
		if got := parseInline(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseInline(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// plain renders with no styling, which is what lipgloss emits without a
// terminal, so tests see the text and layout only.
func plain(s string, w int) []string {
	return strings.Split(renderMarkdown(s, w, lipgloss.NewStyle()), "\n")
}

func TestRenderMarkdownStripsMarkers(t *testing.T) {
	got := plain("This is **important** and `code`.", 80)
	want := []string{"This is important and code."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderMarkdownBlocks(t *testing.T) {
	in := "## Plan\n- first **item**\n  - nested\n* star\n3. third\n> quoted\n\n```\nfunc x() {}\n```\n---"
	got := plain(in, 40)
	want := []string{
		"Plan",
		"• first item",
		"  • nested",
		"• star",
		"3. third",
		"│ quoted",
		"",
		"func x() {}",
		strings.Repeat("─", 24),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestRenderMarkdownWraps(t *testing.T) {
	got := plain("- one two **three four** five six seven", 14)
	want := []string{
		"• one two",
		"  three four",
		"  five six",
		"  seven",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, l := range plain("a verylongunbrokenwordthatdoesnotfit end", 10) {
		if w := lipgloss.Width(l); w > 10 {
			t.Errorf("line %q is %d wide, max 10", l, w)
		}
	}
	for _, l := range plain("```\n"+strings.Repeat("x", 25)+"\n```", 10) {
		if w := lipgloss.Width(l); w > 10 {
			t.Errorf("code line %q is %d wide, max 10", l, w)
		}
	}
}

func TestRenderMarkdownStyles(t *testing.T) {
	// Styles are applied per span on top of the base style.
	base := lipgloss.NewStyle()
	if !styleFor(base, mdBold).GetBold() || styleFor(base, 0).GetBold() {
		t.Error("bold span should be bold, plain should not")
	}
	if styleFor(base, mdCode).GetForeground() != cCode {
		t.Error("code span should use the code color")
	}
}
