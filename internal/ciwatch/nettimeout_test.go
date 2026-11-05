package ciwatch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/netx"
)

// TestPollOverNetTimeoutIsSilent: a gh call that timed out on a dead
// connection is a wait, like the rate limit: the poll logs nothing, reports
// nothing and keeps what it knew (#788).
func TestPollOverNetTimeoutIsSilent(t *testing.T) {
	gh := newFake()
	w, errs := newWatcher(t, gh, t4)
	gh.checks("60", check("fail", jobLink))
	if evs := w.Poll(context.Background()); len(evs) != 1 || len(*errs) != 0 {
		t.Fatalf("first poll: events=%#v errors=%v", evs, *errs)
	}
	gh.errs["60"] = fmt.Errorf("gh pr checks: %w", &netx.TimeoutError{Op: "gh pr checks", After: time.Minute})
	if evs := w.Poll(context.Background()); len(evs) != 0 || len(*errs) != 0 {
		t.Fatalf("poll over a timeout: events=%#v errors=%v", evs, *errs)
	}
	delete(gh.errs, "60")
	if evs := w.Poll(context.Background()); len(evs) != 0 || len(*errs) != 0 {
		t.Fatalf("after the network came back: events=%#v errors=%v; want state kept", evs, *errs)
	}
}
