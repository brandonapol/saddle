package forge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrMergePending is what MergePR returns when GitHub accepted a merge but
// hasn't finished it yet: the merge is in progress, not failed (the
// "pending" outcome of #822). The caller checks the PR's state later.
var ErrMergePending = errors.New("merge accepted; GitHub is still merging it")

// DefaultMergePoll are the waits between reads of a PR's state after an
// async merge, about half a minute in all.
var DefaultMergePoll = []time.Duration{2 * time.Second, 3 * time.Second, 5 * time.Second, 5 * time.Second, 10 * time.Second}

// prNumber is the number a PR ref names: 12, #12 or a /pull/12 URL.
func prNumber(ref string) (int, bool) {
	n := strings.TrimPrefix(ref, "#")
	if i := strings.LastIndex(n, "/pull/"); i >= 0 {
		n = n[i+len("/pull/"):]
	}
	n, _, _ = strings.Cut(n, "/")
	v, err := strconv.Atoi(n)
	return v, err == nil && v > 0
}

func (g *GitHub) sleep(d time.Duration) {
	if g.Sleep != nil {
		g.Sleep(d)
		return
	}
	time.Sleep(d)
}

// MergePRAsync merges ref through the async merge API without trying gh pr
// merge first, for a caller GitHub already told that ref merges only that
// way (forge.StackedNeedsAsyncMerge, #826). Outcomes are MergePR's.
func (g *GitHub) MergePRAsync(ctx context.Context, ref, method, head string) error {
	return g.mergeAsync(ctx, ref, method, head)
}

// mergeAsync merges ref with PUT pulls/{n}/merge-async, the only merge
// GitHub accepts for a PR in a native stack, then reads the PR's state until
// it shows MERGED. head, when set, is passed as sha so GitHub merges only
// that head. Still open after the poll is ErrMergePending.
func (g *GitHub) mergeAsync(ctx context.Context, ref, method, head string) error {
	n, ok := prNumber(ref)
	if !ok {
		p, err := g.PR(ctx, ref, PRNumber)
		if err != nil {
			return err
		}
		n = p.Number
	}
	args := []string{"api", "-X", "PUT", fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/merge-async", n), "-f", "merge_method=" + method}
	if head != "" {
		args = append(args, "-f", "sha="+head)
	}
	if _, err := g.run(ctx, args...); err != nil {
		return err
	}
	poll := g.MergePoll
	if poll == nil {
		poll = DefaultMergePoll
	}
	for _, d := range poll {
		g.sleep(d)
		p, err := g.PR(ctx, ref, PRState)
		if err != nil {
			continue // a failed read says nothing about the merge
		}
		switch p.State {
		case StateMerged:
			return nil
		case StateClosed:
			return &RefusedError{Reason: PRClosed, Err: fmt.Errorf("async merge of #%d: the PR was closed instead of merged", n)}
		}
	}
	return fmt.Errorf("#%d: %w", n, ErrMergePending)
}
