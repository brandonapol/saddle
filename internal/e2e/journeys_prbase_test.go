//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"
)

// TestJourneyManualRetargetIsNotAnEdge (#359, as narrowed by #934): three
// unrelated tasks go up as three PRs on main and the owner retargets two of
// them by hand. A hand retarget is not an after edge (ADR 0012 rule 8), so
// restack records no dependency and puts both back on main, each in a
// pr_base event naming the old and new base.
func TestJourneyManualRetargetIsNotAnEdge(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	urls := landThree(t, w)
	s := w.GHState()
	t1, t2, t3 := prNumber(t, s, urls["t1"]), prNumber(t, s, urls["t2"]), prNumber(t, s, urls["t3"])
	if r := w.Exec(w.Repo, "gh", "pr", "edit", strconv.Itoa(t2.Number), "--base", t1.Head); r.Code != 0 {
		t.Fatalf("retarget t2 by hand: %s", r)
	}
	if r := w.Exec(w.Repo, "gh", "pr", "edit", strconv.Itoa(t1.Number), "--base", t3.Head); r.Code != 0 {
		t.Fatalf("retarget t1 by hand: %s", r)
	}

	w.MustMCP("t0", "restack", nil)
	s = w.GHState()
	for _, id := range []string{"t1", "t2"} {
		if got := prNumber(t, s, urls[id]).Base; got != "main" {
			t.Fatalf("after restack %s's PR targets %s, want main", id, got)
		}
	}
	a := w.App()
	if deps := events(t, a, "depends_on"); len(deps) != 0 {
		t.Fatalf("depends_on events = %q, want none from a hand retarget", deps)
	}
	bases := strings.Join(events(t, a, "pr_base"), "\n")
	if !strings.Contains(bases, t1.Head+" → main") || !strings.Contains(bases, t3.Head+" → main") {
		t.Fatalf("pr_base events = %q, want both changes back named", bases)
	}
}
