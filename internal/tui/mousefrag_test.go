package tui

import (
	"io"
	"runtime"
	"strings"
	"sync"
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

func altRune(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true} }

// alt+[ is held until the next key decides it: "<" and report parameters
// make it a split mouse report, anything else releases it ahead of that key
// (#201). Alone, the timer releases it.
func TestMouseScrubReleasesAltBracket(t *testing.T) {
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	var s mouseScrub
	if out, c := s.feed(altRune('[')); out != nil || c == nil {
		t.Fatalf("alt+[ should be held with a timer, got %v", out)
	}
	if out, _ := s.feed(runes("x")); len(out) != 2 || out[0].String() != "alt+[" || out[1].String() != "x" {
		t.Errorf("alt+[ x: got %v", out)
	}
	s.feed(altRune('['))
	if out, _ := s.feed(runes("<65;1")); out != nil {
		t.Errorf("alt+[ <65;1 is a report, got %v", out)
	}
	if out, _ := s.feed(runes("2;3M")); out != nil {
		t.Errorf("report tail got through: %v", out)
	}
	_, c := s.feed(altRune('['))
	msg := c()
	if out := s.flush(msg.(escFlushMsg).seq); len(out) != 1 || out[0].String() != "alt+[" {
		t.Errorf("timer released %v", out)
	}
	if out := s.flush(msg.(escFlushMsg).seq); out != nil {
		t.Errorf("a second flush released %v", out)
	}
}

// With a wait set, a bare esc is held: a lone rune right behind it joins it
// into alt+<rune>, as when ESC ` arrives in two reads (#200). Once the timer
// fires, esc goes on its own and the next rune is just a rune.
func TestMouseScrubJoinsSplitAlt(t *testing.T) {
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	s := mouseScrub{wait: escWait}
	if out, c := s.feed(escKey()); out != nil || c == nil {
		t.Fatalf("esc should be held with a timer, got %v", out)
	}
	if out, _ := s.feed(runes("`")); len(out) != 1 || out[0].String() != "alt+`" {
		t.Errorf("esc ` split: got %v", out)
	}

	_, c := s.feed(escKey())
	if out := s.flush(c().(escFlushMsg).seq); len(out) != 1 || out[0].String() != "esc" {
		t.Errorf("timer released %v", out)
	}
	if out, _ := s.feed(runes("j")); len(out) != 1 || out[0].String() != "j" {
		t.Errorf("j after the timer: got %v", out)
	}

	// esc then a whole report is dropped, esc and all.
	s.feed(escKey())
	if out, _ := s.feed(runes("[<65;1;2M")); out != nil {
		t.Errorf("esc + report: got %v", out)
	}
	// esc esc: the first goes, the second waits.
	s.feed(escKey())
	if out, c := s.feed(escKey()); len(out) != 1 || out[0].String() != "esc" || c == nil {
		t.Errorf("esc esc: got %v", out)
	}
}

// Through the real parser: ESC and ` in separate reads still toggle the
// pane closed, and nothing reaches chat.
func TestSplitAltBacktickTogglesTerminal(t *testing.T) {
	msgs := parse(t, "\x1b", "`")
	m := newTermModel(t)
	m.scrub.wait = escWait
	openShell(t, m)
	for _, msg := range msgs {
		m.Update(msg)
	}
	if m.termOpen || m.input.Value() != "" {
		t.Errorf("parsed %v: pane open %v, chat input %q", msgs, m.termOpen, m.input.Value())
	}
}

// ttyReader plays chunks as a terminal delivers them: each arrives at its
// offset from the first Read, and Read blocks until the next one has.
// pending reports a chunk that has arrived but not been read, as FIONREAD
// does on a tty.
type ttyReader struct {
	mu     sync.Mutex
	start  time.Time
	chunks []string
	at     []time.Duration
}

func (r *ttyReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.start.IsZero() {
		r.start = time.Now()
	}
	if len(r.chunks) == 0 {
		r.mu.Unlock()
		select {} // block until the program quits
	}
	due := r.start.Add(r.at[0])
	r.mu.Unlock()
	time.Sleep(time.Until(due))
	r.mu.Lock()
	defer r.mu.Unlock()
	n := copy(p, r.chunks[0])
	r.chunks, r.at = r.chunks[1:], r.at[1:]
	return n, nil
}

func (r *ttyReader) drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.start.IsZero() && len(r.chunks) == 0
}

func (r *ttyReader) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.start.IsZero() && len(r.chunks) > 0 && time.Since(r.start) >= r.at[0]
}

// scrubbed runs keys through a mouseScrub the way the TUI does and keeps
// what comes out. It quits once r is read and nothing is held.
type scrubbed struct {
	s    mouseScrub
	r    *ttyReader
	keys []string
}

func (m *scrubbed) Init() tea.Cmd { return nil }
func (m *scrubbed) View() string  { return "" }
func (m *scrubbed) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var out []tea.KeyMsg
	var c tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		out, c = m.s.feed(msg)
	case escFlushMsg:
		out = m.s.flush(msg.seq)
	}
	for _, k := range out {
		m.keys = append(m.keys, k.String())
	}
	if m.s.held == nil && m.r.drained() {
		return m, tea.Quit
	}
	return m, c
}

// spin keeps every CPU busy, several goroutines each, until stop closes.
func spin(stop <-chan struct{}) {
	for range 4 * runtime.GOMAXPROCS(0) {
		go func() {
			for x := 0; ; x++ {
				if x%1024 == 0 {
					select {
					case <-stop:
						return
					default:
					}
				}
			}
		}()
	}
}

// #249: ESC and ` written back to back but read apart, on a machine too
// busy to read the second half within escWait, still make alt+`. The hold
// ends when the input goes idle, not on a fixed timer.
func TestSplitAltUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	stop := make(chan struct{})
	defer close(stop)
	spin(stop)
	for i := range 50 {
		r := &ttyReader{chunks: []string{"\x1b", "`"}, at: []time.Duration{0, time.Millisecond}}
		m := &scrubbed{s: mouseScrub{wait: escWait, pending: r.pending}, r: r}
		p := tea.NewProgram(m, tea.WithInput(r), tea.WithOutput(io.Discard), tea.WithoutSignals())
		timer := time.AfterFunc(10*time.Second, p.Quit)
		_, err := p.Run()
		timer.Stop()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(m.keys, " "); got != "alt+`" {
			t.Fatalf("run %d: keys = %q, want alt+`", i, got)
		}
	}
}
