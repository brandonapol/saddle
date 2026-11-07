package forge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/netx"
)

// Fixtures of what gh really printed, as CLI wraps it: "gh <op>: <exec
// error>: <stderr>".
var (
	// PR #811, event 73601.
	errFixtureAsyncMerge = errors.New("gh pr merge: exit status 1: GraphQL: Pull request is part of a stack and must be merged using the asynchronous merge REST API (mergePullRequest)")
	// #341, #814.
	errFixtureBaseLocked = errors.New("gh pr edit: exit status 1: GraphQL: Cannot change the base branch because the pull request is part of a stack. (updatePullRequest)")
	// Events 72474, 73218, 73247.
	errFixtureKilled = errors.New("gh pr checks: signal: killed: ")
)

func TestClassifyRefusals(t *testing.T) {
	cases := []struct {
		err  error
		want Reason
	}{
		{errFixtureAsyncMerge, StackedNeedsAsyncMerge},
		{errFixtureBaseLocked, StackedBaseLocked},
		{errors.New("gh pr edit: exit status 1: GraphQL: Cannot change the base branch of a pull request in the merge queue. (updatePullRequest)"), InMergeQueue},
		{errors.New("gh pr merge: exit status 1: GraphQL: Required status check \"test\" is failing. (mergePullRequest)"), RequiredCheckFailing},
		{errors.New("gh pr merge: exit status 1: GraphQL: At least 1 approving review is required by reviewers with write access. (mergePullRequest)"), ReviewRequired},
		{errors.New("gh pr merge: exit status 1: GraphQL: Head branch was modified. Review and try the merge again. (mergePullRequest)"), HeadMoved},
		{errors.New("gh pr edit: exit status 1: GraphQL: Cannot change the base branch of a closed pull request. (updatePullRequest)"), PRClosed},
		{errors.New("gh pr create: exit status 1: a pull request for branch \"b\" into branch \"main\" already exists:\nhttps://github.com/o/r/pull/9"), PRExists},
		{errors.New("gh api repos/{owner}/{repo}/stacks: exit status 1: gh: Not Found (HTTP 404)"), NotFound},
		{errors.New(`gh stack link: exit status 1: unknown command "stack" for "gh"`), StackToolMissing},
		{errors.New("gh stack link: exit status 1: stacked PRs are not enabled for this repository"), StacksDisabled},
		{errors.New("gh stack link: exit status 1: PR #4 belongs to a different stack"), StackConflict},
	}
	for _, c := range cases {
		got := Classify(c.err)
		r, ok := RefusalOf(got)
		if !ok || r != c.want {
			t.Errorf("Classify(%q) = %v (reason %v, refused %v), want refused %v", c.err, got, r, ok, c.want)
		}
		if !errors.Is(got, ErrRefused) || errors.Is(got, ErrTransient) || errors.Is(got, ErrDeterministic) {
			t.Errorf("Classify(%q) matches the wrong outcome", c.err)
		}
		if !errors.Is(got, c.err) {
			t.Errorf("Classify(%q) lost the raw error", c.err)
		}
		if got.Error() != c.err.Error() {
			t.Errorf("Classify changed the message: %q", got.Error())
		}
	}
}

func TestClassifyTransient(t *testing.T) {
	for _, err := range []error{
		errFixtureKilled,
		errors.New("gh pr merge: exit status 1: HTTP 502: Bad Gateway"),
		errors.New("gh api graphql: exit status 1: Post \"https://api.github.com/graphql\": read tcp: connection reset by peer"),
		fmt.Errorf("gh pr view: %w", &netx.TimeoutError{Op: "gh pr view", After: time.Minute}),
		context.Canceled,
	} {
		got := Classify(err)
		if !errors.Is(got, ErrTransient) || !Transient(got) {
			t.Errorf("Classify(%q) = %v, want ErrTransient", err, got)
		}
		if _, ok := RefusalOf(got); ok {
			t.Errorf("Classify(%q) is a refusal", err)
		}
	}
	// A timeout stays a timeout.
	if !errors.Is(Classify(&netx.TimeoutError{Op: "x"}), ErrNetTimeout) {
		t.Error("a classified timeout no longer matches ErrNetTimeout")
	}
}

func TestClassifyRateLimitAndDeterministic(t *testing.T) {
	until := time.Now().Add(time.Hour)
	rl := &RateLimitError{Until: until}
	if got := Classify(rl); got != error(rl) {
		t.Errorf("Classify(rate limit) = %v, want it unchanged", got)
	}
	got := Classify(errors.New("gh pr merge: exit status 1: API rate limit exceeded for user ID 1"))
	if u, ok := RateLimitUntil(got); !ok || !u.After(time.Now()) || !Transient(got) {
		t.Errorf("rate-limit text = %v, want a RateLimitError in the future", got)
	}

	raw := errors.New("gh pr merge: exit status 1: Pull request is not mergeable")
	got = Classify(raw)
	if !errors.Is(got, ErrDeterministic) || errors.Is(got, ErrTransient) || errors.Is(got, ErrRefused) {
		t.Errorf("Classify(%q) = %v, want ErrDeterministic", raw, got)
	}
	if got.Error() != raw.Error() || !errors.Is(got, raw) {
		t.Errorf("deterministic outcome lost the raw message: %q", got)
	}
	if Classify(nil) != nil {
		t.Error("Classify(nil) != nil")
	}
	// Classifying twice changes nothing.
	once := Classify(errFixtureBaseLocked)
	if twice := Classify(once); twice != once {
		t.Errorf("Classify isn't idempotent: %v then %v", once, twice)
	}
	if !errors.Is(Classify(fmt.Errorf("x: %w", ErrUnsupported)), ErrUnsupported) {
		t.Error("ErrUnsupported lost")
	}
}

func TestReasonString(t *testing.T) {
	for r := Reason(1); r < reasonEnd; r++ {
		if s := r.String(); s == "" || s == "unknown" {
			t.Errorf("Reason(%d) has no name", r)
		}
	}
}
