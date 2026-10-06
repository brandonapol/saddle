package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/store"
)

// A box's text, as copied, has no color codes, no border runes and no
// trailing padding.
func TestBoxLinesStripsANSIAndBorders(t *testing.T) {
	body := lipgloss.NewStyle().Foreground(cAccent).Render("  go test ./...") + "\n" +
		sDim.Render("  FAIL internal/tui")
	got := boxLines(box("PEEK · t1", 40, 5, false, body))
	want := []string{"  go test ./...", "  FAIL internal/tui"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("boxLines = %q, want %q", got, want)
	}
	if got := dedent(got); got[0] != "go test ./..." || got[1] != "FAIL internal/tui" {
		t.Errorf("dedent = %q", got)
	}
}

// copyModel is a view model with a fake clipboard and a known chat.
func copyModel(t *testing.T, w, h int) (*model, *fakeClip) {
	t.Helper()
	m := newViewModel(w, h)
	f := &fakeClip{env: map[string]string{"WAYLAND_DISPLAY": "w"}, path: []string{"wl-copy"}}
	cl := f.clipboard()
	m.clip = &cl
	m.chat = []chatLine{
		{role: store.ChatUser, text: "run the tests"},
		{role: store.ChatAssistant, text: "FAIL internal/tui\nsee https://example.com/pr/9"},
	}
	m.peek = "$ make check\nok  internal/app\n"
	m.renderChat()
	return m, f
}

func ctrlY() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlY} }

func TestCopyModeYanksSelectedChatLines(t *testing.T) {
	m, f := copyModel(t, 120, 40)
	press(m, ctrlY())
	if !m.cp.on || m.cp.srcs[m.cp.src] != "chat" {
		t.Fatalf("ctrl+y from chat should start copy mode on the chat: %+v", m.cp)
	}
	if m.input.Value() != "" {
		t.Errorf("ctrl+y leaked into the input: %q", m.input.Value())
	}
	// The cursor starts on the last line; select it and the one above.
	press(m, runeKey('V'))
	press(m, runeKey('k'))
	press(m, runeKey('y'))
	if m.cp.on {
		t.Error("y should leave copy mode")
	}
	want := "FAIL internal/tui\nsee https://example.com/pr/9"
	if len(f.ran) != 1 || f.ran[0] != "wl-copy<<"+want {
		t.Fatalf("clipboard got %q, want %q", f.ran, want)
	}
	if !strings.Contains(m.flash, "copied 2 lines") {
		t.Errorf("flash = %q", m.flash)
	}
}

func TestCopyModeCursorLineAndSources(t *testing.T) {
	m, f := copyModel(t, 120, 40)
	press(m, ctrlY())
	press(m, runeKey('g')) // top: "you"
	press(m, runeKey('j'))
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(f.ran) != 1 || f.ran[0] != "wl-copy<<run the tests" {
		t.Fatalf("enter without a selection copies the cursor line; got %q", f.ran)
	}
	if !strings.Contains(m.flash, "copied 1 line") {
		t.Errorf("flash = %q", m.flash)
	}

	// tab moves to the peek, whose lines are the agent's screen.
	press(m, ctrlY())
	press(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.cp.srcs[m.cp.src]; got != "peek" {
		t.Fatalf("tab: source %q, want peek", got)
	}
	press(m, runeKey('Y'))
	if got := f.ran[len(f.ran)-1]; got != "wl-copy<<$ make check\nok  internal/app" {
		t.Errorf("Y copies the whole source; got %q", got)
	}

	// esc leaves without copying.
	press(m, ctrlY())
	press(m, tea.KeyMsg{Type: tea.KeyEscape})
	if m.cp.on || len(f.ran) != 2 {
		t.Errorf("esc should cancel: on=%v ran=%d", m.cp.on, len(f.ran))
	}
}

// From the agent list ctrl+y copies from the peek, and from the merge view
// from the merge train.
func TestCopyModeStartsOnFocusedPane(t *testing.T) {
	m, _ := copyModel(t, 120, 40)
	press(m, tea.KeyMsg{Type: tea.KeyTab})
	press(m, ctrlY())
	if got := m.cp.srcs[m.cp.src]; got != "peek" {
		t.Errorf("from tasks: %q, want peek", got)
	}
	press(m, tea.KeyMsg{Type: tea.KeyEscape})
	m.briefOn = true
	press(m, ctrlY())
	if got := m.cp.srcs[m.cp.src]; got != "brief" {
		t.Errorf("with the brief shown: %q, want brief", got)
	}
	press(m, tea.KeyMsg{Type: tea.KeyEscape})
	m.Update(altKey('3'))
	press(m, ctrlY())
	if got := m.cp.srcs[m.cp.src]; got != "merge" {
		t.Errorf("merge view: %q, want merge", got)
	}
	for _, l := range m.cp.lines {
		if strings.ContainsAny(l, "\x1b╭╰│") {
			t.Errorf("merge line keeps codes or borders: %q", l)
		}
	}
}

func TestCopyViewFits(t *testing.T) {
	for _, w := range []int{36, 120} {
		m, _ := copyModel(t, w, 30)
		m.chat = append(m.chat, chatLine{role: store.ChatAssistant, text: strings.Repeat("a very long line without breaks ", 10)})
		press(m, ctrlY())
		press(m, runeKey('V'))
		press(m, runeKey('k'))
		out := m.View()
		checkScreen(t, "copy mode", out, w, 30)
		if !strings.Contains(out, "COPY") {
			t.Errorf("width %d: no copy pane:\n%s", w, out)
		}
		if !strings.Contains(m.viewKeys(w), "y") {
			t.Errorf("width %d: footer lacks the yank key: %q", w, m.viewKeys(w))
		}
	}
}

func TestHelpListsCopyKeys(t *testing.T) {
	m := newViewModel(120, 50)
	m.helpOpen = true
	out := m.View()
	for _, want := range []string{"Copy", "ctrl+y", "shift+drag", "drag"} {
		if !strings.Contains(out, want) {
			t.Errorf("help overlay lacks %q:\n%s", want, out)
		}
	}
}

// screenRow finds the row and column of s in a rendered screen.
func screenRow(t *testing.T, screen, s string) (x, y int) {
	t.Helper()
	for i, l := range strings.Split(screen, "\n") {
		plain := []rune(stripCodes(l))
		if j := strings.Index(string(plain), s); j >= 0 {
			return len([]rune(string(plain)[:j])), i
		}
	}
	t.Fatalf("%q not on screen:\n%s", s, screen)
	return 0, 0
}

func mouse(x, y int, a tea.MouseAction) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: a, Button: tea.MouseButtonLeft}
}

// Dragging over the chat copies the rows under the drag on release, without
// the box border or the colors.
func TestMouseDragCopiesPaneRows(t *testing.T) {
	m, f := copyModel(t, 120, 40)
	x, y := screenRow(t, m.View(), "FAIL internal/tui")
	press := func(msg tea.MouseMsg) {
		_, c := m.Update(msg)
		drain(m, c)
	}
	press(mouse(x, y, tea.MouseActionPress))
	press(mouse(x+3, y+1, tea.MouseActionMotion))
	if out := m.View(); !strings.Contains(out, "FAIL internal/tui") {
		t.Fatalf("drag highlight lost the text:\n%s", out)
	}
	press(tea.MouseMsg{X: x + 3, Y: y + 1, Action: tea.MouseActionRelease})
	want := "FAIL internal/tui\nsee https://example.com/pr/9"
	if len(f.ran) != 1 || f.ran[0] != "wl-copy<<"+want {
		t.Fatalf("drag copied %q, want %q", f.ran, want)
	}

	// A click without a drag copies nothing.
	press(mouse(x, y, tea.MouseActionPress))
	press(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionRelease})
	if len(f.ran) != 1 {
		t.Errorf("a click copied: %q", f.ran)
	}

	// The peek on the left copies its own rows, not the chat's.
	px, py := screenRow(t, m.View(), "$ make check")
	press(mouse(px, py, tea.MouseActionPress))
	press(mouse(px, py+1, tea.MouseActionMotion))
	press(tea.MouseMsg{X: px, Y: py + 1, Action: tea.MouseActionRelease})
	if got := f.ran[len(f.ran)-1]; got != "wl-copy<<$ make check\nok  internal/app" {
		t.Errorf("peek drag copied %q", got)
	}
}

// Keys typed faster than the TUI reads them arrive as one message ("Vk");
// copy mode acts on each.
func TestCopyModeRunsCoalescedKeys(t *testing.T) {
	m, f := copyModel(t, 120, 40)
	press(m, ctrlY())
	press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Vky")})
	if len(f.ran) != 1 || f.ran[0] != "wl-copy<<FAIL internal/tui\nsee https://example.com/pr/9" {
		t.Fatalf("Vky copied %q", f.ran)
	}
}
