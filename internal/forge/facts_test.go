package forge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	repoJSON  = `{"full_name":"o/r","html_url":"https://github.com/o/r","allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":false,"allow_auto_merge":true}`
	rulesJSON = `[{"type":"merge_queue","parameters":{}},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"},{"context":"lint"}]}}]`
	protJSON  = `{"required_status_checks":{"contexts":["test","build"]}}`
)

func factsGH() *scriptedGH {
	return &scriptedGH{rules: []*ghRule{
		{prefix: "api repos/{owner}/{repo}/stacks?per_page=1", out: `[]`},
		{prefix: "api repos/{owner}/{repo}/rules/branches/main", out: rulesJSON},
		{prefix: "api repos/{owner}/{repo}/branches/main/protection", out: protJSON},
		{prefix: "api repos/{owner}/{repo}", out: repoJSON},
	}}
}

func TestReadFactsProbes(t *testing.T) {
	s := factsGH()
	g := &GitHub{Run: s.run}
	f, err := ReadFacts(context.Background(), g, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !f.StackedPRs || !f.AutoMerge || !f.MergeQueue || !f.Merge.Squash || f.Merge.Merge || f.Base != "main" {
		t.Errorf("facts = %+v", f)
	}
	if want := []string{"build", "lint", "test"}; !slices.Equal(f.RequiredChecks, want) {
		t.Errorf("required checks = %v, want %v", f.RequiredChecks, want)
	}
	if len(f.Unread) != 0 {
		t.Errorf("unread = %v", f.Unread)
	}
}

// Facts are cached per repo for FactsTTL: a second read inside it runs no
// gh, one after it probes again.
func TestReadFactsCachesForAnHour(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	defer func(f func() time.Time) { factsNow = f }(factsNow)
	factsNow = func() time.Time { return now }
	s := factsGH()
	g := &GitHub{Run: s.run}
	ctx := context.Background()
	if _, err := ReadFacts(ctx, g, "main"); err != nil {
		t.Fatal(err)
	}
	n := len(s.calls)
	now = now.Add(59 * time.Minute)
	if _, err := ReadFacts(ctx, g, "main"); err != nil || len(s.calls) != n {
		t.Fatalf("second read inside the TTL ran gh: %q (%v)", s.joined()[n:], err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := ReadFacts(ctx, g, "main"); err != nil || len(s.calls) == n {
		t.Fatalf("read past the TTL didn't probe again (%v)", err)
	}
}

// A repo without Stacked PRs answers 404; no admin access hides the
// protection. Neither is an error, and a failed probe is never cached.
func TestReadFactsDegrades(t *testing.T) {
	s := &scriptedGH{rules: []*ghRule{
		{prefix: "api repos/{owner}/{repo}/stacks", err: errors.New("gh api repos/{owner}/{repo}/stacks?per_page=1: exit status 1: gh: Not Found (HTTP 404)")},
		{prefix: "api repos/{owner}/{repo}/rules", out: `[]`},
		{prefix: "api repos/{owner}/{repo}/branches", err: errors.New("gh api: exit status 1: gh: Must have admin rights to Repository. (HTTP 403)")},
		{prefix: "api repos/{owner}/{repo}", out: `{"full_name":"o/r"}`},
	}}
	f, err := ReadFacts(context.Background(), &GitHub{Run: s.run}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if f.StackedPRs || f.MergeQueue || len(f.RequiredChecks) != 0 {
		t.Errorf("facts = %+v", f)
	}
	if len(f.Unread) != 2 {
		t.Errorf("unread = %v, want merge settings and branch protection", f.Unread)
	}

	s = &scriptedGH{rules: []*ghRule{{prefix: "api", err: errors.New("gh api: signal: killed")}}}
	g := &GitHub{Run: s.run}
	if _, err := ReadFacts(context.Background(), g, "main"); !Transient(err) {
		t.Fatalf("killed probe = %v, want transient", err)
	}
	if _, err := ReadFacts(context.Background(), g, "main"); err == nil {
		t.Fatal("a failed probe was cached")
	}
}

func TestReadFactsUnsupported(t *testing.T) {
	if _, err := ReadFacts(context.Background(), Unsupported{}, "main"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

// stackGH is a repo with Stacked PRs where PR 5 is in stack 3 with PR 4.
func stackGH(extra ...*ghRule) *scriptedGH {
	return &scriptedGH{rules: append(extra,
		&ghRule{prefix: "api repos/{owner}/{repo}/stacks?per_page=1", out: `[]`},
		&ghRule{prefix: "api repos/{owner}/{repo}/stacks?pull_request=5", out: `[{"number":3,"open":true,"pull_requests":[{"number":4},{"number":5}]}]`},
		&ghRule{prefix: "api repos/{owner}/{repo}/stacks?pull_request=", out: `[]`},
		&ghRule{prefix: "api repos/{owner}/{repo}", out: repoJSON},
		&ghRule{prefix: "api -X PUT", out: `{"status":"pending"}`},
		&ghRule{prefix: "pr view", out: `{"state":"MERGED"}`},
	)}
}

// A PR Facts says is stacked merges through merge-async without ever
// trying gh pr merge (#822).
func TestMergePRStackedNeverCallsGHPRMerge(t *testing.T) {
	s := stackGH(&ghRule{prefix: "pr merge", err: errors.New("must not run")})
	g := &GitHub{Run: s.run, MergePoll: []time.Duration{time.Millisecond}, Sleep: func(time.Duration) {}}
	if err := g.MergePR(context.Background(), "https://github.com/o/r/pull/5", "squash", "abc"); err != nil {
		t.Fatalf("MergePR = %v", err)
	}
	for _, c := range s.joined() {
		if strings.HasPrefix(c, "pr merge") {
			t.Fatalf("ran %q; calls %q", c, s.joined())
		}
	}
	if !slices.Contains(s.joined(), "api -X PUT repos/{owner}/{repo}/pulls/5/merge-async -f merge_method=squash -f sha=abc") {
		t.Fatalf("no async merge in %q", s.joined())
	}
}

// A PR outside any stack merges with gh pr merge as before.
func TestMergePRUnstackedUsesGHPRMerge(t *testing.T) {
	s := stackGH()
	g := &GitHub{Run: s.run}
	if err := g.MergePR(context.Background(), "9", "squash", ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.joined(), "pr merge 9 --squash") {
		t.Fatalf("calls %q", s.joined())
	}
}

// MergePR's refusals come back typed.
func TestMergePRRefusalTyped(t *testing.T) {
	s := &scriptedGH{rules: []*ghRule{
		{prefix: "pr merge", err: errors.New("gh pr merge: exit status 1: GraphQL: Head branch was modified. Review and try the merge again.")},
		{prefix: "api repos/{owner}/{repo}/stacks", err: errors.New("gh api: exit status 1: (HTTP 404)")},
	}}
	err := (&GitHub{Run: s.run}).MergePR(context.Background(), "9", "squash", "abc")
	if r, ok := RefusalOf(err); !ok || r != HeadMoved {
		t.Fatalf("MergePR = %v, want refused HeadMoved", err)
	}
}

// MovePR across stacks unlinks the PR's stack, edits its base, then links
// the target stack, in that order (#822).
func TestMovePRAcrossStacks(t *testing.T) {
	s := stackGH()
	err := MovePR(context.Background(), &GitHub{Run: s.run}, "5", "saddle/t7",
		NativeStack{Trunk: "main", PRs: []string{"7", "5"}})
	if err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, c := range s.joined() {
		if strings.HasPrefix(c, "stack ") || strings.HasPrefix(c, "pr edit") {
			writes = append(writes, c)
		}
	}
	want := []string{"stack unstack 3", "pr edit 5 --base saddle/t7", "stack link --base main 7 5"}
	if !slices.Equal(writes, want) {
		t.Fatalf("writes = %q, want %q", writes, want)
	}
}

// A PR in no stack just changes base; a forge without MovePR falls back to
// SetPRBase.
func TestMovePRUnstacked(t *testing.T) {
	s := stackGH()
	if err := MovePR(context.Background(), &GitHub{Run: s.run}, "9", "main", NativeStack{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.joined() {
		if strings.HasPrefix(c, "stack ") {
			t.Fatalf("ran %q for an unstacked PR", c)
		}
	}
	if !slices.Contains(s.joined(), "pr edit 9 --base main") {
		t.Fatalf("calls %q", s.joined())
	}
}

// noStacks is a facts cache that already knows the repo has no Stacked PRs,
// so a test's scripted gh answers aren't spent on the probe.
func noStacks() *factsCache {
	c := &factsCache{}
	c.put(Facts{ProbedAt: factsNow()})
	return c
}
