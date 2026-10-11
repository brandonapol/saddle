package tmux

import (
	"time"
)

// Default retry policy for SendWhenIdle. A driver that implements Paced
// sets its own instead.
var (
	RetryEvery = 3 * time.Second
	RetryFor   = 2 * time.Minute
)

// How long Deliver watches the pane for a sign the text was submitted.
var (
	ConfirmEvery = 250 * time.Millisecond
	ConfirmFor   = 5 * time.Second
)

// Timing is how Deliver retries over a draft and watches for the submit.
type Timing struct {
	RetryEvery, RetryFor     time.Duration
	ConfirmEvery, ConfirmFor time.Duration
}

// Paced drivers set their own Timing. Tests use it to speed delivery up
// without writing the package defaults, which parallel tests read (#384).
type Paced interface {
	Timing() Timing
}

// TimingOf returns d's own timing, or the package defaults.
func TimingOf(d Driver) Timing {
	if p, ok := d.(Paced); ok {
		return p.Timing()
	}
	return Timing{RetryEvery: RetryEvery, RetryFor: RetryFor, ConfirmEvery: ConfirmEvery, ConfirmFor: ConfirmFor}
}

// StyledCapturer captures a pane with its SGR escapes, which tell Claude
// Code's grey suggestion from typed text. Drivers without it fall back to a
// plain Capture.
type StyledCapturer interface {
	CaptureStyled(id string, lines int) (string, error)
}

// Delivery tunes Deliver.
type Delivery struct {
	// Needed, if non-nil, is checked before each attempt; once it returns
	// false the send is dropped.
	Needed func() bool
	// Sent, if non-nil, is called once the pane changed after the text was
	// submitted. It is never called when the pane stayed the same.
	Sent func()
	// Force types even over what looks like a draft, clearing it first. For
	// waking an agent that has sat idle with notices for too long.
	Force bool
}

// SendWhenIdle types text into the window only while its prompt is empty or
// shows just a grey suggestion. If the user is composing, it retries every
// RetryEvery for up to RetryFor in a goroutine, then gives up. needed, if
// non-nil, is checked before each attempt; once it returns false the send is
// dropped. The first attempt runs synchronously, so the common case needs no
// goroutine.
func SendWhenIdle(d Driver, id, text string, needed func() bool) {
	Deliver(d, id, text, Delivery{Needed: needed})
}

// Deliver is SendWhenIdle with a delivery callback and a force switch. The
// text is typed and submitted with Enter at most once.
func Deliver(d Driver, id, text string, o Delivery) {
	if trySend(d, id, text, o) {
		return
	}
	tm := TimingOf(d)
	every, total := tm.RetryEvery, tm.RetryFor
	go func() {
		deadline := time.Now().Add(total)
		for time.Now().Before(deadline) {
			time.Sleep(every)
			if trySend(d, id, text, o) {
				return
			}
		}
	}()
}

// capture reads the bottom of a pane, with escapes when the driver can.
func capture(d Driver, id string) (string, error) {
	if s, ok := d.(StyledCapturer); ok {
		return s.CaptureStyled(id, 15)
	}
	return d.Capture(id, 15)
}

// trySend returns true when the attempt is finished (sent, failed, or no
// longer needed) and false when a draft is in the way.
func trySend(d Driver, id, text string, o Delivery) bool {
	if o.Needed != nil && !o.Needed() {
		return true
	}
	if !d.Alive(id) {
		return true
	}
	pane, err := capture(d, id)
	in := ReadInput(pane)
	if err == nil && in == InputDraft && !o.Force {
		return false
	}
	// Typing replaces Claude Code's suggestion, but clear the line first so
	// nothing grey or stale is ever appended to. C-u only kills the line.
	if err == nil && in != InputEmpty {
		_ = d.SendKeys(id, "C-u")
	}
	before, _ := d.Capture(id, 15)
	if d.SendText(id, text) != nil || o.Sent == nil {
		return true
	}
	go confirm(d, id, before, o.Sent)
	return true
}

// confirm calls sent once the pane differs from before, or never. It looks
// at least once, even if it first wakes past the deadline on a loaded
// machine.
func confirm(d Driver, id, before string, sent func()) {
	tm := TimingOf(d)
	deadline := time.Now().Add(tm.ConfirmFor)
	for {
		time.Sleep(tm.ConfirmEvery)
		if now, err := d.Capture(id, 15); err == nil && now != before {
			sent()
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
	}
}
