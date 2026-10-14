package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// chatText renders the whole chat log as the viewport would hold it, with
// whitespace collapsed.
func chatText(m *model) string {
	m.renderChat()
	var b strings.Builder
	for _, c := range m.chat {
		b.WriteString(c.out + "\n")
	}
	// Word-wrap at the chat width breaks phrases; compare words.
	return strings.Join(strings.Fields(b.String()), " ")
}

// #402: a worker's done summary, full of test names and identifiers, comes
// through the relay path (done notice, then the orchestrator's reply). The
// chat shows plain headlines; the identifiers sit in folded details that
// alt+d opens.
func TestRelayedDoneSummaryShowsHeadlineAndFoldsDetails(t *testing.T) {
	m := slashModel(t)
	fakeProc(t, m)
	summary := "Added Watcher.Tiers and app.NewAutomerge; covered by TestTierFloorDocsOnly, TestMaxTierGateHolds in internal/automerge/tier_test.go."
	if err := m.app.Store.Notify(app.OrchestratorID, store.NoticeAction,
		fmt.Sprintf("%s %q is done and queued in the merge train: %s\nRun the saddle land tool when you're ready.", "t183", "Risk tier floor", summary)); err != nil {
		t.Fatal(err)
	}
	m.deliver()
	m.handleEvent(orch.Event{Kind: orch.Text, Text: "The risk-tier work is half in: one task finished and is queued to land. Nothing needs you."})

	out := chatText(m)
	for _, want := range []string{`t183 "Risk tier floor" is done and queued to land.`, "The risk-tier work is half in", "Nothing needs you.", foldHint} {
		if !strings.Contains(out, want) {
			t.Errorf("chat lacks %q:\n%s", want, out)
		}
	}
	for _, id := range []string{"TestTierFloorDocsOnly", "Watcher.Tiers", "app.NewAutomerge", "tier_test.go"} {
		if strings.Contains(out, id) {
			t.Errorf("folded chat shows %q:\n%s", id, out)
		}
	}

	m.Update(altKey('d'))
	out = chatText(m)
	for _, want := range []string{"TestTierFloorDocsOnly", "app.NewAutomerge"} {
		if !strings.Contains(out, want) {
			t.Errorf("open details lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, foldHint) {
		t.Errorf("open details still show the fold hint:\n%s", out)
	}
}

// #402: three digest lines in a row render as one rolling line, and the
// notices hint isn't on it until details open.
func TestDigestLinesMergeIntoOne(t *testing.T) {
	m := slashModel(t)
	m.chat = nil
	for _, l := range []string{
		"Since 09:23: 3 PRs merged (#12 #13 #14), restacked 3 times.",
		"Since 09:40: 1 PR merged (#15), 1 task landed (t4), restacked once.",
		"Since 10:02: 3 PRs merged (#16 #17 #18), restacked 3 times.",
	} {
		m.addChat(store.ChatEvent, l)
	}
	if len(m.chat) != 1 {
		t.Fatalf("chat = %+v, want one merged line", m.chat)
	}
	out := chatText(m)
	if strings.Count(out, "Since ") != 1 || !strings.Contains(out, "Since 09:23: 7 PRs merged") || !strings.Contains(out, "restacked 7 times") {
		t.Fatalf("merged digest:\n%s", out)
	}
	if strings.Contains(out, "notices --all") {
		t.Errorf("digest shows the notices hint while folded:\n%s", out)
	}
	m.toggleDetails()
	if out := chatText(m); strings.Count(out, "notices --all") != 1 {
		t.Errorf("open digest should show the hint once:\n%s", out)
	}

	// The stored history keeps every line; reloading it merges them again.
	hist, _ := m.app.Store.Chat(10)
	var again []chatLine
	for _, c := range hist {
		again = appendChat(again, chatLine{role: c.Role, text: c.Text})
	}
	if len(hist) != 3 || len(again) != 1 {
		t.Errorf("history %d lines, reloaded %d", len(hist), len(again))
	}

	// Anything in between keeps digests apart.
	m.addChat(store.ChatAssistant, "Two more merged.")
	m.addChat(store.ChatEvent, "Since 10:30: 1 PR merged (#19).")
	if len(m.chat) != 3 {
		t.Errorf("chat = %d lines, want 3", len(m.chat))
	}
}

// #402: the headline splits at the first blank line outside a code fence;
// a one-paragraph message and a needs-you message don't fold.
func TestSplitHeadline(t *testing.T) {
	for _, tc := range []struct{ in, head, details string }{
		{"All quiet. Nothing needs you.", "All quiet. Nothing needs you.", ""},
		{"Two PRs merged.\n\n- TestA\n- TestB", "Two PRs merged.", "- TestA\n- TestB"},
		{"Headline\nstill headline\n\nmore\n\nand more", "Headline\nstill headline", "more\n\nand more"},
		{"```\na\n\nb\n```", "```\na\n\nb\n```", ""},
	} {
		h, d := splitHeadline(tc.in)
		if h != tc.head || d != tc.details {
			t.Errorf("splitHeadline(%q) = %q, %q; want %q, %q", tc.in, h, d, tc.head, tc.details)
		}
	}
	if _, d := foldText(chatLine{role: store.ChatAssistant, text: "‼ t3 needs a decision.\n\nIt asks about the schema."}); d != "" {
		t.Errorf("a needs-you message folded: %q", d)
	}
}

// #402: in the narrow sidebar a folded message wraps rather than truncates.
func TestFoldedMessageWrapsInNarrowSidebar(t *testing.T) {
	const w = 30
	head := "The risk-tier work is half in: two PRs merged and four follow-ups are queued as issues."
	out := renderFolded(chatLine{role: store.ChatEvent, text: `t1 "x" is done and queued in the merge train: ` + head}, w, lipgloss.NewStyle().Width(w-2), false)
	for _, l := range strings.Split(out, "\n") {
		if lipgloss.Width(l) > w {
			t.Errorf("line wider than %d: %q", w, l)
		}
	}
	if strings.Contains(out, "…") {
		t.Errorf("truncated:\n%s", out)
	}
	for _, word := range strings.Fields("is done and queued to land.") {
		if !strings.Contains(out, word) {
			t.Errorf("headline lost %q:\n%s", word, out)
		}
	}
}

// #465: a plain orchestrator message (lead paragraph, bullet list, trailing
// paragraphs) is never folded or dimmed; only the "‼" form is special.
func TestPlainMessageAfterBulletListNotFolded(t *testing.T) {
	text := "Here is where things stand.\n\n- **Landed:** t1 and t2\n- **Running:** t3\n- **Blocked:** t4\n\nFirst trailing paragraph.\n\nSecond trailing paragraph."
	c := chatLine{role: store.ChatAssistant, text: text}
	if head, d := foldText(c); d != "" || head != text {
		t.Fatalf("foldText folded a plain message: %q, %q", head, d)
	}
	out := renderFolded(c, 70, lipgloss.NewStyle().Width(68), false)
	if strings.Contains(out, foldHint) {
		t.Errorf("fold hint shown:\n%s", out)
	}
	for _, w := range []string{"First trailing paragraph.", "Second trailing paragraph."} {
		if !strings.Contains(out, w) {
			t.Errorf("lost %q:\n%s", w, out)
		}
	}
}
