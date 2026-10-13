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
// delay stands in for a reader starved of CPU: each Read waits that long
// first. drained closes when Read is called with no chunks left.
type chunkReader struct {
	chunks  []string
	delay   time.Duration
	drained chan struct{}
}

func (r *chunkReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	if len(r.chunks) == 0 {
		if r.drained != nil {
			close(r.drained)
			r.drained = nil
		}
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
	return parseFrom(t, &chunkReader{chunks: chunks})
}

// parseFrom quits once r is drained, not after a fixed time, so a reader
// slowed by machine load still gets every chunk parsed (#435). Bubble Tea's
// reader hands each message over an unbuffered channel before it reads
// again, so by the time Read finds nothing left, the event loop holds every
// key, and Quit queues behind them.
func parseFrom(t *testing.T, r *chunkReader) []tea.Msg {
	t.Helper()
	r.drained = make(chan struct{})
	drained := r.drained
	rec := &recorder{}
	p := tea.NewProgram(rec, tea.WithInput(r), tea.WithOutput(io.Discard))
	go func() {
		select {
		case <-drained:
		case <-time.After(10 * time.Second):
		}
		p.Quit()
	}()
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	return rec.msgs
}

// #435: a reader too starved to finish within a fixed deadline still has
// every chunk parsed.
func TestParseWaitsForSlowReader(t *testing.T) {
	msgs := parseFrom(t, &chunkReader{chunks: []string{"\x1b", "`"}, delay: 150 * time.Millisecond})
	if len(msgs) != 2 {
		t.Fatalf("parsed %v, want esc and `", msgs)
	}
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

// Real typing that merely resembles a fragment must still get through: to
// chat, and as keys a focused terminal pane would get (#201).
func TestMouseScrubKeepsRealInput(t *testing.T) {
	runes := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	cases := []struct {
		name string
		keys []tea.KeyMsg
		want string   // chat input
		pass []string // keys the scrubber lets through
	}{
		{"digits and semicolons", []tea.KeyMsg{runes("12;3"), runes("M")}, "12;3M", []string{"12;3", "M"}},
		{"angle bracket", []tea.KeyMsg{runes("<65;1M")}, "<65;1M", []string{"<65;1M"}},
		{"esc then text", []tea.KeyMsg{{Type: tea.KeyEscape}, runes("1;2M")}, "1;2M", []string{"esc", "1;2M"}},
		{"alt+[ then text", []tea.KeyMsg{altRune('['), runes("x")}, "x", []string{"alt+[", "x"}},
		{"report ends, typing resumes", []tea.KeyMsg{{Type: tea.KeyEscape}, runes("[<65;1;2M"), runes("7")}, "7", []string{"esc", "7"}},
		{"paste", []tea.KeyMsg{{Type: tea.KeyEscape}, {Type: tea.KeyRunes, Runes: []rune("[<1;2M"), Paste: true}}, "[<1;2M", []string{"esc", "[[<1;2M]"}},
	}
	for _, c := range cases {
		m := chatModel()
		for _, k := range c.keys {
			m.Update(k)
		}
		if got := m.input.Value(); got != c.want {
			t.Errorf("%s: input = %q, want %q", c.name, got, c.want)
		}
		var s mouseScrub
		var pass []string
		for _, k := range c.keys {
			out, _ := s.feed(k)
			for _, o := range out {
				pass = append(pass, o.String())
			}
		}
		if strings.Join(pass, " ") != strings.Join(c.pass, " ") {
			t.Errorf("%s: scrubber let through %q, want %q", c.name, pass, c.pass)
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

// slowHandoff stands in for Bubble Tea's reader starved of CPU between
// reading bytes and handing their keys over: each Read returns delay late,
// after the inputWatch below it has seen the read end.
type slowHandoff struct {
	r     io.Reader
	delay time.Duration
}

func (s slowHandoff) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	time.Sleep(s.delay)
	return n, err
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
		if got := runScrubbed(t, r, 0); got != "alt+`" {
			t.Fatalf("run %d: keys = %q, want alt+`", i, got)
		}
	}
}

// #459: ESC and ` read apart, by a reader that then takes longer than
// escWait to hand ` over, still make alt+`. Once read, ` is no longer unread
// in the terminal, so only the reader's state shows it is on its way.
func TestSplitAltWhileReaderHoldsKey(t *testing.T) {
	r := &ttyReader{chunks: []string{"\x1b", "`"}, at: []time.Duration{0, time.Millisecond}}
	if got := runScrubbed(t, r, 4*escWait); got != "alt+`" {
		t.Fatalf("keys = %q, want alt+`", got)
	}
}

// A lone esc is still sent once the input goes idle: a reader waiting in
// Read holds no keys.
func TestLoneEscFlushesWithInputWatch(t *testing.T) {
	r := &ttyReader{chunks: []string{"\x1b"}, at: []time.Duration{0}}
	if got := runScrubbed(t, r, 0); got != "esc" {
		t.Fatalf("keys = %q, want esc", got)
	}
}

// runScrubbed feeds what Bubble Tea parses from r through a mouseScrub, the
// way the TUI does, and returns the keys that came out. Each read's keys are
// handed over handoff late.
func runScrubbed(t *testing.T, r *ttyReader, handoff time.Duration) string {
	t.Helper()
	w := &inputWatch{r: r}
	m := &scrubbed{s: mouseScrub{wait: escWait, pending: r.pending, input: w}, r: r}
	p := tea.NewProgram(m, tea.WithInput(slowHandoff{w, handoff}), tea.WithOutput(io.Discard), tea.WithoutSignals())
	timer := time.AfterFunc(10*time.Second, p.Quit)
	_, err := p.Run()
	timer.Stop()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(m.keys, " ")
}
