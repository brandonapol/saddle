package tmux

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadInputFixtures(t *testing.T) {
	for name, want := range map[string]Input{
		"claude_empty":                  InputEmpty,
		"claude_suggestion":             InputGhost,
		"claude_placeholder":            InputGhost,
		"claude_draft":                  InputDraft,
		"claude_draft_after_suggestion": InputDraft,
	} {
		pane := fixture(t, name)
		if got := ReadInput(pane); got != want {
			t.Errorf("ReadInput(%s) = %v, want %v", name, got, want)
		}
		if got := HasDraft(pane); got != (want == InputDraft) {
			t.Errorf("HasDraft(%s) = %v", name, got)
		}
	}
}

func TestReadInputGreyColours(t *testing.T) {
	for line, want := range map[string]Input{
		"❯ \x1b[38;5;246msuggested\x1b[39m":       InputGhost,
		"❯ \x1b[90msuggested\x1b[0m":              InputGhost,
		"❯ \x1b[38;2;128;128;128msuggested\x1b[m": InputGhost,
		"❯ \x1b[2mfaint\x1b[22m typed":            InputDraft,
		"❯ \x1b[38;5;231mtyped\x1b[39m":           InputDraft,
		"❯ \x1b[38;2;200;80;80mtyped":             InputDraft,
	} {
		if got := ReadInput("out\n" + line); got != want {
			t.Errorf("ReadInput(%q) = %v, want %v", line, got, want)
		}
	}
}

// styledDriver captures with escapes, records keys and lets a test change
// the pane once text is sent.
type styledDriver struct {
	fakeDriver
	styled string
	keys   []string
	onSend func()
}

func (s *styledDriver) CaptureStyled(string, int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.styled, nil
}

func (s *styledDriver) SendKeys(_ string, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, keys...)
	return nil
}

func (s *styledDriver) SendText(id, t string) error {
	_ = s.fakeDriver.SendText(id, t)
	if s.onSend != nil {
		s.onSend()
	}
	return nil
}

func (s *styledDriver) setStyled(p string) { s.mu.Lock(); s.styled = p; s.mu.Unlock() }

func (s *styledDriver) keysSent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

func newStyled(t *testing.T, name string) *styledDriver {
	p := fixture(t, name)
	return &styledDriver{fakeDriver: fakeDriver{pane: StripANSI(p), alive: true}, styled: p}
}

func TestDeliversOverSuggestion(t *testing.T) {
	f := newStyled(t, "claude_suggestion")
	SendWhenIdle(f, "@1", "hi", nil)
	if f.count() != 1 {
		t.Fatalf("sent %d over a suggestion, want 1", f.count())
	}
	if k := f.keysSent(); len(k) != 1 || k[0] != "C-u" {
		t.Fatalf("keys %v: want the suggestion cleared with C-u first", k)
	}
}

func TestEmptyPromptNeedsNoClear(t *testing.T) {
	f := newStyled(t, "claude_empty")
	SendWhenIdle(f, "@1", "hi", nil)
	if f.count() != 1 || len(f.keysSent()) != 0 {
		t.Fatalf("sent %d, keys %v", f.count(), f.keysSent())
	}
}

func TestNotDeliveredOverRealDraft(t *testing.T) {
	fast(t)
	for _, name := range []string{"claude_draft", "claude_draft_after_suggestion"} {
		f := newStyled(t, name)
		SendWhenIdle(f, "@1", "hi", nil)
		time.Sleep(150 * time.Millisecond)
		if f.count() != 0 || len(f.keysSent()) != 0 {
			t.Fatalf("%s: typed over a draft (sent %d, keys %v)", name, f.count(), f.keysSent())
		}
	}
}

func TestForceClearsDraft(t *testing.T) {
	f := newStyled(t, "claude_draft")
	Deliver(f, "@1", "hi", Delivery{Force: true})
	if f.count() != 1 {
		t.Fatalf("sent %d, want 1", f.count())
	}
	if k := f.keysSent(); len(k) != 1 || k[0] != "C-u" {
		t.Fatalf("keys %v, want C-u", k)
	}
}

func fastConfirm(t *testing.T) {
	e, f := ConfirmEvery, ConfirmFor
	ConfirmEvery, ConfirmFor = 5*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() { ConfirmEvery, ConfirmFor = e, f })
}

func TestSentAfterPaneChanged(t *testing.T) {
	fastConfirm(t)
	f := newStyled(t, "claude_suggestion")
	f.onSend = func() {
		go func() {
			time.Sleep(15 * time.Millisecond)
			f.setPane("output\n❯ hi\n\nthinking…")
		}()
	}
	var mu sync.Mutex
	sent := 0
	Deliver(f, "@1", "hi", Delivery{Sent: func() { mu.Lock(); sent++; mu.Unlock() }})
	if !waitFor(func() bool { mu.Lock(); defer mu.Unlock(); return sent == 1 }) {
		t.Fatal("Sent never called after the pane changed")
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if sent != 1 || f.count() != 1 {
		t.Fatalf("Sent called %d times, text typed %d times; want 1 and 1", sent, f.count())
	}
}

func TestNotSentWhenPaneNeverChanges(t *testing.T) {
	fastConfirm(t)
	f := newStyled(t, "claude_empty")
	var mu sync.Mutex
	sent := false
	Deliver(f, "@1", "hi", Delivery{Sent: func() { mu.Lock(); sent = true; mu.Unlock() }})
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if sent {
		t.Fatal("Sent called although the pane never changed")
	}
	if f.count() != 1 {
		t.Fatalf("typed %d times, want exactly 1 (no resubmit)", f.count())
	}
}
