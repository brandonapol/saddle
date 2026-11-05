//go:build e2e

package e2e

import (
	"slices"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

// TestJourneyParkRepairCapCreatesNeedsHumanLabel (#787): the repo lacks the
// needs-human label, yet parking a repair-capped layer succeeds, the label
// exists afterwards and no decision default failed.
func TestJourneyParkRepairCapCreatesNeedsHumanLabel(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	urls := landTwo(t, w)
	low := prNumber(t, w.GHState(), urls["t1"])
	a := w.App()
	// saddle init already created the labels; this repo predates that.
	must(t, w.GH.Update(func(s *fakegh.State) error {
		s.Labels = slices.DeleteFunc(s.Labels, func(l string) bool { return l == "needs-human" })
		return nil
	}))
	if slices.Contains(w.GHState().Labels, "needs-human") {
		t.Fatal("precondition: repo already has the needs-human label")
	}
	if hasLabel(w.GHState().PR(low.Number), "needs-human") {
		t.Fatal("precondition: PR already labeled")
	}
	must(t, a.ParkRepairCap("t1", time.Minute))
	s := w.GHState()
	if !hasLabel(s.PR(low.Number), "needs-human") {
		t.Fatalf("PR labels = %v, want needs-human", s.PR(low.Number).Labels)
	}
	evs, err := a.Store.EventsSince(0)
	must(t, err)
	for _, e := range evs {
		if e.Kind == app.EventDecisionDefaultFailed {
			t.Fatalf("decision default failed: %+v", e)
		}
	}
}
