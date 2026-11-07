package doctor

import (
	"errors"
	"strings"
	"testing"
)

// Doctor shows the forge facts saddle acts on (#822): whether the repo has
// Stacked PRs, a merge queue on base, its required checks and auto-merge.
func TestForgeFactsShown(t *testing.T) {
	f := healthy(t)
	f.gh["api repos/{owner}/{repo}/stacks?per_page=1"] = res{out: "[]"}
	f.gh["api repos/{owner}/{repo}/rules/branches/main"] = res{out: `[{"type":"merge_queue"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`}
	r := find(t, Run(f), CheckForgeFacts)
	if r.Status != OK {
		t.Fatalf("%s %q", r.Status, r.Detail)
	}
	for _, want := range []string{"Stacked PRs on", "merge queue on main", "required checks: test"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q lacks %q", r.Detail, want)
		}
	}
}

// A transient failure is a warning, never a failure; facts the probe can't
// read are named.
func TestForgeFactsDegrade(t *testing.T) {
	f := healthy(t)
	f.gh["api repos/{owner}/{repo}/stacks?per_page=1"] = res{err: errors.New("gh api: signal: killed")}
	if r := find(t, Run(f), CheckForgeFacts); r.Status != Warn {
		t.Fatalf("killed probe: %s %q", r.Status, r.Detail)
	}

	f = healthy(t)
	r := find(t, Run(f), CheckForgeFacts)
	if r.Status != OK || !strings.Contains(r.Detail, "Stacked PRs off") || !strings.Contains(r.Detail, "couldn't read") {
		t.Fatalf("%s %q", r.Status, r.Detail)
	}
}
