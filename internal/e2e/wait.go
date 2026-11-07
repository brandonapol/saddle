package e2e

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// Timeout is how long Eventually waits by default. E2E_TIMEOUT (a Go
// duration) overrides it for slow machines.
func Timeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("E2E_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return 45 * time.Second
}

// Poll returns nil once cond does, or cond's last error after timeout. It
// checks often at first and backs off to 100ms; it never sleeps a fixed
// time waiting for something to happen.
func Poll(timeout time.Duration, cond func() error) error {
	deadline := time.Now().Add(timeout)
	every := 5 * time.Millisecond
	for {
		err := cond()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(every)
		every = min(every*2, 100*time.Millisecond)
	}
}

// Eventually fails t unless cond returns nil within Timeout. what names
// the condition in the failure.
func Eventually(t testing.TB, what string, cond func() error) {
	t.Helper()
	start := time.Now()
	if err := Poll(Timeout(), cond); err != nil {
		t.Fatalf("waited %s for %s: %v", time.Since(start).Round(time.Millisecond), what, err)
	}
}

// WaitPane polls target's pane until ok holds for its text, within Timeout
// (E2E_TIMEOUT scales it for loaded machines). It returns the pane that
// satisfied ok. On timeout it fails t naming what, with the last pane text.
// Use it instead of sleeping for a pane to settle (#882).
func (x *Tmux) WaitPane(t testing.TB, target, what string, ok func(pane string) bool) string {
	t.Helper()
	return waitPane(t, Timeout(), func() (string, error) { return x.Capture(target) }, what, ok)
}

var errPaneNotYet = errors.New("condition not met")

// waitPane is WaitPane over any capture, with its timeout given.
func waitPane(t testing.TB, timeout time.Duration, capture func() (string, error), what string, ok func(string) bool) string {
	t.Helper()
	var last string
	var captureErr error
	start := time.Now()
	err := Poll(timeout, func() error {
		s, err := capture()
		if err != nil {
			captureErr = err
			return err
		}
		last, captureErr = s, nil
		if ok(s) {
			return nil
		}
		return errPaneNotYet
	})
	if err != nil {
		why := ""
		if captureErr != nil {
			why = fmt.Sprintf(" (capture: %v)", captureErr)
		}
		t.Fatalf("waited %s for %s%s; last pane:\n%s", time.Since(start).Round(time.Millisecond), what, why, last)
	}
	return last
}

// itoa is strconv.Itoa, for building args.
func itoa(n int) string { return strconv.Itoa(n) }
