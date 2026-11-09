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
	g := &GitHub{Run: s.run, facts: noStacks(), MergePoll: []time.Duration{time.Second, 2 * time.Second, 3 * time.Second},
		Sleep: func(d time.Duration) { slept = append(slept, d) }}
	if err := g.MergePR(context.Background(), "https://github.com/o/r/pull/811", "squash", "abc123"); err != nil {
		t.Fatalf("MergePR = %v, want nil", err)
	}
	got := s.joined()
	want := []string{"pr merge https://github.com/o/r/pull/811 --squash --match-head-commit abc123",
		"api -X PUT repos/{owner}/{repo}/pulls/811/merge-async -f merge_method=squash -f sha=abc123"}
	if len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("calls = %q, want %q first", got, want)
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
	g := &GitHub{Run: s.run, facts: noStacks(), MergePoll: []time.Duration{time.Millisecond}, Sleep: func(time.Duration) {}}
	err := g.MergePR(context.Background(), "12", "squash", "")
	if !errors.Is(err, ErrMergePending) {
		t.Fatalf("MergePR = %v, want ErrMergePending", err)
	}
	if got := s.joined(); len(got) < 2 || got[1] != "api -X PUT repos/{owner}/{repo}/pulls/12/merge-async -f merge_method=squash" {
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
	g := &GitHub{Run: s.run, facts: noStacks(), MergePoll: []time.Duration{time.Millisecond}, Sleep: func(time.Duration) {}}
	err := g.MergePR(context.Background(), "12", "squash", "")
	if err == nil || errors.Is(err, ErrMergePending) {
		t.Fatalf("MergePR = %v, want a failure", err)
	}
}

// Other merge errors are returned as they were, with no async attempt.
func TestGitHubMergePROtherErrorNoAsync(t *testing.T) {
	boom := errors.New("gh pr merge: Pull request is not mergeable")
	s := &scriptedGH{rules: []*ghRule{{prefix: "pr merge", err: boom}}}
	g := &GitHub{Run: s.run, facts: noStacks(), Sleep: func(time.Duration) {}}
	if err := g.MergePR(context.Background(), "12", "squash", ""); !errors.Is(err, boom) {
		t.Fatalf("MergePR = %v, want %v", err, boom)
	}
	if got := s.joined(); len(got) != 1 || got[0] != "pr merge 12 --squash" {
		t.Fatalf("calls = %q, want only gh pr merge", got)
	}
}
