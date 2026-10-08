//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

// TestJourneyAutomergeCollapsesRedBottom reproduces the 2026-10-03 deadlock
// (#196): t1's PR is red from a flaky test, the fix landed as t2's PR above
// it and is green, and auto-merge, merging bottom-up on green only, waited
// on t1 forever. Now status says a collapse is next, and one watcher tick
// merges t2's PR into main carrying t1, with no human edit.
func TestJourneyAutomergeCollapsesRedBottom(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	low, high := redBottomStack(t, w)
	url := w.Task("t2").PR
	w.MustSaddle("automerge", "on")

	out := w.MustSaddle("automerge", "status").Stdout
	if !strings.Contains(out, "its CI is red") || !strings.Contains(out, "collapse the stack into "+url) {
		t.Fatalf("status doesn't say a collapse is next:\n%s", out)
	}

	st := w.AutomergeTick()
	if st.Merged != url || st.Stopped != "" {
		t.Fatalf("tick merged %q (stopped %q), want t2's %s", st.Merged, st.Stopped, url)
	}
	checkCollapsed(t, w, low, high)
	if got := events(t, w.App(), automerge.EventCollapsed); len(got) != 1 || !strings.Contains(got[0], url) {
		t.Fatalf("collapsed events = %q, want one naming %s", got, url)
	}

	// The next tick finds nothing left to do and doesn't stop.
	if st := w.AutomergeTick(); st.Merged != "" || st.Stopped != "" || len(st.Stacks) != 0 {
		t.Fatalf("second tick = %+v, want an empty stack and no stop", st)
	}
}

// TestJourneyAutomergeNeverMergesCIRed: once the ci-red watcher holds t1
// red, auto-merge merges neither t1 nor t2 above it, not even by collapsing
// into green t2; status names the red layer and promises no collapse.
func TestJourneyAutomergeNeverMergesCIRed(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	low, high := redBottomStack(t, w)
	if rep := w.CIRedTick(); len(rep.Red) != 1 || rep.Red[0].Task != "t1" {
		t.Fatalf("red layers = %+v, want t1", rep.Red)
	}
	w.MustSaddle("automerge", "on")

	out := w.MustSaddle("automerge", "status").Stdout
	if !strings.Contains(out, "CI is red on its PR") || strings.Contains(out, "collapse the stack") {
		t.Fatalf("status doesn't hold the stack for ci-red:\n%s", out)
	}
	for range 2 {
		if st := w.AutomergeTick(); st.Merged != "" || st.Stopped != "" {
			t.Fatalf("tick merged %q (stopped %q) past ci-red", st.Merged, st.Stopped)
		}
	}
	s := w.GHState()
	for _, p := range []*fakegh.PR{low, high} {
		if got := s.PR(p.Number); got.State != "OPEN" {
			t.Fatalf("PR #%d = %s, want open: ci-red holds it", p.Number, got.State)
		}
	}
	if got := events(t, w.App(), automerge.EventCollapsed); len(got) != 0 {
		t.Fatalf("collapsed past ci-red: %q", got)
	}
}
