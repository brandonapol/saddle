package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// #764: a default whose action hits GitHub's rate limit is a wait, not a
// failure. The card stays pending with RetryAfter at the reset, logs no
// decision_default_failed and returns no error (which the sentinel would
// raise as sentinel_error every tick); it runs again after the reset.
func TestExpireDecisionsRateLimitedDefaultWaits(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now().UTC()
	c := testCard("x")
	c.Opened = now.Add(-time.Hour)
	must(t, a.OpenDecision(c))
	until := now.Add(20 * time.Minute).Truncate(time.Second)
	runs := 0
	limited := func(DecisionCard, DecisionOption) error {
		runs++
		return fmt.Errorf("label needs-human: %w", &forge.RateLimitError{Until: until})
	}

	got, err := a.ExpireDecisions(now, limited)
	if err != nil || len(got) != 0 {
		t.Fatalf("rate-limited default: closed %+v, err %v; want pending and no error", got, err)
	}
	if hasEvent(t, a, EventDecisionDefaultFailed, "x") {
		t.Fatal("rate-limited default logged decision_default_failed")
	}
	open, _ := a.OpenDecisions()
	if len(open) != 1 || !open[0].RetryAfter.Equal(until) {
		t.Fatalf("open = %+v; want the card pending with retry_after %v", open, until)
	}
	if _, err := a.ExpireDecisions(now.Add(time.Minute), limited); err != nil || runs != 1 {
		t.Fatalf("ran %d times before retry_after (err %v); want 1", runs, err)
	}
	ok := func(DecisionCard, DecisionOption) error { return nil }
	if got, err := a.ExpireDecisions(until.Add(time.Second), ok); err != nil || len(got) != 1 {
		t.Fatalf("after the reset: closed %+v, err %v; want the default run", got, err)
	}
}
