package e2e

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fatalRecorder is a testing.TB whose Fatalf records the message and stops
// the calling goroutine, the way t.Fatalf does.
type fatalRecorder struct {
	testing.TB
	msg string
}

func (r *fatalRecorder) Helper() {}
func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	panic(r)
}

// runRecorded runs f against a fatalRecorder and returns the fatal message,
// or "" when f returned without failing.
func runRecorded(t *testing.T, f func(tb testing.TB)) (msg string) {
	t.Helper()
	r := &fatalRecorder{TB: t}
	defer func() {
		if p := recover(); p != nil {
			if p != r {
				panic(p)
			}
			msg = r.msg
		}
	}()
	f(r)
	return ""
}

// #882: waitPane returns as soon as the pane satisfies the condition, not
// after some fixed pause.
func TestWaitPaneReturnsOnceConditionHolds(t *testing.T) {
	var calls atomic.Int32
	capture := func() (string, error) {
		if calls.Add(1) < 3 {
			return "starting", nil
		}
		return "ready $", nil
	}
	start := time.Now()
	var got string
	if msg := runRecorded(t, func(tb testing.TB) {
		got = waitPane(tb, time.Minute, capture, "the prompt", func(s string) bool { return strings.Contains(s, "$") })
	}); msg != "" {
		t.Fatalf("waitPane failed: %s", msg)
	}
	if got != "ready $" {
		t.Fatalf("returned %q, want the pane that satisfied the condition", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("captured %d times, want 3: it should stop polling once the condition holds", n)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s for a condition that held on the third capture", d)
	}
}

// #882: on timeout the failure names what was awaited and prints the last
// pane text, so a flake can be read from the log.
func TestWaitPaneFailsWithLastPaneText(t *testing.T) {
	var n atomic.Int32
	capture := func() (string, error) {
		return fmt.Sprintf("frame %d\nstill loading", n.Add(1)), nil
	}
	msg := runRecorded(t, func(tb testing.TB) {
		waitPane(tb, 50*time.Millisecond, capture, "the prompt", func(string) bool { return false })
	})
	if msg == "" {
		t.Fatal("waitPane returned although the condition never held")
	}
	last := fmt.Sprintf("frame %d\nstill loading", n.Load())
	if !strings.Contains(msg, "the prompt") || !strings.Contains(msg, last) {
		t.Fatalf("failure should name the wait and show the last pane %q; got:\n%s", last, msg)
	}
}

// A capture error is reported too, so a dead pane isn't a silent timeout.
func TestWaitPaneReportsCaptureError(t *testing.T) {
	capture := func() (string, error) { return "", errors.New("can't find pane: e2e-tui") }
	msg := runRecorded(t, func(tb testing.TB) {
		waitPane(tb, 20*time.Millisecond, capture, "the prompt", func(string) bool { return true })
	})
	if !strings.Contains(msg, "can't find pane") {
		t.Fatalf("failure should carry the capture error; got:\n%s", msg)
	}
}
