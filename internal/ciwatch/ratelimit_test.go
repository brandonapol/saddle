package ciwatch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// TestPollUnderRateLimitIsSilent: while GitHub is rate-limited a poll logs
// no errors and no events, stops spending calls, and keeps what it knew, so
// the first poll after the reset reports nothing new (#764).
func TestPollUnderRateLimitIsSilent(t *testing.T) {
	gh := newFake()
	t5 := Target{Task: "t5", Branch: "saddle/t5-x", PR: "61"}
	gh.checks("60", check("fail", jobLink))
	gh.checks("61", check("fail", jobLink2))
	w, errs := newWatcher(t, gh, t4, t5)
	if evs := w.Poll(context.Background()); len(evs) != 2 || len(*errs) != 0 {
		t.Fatalf("first poll: events=%#v errors=%v", evs, *errs)
	}

	limited := fmt.Errorf("gh pr checks: %w", &forge.RateLimitError{Until: time.Now().Add(time.Hour)})
	gh.errs["60"], gh.errs["61"] = limited, limited
	gh.calls = nil
	for range 3 {
		if evs := w.Poll(context.Background()); len(evs) != 0 {
			t.Fatalf("poll under the limit: events=%#v", evs)
		}
	}
	if len(*errs) != 0 {
		t.Fatalf("poll under the limit logged %d errors: %v", len(*errs), *errs)
	}
	if n := gh.count("checks"); n != 3 {
		t.Fatalf("checks calls = %d, want 1 per poll: the rest wait for the reset", n)
	}

	delete(gh.errs, "60")
	delete(gh.errs, "61")
	if evs := w.Poll(context.Background()); len(evs) != 0 || len(*errs) != 0 {
		t.Fatalf("after the reset: events=%#v errors=%v; want state kept, nothing new", evs, *errs)
	}
}

// TestTargetsUnderRateLimitIsSilent: a Targets error from the rate limit
// logs nothing.
func TestTargetsUnderRateLimitIsSilent(t *testing.T) {
	var errs []error
	w, err := New(Config{
		CI: newFake(),
		Targets: func(context.Context) ([]Target, error) {
			return nil, &forge.RateLimitError{Until: time.Now().Add(time.Hour)}
		},
		OnError: func(_ Target, err error) { errs = append(errs, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if evs := w.Poll(context.Background()); len(evs) != 0 || len(errs) != 0 {
		t.Fatalf("events=%#v errors=%v", evs, errs)
	}
}
