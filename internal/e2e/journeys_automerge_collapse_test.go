//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/automerge"
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
