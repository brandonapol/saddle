package termpane

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// collector keeps every message Bubble Tea's input parser emits.
type collector struct{ msgs []tea.Msg }

func (c *collector) Init() tea.Cmd { return nil }
func (c *collector) View() string  { return "" }
func (c *collector) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	c.msgs = append(c.msgs, msg)
	return c, nil
}

// blockAfter reads s, then blocks until the program quits.
type blockAfter struct{ r io.Reader }

func (b blockAfter) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		select {}
	}
	return n, err
}

// parsed runs s through Bubble Tea's real input parser and returns the one
// message it is not a key or a lifecycle event for, or the key.
func parsed(t *testing.T, s string) tea.Msg {
	t.Helper()
	c := &collector{}
	p := tea.NewProgram(c, tea.WithInput(blockAfter{strings.NewReader(s)}), tea.WithOutput(io.Discard))
	go func() { time.Sleep(100 * time.Millisecond); p.Quit() }()
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	for _, m := range c.msgs {
		switch m.(type) {
		case tea.WindowSizeMsg:
			continue
		}
		if _, ok := m.(interface{ String() string }); ok {
			return m
		}
	}
	t.Fatalf("%q parsed to nothing usable: %#v", s, c.msgs)
	return nil
}

func TestExtendedKeyDecodesWhatBubbleTeaDrops(t *testing.T) {
	cases := []struct{ seq, want string }{
		{"\x1b[96;3u", "alt+`"},
		{"\x1b[96;5u", "ctrl+@"},
		{"\x1b[96;7u", "alt+ctrl+@"},
		{"\x1b[27;3;96~", "alt+`"},
		{"\x1b[27;5;96~", "ctrl+@"},
		{"\x1b[97;5u", "ctrl+a"},
		{"\x1b[97;2u", "A"},
		{"\x1b[49:33;2u", "!"},
		{"\x1b[13;3u", "alt+enter"},
		{"\x1b[96;3:1u", "alt+`"},
	}
	for _, c := range cases {
		msg := parsed(t, c.seq)
		if _, isKey := msg.(tea.KeyMsg); isKey {
			t.Fatalf("%q: Bubble Tea now parses it (%v); ExtendedKey may be redundant", c.seq, msg)
		}
		k, ok := ExtendedKey(msg)
		if !ok {
			t.Errorf("%q: not decoded (msg %T %v)", c.seq, msg, msg)
			continue
		}
		if k.String() != c.want {
			t.Errorf("%q: got %q, want %q", c.seq, k.String(), c.want)
		}
	}
}

func TestExtendedKeyLeavesOthersAlone(t *testing.T) {
	for _, seq := range []string{
		"\x1b[96;3:3u",  // a release
		"\x1b[96;9u",    // super+`
		"\x1b[27;5;96X", // not a key
		"\x1b[5;5~",     // ctrl+pgup's own encoding
	} {
		msg := parsed(t, seq)
		if k, ok := ExtendedKey(msg); ok {
			t.Errorf("%q decoded as %q", seq, k.String())
		}
	}
	if _, ok := ExtendedKey(tea.KeyMsg{Type: tea.KeyCtrlAt}); ok {
		t.Error("a key message is not an extended key")
	}
	if _, ok := ExtendedKey(nil); ok {
		t.Error("nil decoded")
	}
}
