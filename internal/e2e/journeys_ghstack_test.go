//go:build e2e

package e2e

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

// ghStackWorld is a World whose stacks are published with gh-stack (#211).
func ghStackWorld(t *testing.T) *World {
	return world(t, Options{Tables: "[train]\nstack_backend = \"gh-stack\"\n"})
}

// ghStackCalls are the gh stack calls the fake GitHub saw.
func ghStackCalls(s *fakegh.State) []string {
	var out []string
	for _, c := range s.Calls {
		if len(c) > 1 && c[0] == "stack" {
			out = append(out, strings.Join(c, " "))
		}
	}
	return out
}

// TestJourneyCustomStackCreate: three unrelated tasks land as three PRs on
// main. The owner makes t3 and t1 one stack, t3 at the bottom: the PRs are
// rebased and retargeted bottom to top as given though they share no files,
// t2 stays on main, and the stack is linked on GitHub with gh stack link by
// PR URL. Saddle never runs a gh stack command that pushes.
func TestJourneyCustomStackCreate(t *testing.T) {
	w := ghStackWorld(t)
	urls := landThree(t, w)

	out := w.MustSaddle("stack", "create", "ui", "t3", "t1").Stdout
	if !strings.Contains(out, "stack ui: t3 → t1") || !strings.Contains(out, "linked on GitHub") {
		t.Fatalf("create:\n%s", out)
	}
	s := w.GHState()
	t1, t2, t3 := prNumber(t, s, urls["t1"]), prNumber(t, s, urls["t2"]), prNumber(t, s, urls["t3"])
	if t3.Base != "main" || t1.Base != t3.Head || t2.Base != "main" {
		t.Fatalf("bases: t3 → %s, t1 → %s, t2 → %s; want t3 on main, t1 on t3, t2 on main", t3.Base, t1.Base, t2.Base)
	}
	if len(s.Stacks) != 1 || !slices.Equal(s.Stacks[0].PRs, []int{t3.Number, t1.Number}) || !s.Stacks[0].Open {
		t.Fatalf("GitHub stacks = %+v, want one open stack #%d → #%d", s.Stacks, t3.Number, t1.Number)
	}
	// t1's PR holds only its own work on top of t3's.
	if got := w.OriginFile(t1.Head, "beta/work.txt"); got != "" {
		t.Fatalf("t1's PR carries t2's work: %q", got)
	}
	if got := w.OriginFile(t1.Head, "gamma/work.txt"); got == "" {
		t.Fatal("t1's PR doesn't sit on t3's work")
	}
	for _, c := range ghStackCalls(s) {
		f := strings.Fields(c)
		if !slices.Contains([]string{"--version", "link", "view", "merge"}, f[1]) {
			t.Fatalf("saddle ran %q; only link, view and merge are allowed", c)
		}
		if f[1] == "link" && !strings.Contains(c, "/pull/") {
			t.Fatalf("link by branch would push: %q", c)
		}
	}
	if out := w.MustSaddle("stack", "show", "ui").Stdout; !strings.Contains(out, "1. t3") || !strings.Contains(out, "2. t1") || !strings.Contains(out, "GitHub stack #") {
		t.Fatalf("show:\n%s", out)
	}
	if out := w.MustSaddle("stack", "list").Stdout; !strings.Contains(out, "ui: t3 → t1") {
		t.Fatalf("list:\n%s", out)
	}
	// prs again keeps the layout and the one GitHub stack.
	w.MustSaddle("prs")
	s = w.GHState()
	if len(s.Stacks) != 1 || prNumber(t, s, urls["t1"]).Base != t3.Head {
		t.Fatalf("after prs: stacks %+v, t1 → %s", s.Stacks, prNumber(t, s, urls["t1"]).Base)
	}
}

// TestJourneyCustomStackFallback: on a repo without Stacked PRs, create
// still records the stack and publishes it the saddle way, says so, and
// logs one fallback event however many times prs runs.
func TestJourneyCustomStackFallback(t *testing.T) {
	w := ghStackWorld(t)
	must(t, w.GH.Update(func(s *fakegh.State) error { s.StacksDisabled = true; return nil }))
	urls := landThree(t, w)

	out := w.MustSaddle("stack", "create", "ui", "t3", "t1").Stdout
	if !strings.Contains(out, "not linked on GitHub") || !strings.Contains(out, "chained by PR bases") {
		t.Fatalf("create:\n%s", out)
	}
	s := w.GHState()
	if t1, t3 := prNumber(t, s, urls["t1"]), prNumber(t, s, urls["t3"]); t1.Base != t3.Head {
		t.Fatalf("t1 → %s, want t3's %s", t1.Base, t3.Head)
	}
	if len(s.Stacks) != 0 {
		t.Fatalf("stacks on a disabled repo: %+v", s.Stacks)
	}
	w.MustSaddle("prs")
	if evs := events(t, w.App(), "gh_stack_fallback"); len(evs) != 1 {
		t.Fatalf("fallback events = %q, want one", evs)
	}
	if out := w.Saddle("doctor").Stdout; !strings.Contains(out, "Stacked PRs aren't enabled") {
		t.Fatalf("doctor doesn't say why:\n%s", out)
	}
}

// TestJourneyGhStackAtomicMerge: a custom stack of two PRs merges into main
// in one gh stack merge call, all or nothing; saddle then restacks, so both
// tasks leave the PR stack as merged and the unrelated PR stays open.
func TestJourneyGhStackAtomicMerge(t *testing.T) {
	w := ghStackWorld(t)
	urls := landThree(t, w)
	w.MustSaddle("stack", "create", "ui", "t3", "t1")
	must(t, w.GH.SetAllChecks(fakegh.Pass))

	out := w.MustSaddle("stack", "merge", "ui").Stdout
	if !strings.Contains(out, "merged t3 → t1 up to "+urls["t1"]) {
		t.Fatalf("merge:\n%s", out)
	}
	s := w.GHState()
	var merges []string
	for _, c := range ghStackCalls(s) {
		if strings.HasPrefix(c, "stack merge") {
			merges = append(merges, c)
		}
	}
	t1 := prNumber(t, s, urls["t1"])
	if len(merges) != 1 || !strings.Contains(merges[0], "--yes") || !strings.HasPrefix(merges[0], "stack merge "+strconv.Itoa(t1.Number)) {
		t.Fatalf("stack merges = %q, want one up to #%d with --yes", merges, t1.Number)
	}
	for _, id := range []string{"t1", "t3"} {
		if p := prNumber(t, s, urls[id]); p.State != "MERGED" {
			t.Fatalf("%s's PR = %s, want MERGED", id, p.State)
		}
	}
	if p := prNumber(t, s, urls["t2"]); p.State != "OPEN" || p.Base != "main" {
		t.Fatalf("t2's PR = %s on %s, want open on main", p.State, p.Base)
	}
	if s.Stacks[0].Open {
		t.Fatal("GitHub stack still open after merging all of it")
	}
	for _, f := range []string{"alpha/work.txt", "gamma/work.txt"} {
		if w.OriginFile("main", f) == "" {
			t.Fatalf("main lacks %s", f)
		}
	}
	for _, id := range []string{"t1", "t3"} {
		if v := w.Task(id); !strings.HasPrefix(v.Train, "merged") {
			t.Fatalf("%s train = %q, want merged", id, v.Train)
		}
	}
	if evs := events(t, w.App(), "gh_stack_merge"); len(evs) != 1 {
		t.Fatalf("merge events = %q", evs)
	}

	// A stack GitHub won't merge moves nothing.
	w2 := ghStackWorld(t)
	urls2 := landThree(t, w2)
	w2.MustSaddle("stack", "create", "ui", "t3", "t1")
	s2 := w2.GHState()
	top := prNumber(t, s2, urls2["t1"]).Number
	must(t, w2.GH.Update(func(s *fakegh.State) error { s.PR(top).Mergeable = "CONFLICTING"; return nil }))
	if r := w2.Saddle("stack", "merge", "ui"); r.Code == 0 || !strings.Contains(r.Stderr, "nothing was merged") {
		t.Fatalf("merge of a conflicting stack: %s", r)
	}
	s2 = w2.GHState()
	if p := prNumber(t, s2, urls2["t3"]); p.State != "OPEN" {
		t.Fatalf("t3 = %s after a refused stack merge, want OPEN", p.State)
	}
}
