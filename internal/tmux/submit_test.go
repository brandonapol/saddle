package tmux

import (
	"sync"
	"testing"
	"time"
)

// stickyDriver is a pane whose input box keeps typed text until it has had
// enough Enters: SendText's own Enter is dropped, the way a paste still
// being taken in swallows it (#467).
type stickyDriver struct {
	mu     sync.Mutex
	draft  string
	need   int // Enters (after SendText's) before the box submits
	enters int
}

func (s *stickyDriver) HasSession() bool                                  { return true }
func (s *stickyDriver) NewSession(string, string, string) (string, error) { return "", nil }
func (s *stickyDriver) NewWindow(string, string, string) (string, error)  { return "", nil }
func (s *stickyDriver) KillWindow(string) error                           { return nil }
func (s *stickyDriver) Alive(string) bool                                 { return true }
func (s *stickyDriver) KillSession() error                                { return nil }
func (s *stickyDriver) Timing() Timing {
	return Timing{RetryEvery: time.Millisecond, RetryFor: 50 * time.Millisecond, ConfirmEvery: time.Millisecond, ConfirmFor: 50 * time.Millisecond}
}
func (s *stickyDriver) SendText(_, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draft = text
	return nil
}
func (s *stickyDriver) SendKeys(_ string, keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if k == "Enter" {
			s.enters++
			if s.enters >= s.need {
				s.draft = ""
			}
		}
	}
	return nil
}
func (s *stickyDriver) Capture(string, int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "output\n╭──────╮\n│ > " + s.draft + " │\n╰──────╯", nil
}
func (s *stickyDriver) pressed() int { s.mu.Lock(); defer s.mu.Unlock(); return s.enters }

// #467: text left in the input box is submitted with another Enter, and
// Submit says the box emptied.
func TestSubmitPressesEnterWhileTextSits(t *testing.T) {
	t.Parallel()
	d := &stickyDriver{need: 2}
	ok, err := Submit(d, "@1", "[saddle] call the saddle done tool again")
	if err != nil || !ok {
		t.Fatalf("Submit = %v, %v; want the box emptied", ok, err)
	}
	if n := d.pressed(); n != 2 {
		t.Fatalf("pressed Enter %d times, want 2", n)
	}
}

// A box that never takes the text is reported, after a bounded number of
// Enters.
func TestSubmitGivesUp(t *testing.T) {
	t.Parallel()
	d := &stickyDriver{need: 100}
	ok, err := Submit(d, "@1", "hello there")
	if err != nil || ok {
		t.Fatalf("Submit = %v, %v; want false", ok, err)
	}
	if n := d.pressed(); n != SubmitRetries {
		t.Fatalf("pressed Enter %d times, want %d", n, SubmitRetries)
	}
}

// Someone else's draft is not saddle's text: no Enter for it.
func TestUnsubmittedIgnoresOtherDrafts(t *testing.T) {
	t.Parallel()
	pane := "output\n╭──────╮\n│ > half typed │\n╰──────╯"
	if Unsubmitted(pane, "[saddle] wake up") {
		t.Fatal("a user's draft counted as saddle's unsubmitted text")
	}
	if !Unsubmitted(pane, "half typed") {
		t.Fatal("saddle's own text in the box not seen")
	}
	if Unsubmitted("output\n╭──────╮\n│ >    │\n╰──────╯", "half typed") {
		t.Fatal("an empty box counted as unsubmitted")
	}
}

// #467: Deliver checks its text was submitted, too.
func TestDeliverResubmitsStuckText(t *testing.T) {
	t.Parallel()
	d := &stickyDriver{need: 1}
	Deliver(d, "@1", "[saddle] notices", Delivery{})
	if !waitFor(func() bool { return d.pressed() == 1 }) {
		t.Fatalf("Deliver pressed Enter %d times, want 1", d.pressed())
	}
}
