package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// chunkReader hands out one chunk per Read, the way a slow terminal can.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		select {} // block until the program quits
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	time.Sleep(5 * time.Millisecond) // separate reads, not one coalesced buffer
	return n, nil
}

// recorder keeps the input messages Bubble Tea's parser emits.
type recorder struct{ msgs []tea.Msg }

func (r *recorder) Init() tea.Cmd { return nil }
func (r *recorder) View() string  { return "" }
func (r *recorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg:
		r.msgs = append(r.msgs, msg)
	}
	return r, nil
}

// parse runs chunks through Bubble Tea's real input parser.
func parse(t *testing.T, chunks ...string) []tea.Msg {
	t.Helper()
	r := &recorder{}
	p := tea.NewProgram(r, tea.WithInput(&chunkReader{chunks: chunks}), tea.WithOutput(io.Discard))
	go func() { time.Sleep(time.Duration(len(chunks))*5*time.Millisecond + 100*time.Millisecond); p.Quit() }()
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	return r.msgs
}

func chatModel() *model {
	m := &model{keys: newKeyMap(), vp: viewport.New(60, 10)}
	m.input = textarea.New()
	m.input.Focus()
	return m
}

// A wheel report cut anywhere must not leave text in the input box. This is
// the "[<65;139;21M" junk seen in the orchestrator pane.
func TestSplitMouseReportNeverReachesInput(t *testing.T) {
	const report = "\x1b[<65;139;21M"
	for i := 1; i < len(report); i++ {
		msgs := parse(t, report[:i], report[i:], "hi")
		m := chatModel()
		for _, msg := range msgs {
			m.Update(msg)
		}
		if got := m.input.Value(); got != "hi" {
			t.Errorf("split %q|%q: input = %q, want %q (parsed %v)", report[:i], report[i:], got, "hi", msgs)
		}
	}
}

// Back-to-back reports with one split in the middle, as a fast scroll sends.
func TestSplitMouseReportAmongWholeOnes(t *testing.T) {
	msgs := parse(t, "\x1b[<65;139;21M\x1b[<65;1", "39;21M\x1b[<64;139;21M")
	m := chatModel()
	for _, msg := range msgs {
		m.Update(msg)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("input = %q, want empty (parsed %v)", got, msgs)
	}
}

// Real typing that merely resembles a fragment must still get through.
func TestMouseScrubKeepsRealInput(t *testing.T) {
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	cases := []struct {
		name string
		keys []tea.KeyMsg
		want string
	}{
		{"digits and semicolons", []tea.KeyMsg{runes("12;3"), runes("M")}, "12;3M"},
		{"angle bracket", []tea.KeyMsg{runes("<65;1M")}, "<65;1M"},
		{"esc then text", []tea.KeyMsg{{Type: tea.KeyEscape}, runes("1;2M")}, "1;2M"},
		{"alt+[ then text", []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("["), Alt: true}, runes("x")}, "x"},
		{"report ends, typing resumes", []tea.KeyMsg{{Type: tea.KeyEscape}, runes("[<65;1;2M"), runes("7")}, "7"},
		{"paste", []tea.KeyMsg{{Type: tea.KeyEscape}, {Type: tea.KeyRunes, Runes: []rune("[<1;2M"), Paste: true}}, "[<1;2M"},
	}
	for _, c := range cases {
		m := chatModel()
		for _, k := range c.keys {
			m.Update(k)
		}
		if got := m.input.Value(); got != c.want {
			t.Errorf("%s: input = %q, want %q", c.name, got, c.want)
		}
	}
}

// Scrolling and typing must not re-render the whole chat each time; only new
// or changed lines are rendered.
func TestRenderChatReusesRenderedLines(t *testing.T) {
	m := chatModel()
	m.chat = []chatLine{{role: "assistant", text: "**one**"}, {role: "assistant", text: "two"}}
	m.renderChat()
	m.chat[0].out = "CACHED"
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !strings.Contains(m.vp.View(), "CACHED") {
		t.Fatal("an unchanged line was rendered again")
	}

	m.Update(salienceMsg{idx: 0, needs: false})
	if strings.Contains(m.vp.View(), "CACHED") {
		t.Fatal("a line whose attention changed kept its stale rendering")
	}

	m.chat[1].out = "CACHED"
	m.vp.Width = 50
	m.renderChat()
	if strings.Contains(m.vp.View(), "CACHED") {
		t.Fatal("a width change kept renderings for the old width")
	}
}
