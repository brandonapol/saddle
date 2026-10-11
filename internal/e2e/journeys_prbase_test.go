//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"
)

// TestJourneyManualRetargetSurvivesRestack (#359): three unrelated tasks go
// up as three PRs on main. The owner retargets t2's PR onto t1's branch with
// gh pr edit --base so its CI runs on top of t1. Restack keeps that base and
// records the dependency, so prs keeps it too. The owner also moves t1's PR
// onto t3's branch, which landed after it; saddle can't stack it there, so
// restack puts it back on main and says so in a pr_base event naming both
// bases.
func TestJourneyManualRetargetSurvivesRestack(t *testing.T) {
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
	if got := prNumber(t, s, urls["t2"]).Base; got != t1.Head {
		t.Fatalf("after restack t2's PR targets %s, want the hand-set %s", got, t1.Head)
	}
	if got := w.OriginFile(prNumber(t, s, urls["t2"]).Head, "alpha/work.txt"); got == "" {
		t.Fatal("t2's PR doesn't sit on t1's work")
	}
	if got := prNumber(t, s, urls["t1"]).Base; got != "main" {
		t.Fatalf("t1's PR targets %s, want main", got)
	}
	a := w.App()
	if deps := events(t, a, "depends_on"); len(deps) != 1 || !strings.Contains(deps[0], "manual retarget") {
		t.Fatalf("depends_on events = %q, want t2's edge from the retarget", deps)
	}
	if bases := events(t, a, "pr_base"); len(bases) != 1 || !strings.Contains(bases[0], t3.Head+" → main") {
		t.Fatalf("pr_base events = %q, want t1's change back named", bases)
	}

	w.MustSaddle("prs")
	if got := prNumber(t, w.GHState(), urls["t2"]).Base; got != t1.Head {
		t.Fatalf("after prs t2's PR targets %s, want %s", got, t1.Head)
	}
}
