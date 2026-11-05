package forge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedGH answers each gh call by the first matching rule; a rule with
// times > 0 is used up after that many calls.
type scriptedGH struct {
	calls [][]string
	rules []*ghRule
}

type ghRule struct {
	prefix string // matched against the joined args
	out    string
	err    error
	times  int
}

func (s *scriptedGH) run(_ context.Context, args ...string) (string, error) {
	s.calls = append(s.calls, args)
	line := strings.Join(args, " ")
	for _, r := range s.rules {
		if r.times < 0 || !strings.HasPrefix(line, r.prefix) {
			continue
		}
		if r.times > 0 {
			if r.times--; r.times == 0 {
				r.times = -1
			}
		}
		return r.out, r.err
	}
	return "", nil
}

func (s *scriptedGH) joined() []string {
	var out []string
	for _, c := range s.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

var errAsyncRefusal = errors.New("gh pr merge: exit status 1: GraphQL: Pull request is part of a stack and must be merged using the asynchronous merge REST API (mergePullRequest)")

// A stacked PR that gh pr merge refuses goes through merge-async with the
// method and head, and the PR is polled until GitHub shows it merged (#838).
func TestGitHubMergePRFallsBackToMergeAsync(t *testing.T) {
	s := &scriptedGH{rules: []*ghRule{
		{prefix: "pr merge", err: errAsyncRefusal},
		{prefix: "api -X PUT", out: `{"status":"pending","details":{"uuid":"u-1"}}`},
		{prefix: "pr view", out: `{"state":"OPEN"}`, times: 1},
		{prefix: "pr view", out: `{"state":"MERGED"}`},
	}}
	var slept []time.Duration
	g := &GitHub{Run: s.run, MergePoll: []time.Duration{time.Second, 2 * time.Second, 3 * time.Second},
		Sleep: func(d time.Duration) { slept = append(slept, d) }}
	if err := g.MergePR(context.Background(), "https://github.com/o/r/pull/811", "squash", "abc123"); err != nil {
		t.Fatalf("MergePR = %v, want nil", err)
	}
	got := s.joined()
	want := "api -X PUT repos/{owner}/{repo}/pulls/811/merge-async -f merge_method=squash -f sha=abc123"
	if len(got) < 2 || got[1] != want {
		t.Fatalf("second call = %q, want %q (all: %q)", got[1], want, got)
	}
	if len(slept) != 2 {
		t.Fatalf("polled %d times (%v), want 2: once open, once merged", len(slept), slept)
	}
}

// A merge GitHub accepted but hasn't finished within the poll is pending,
// not failed.
func TestGitHubMergePRAsyncStillPending(t *testing.T) {
	s := &scriptedGH{rules: []*ghRule{
		{prefix: "pr merge", err: errAsyncRefusal},
		{prefix: "api -X PUT", out: `{"status":"pending"}`},
		{prefix: "pr view", out: `{"state":"OPEN"}`},
	}}
	g := &GitHub{Run: s.run, MergePoll: []time.Duration{time.Millisecond}, Sleep: func(time.Duration) {}}
	err := g.MergePR(context.Background(), "12", "squash", "")
	if !errors.Is(err, ErrMergePending) {
		t.Fatalf("MergePR = %v, want ErrMergePending", err)
	}
	if got := s.joined(); got[1] != "api -X PUT repos/{owner}/{repo}/pulls/12/merge-async -f merge_method=squash" {
		t.Fatalf("calls = %q", got)
	}
}

// A PR closed while the async merge ran is a real failure.
func TestGitHubMergePRAsyncClosed(t *testing.T) {
	s := &scriptedGH{rules: []*ghRule{
		{prefix: "pr merge", err: errAsyncRefusal},
		{prefix: "api -X PUT", out: `{"status":"pending"}`},
		{prefix: "pr view", out: `{"state":"CLOSED"}`},
	}}
	g := &GitHub{Run: s.run, MergePoll: []time.Duration{time.Millisecond}, Sleep: func(time.Duration) {}}
	err := g.MergePR(context.Background(), "12", "squash", "")
	if err == nil || errors.Is(err, ErrMergePending) {
		t.Fatalf("MergePR = %v, want a failure", err)
	}
}

// Other merge errors are returned as they were, with no async attempt.
func TestGitHubMergePROtherErrorNoAsync(t *testing.T) {
	boom := errors.New("gh pr merge: Pull request is not mergeable")
	s := &scriptedGH{rules: []*ghRule{{prefix: "pr merge", err: boom}}}
	g := &GitHub{Run: s.run, Sleep: func(time.Duration) {}}
	if err := g.MergePR(context.Background(), "12", "squash", ""); !errors.Is(err, boom) {
		t.Fatalf("MergePR = %v, want %v", err, boom)
	}
	if len(s.calls) != 1 {
		t.Fatalf("calls = %q, want only gh pr merge", s.joined())
	}
}

// saddle/base only gates merge order; no rollup saddle reads carries it, so
// nothing counts it as CI (#838).
func TestSaddleBaseIsNotCI(t *testing.T) {
	ctx := context.Background()
	view := `{"state":"OPEN","statusCheckRollup":[` +
		`{"__typename":"StatusContext","context":"saddle/base","state":"FAILURE"},` +
		`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`
	g := &GitHub{Run: (&fakeGH{replies: []string{view}}).run}
	pr, err := g.PR(ctx, "1", PRState|PRChecks)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Checks) != 1 || pr.Checks[0].Name != "test" {
		t.Fatalf("PR checks = %+v, want only test", pr.Checks)
	}

	checks := `[{"name":"saddle/base","workflow":"","bucket":"fail","link":""},{"name":"test","workflow":"ci","bucket":"pass","link":"l"}]`
	g = &GitHub{Run: (&fakeGH{replies: []string{checks}}).run}
	cs, err := g.Checks(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "test" {
		t.Fatalf("Checks = %+v, want only test", cs)
	}

	page := `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"url":"u","state":"OPEN",` +
		`"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"StatusContext","context":"saddle/base","state":"FAILURE"}]}}}]}}}}`
	prs, _, err := parseOpenPRs(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || len(prs[0].Checks) != 0 {
		t.Fatalf("open PRs = %+v, want no checks", prs)
	}
	if !SaddleOwned(CheckRun{Context: BaseStatus}) || SaddleOwned(CheckRun{Name: "test"}) {
		t.Fatal("SaddleOwned misclassifies")
	}
}
