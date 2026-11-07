package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// GitHub's native stacks (Stacked PRs), as MergePR and MovePR need them.
// The gh-stack driver for everything else is internal/ghstack.

// stacksAPI is the repo's stacks REST endpoint; gh fills in owner and repo.
const stacksAPI = "repos/{owner}/{repo}/stacks"

// MergePR never passes --admin, so branch protection always applies. A PR
// in a native stack goes the way GitHub requires, the async merge API,
// without trying gh pr merge first; the stack is looked up only when Facts
// says the repo has Stacked PRs. A refusal for want of the async API (stale
// facts) falls back to it too. ErrMergePending means GitHub is still
// merging it (#822, #838).
func (g *GitHub) MergePR(ctx context.Context, ref, method, head string) error {
	if g.stackOf(ctx, ref) != 0 {
		return g.mergeAsync(ctx, ref, method, head)
	}
	args := []string{"pr", "merge", ref, "--" + method}
	if head != "" {
		args = append(args, "--match-head-commit", head)
	}
	_, err := g.run(ctx, args...)
	if r, _ := RefusalOf(err); r == StackedNeedsAsyncMerge {
		return g.mergeAsync(ctx, ref, method, head)
	}
	return err
}

// stackOf is the number of the open native stack holding PR ref, 0 when
// none does, the repo has no Stacked PRs, or GitHub can't say.
func (g *GitHub) stackOf(ctx context.Context, ref string) int {
	n, ok := prNumber(ref)
	if !ok {
		return 0
	}
	if f, err := g.Facts(ctx, ""); err != nil || !f.StackedPRs {
		return 0
	}
	out, err := g.run(ctx, "api", stacksAPI+"?pull_request="+strconv.Itoa(n))
	if err != nil {
		return 0
	}
	var stacks []struct {
		Number int  `json:"number"`
		Open   bool `json:"open"`
		PRs    []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	}
	if json.Unmarshal([]byte(out), &stacks) != nil {
		return 0
	}
	for _, s := range stacks {
		if !s.Open {
			continue
		}
		for _, p := range s.PRs {
			if p.Number == n {
				return s.Number
			}
		}
	}
	return 0
}

// NativeStack is the native stack a PR should end up in: its PRs (numbers
// or URLs, never branches) bottom to top, the bottom one based on Trunk.
// The zero NativeStack is none.
type NativeStack struct {
	Trunk string
	PRs   []string
}

// prMover is a forge that moves PRs between native stacks.
type prMover interface {
	MovePR(ctx context.Context, ref, base string, stack NativeStack) error
}

// MovePR points PR ref at base and leaves it in stack (none when zero). On
// GitHub a PR in a native stack can't change base (StackedBaseLocked), so
// MovePR unstacks the stack holding it, edits the base, then links stack.
// Each step leaves a valid state: the reconciler's next tick relinks
// whatever a failure in between left unlinked, including the rest of the
// old stack (ADR 0009). A forge without native stacks just sets the base.
func MovePR(ctx context.Context, f Forge, ref, base string, stack NativeStack) error {
	if m, ok := f.(prMover); ok {
		return m.MovePR(ctx, ref, base, stack)
	}
	return Classify(f.SetPRBase(ctx, ref, base))
}

func (g *GitHub) MovePR(ctx context.Context, ref, base string, stack NativeStack) error {
	if n := g.stackOf(ctx, ref); n != 0 {
		if _, err := g.run(ctx, "stack", "unstack", strconv.Itoa(n)); err != nil {
			return err
		}
	}
	if err := g.SetPRBase(ctx, ref, base); err != nil {
		return err
	}
	if len(stack.PRs) < 2 {
		return nil
	}
	args := []string{"stack", "link"}
	if stack.Trunk != "" {
		args = append(args, "--base", stack.Trunk)
	}
	if _, err := g.run(ctx, append(args, stack.PRs...)...); err != nil {
		return fmt.Errorf("link %v after moving %s onto %s: %w", stack.PRs, ref, base, err)
	}
	return nil
}
