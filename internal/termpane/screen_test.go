package termpane

import (
	"strings"
	"testing"
)

func feed(s *Screen, in string) { s.Feed([]byte(in)) }

func line(s *Screen, y int) string { return strings.TrimRight(strings.Split(s.Text(), "\n")[y], " ") }

func TestScreenPrintsAndWraps(t *testing.T) {
	s := NewScreen(5, 3)
	feed(s, "hello world")
	if got := line(s, 0); got != "hello" {
		t.Fatalf("line 0 = %q", got)
	}
	if got := line(s, 1); got != " worl" {
		t.Fatalf("line 1 = %q", got)
	}
	if got := line(s, 2); got != "d" {
		t.Fatalf("line 2 = %q", got)
	}
}

func TestScreenCRLFAndScrollback(t *testing.T) {
	s := NewScreen(10, 2)
	feed(s, "one\r\ntwo\r\nthree\r\nfour")
	if line(s, 0) != "three" || line(s, 1) != "four" {
		t.Fatalf("screen = %q", s.Text())
	}
	sb := s.Scrollback()
	if len(sb) != 2 || strings.TrimRight(sb[0], " ") != "one" || strings.TrimRight(sb[1], " ") != "two" {
		t.Fatalf("scrollback = %q", sb)
	}
}

func TestScrollbackIsCapped(t *testing.T) {
	s := NewScreen(4, 1)
	for i := 0; i < maxScrollback+50; i++ {
		feed(s, "x\r\n")
	}
	if n := len(s.Scrollback()); n != maxScrollback {
		t.Fatalf("scrollback len = %d, want %d", n, maxScrollback)
	}
}

func TestCursorMovementAndErase(t *testing.T) {
	s := NewScreen(10, 3)
	feed(s, "abcdef\x1b[1;3HX")
	if line(s, 0) != "abXdef" {
		t.Fatalf("CUP: %q", line(s, 0))
	}
	feed(s, "\x1b[K")
	if line(s, 0) != "abX" {
		t.Fatalf("EL: %q", line(s, 0))
	}
	feed(s, "\x1b[2J")
	if strings.TrimSpace(s.Text()) != "" {
		t.Fatalf("ED 2: %q", s.Text())
	}
	feed(s, "\x1b[3;2Hz\x1b[A\x1b[Dy")
	if x, y := s.Cursor(); x != 2 || y != 1 {
		t.Fatalf("cursor = %d,%d", x, y)
	}
	if line(s, 1) != " y" || line(s, 2) != " z" {
		t.Fatalf("screen = %q", s.Text())
	}
}

func TestBackspaceAndTab(t *testing.T) {
	s := NewScreen(20, 1)
	feed(s, "ab\bc\tz")
	if line(s, 0) != "ac      z" {
		t.Fatalf("got %q", line(s, 0))
	}
}

func TestAltScreenRestoresMain(t *testing.T) {
	s := NewScreen(10, 2)
	feed(s, "shell$ ")
	feed(s, "\x1b[?1049h\x1b[2J\x1b[Hvim stuff")
	if !s.AltScreen() {
		t.Fatal("alt screen not active")
	}
	if line(s, 0) != "vim stuff" {
		t.Fatalf("alt = %q", line(s, 0))
	}
	feed(s, "\x1b[?1049l")
	if s.AltScreen() {
		t.Fatal("alt screen still active")
	}
	if line(s, 0) != "shell$" {
		t.Fatalf("main not restored: %q", line(s, 0))
	}
	if x, _ := s.Cursor(); x != 7 {
		t.Fatalf("cursor not restored: %d", x)
	}
}

func TestScrollRegion(t *testing.T) {
	s := NewScreen(5, 4)
	feed(s, "top\r\na\r\nb\r\nbot")
	feed(s, "\x1b[2;3r\x1b[3;1H\n")
	got := []string{line(s, 0), line(s, 1), line(s, 2), line(s, 3)}
	want := []string{"top", "b", "", "bot"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("region scroll: %q, want %q", got, want)
		}
	}
	if len(s.Scrollback()) != 0 {
		t.Fatal("region scroll must not feed scrollback")
	}
}

func TestInsertDeleteChars(t *testing.T) {
	s := NewScreen(10, 1)
	feed(s, "abcdef\x1b[1;2H\x1b[2P")
	if line(s, 0) != "adef" {
		t.Fatalf("DCH: %q", line(s, 0))
	}
	feed(s, "\x1b[2@")
	if line(s, 0) != "a  def" {
		t.Fatalf("ICH: %q", line(s, 0))
	}
}

func TestOSCIsIgnored(t *testing.T) {
	s := NewScreen(20, 1)
	feed(s, "\x1b]0;title\x07ok\x1b]2;t\x1b\\!")
	if line(s, 0) != "ok!" {
		t.Fatalf("got %q", line(s, 0))
	}
}

func TestUTF8SplitAcrossWrites(t *testing.T) {
	s := NewScreen(10, 1)
	b := []byte("é→")
	s.Feed(b[:1])
	s.Feed(b[1:3])
	s.Feed(b[3:])
	if line(s, 0) != "é→" {
		t.Fatalf("got %q", line(s, 0))
	}
}

func TestEscapeSplitAcrossWrites(t *testing.T) {
	s := NewScreen(10, 2)
	feed(s, "ab\x1b[")
	feed(s, "2;1Hc")
	if line(s, 1) != "c" {
		t.Fatalf("got %q", s.Text())
	}
}

func TestDeviceStatusReply(t *testing.T) {
	s := NewScreen(10, 5)
	feed(s, "\x1b[3;4H\x1b[6n")
	if got := string(s.TakeReplies()); got != "\x1b[3;4R" {
		t.Fatalf("reply = %q", got)
	}
	if len(s.TakeReplies()) != 0 {
		t.Fatal("replies not drained")
	}
}

func TestResizeKeepsCursorVisible(t *testing.T) {
	s := NewScreen(10, 4)
	feed(s, "1\r\n2\r\n3\r\n4")
	s.Resize(6, 2)
	if line(s, 0) != "3" || line(s, 1) != "4" {
		t.Fatalf("after shrink: %q", s.Text())
	}
	if _, y := s.Cursor(); y != 1 {
		t.Fatalf("cursor row = %d", y)
	}
	if sb := s.Scrollback(); len(sb) != 2 {
		t.Fatalf("shrunk lines should go to scrollback: %q", sb)
	}
	s.Resize(8, 3)
	if c, r := s.Size(); c != 8 || r != 3 {
		t.Fatalf("size = %d,%d", c, r)
	}
	feed(s, "\x1b[3;1Hlast")
	if line(s, 2) != "last" {
		t.Fatalf("grown row unusable: %q", s.Text())
	}
}

func TestSGRRendersColors(t *testing.T) {
	s := NewScreen(10, 1)
	feed(s, "\x1b[1;31mred\x1b[0m plain")
	out := s.Render(0, -1, -1)
	if !strings.Contains(out, "\x1b[0;1;31mred") {
		t.Fatalf("render missing color: %q", out)
	}
	if !strings.Contains(out, "\x1b[0m plain") {
		t.Fatalf("render missing reset: %q", out)
	}
}

func TestRenderScrollOffsetShowsHistory(t *testing.T) {
	s := NewScreen(6, 2)
	feed(s, "a\r\nb\r\nc\r\nd")
	out := strings.Split(stripSGR(s.Render(2, -1, -1)), "\n")
	if strings.TrimSpace(out[0]) != "a" || strings.TrimSpace(out[1]) != "b" {
		t.Fatalf("offset 2: %q", out)
	}
	out = strings.Split(stripSGR(s.Render(99, -1, -1)), "\n")
	if strings.TrimSpace(out[0]) != "a" {
		t.Fatalf("offset clamps: %q", out)
	}
}

func TestAppCursorKeysMode(t *testing.T) {
	s := NewScreen(10, 1)
	if s.AppCursor() {
		t.Fatal("app cursor on by default")
	}
	feed(s, "\x1b[?1h")
	if !s.AppCursor() {
		t.Fatal("DECCKM not set")
	}
}

func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
