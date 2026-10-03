package tmux

import (
	"time"
)

// Retry policy for SendWhenIdle. Variables so tests can shorten them.
var (
	RetryEvery = 3 * time.Second
	RetryFor   = 2 * time.Minute
)

// How long Deliver watches the pane for a sign the text was submitted.
var (
	ConfirmEvery = 250 * time.Millisecond
	ConfirmFor   = 5 * time.Second
)

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
	every, total := RetryEvery, RetryFor
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

// confirm calls sent once the pane differs from before, or never.
func confirm(d Driver, id, before string, sent func()) {
	deadline := time.Now().Add(ConfirmFor)
	for time.Now().Before(deadline) {
		time.Sleep(ConfirmEvery)
		if now, err := d.Capture(id, 15); err == nil && now != before {
			sent()
			return
		}
	}
}
