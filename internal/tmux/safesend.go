package tmux

import (
	"strings"
	"time"
)

// Retry policy for SendWhenIdle. Variables so tests can shorten them.
var (
	RetryEvery = 3 * time.Second
	RetryFor   = 2 * time.Minute
)

// HasDraft reports whether the prompt input in a captured pane holds text the
// user is typing. It looks at the last prompt line ("> text", "❯ text", with
// or without a surrounding box border). Placeholder hints don't count.
func HasDraft(pane string) bool {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		l = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(l, "│"), "│"))
		var rest string
		switch {
		case strings.HasPrefix(l, "❯"):
			rest = strings.TrimPrefix(l, "❯")
		case strings.HasPrefix(l, ">"):
			rest = strings.TrimPrefix(l, ">")
		default:
			continue
		}
		rest = strings.TrimSpace(rest)
		return rest != "" && !strings.HasPrefix(rest, "Try ")
	}
	return false
}

// SendWhenIdle types text into the window only while its prompt is empty. If
// the user is composing, it retries every RetryEvery for up to RetryFor in a
// goroutine, then gives up. needed, if non-nil, is checked before each
// attempt; once it returns false the send is dropped. The first attempt runs
// synchronously, so the common case needs no goroutine.
func SendWhenIdle(d Driver, id, text string, needed func() bool) {
	if trySend(d, id, text, needed) {
		return
	}
	every, total := RetryEvery, RetryFor
	go func() {
		deadline := time.Now().Add(total)
		for time.Now().Before(deadline) {
			time.Sleep(every)
			if trySend(d, id, text, needed) {
				return
			}
		}
	}()
}

// trySend returns true when the attempt is finished (sent, failed, or no
// longer needed) and false when a draft is in the way.
func trySend(d Driver, id, text string, needed func() bool) bool {
	if needed != nil && !needed() {
		return true
	}
	if !d.Alive(id) {
		return true
	}
	pane, err := d.Capture(id, 15)
	if err == nil && HasDraft(pane) {
		return false
	}
	_ = d.SendText(id, text)
	return true
}
