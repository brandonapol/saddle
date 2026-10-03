//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

// TestJourneyGCAfterSquashMerges: a stack is squash-merged on GitHub, so
// none of its branches is an ancestor of main. gc still sees the work is on
// main and removes the tasks' worktrees and branches, local and remote,
// and doctor's leftovers warning clears. A killed task's unmerged branch is
// kept, with the reason.
func TestJourneyGCAfterSquashMerges(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	urls := landTwo(t, w)
	w.Spawn("t3", "Gamma work", []string{"gamma/**"},
		fa.Write("gamma/work.txt", "gamma\n"), fa.Commit("work in gamma"), fa.Wait("never-comes"))
	w.WaitAgentLog("t3", "step: 2 commit ok")
	t3 := w.Task("t3")
	w.MustSaddle("kill", "t3")

	low := prNumber(t, w.GHState(), urls["t1"])
	must(t, w.GH.MergeByHand(low.Number, "squash"))
	w.MustMCP("t0", "restack", nil)
	high := prNumber(t, w.GHState(), urls["t2"])
	must(t, w.GH.MergeByHand(high.Number, "squash"))
	w.MustMCP("t0", "restack", nil)

	t1, t2 := w.Task("t1"), w.Task("t2")
	w.Git(w.Repo, "fetch", "-q", "origin")
	if r := w.MustSaddle("doctor", "--json"); doctorChecks(t, r.Stdout)["leftovers"].Status != "warn" {
		t.Fatalf("doctor before gc: leftovers = %+v, want a warning", doctorChecks(t, r.Stdout)["leftovers"])
	}
	if r := w.MustSaddle("gc", "--dry-run"); !strings.Contains(r.Stdout, "leftover") {
		t.Fatalf("gc --dry-run lists nothing: %s", r)
	}

	r := w.MustSaddle("gc")
	for _, b := range []string{t1.Branch, t2.Branch} {
		if !strings.Contains(r.Stdout, "removed  branch   "+b) {
			t.Errorf("gc didn't remove merged branch %s:\n%s", b, r.Stdout)
		}
		if !strings.Contains(r.Stdout, "removed  remote   origin/"+b) {
			t.Errorf("gc didn't remove merged remote branch origin/%s:\n%s", b, r.Stdout)
		}
		if out := w.Git(w.Repo, "branch", "--list", b); out != "" {
			t.Errorf("branch %s is still there", b)
		}
		if out := w.Git(w.Origin, "--git-dir", w.Origin, "branch", "--list", b); out != "" {
			t.Errorf("origin still has %s", b)
		}
	}
	for _, v := range []struct{ id, wt string }{{"t1", t1.Worktree}, {"t2", t2.Worktree}, {"t3", t3.Worktree}} {
		if _, err := os.Stat(v.wt); err == nil {
			t.Errorf("%s's worktree %s survived gc", v.id, v.wt)
		}
	}
	if !strings.Contains(r.Stdout, "kept     branch   "+t3.Branch) {
		t.Errorf("gc didn't report keeping the killed task's unmerged branch %s:\n%s", t3.Branch, r.Stdout)
	}
	if out := w.Git(w.Repo, "log", "-1", "--format=%s", t3.Branch); out != "work in gamma" {
		t.Errorf("unmerged branch %s lost its work: %q", t3.Branch, out)
	}

	r = w.MustSaddle("doctor", "--json")
	if c := doctorChecks(t, r.Stdout)["leftovers"]; c.Status != "ok" || !strings.Contains(c.Detail, "1 kept") {
		t.Fatalf("doctor after gc: leftovers = %+v, want ok with the kept branch noted", c)
	}
	// A second gc has nothing left to remove.
	if r := w.MustSaddle("gc"); strings.Contains(r.Stdout, "removed") {
		t.Fatalf("second gc removed more:\n%s", r.Stdout)
	}
}
