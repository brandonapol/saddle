package banner

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHowdyFitsSmallTerminal(t *testing.T) {
	lines := strings.Split(strings.TrimRight(Howdy(), "\n"), "\n")
	if len(lines) > 12 {
		t.Fatalf("%d lines, want <= 12", len(lines))
	}
	for i, l := range lines {
		if n := utf8.RuneCountInString(l); n > 80 {
			t.Errorf("line %d is %d columns, want <= 80: %q", i, n, l)
		}
		if strings.Contains(l, "\x1b") {
			t.Errorf("Howdy() must be plain text, line %d has an escape", i)
		}
	}
	if !strings.Contains(strings.ToLower(Howdy()), "howdy") {
		t.Fatal("banner doesn't say howdy")
	}
}

// tty pretends every writer is a terminal.
func tty(io.Writer) bool { return true }

func TestPrintOnTTYUsesColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var b bytes.Buffer
	Print(&b, Options{IsTTY: tty})
	if !strings.Contains(b.String(), "\x1b[") {
		t.Fatalf("no color on a TTY: %q", b.String())
	}
	if !strings.Contains(stripANSI(b.String()), strings.TrimRight(Howdy(), "\n")) {
		t.Fatalf("printed banner differs from Howdy():\n%s", b.String())
	}
}

func TestPrintNoColorDropsEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var b bytes.Buffer
	Print(&b, Options{IsTTY: tty})
	if b.Len() == 0 {
		t.Fatal("NO_COLOR suppressed the banner; it should only drop color")
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Fatalf("NO_COLOR set but output has escapes: %q", b.String())
	}
}

func TestPrintSuppressed(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var b bytes.Buffer
	Print(&b, Options{Quiet: true, IsTTY: tty})
	if b.Len() != 0 {
		t.Fatalf("--quiet printed %q", b.String())
	}
	// A bytes.Buffer is not a terminal: the default check must say so.
	Print(&b, Options{})
	if b.Len() != 0 {
		t.Fatalf("non-TTY printed %q", b.String())
	}
}

func TestPrintForceSkipsTTYCheckButNotQuiet(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var b bytes.Buffer
	Print(&b, Options{Force: true})
	if b.Len() == 0 || strings.Contains(b.String(), "\x1b") {
		t.Fatalf("forced to a non-TTY: want the plain banner, got %q", b.String())
	}
	b.Reset()
	Print(&b, Options{Force: true, Quiet: true})
	if b.Len() != 0 {
		t.Fatalf("Force overrode Quiet: %q", b.String())
	}
}

func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}
