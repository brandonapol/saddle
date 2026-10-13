package tmux

import (
	"strings"
	"time"
)

// A typed line sometimes stays in Claude Code's input box: the Enter that
// followed it arrived while the pasted text was still being taken in, so
// nothing was submitted and the agent never woke (#467). Submit checks the
// box afterwards and presses Enter again while saddle's own text sits there.

// SubmitRetries is how many extra Enters Submit presses before giving up.
const SubmitRetries = 3

// Unsubmitted reports whether pane's input box still holds text: a draft
// that shows the start of what was typed. A draft of other text is the
// user's, and is left alone.
func Unsubmitted(pane, text string) bool {
	if ReadInput(pane) != InputDraft {
		return false
	}
	head := submitHead(text)
	return head != "" && strings.Contains(StripANSI(pane), head)
}

// submitHead is the start of text's first line, short enough to fit on one
// line of the input box.
func submitHead(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	r := []rune(strings.TrimSpace(line))
	if len(r) > 24 {
		r = r[:24]
	}
	return strings.TrimSpace(string(r))
}

// Submit types text into id and presses Enter, then makes sure it was
// submitted (EnsureSubmitted). It reports whether the input box emptied.
func Submit(d Driver, id, text string) (bool, error) {
	if err := d.SendText(id, text); err != nil {
		return false, err
	}
	return EnsureSubmitted(d, id, text), nil
}

// EnsureSubmitted watches id's input box after text was typed: while text
// still sits there unsubmitted it presses Enter, up to SubmitRetries times.
// It reports whether the box is free of text at the end.
func EnsureSubmitted(d Driver, id, text string) bool {
	every := TimingOf(d).ConfirmEvery
	for i := 0; ; i++ {
		time.Sleep(every)
		pane, err := capture(d, id)
		if err != nil {
			return false
		}
		if !Unsubmitted(pane, text) {
			return true
		}
		if i == SubmitRetries {
			return false
		}
		_ = d.SendKeys(id, "Enter")
	}
}
