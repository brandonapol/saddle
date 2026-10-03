package e2e

import (
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

// itoa is strconv.Itoa, for building args.
func itoa(n int) string { return strconv.Itoa(n) }

// errorf is fmt.Errorf, for conditions.
func errorf(format string, a ...any) error { return fmt.Errorf(format, a...) }
