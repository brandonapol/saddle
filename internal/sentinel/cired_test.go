package sentinel

import (
	"testing"
	"time"
)

func TestRollupVerdict(t *testing.T) {
	for _, c := range []struct {
		r    rollup
		want string
	}{
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SUCCESS"}, "pass"},
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "FAILURE"}, "fail"},
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "TIMED_OUT"}, "fail"},
		{rollup{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SKIPPED"}, "pass"},
		{rollup{Typename: "CheckRun", Status: "IN_PROGRESS"}, "pending"},
		{rollup{Typename: "StatusContext", State: "ERROR"}, "fail"},
		{rollup{Typename: "StatusContext", State: "PENDING"}, "pending"},
		{rollup{Typename: "StatusContext", State: "SUCCESS"}, "pass"},
	} {
		if got := c.r.verdict(); got != c.want {
			t.Errorf("%+v: verdict = %s, want %s", c.r, got, c.want)
		}
	}
	if l := (rollup{Typename: "CheckRun", Name: "lint", Workflow: "CI"}).label(); l != "CI / lint" {
		t.Errorf("label = %q", l)
	}
}

// Quiet cycles back off up to the cap; anything red or changing resets it.
func TestCIRedBackoff(t *testing.T) {
	iv, maxIv := time.Minute, 5*time.Minute
	w := iv
	var got []time.Duration
	for range 4 {
		w = nextWait(w, iv, maxIv, true)
		got = append(got, w)
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	}
	if w = nextWait(w, iv, maxIv, false); w != iv {
		t.Fatalf("a red cycle waited %v, want %v", w, iv)
	}
}
